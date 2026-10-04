# 复查结论：账本必须只认 `quota_released`（`completing` 会计入）

分支：`w-dev` · 范围：`internal/repository/quota.go`、`internal/repository/repository.go`、
`internal/repository/uploads.go`、`internal/api/uploads.go` + 测试/文档。

## 1. 复查发现（确认是缺陷）

原实现里三处账本查询带了 `status = 'active'`：

```sql
-- 1) quota.go  CountActiveUploadSessions（init 会话数闸门）
SELECT COUNT(*) FROM upload_sessions WHERE status = 'active'
-- 2) quota.go  RecomputeUploadQuota（cleanup worker + 启动对账）
SELECT COALESCE(SUM(file_size), 0) FROM upload_sessions
  WHERE status = 'active' AND quota_released = 0
-- 3) repository.go  uploadQuotaSeedRow（老库迁移播种）
INSERT INTO upload_quota ... SELECT 1, COALESCE(SUM(file_size), 0) ... FROM upload_sessions
  WHERE status = 'active' AND quota_released = 0
```

而 `quota_released` 才是预留的所有权标志：`active→completing` 是 COMPLETE 抢在
clip INSERT 之前的并发闸门，**不是释放**；释放只发生在终态转移（complete 成功 / DELETE /
过期清理）的同一个事务里（`releaseQuotaTx` 的 CAS）。

于是 `completing` 行**持有预留却不被统计**，后果不只是显示不准：

1. **对账会“修坏”账本**：清理线程每轮用上面第 2 条把 `reserved_bytes` 覆盖成 `active` 子集之和。
   处于 `completing`（clip INSERT 进行中、容器已满写）的字节被抹掉，下一次 init 可以把同一份字节
   再预留一次 → 配额被绕过，同时“盘上字节 > 账本”的不变式被破坏。
2. **会话数闸门可被绕过**：`completing` 会话不占额度，`MaxActiveUploadSessions` 可被超发。
3. **老库升级会少播**：崩溃在 `completing` 的行不会被播种，升级后同样出现免费额度。
4. 更隐蔽的一点：账本查询与释放 CAS 的判据不一致（`status='active' AND quota_released=0`
   vs `quota_released=0`），任何未来对 `status` 的增删都会**静默**改变会计结果。

## 2. 修复

判据收敛成**一个常量**：`reservationsHeldPredicate = "quota_released = 0"`（`internal/repository/quota.go`），
账本查询/释放 CAS/播种全部引用它，不再出现 `status`。

| 位置 | 修改 |
| --- | --- |
| `RecomputeUploadQuota` | `WHERE quota_released = 0`（去掉 `status='active'`），注释写明“加 status 是 bug 而非收紧” |
| `uploadQuotaSeedRow` | `WHERE quota_released = 0`；迁移时先把确定不持有预留的 `completed` 行标记为已释放（新列对老行回填 0），保证播种=收敛值 |
| `CountUploadSessionsHoldingQuota`（新增） | init 会话数闸门改用它；旧的 `CountActiveUploadSessions` 保留为纯诊断计数并注明不可用于配额 |
| `ExpectedReservedBytes` / `UploadQuotaDrift`（新增） | 账本不变式的可读探针（诊断 + 测试） |
| `releaseQuotaTx` | CAS 谓词改为引用同一常量（行为不变，消除双判据） |
| `PurgeExpiredUploadsWithGrace`（新增） | 过期 `completing` 行不再被**永久**跳过：宽限期内保护（可能是进行中的 clip INSERT），超过 `CompletingPurgeGraceSeconds`(900s) 判定为硬杀残留 → 删行 + 归还预留 + 删除容器；若已有 clip 引用容器则 `HasClip=true` 保留文件 |
| `App.rollbackDanglingClip`（新增兜底） | 万一 COMPLETE 提交时发现行已消失且容器已被 unlink，删除刚写入的 clip 行（同时归还 stored 配额）并返回 409，绝不发布指向不存在文件的 201 |

关键不变式（现在由测试端到端锁定）：

```
upload_quota.reserved_bytes == SUM(file_size) FROM upload_sessions WHERE quota_released = 0
```

## 3. 新增/更新的测试

repository（`internal/repository/quota_consistency_test.go`）：

| 用例 | 锁定内容 |
| --- | --- |
| `TestLedgerCountsCompletingSessions` | 复查缺陷的直接回归：`active` + `completing` 都在账本里；`RecomputeUploadQuota` 返回 1000 而不是 400；complete 成功后只减去 `completing` 的那份 |
| `TestLedgerInvariantAcrossEveryTransition` | 单个会话走完 init 预留→两块 chunk→抢闸→回滚→再抢闸→complete，每一步 `UploadQuotaDrift` 都为 0，并在中间注入 99 漂移验证修复方向 |
| `TestFreshCompletingRowSurvivesPurgeWithoutLosingQuota` | 宽限期内被保护的 `completing` 行，其预留不得被对账抹掉 |
| `TestPurgeReclaimsStaleCompletingAfterGrace` | 超期 `completing` 行被回收：预留恰好归还一次；宽限期内不回收 |
| `TestPurgeKeepsContainerOfCompletingRowThatAlreadyHasClip` | clip 已引用容器时 victim 报 `HasClip`，文件保留 |
| `TestLegacyDatabaseQuotaSeedCountsEveryHolder` | 老库播种 = active(777) + completing(555)，`completed` 被标记已释放，对账为 no-op |
| `TestReservationHolderCounterDrivesTheSessionCap` | 闸门计数包含 `completing`，`CountActiveUploadSessions` 不包含（两个问题两套计数） |

api（`internal/api/uploadquota_completing_test.go` + 既有用例增强）：

| 用例 | 锁定内容 |
| --- | --- |
| `TestSessionCapCountsCompletingSessions` | 会话上限=1 时，`completing` 仍占额度（507 `upload_session_limit`）；重试 COMPLETE 409 不动账本；回滚后 release 再次占额度；complete 后额度归零并放行下一次 init |
| `TestPurgeReclaimsStaleCompletingSessionThroughTheCleanupWorker` | 走 `purgeExpiredUploadsPeriodic`：行被清理、容器被 unlink、预留归零、二次清理不重复归还 |
| `TestRollbackDanglingClipGuard` | 容器存在 → clip 保留；容器消失 → clip 行删除且 stored 配额归零；路径缺失/stat 报错 → 绝不动用户数据 |
| `TestInitReplayStaleInFlightIsConflict`（增强） | 幂等槽被过期 `completing` 行占用时，`RecomputeUploadQuota` 仍为 1024（不因 status 过滤而放走） |

## 4. 验收命令

```bash
cd go-backend
go vet ./...
go test ./...                 # 全绿
go test ./... -race -count=2   # 可选：并发/重复回归
```

只跑本次相关用例：

```bash
go test ./internal/repository/ -run 'Ledger|Completing|Purge|Legacy|Reservation' -v
go test ./internal/api/ -run 'Completing|Stale|Dangling|SessionLimit|Quota' -v
```
