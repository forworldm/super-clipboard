# 上传成功后的会话保留窗口（保留时长 + 幂等重放）

分支：`w-dev` · 前置提交：`avoid chunk files`（分块直写预分配文件、数据库为唯一进度来源）
本文档记录四项审计/改动：配额一致性审查、purge 二次释放回归测试、成功会话保留行为测试、
以及成功后 `expires_at` 收缩并让幂等路径忽略 `expires_at`。

## 1. 配额生命周期审查（与活动上传会话的交互）

预留（reserve）与释放（release）的**唯一入口**如下，均为短事务、CAS 语义，文件 IO 在锁外：

| 时机 | 代码 | 配额动作 | 幂等保证 |
| --- | --- | --- | --- |
| `POST /api/uploads/init` | `ReserveUploadQuota(fileSize, limit)` | `reserved_bytes += fileSize`，`reserved_bytes + fileSize <= limit` 作为 UPDATE 谓词 | 失败（507）时账本不动 |
| init 中预留后、行落库前的任何提前返回 | `CompensateUploadQuota(fileSize)`（`defer`） | `reserved_bytes -= fileSize`，仅当 `reserved_bytes >= fileSize` | 第二次调用返回 `false`，永不写负 |
| 会话行落库成功后容器创建失败 | `AbortUploadSession` | 行内 `quota_released` CAS 命中后 `-= fileSize` | `reservationHeld=false`，`defer` 不再补偿 |
| `DELETE /api/uploads/{id}` | `AbortUploadSession` | 同上 | `quota_released` CAS，重复 DELETE 404 且不动账本 |
| `POST .../complete` 成功 | `CompleteUploadSession`（同一事务） | 同上，且 `expires_at` 收缩为短窗口 | CAS 命中即释放，崩溃也不会永久占用 |
| complete 失败回滚 | `FailComplete` | **不释放**（会话回到 `active`，字节仍在盘上） | 只有终态释放，`RecomputeUploadQuota` 与之一致 |
| 过期清理（进程启动 + 周期 worker） | `PurgeExpiredUploads` → `releaseQuotaTx` | 删行前释放一次 | 行内 CAS：已成功释放过的行不会二次扣减 |
| 对账 | `RecomputeUploadQuota` | 用 `SUM(file_size) WHERE status='active' AND quota_released=0` 覆盖账本 | 只修计数，不动任何文件 |

**审查结论（错误路径一致性）**

1. init 的每一条提前返回路径（快照复用、业务校验、验证码、507、磁盘水位、插入冲突）都恰好归还一次预留：
   `reservationHeld` 只在 `CreateOrGetUploadSession` 返回 `created=true` 时置为 `false`（此时预留已由行持有），
   其余路径由 `defer` 补偿一次。`CompensateUploadQuota` 带 `reserved_bytes >= fileSize` 谓词，
   即使被重复调用也只会返回 `false`，不会吞掉别人的预留（新增测试
   `TestCompensateUploadQuotaNeverEatsLiveReservations`）。
2. 行已落库、容器创建失败的路径（`PreallocateFile` 失败）此前只做 `_, _ = AbortUploadSession(...)`，
   错误被丢弃；现在改为记录日志并显式走 `AbortUploadSession`（行持有预留 → 由行的 CAS 释放，
   `defer` 不再补偿），可注入失败点由 `App.preallocateUploadFn` 提供（新增测试
   `TestInitPreallocationFailureReturnsReservation`：哨兵 700B 保持不动、行与容器都不残留、同一 `requestId` 可立即重试成功）。
3. complete 失败必须保留预留：字节仍在盘上、会话回到 `active`，释放会形成“最多一个 TTL 的配额绕过”，
   并与对账 SQL 矛盾（既有测试 `TestFailCompleteKeepsReservationUntilTerminal` / `QuotaHeldAfterCompleteRollback` 锁定）。
4. 成功会话的释放发生在 `CompleteUploadSession` 的同一事务内；行随后被 purge 删除时携带的
   `quota_released=1` 让 CAS 必然落空，因此**不会**二次扣减（见第 2 节测试）。
