# 已保存文件总量配额（Stored Clip Quota）

分支：`w-dev` · 目标：把「已落库的片段文件总大小」限制在一个配置值内。
与 `upload_quota`（约束**活动上传会话已预留**的字节，见 `docs/upload-quota.md`）互补：
上传配额管的是「还没落库的字节」，本配额管的是「已经落库、会长期占据磁盘的字节」。
API 契约不变，无全局锁，文件 IO 全部在锁外。

## 1. 配置（config.Settings + 环境变量）

| 配置项 | 环境变量 | 默认 | 语义 |
| --- | --- | --- | --- |
| `StoredTotalQuotaBytes` | `SUPER_CLIPBOARD_STORED_TOTAL_QUOTA_BYTES` | `10 GiB` | `SUM(clips.file_size)` 上限，`0` = 不限 |

- 负数 → 启动报错（配置错误）；`0` → 不限（但仍记账，方便以后开启限额）。
- `StoredTotalQuotaBytes > 0 && StoredTotalQuotaBytes < MaxFileSizeBytes` → 只写入
  `Settings.Warnings` 并由 `main.go` 打 `WARN`，**不阻止启动**（与上传配额同策略）。
- 默认值高于单文件上限，因此默认配置不会产生告警。

## 2. 存储结构

```sql
CREATE TABLE clip_quota (            -- 单行账本，与 upload_quota 同构
  id INTEGER PRIMARY KEY CHECK (id = 1),
  used_bytes INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_clips_file_size ON clips(file_size);
```

老库迁移（`ensureSchema`）：建表后按
`SUM(file_size) FROM clips`（`WHERE 1` 用于避开 SQLite 对
`INSERT ... SELECT ... ON CONFLICT` 的解析歧义）一次性播种，`ON CONFLICT(id) DO NOTHING`
保证重启/多次迁移不会重复计数。文本片段 `file_size` 为 `NULL`，不占额度。

## 3. 扣减时机：插入 clip 的那一次 CAS（设计项 2.1）

`CreateClip` 在**同一个事务**里先记账、后 `INSERT`：

```sql
-- reserveClipQuotaTx
UPDATE clip_quota SET used_bytes = used_bytes + ?, updated_at = ?
  WHERE id = 1 AND used_bytes >= 0 AND used_bytes + ? <= ?;   -- limit > 0
-- limit = 0（不限）：去掉上限谓词，仍然累加
```

- `RowsAffected == 0` → 返回 typed `apperr.StorageError`（`Code = apperr.StorageCodeClipQuota`，
  `HTTPStatus() == 507`），文案：
  `存储配额不足：已保存 X 字节，再保存 Y 字节将超出总量配额 Z 字节`。
- 拒绝发生在 `INSERT` 之前且同事务 → **不会落库半条记录**，也不会产生脏计数；
  `INSERT` 自身失败（直链码冲突等）会连带回滚本次记账。
- 覆盖全部两个文件片段入口：分块上传 `COMPLETE`
  （`createClipFromCompletedUpload` → `CreateClip`）与 `POST /api/clips` 的 data URL 片段。
  失败时前者删除已组装的 staged 文件并把会话回滚为 `active`（分片保留、可续传/重试），
  后者删除已写盘的临时文件。
- 记账与上限都由 SQLite 在 `UPDATE` 内比较并交换（CAS）：并发插入在「刚好装得下 M 个」的
  预算下恰好放行 M 个，其余拿到 507。
- 只有「插入的那一刻」生效：**不**对上传中的会话字节重复计费，也不追求每一步精确额度，
  最终收敛到配置值即可。

## 4. 归还时机（设计项 2.2）

归还与 clip 行的删除**同事务**，避免任何路径漏改导致大小不匹配：

| 触发点 | 说明 |
| --- | --- |
| `DeleteClip` | 所有删除入口的统一实现：`DELETE /api/clips/{id}`、`POST /api/clips/{id}/download` 达上限、直链失效、文件丢失、`GET /api/clips` 清理 |
| `handleAdminDeleteClip` | 走同一个 `DeleteClip` |
| `PurgeInactive` | 定时/启动清理：`expires_at <= now` 或 `download_count >= max_downloads`；一次事务内逐个 `DELETE` 并逐个归还 |
| `RecomputeClipQuota` | 清理 worker 每轮 + 启动 `ReconcileUploadsOnStartup` 第 7 步执行对账 |

```sql
-- releaseClipQuotaTx（与删除同事务）
UPDATE clip_quota SET used_bytes = used_bytes - ?, updated_at = ?
  WHERE id = 1 AND used_bytes >= ?;      -- 谓词保证永不为负
```

- 只对「本次真的删掉了行」（`RowsAffected == 1`）归还，跨进程重复删除不会双倍归还；
- `file_size` 为 `NULL/0`（文本片段、老数据）时归还为空操作；
- 清理里唯一的批量删除点 `PurgeInactive` 已改为读取 `file_size` 并归还；
  `IncrementDownloads` 只改 `download_count`，不影响大小；`ensureSchema` 的
  `UPDATE clips SET owner_id` 也不影响大小。

## 5. 对账（收敛保证）

```
recomputed = SUM(file_size) FROM clips        -- 权威值
clip_quota.used_bytes <- recomputed           -- 有漂移才写，并打 WARN
```

启动时（`ReconcileUploadsOnStartup`）与清理 worker 每轮（`cleanupWorker`）各执行一次；
只修计数，不删文件。账本行被手工删掉时，下一次记账会按
`SUM(file_size)` 重新播种（自带自愈），删除则是空操作。

> 与 Python 版混跑：Python 后端不会维护本账本，其插入的片段在下一轮对账时被计入
> （漂移只会让限额短暂偏松，不会让计数错误）。Go 版本内部所有路径始终精确一致。

## 6. 响应（typed error → 507）

| Code | HTTP | 文案 |
| --- | --- | --- |
| `apperr.StorageCodeClipQuota` | 507 | `存储配额不足：已保存 X 字节，再保存 Y 字节将超出总量配额 Z 字节` |

响应体保持 `{"detail": "..."}`；`COMPLETE` 失败时同时把会话回滚为 `active`（分片保留），
客户端删除部分旧片段后可直接重试 `COMPLETE` 成功。

## 7. 提交与验收

| 提交 | 内容 | 验收命令 |
| --- | --- | --- |
| `config: 新增已保存文件总量配额配置` | 设计项 1 | `go test ./internal/config/ -run StoredQuota -v` |
| `schema: clip_quota 单行账本 + 播种迁移` | 设计项 2 | `go test ./internal/repository/ -run MigratesClipQuota -v` |
| `repository: 插入 CAS 扣减 / 删除归还 / 对账` | 设计项 3、4、5 | `go test ./internal/repository/ -run ClipQuota -v` |
| `api: COMPLETE / data URL 507 与会话回滚` | 设计项 3、6 | `go test ./internal/api/ -run StoredQuota -v` |
| `docs: 验收说明` | 本文档 | 人工核对 |

整体回归：`cd go-backend && go vet ./... && go test ./... -race -count=2`（全绿，含既有用例）。
