# 分块上传实现审查（chunked upload race review）

审查对象：`go-backend/internal/{api/uploads.go, repository/uploads.go, repository/quota.go, storage/chunks.go}`
与前端 `src/utils/uploads.ts`。结论：**主流程正确**，并发路径由「短事务 + CAS SQL + 文件原子 rename」保证；
发现 4 处实质问题（含 2.4 前端、2.5 init 重试死代码）与 3 处理论风险，实质问题均已修复并补测试。

## 1. 并发模型（现状核对）

| 步骤 | 并发手段 | 结论 |
| --- | --- | --- |
| `POST /api/uploads/init` | 配额/会话数/磁盘水位三门禁 + `(environment_id, request_id)` 部分唯一索引 + 唯一冲突后有界重试（重读赢家或自己占用键槽，见 2.5） | 竞态下只有一个 session 建立，配额 CAS（`reserved_bytes + ? <= ?`）不会超卖 |
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

### 2.2 过期清理可能删除「正在 COMPLETE」的容器

`PurgeExpiredUploads` 原先不带状态过滤。若 TTL 恰好在 COMPLETE 期间到期，清理线程会删掉行并
`os.Remove(staged_path)`，随后 clip 插入成功 → clip 指向一个已被 unlink 的文件（下载报
「文件已丢失」）。修复（本分支已改）：清理对 `completing` 行给**宽限期保护**而不是永久跳过——
新实现里分块在 PUT 阶段就落盘并 fsync，COMPLETE 不再做文件 IO，`completing` 只是毫秒级并发闸门，
永久跳过会让被硬杀残留的行**一直占着预留**（账本里它的字节仍然有效），所以超过
`CompletingPurgeGraceSeconds`（900s）就回收；仍在宽限期内（可能是进行中的 clip INSERT）则保护。
见 `docs/upload-quota.md` §4.1。
测试：`TestPurgeExpiredUploadsSkipsCompleting`（保护）、`TestPurgeReclaimsStaleCompletingAfterGrace`（回收）。

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

### 2.5 `CreateOrGetUploadSession` 的重试循环形同虚设（死代码 + 可恢复的 init 被丢弃）

审查发现的第 4 处实质问题：`CreateOrGetUploadSession` 里的
`for attempt := 0; attempt < 3; attempt++` **循环体每一条分支都会 `return`**，
因此循环永远只跑一轮：

* 它不是「无限循环」类 bug —— 控制流是有界的，既有测试（含
  `TestInitReplayRefusesStaleInFlightSession`）覆盖的行为全部正确；
* 但它**谎报了重试语义**：循环写法 + 循环外那句 “Should be unreachable for bounded
  retries” 都暗示存在重试，实际不存在。代码与注释互相矛盾，属可维护性缺陷。

同时它掩盖了一个真实（罕见）的失败窗口：唯一索引冲突后旧实现直接调用
`replayExistingSession`——如果赢家在「我方插入失败」与「重读」之间消失了
（清理 worker 恰好回收了已过期的赢家），重读拿不到任何行，于是整个 init 以
`unable to create upload session (idempotency conflict)` 失败（API 层 500），
**尽管此刻重新插入一定能成功**。

修复：把循环变成它本来应该代表的东西 —— 有界重试（`maxInitAttempts = 3`）：

```go
for attempt := 0; attempt < maxInitAttempts; attempt++ {
    // ... 读键：命中存活赢家 → 直接重放（return）
    // ... 过期键：先回收（无法回收则立即 SessionUnavailableError，不重试）
    // ... 插入：成功 → return；非唯一索引错误 / 无 requestId → return
    // 仅当「插入因 UNIQUE 冲突失败且带 requestId」才进入下一轮：
    // 下一轮重新读键，于是要么重放赢家，要么自己插入成功并占用键槽
}
return r.replayExistingSession(env, requestID) // 重试耗尽：按键的当前归属上报
```

只有「UNIQUE 冲突 + 有 requestId」可重试；校验错误、DB/IO 错误、以及处于 `completing`
的过期赢家（永久条件）一律立即返回，因此循环不会空转，`INSERT` 最多执行
`maxInitAttempts` 次。

测试（`internal/repository/uploads_init_test.go`，用 SQLite 触发器确定性地复现竞态前
提，`RAISE(FAIL)` 能在语句中止的同时保留触发器自身的写入）：

| 用例 | 断言 |
| --- | --- |
| `TestCreateOrGetUploadSessionRetriesTransientUniqueConflict` | 首次插入被 UNIQUE 冲突拒绝且键槽为空 → 重试后 `created=true`、只有 1 行 session、键可用；再次调用走重放（不再插入） |
| `TestCreateOrGetUploadSessionRetryReplaysWinner` | 首次插入冲突时并发赢家出现 → 重试返回**赢家**（`created=false`、id 为赢家 id），键槽仍只有 1 行 |
| `TestCreateOrGetUploadSessionRetriesAreBounded` | 永久冲突时恰好尝试 `maxInitAttempts` 次后返回错误，不空转，仓库保持可用 |
| `TestCreateOrGetUploadSessionNonRetryableErrorsReturnImmediately` | 非 UNIQUE 失败只尝试 1 次；`completing` 过期赢家立即返回 `SessionUnavailableError` |

回归证据：把函数体临时还原成「死循环」版本后，
`RetriesTransientUniqueConflict` 报 `unable to create upload session (idempotency conflict)`、
`RetriesAreBounded` 报 `got 1` 次尝试 —— 均按预期失败，证明测试确实锁住了这次修复。

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
