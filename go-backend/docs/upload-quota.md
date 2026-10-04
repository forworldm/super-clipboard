# 分块上传 · 全局磁盘配额（Global Disk Quota）

分支：`w-dev` · 目标：防止会话无限占盘。API 契约不变，无全局锁，文件 IO 均在锁外。

## 1. 配置（config.Settings + 环境变量）

| 配置项 | 环境变量 | 默认 | 语义 |
| --- | --- | --- | --- |
| `UploadTotalQuotaBytes` | `SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES` | `10 GiB` | 所有活跃会话预留字节之和上限，`0` = 不限 |
| `MaxActiveUploadSessions` | `SUPER_CLIPBOARD_MAX_ACTIVE_UPLOAD_SESSIONS` | `1000` | **持有预留**的会话数上限（`quota_released=0`，即 `active` + `completing`），`0` = 不限 |
| `MinFreeDiskBytes` | `SUPER_CLIPBOARD_MIN_FREE_DISK_BYTES` | `1 GiB` | 可用空间水位，低于则拒绝写入，`0` = 不限 |

校验：三个值任一为负数 → 启动报错（配置错误）；
`UploadTotalQuotaBytes > 0 && UploadTotalQuotaBytes < MaxFileSizeBytes` → 只写入
`Settings.Warnings` 并由 `main.go` 打 `WARN` 日志，**不阻止启动**。

## 2. 存储结构

```sql
CREATE TABLE upload_quota (           -- 单行账本
  id INTEGER PRIMARY KEY CHECK (id = 1),
  reserved_bytes INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
ALTER TABLE upload_sessions ADD COLUMN quota_released INTEGER NOT NULL DEFAULT 0;
```

### 2.1 唯一判据：`quota_released = 0`（不叠加 `status`）

账本的唯一不变式：

```
upload_quota.reserved_bytes == SUM(file_size) FROM upload_sessions WHERE quota_released = 0
```

`quota_released` 是**预留的所有权标志**，`status` 是另一条状态机——两者不可互相替代：

| status | 是否持有预留 | 说明 |
| --- | --- | --- |
| `active` | ✅ | 分块正在写入 |
| `completing` | ✅ | `active→completing` 只是 COMPLETE 的并发闸门（抢在 clip INSERT 之前），**不是释放**；释放只发生在终态转移 |
| `completed` | ❌ | `quota_released=1`，与 `completing→completed` 同一事务内释放 |

因此**任何账本查询都不许带 `status` 条件**（`reservationsHeldPredicate`，`internal/repository/quota.go`）：
加 `status='active'` 会漏掉 `completing` 行，而 `RecomputeUploadQuota` 会把这个“差额”当成漂移
**覆盖性修正掉**——同一份字节被下一次 init 再预留一次（配额绕过）。回归测试：
`TestLedgerCountsCompletingSessions`、`TestFreshCompletingRowSurvivesPurgeWithoutLosingQuota`、
`TestSessionCapCountsCompletingSessions`。

### 2.2 老库迁移（`ensureSchema`）

自动 `ALTER` 补列；由于新列对老行一律回填 `0`，迁移会同时把**确定不持有预留**的
`completed` 行标记为已释放（`UPDATE ... SET quota_released=1 WHERE status='completed'`），
再按 `SUM(file_size) WHERE quota_released = 0` 一次性播种（`active` + `completing`），
升级不会凭空发放额度，也不会把已结束会话的字节算进来。
回归测试：`TestLegacyDatabaseQuotaSeedCountsEveryHolder`。

## 3. init 三道闸门（`handleInitUpload`，顺序固定）

1. **会话数**：`COUNT(*) WHERE quota_released = 0 < MaxActiveUploadSessions`，否则 507
   （`CountUploadSessionsHoldingQuota`；用 `status='active'` 会把 `completing` 会话漏出闸门）。
2. **配额预留**：事务内原子 CAS
   `UPDATE upload_quota SET reserved_bytes = reserved_bytes + ? WHERE id=1 AND reserved_bytes + ? <= ?`
   `RowsAffected==0` → 507。预留量 = 客户端声明的 `fileSize`。
3. **磁盘水位**：`statfs Bavail*Bsize >= MinFreeDiskBytes`，否则 507 并立即归还预留。

三道闸门全部在**建会话 / 写盘之前**执行，任一失败：不创建会话、不写磁盘、
不留下配额残留（预留由 `defer` 补偿归还）。
captcha 与幂等 replay 语义不变：replay 命中在闸门之前返回，**不重复预留**；
`CreateOrGetUploadSession` 抢锁失败（`created=false`）同样归还本次预留。

## 4. 释放（幂等，只能释放一次）

`quota_released` 标志 + 同事务 CAS：

```sql
UPDATE upload_sessions SET quota_released = 1 WHERE id = ? AND quota_released = 0;
-- 仅当 RowsAffected == 1 才：
UPDATE upload_quota SET reserved_bytes = reserved_bytes - ? WHERE id = 1 AND reserved_bytes >= ?;
```

| 触发点 | 时机 |
| --- | --- |
| `AbortUploadSession` | DELETE / 取消 / `expireUploadNow` |
| `PurgeExpiredUploads` | 过期清理（cleanup worker、启动清理），含超过宽限期的 `completing` 行 |
| `CompleteUploadSession` | complete 成功（选项 A：upload 配额释放，clip 文件不占 upload 配额） |

