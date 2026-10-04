# 分块上传 · 全局磁盘配额（Global Disk Quota）

分支：`w-dev` · 目标：防止会话无限占盘。API 契约不变，无全局锁，文件 IO 均在锁外。

## 1. 配置（config.Settings + 环境变量）

| 配置项 | 环境变量 | 默认 | 语义 |
| --- | --- | --- | --- |
| `UploadTotalQuotaBytes` | `SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES` | `10 GiB` | 所有活跃会话预留字节之和上限，`0` = 不限 |
| `MaxActiveUploadSessions` | `SUPER_CLIPBOARD_MAX_ACTIVE_UPLOAD_SESSIONS` | `1000` | `status='active'` 会话数上限，`0` = 不限 |
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

老库迁移（`ensureSchema`）：自动 `ALTER` 补列（老会话默认 `0` = 仍持有预留），
`upload_quota` 建表并按
`SUM(file_size) WHERE status='active' AND quota_released=0`
一次性播种，升级不会凭空发放额度。

## 3. init 三道闸门（`handleInitUpload`，顺序固定）

1. **会话数**：`COUNT(*) WHERE status='active' < MaxActiveUploadSessions`，否则 507。
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
| `PurgeExpiredUploads` | 过期清理（cleanup worker、启动清理） |
| `FailComplete` | complete 失败回滚 |
| `CompleteUploadSession` | complete 成功（选项 A：upload 配额释放，clip 文件不占 upload 配额） |

## 5. chunk 写入（`handlePutChunk`）

流式写前检查磁盘水位；低于 `MinFreeDiskBytes` → 507，不落盘、不写 receipt。

## 6. 对账（cleanup worker 每轮一次）

```
recompute = SUM(file_size) FROM upload_sessions WHERE status='active' AND quota_released=0
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

整体回归：`cd go-backend && go test ./... -race -count=2`（全绿，含既有用例）。