5. 容器文件只在“行先落库、文件后创建”的顺序下产生，周期 worker 新增的孤儿文件回收
   （`sweepOrphanUploadFiles`）因此不可能删掉进行中的上传。

## 2. purge 不会二次释放已成功会话的配额

`PurgeExpiredUploads` 无条件对受害者调用 `releaseQuotaTx`，正确性完全依赖行内 CAS：

```sql
UPDATE upload_sessions SET quota_released = 1 WHERE id = ? AND quota_released = 0
-- RowsAffected == 1 时才 reserved_bytes -= file_size
```

回归测试（两层，均使用“哨兵预留”使任何二次扣减可见）：

- repository：`TestPurgeExpiredCompletedUploadDoesNotReleaseQuotaTwice`
  —— 5000B 活跃哨兵 + 2048B 上传；complete 后账本 == 5000；强制过期后 purge，
  账本仍 == 5000；purge 后 `ReleaseUploadQuota` / `AbortUploadSession` 均为无操作，第二次 purge 无受害者。
- api：`TestPurgeAfterCompleteDoesNotReleaseQuotaTwice`
  —— 经 `completeUpload` → `purgeExpiredUploadsPeriodic()`，账本保持 1200B 哨兵；
  clip 行与 clip 文件在 purge 后仍可下载；删除 clip 只归还哨兵。

## 3. 成功会话被保留（客户端重试幂等）

成功会话**不再被使用**（clip 才是交付物），但**不删除**：它是 `(environment_id, request_id)`
的幂等凭据，客户端丢响应后重试必须拿到同一个 `uploadId` 与同一个 clip。测试：

- 既有：`TestCompleteIdempotencyAfterLostResponse`（丢失响应后 GET → complete 重放，不产生第二个 clip）。
- 新增：`TestCompletedSessionRetainedForIdempotentRetry`（api）逐条锁定：
  成功后行仍在（`status=completed`、`clip_id` 已写入、`quota_released=1`）；
  重放期内重试 init 得到同一 `uploadId`（200 而非 201）、GET 仍返回 `completed`、complete 仍返回原 clip；
  会话数/容器数始终为 1、账本为 0、clip 列表只有 1 条；purge 之后 clip 与文件存活、`requestId` 可被新上传复用。

## 4. 成功后 `expires_at` 收缩为短窗口；幂等路径不再读 `expires_at`

- 配置：`CompletedUploadTTLSeconds`（`SUPER_CLIPBOARD_UPLOAD_COMPLETED_TTL_SECONDS`，默认 **300s**），
  `EffectiveCompletedUploadTTL()` 会把它夹在 `[1s, EffectiveUploadTTL()]` 之间。
- `CompleteUploadSession` 在同一事务里执行
  `expires_at = min(now + CompletedUploadTTL, 原 expires_at)`：
  成功会话不再占用 24h 的 resume 窗口，行与 `(environment_id, request_id)` 槽位在几分钟内被清理。
- `UploadSession.AcceptsReplay(now)`：**`completed` 会话无条件可重放**（`expires_at` 只决定“何时删行”，
  不决定“重试怎么答”）；`active` / `completing` 仍按 resume TTL 判定过期。
  这样即便重试发生在窗口之后、worker 清理之前，也不会因为 `expires_at` 而开出第二个会话/第二份文件/第二个 clip。
- 应用点：`handleInitUpload`（幂等预检）、`loadActiveUpload`（GET/PUT/complete 的 404/410 判定）、
  `CreateOrGetUploadSession` 与 `replayExistingSession`（并发插入竞争后的复用）。
- 测试：`TestCompletedUploadTTL`（config 边界）、`TestCompleteShortensSessionExpiry`、
  `TestAcceptsReplayIgnoresExpiryForCompletedSession`（repository）、
  `TestCompletedSessionRetainedForIdempotentRetry` 第 2~4 段（api，含“窗口已过但未 purge 仍重放”）。

## 5. 验收

```bash
cd go-backend
gofmt -l .            # 空
go vet ./...          # 空
go test ./... -race -count=2
```