**`FailComplete` 不释放**：complete 失败回滚把会话退回 `active`，容器与 receipts 都还在盘上，
预留必须继续保留（否则字节在盘上而账本为零 = 配额绕过）。释放只由上面的终态转移负责。
`completing` 同理：抢闸成功不等于释放，`AbortUploadSession` 也会拒绝它（回滚/重启后再取消）。

### 4.1 卡死 `completing` 行的回收（`CompletingPurgeGraceSeconds`）

`completing` 只是毫秒级闸门（分块在 PUT 阶段就已落盘并 fsync，COMPLETE 不再做文件 IO），
所以过期清理对 `completing` 行只做**宽限期保护**而非永久跳过：

- `updated_at > now - 900s` → 视为可能正在进行 clip INSERT，保护（不删行、不删文件），
  其字节继续计入账本与对账（对账不会把它抹掉）；
- 超过宽限期 → 认定为被硬杀（SIGKILL）残留：删行 + 归还预留（CAS 只可能成功一次）+ 删除容器；
- 若 clip 行已经引用该容器（`clips.file_path = staged_path`），victim 报告 `HasClip`，
  容器保留给 clip；COMPLETE 端另有兜底 `rollbackDanglingClip`（容器已消失 → 删除刚写入的 clip 行并返回 409，
  绝不发布指向不存在文件的 201）。

回归测试：`TestPurgeReclaimsStaleCompletingAfterGrace`、`TestPurgeKeepsContainerOfCompletingRowThatAlreadyHasClip`、
`TestPurgeReclaimsStaleCompletingSessionThroughTheCleanupWorker`、`TestRollbackDanglingClipGuard`。

## 5. chunk 写入（`handlePutChunk`）

流式写前检查磁盘水位；低于 `MinFreeDiskBytes` → 507，不落盘、不写 receipt。

## 6. 对账（cleanup worker 每轮一次）

```
recompute = SUM(file_size) FROM upload_sessions WHERE quota_released = 0   -- 不带 status
```

直接覆盖 `upload_quota.reserved_bytes` 修正漂移并打日志；只修计数，不删文件。
启动时（`ReconcileUploadsOnStartup`）也执行一次。

## 7. 响应（typed error → 507）

| Code | HTTP | 文案 |
| --- | --- | --- |
| `apperr.StorageCodeQuota` | 507 | `上传配额不足：已预留 X 字节，再预留 Y 字节将超出总量配额 Z 字节` |
| `apperr.StorageCodeSessionLimit` | 507 | `活动上传会话数已达上限：N / M，请完成或取消进行中的上传后重试` |
| `apperr.StorageCodeDisk` | 507 | `磁盘可用空间不足：剩余 X 字节，低于最低水位 Y 字节` |

响应体保持 `{"detail": "..."}`，现有 API 契约不变。

## 7.5 成功会话的保留窗口（与配额的交互）

- complete 成功时，同一事务内 `CompleteUploadSession` 释放预留（`quota_released` CAS）并把
  `expires_at` 收缩为 `min(now + CompletedUploadTTL, 原值)`，默认 300s
  （`SUPER_CLIPBOARD_UPLOAD_COMPLETED_TTL_SECONDS`）。行继续作为 `requestId` 幂等凭据，
  但不再长期占用 resume 窗口。
- 因此 purge 一定会在“配额已释放”之后才看到该行：`releaseQuotaTx` 的行内 CAS 落空，
  不会二次扣减（`TestPurgeExpiredCompletedUploadDoesNotReleaseQuotaTwice` /
  `TestPurgeAfterCompleteDoesNotReleaseQuotaTwice`）。
- 幂等判定 (`UploadSession.AcceptsReplay`) 对 `completed` 会话忽略 `expires_at`；
  详见 `docs/completed-upload-retention.md`。

## 8. 提交与验收

| 提交 | 内容 | 验收命令 |
| --- | --- | --- |
| `config: 新增上传全局磁盘配额配置` | 设计项 1 | `go test ./internal/config/ -run UploadQuota -v` |
| `schema: upload_quota + quota_released` | 设计项 2 | `go test ./internal/repository/ -run LegacyDatabase -v` |
| `repository: CAS 预留 / 幂等释放 / 对账` | 设计项 3、4、6 | `go test ./internal/repository/ -run 'Quota\|Recompute' -v` |
| `storage: statfs 探针` | 设计项 5 | `go test ./internal/storage/ -run FreeDiskBytes -v` |
| `api: 三道闸门 / 水位 / 对账 / 507` | 设计项 3、5、6、7 | `go test ./internal/api/ -run 'Quota\|Watermark' -v` |
| `test: api + repository 双层测试` | 设计项 8 | `go test ./... -race -count=2` |
| `docs: 验收说明` | 本文档 | 人工核对 |
| `fix: 账本只认 quota_released（completing 会计入）` | 设计项 1、3、4、6 | `go test ./... -run 'Ledger\|Completing\|Reservation\|Legacy' -v` |

整体回归：`cd go-backend && go test ./... -race -count=2`（全绿，含既有用例）。
