# 分块上传实现审查（chunked upload race review）

审查对象：`go-backend/internal/{api/uploads.go, repository/uploads.go, repository/quota.go, storage/chunks.go}`
与前端 `src/utils/uploads.ts`。结论：**主流程正确**，并发路径由「短事务 + CAS SQL + 文件原子 rename」保证；
发现 3 处实质问题与 3 处理论风险，前 3 处已修复并补测试。

## 1. 并发模型（现状核对）

| 步骤 | 并发手段 | 结论 |
| --- | --- | --- |
| `POST /api/uploads/init` | 配额/会话数/磁盘水位三门禁 + `(environment_id, request_id)` 部分唯一索引 + 唯一冲突后重读 | 竞态下只有一个 session 建立，配额 CAS（`reserved_bytes + ? <= ?`）不会超卖 |
| `PUT .../chunks/{index}` | 先写临时文件再 `rename`（原子），随后短事务 upsert 收据；无全局锁包住文件 IO | 重传幂等；并发 PUT 之间不互相破坏 |
| `POST .../complete` | `TryBeginComplete`：`UPDATE ... WHERE status='active'`（CAS）抢占 `active→completing`；组装在锁外 | 并发 complete 只有一个能进入合并，其余 409/200 重放 |
| 取消 / 过期 | `AbortUploadSession` 拒绝 `completing`；`PurgeExpiredUploads` 幂等归还配额 | 不会拆掉进行中的合并 |
| 崩溃恢复 | 启动时 `ResetStuckCompleting` + `reconcileDiskState` + 孤儿目录清扫 | 断电后 `completing` 行回滚为 `active` |

`go test -race ./...` 全绿（api 6.5s / repository 2.8s 均无 data race）。

## 2. 已修复的问题

### 2.1 `FailComplete` 曾经把配额预留还回去（配额可被绕过）

`complete` 失败回滚（accessCode 被并发抢走、token 失效、上传期间过期）时，session 回到 `active`
且**分片仍留在磁盘上**，但旧实现同时调用 `releaseQuotaTx`，于是这些字节既占磁盘又不占配额：

* 攻击者可：用同一个 accessCode init 两个 session → 先 complete 成功 → 再 complete 失败回滚，
  第二个 session 的 `file_size` 变为「免费」占用，直到 session TTL 到期才被清理；
* 更隐蔽的是 `RecomputeUploadQuota()`（`SUM(file_size) WHERE status='active' AND quota_released=0`）
  与账本从此不再一致 —— 即使重启也不会自我修复。

修复：预留只在**终态**归还（complete 成功 / DELETE / 过期清理），回滚保留预留。
测试：`TestFailCompleteKeepsQuotaReservationUntilTerminal`（repo）、
`TestQuotaHeldAfterCompleteRollback`（api，含 `RecomputeUploadQuota` 对账）。

### 2.2 过期清理可能删除「正在合并」的 staged 文件

`PurgeExpiredUploads` 原先不带状态过滤。若 TTL 恰好在组装过程中到期，清理线程会删掉行并
`os.Remove(staged_path)`，随后 clip 插入成功 → clip 指向一个已被 unlink 的文件（下载报
「文件已丢失」）。修复：清理跳过 `completing` 行（与 `AbortUploadSession` 的拒绝语义一致）；
合并失败会回滚为 `active`，下一次清理即可回收，进程崩溃的场景由启动恢复兜底。
测试：`TestPurgeExpiredUploadsSkipsCompleting`。

### 2.3 init 幂等重放可能返回「不可用的 session」

`(env, requestId)` 槽位被一个**已过期但处于 `completing`** 的 session 占用时，
`AbortUploadSession` 会拒绝回收，旧实现随即撞唯一索引、再读到同一行，最终给客户端
`200 + uploadId`——而该 id 的 `/complete` 只会 409/410。
修复：新增 `repository.SessionUnavailableError`，`CreateOrGetUploadSession`（含输掉插入竞态的重读路径）
不再返回过期行，handler 映射为 **409「上一个上传会话仍在处理中，请稍后重试」**。
测试：`TestInitReplayRefusesStaleInFlightSession`（repo）、`TestInitReplayStaleInFlightIsConflict`（api）。

### 2.4 前端：冻结参数导致的「重试死循环」

参数在 init 冻结后，若 complete 因 accessCode 冲突回滚，前端仍保存 `(env,中文件名,大小,mtime)` 维度的
resume 槽——用户换个短码重试会重放旧 session，永远 409。
修复：`buildResumeKey` 纳入 accessCode / token 指纹；`isUnrecoverableCompleteFailure`
（409/410/无 `missing` 的 400）会 best-effort `DELETE` 掉回滚后的 session 并清除 resume 状态，
而分片缺失（带 `missing` 的 400）与网络抖动仍保持可续传。

## 3. 新增的竞态探针

`internal/api/uploads_race_test.go`：

* `TestConcurrentChunkVsCompleteIsRaceFree`：最后一分片 PUT 与 complete 并发 6 轮。
  合法结果只有三种（PUT 先到 → 201；complete 见到缺口 → 400 且 `missing` 恰为该分片、重传后成功；
  另一个 completer 已提交 → 重放同一 clip）。断言：无 5xx、clip 字节与原始文件完全一致、
  该 environment 只有一个 clip、session `completed` 且 `clip_id` 指向它。
* `TestConcurrentCompleteExactlyOnce` / `TestConcurrentDeleteVsComplete`（既有）继续覆盖
  「N 个 complete 只产生 1 个 clip」「取消与合并互斥、不留孤儿文件」。

## 4. 仍存在的理论风险（未改，供决策）

1. **最终文件名的微秒时间戳**：`storage.FinalStoragePath` 用 `20060102150405` + 微秒生成文件名，
   两个**同名同 MIME** 的上传若在同一微秒内计算出路径，会 rename 到同一目标（内容互相覆盖，
   两个 clip 指向同一文件）。单进程内窗口 1µs，实际不可达；改动会破坏与 Python 版「磁盘布局兼容」，
   故保留。若未来要彻底消除：追加一个短随机后缀。
2. **完成后的 `upload_chunks` 收据**：complete 成功后只删除分片目录，收据行保留到 session 过期清理
   （`PurgeExpiredUploads` 会一并删除）。属于有界的元数据残留，不影响正确性。
3. **多实例部署**：`r.mu` 是进程内的；配额与状态转换另有 SQL CAS 保护，但同一 SQLite 文件被多个
   进程同时写入仍属非受支持用法（`busy_timeout=10s` 仅缓解）。
