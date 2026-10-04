# 请求参数字节上限（Request Parameter Bounds）

分支：`w-dev` · 目标：让 `POST /api/clips`（以及共享同一批字段的
`POST /api/uploads/init`、`POST /api/tokens/register`）不再接受**无界参数**。

## 1. 问题：旧实现的参数只受 readBody 限制

旧代码里字符串字段的 `max_length` 基本靠「传 0 = 不检查」表示：

| 字段 | 旧约束 | 实际效果 |
| --- | --- | --- |
| `payload.text` | `min_length=0`，无上限 | 只受 `readBody` 的 `maxBodyBytes = 1 MiB` 限制，单个文本片段可存约 1 MiB |
| `accessToken` | `min_length=7`，无上限 | 可写入任意长度字符串，并且它同时是持久 token 表的**查询键** |
| `token`（`/api/tokens/register`） | `min_length=7`，无上限 | 同上，可写入任意长度行键 |
| `payload.file.dataUrl` | `min_length=1`，无上限 | 内联 base64 大小只受请求体上限限制，绕过 1 MiB 上限需走分片上传 |

放大效应：一个请求就能同时占用 Go heap、JSON 解码缓冲、SQLite 行、以及**列表接口
每次回显 `payload.text` 时的响应体**。也就是说，写入侧的「一次几 MB」在读取侧会被
反复放大，而 `maxBodyBytes` 只保护了写入方向。

## 2. 修复：所有客户端字符串都有字符数与字节数双重上限

新增 `internal/schemas/limits.go` 作为唯一定义处，`internal/schemas/validation.go`
新增 `stringBound` / `checkBound`：

```go
type stringBound struct {
    MinRunes int  // pydantic max_length 语义（字符数）
    MaxRunes int
    MaxBytes int  // UTF-8 字节数，真正保护进程/DB 的那一维
    TooLongMessage string // 需要给客户端「替代方案」提示时使用
}
```

- **为什么同时要字节数**：字符数对 4 字节 UTF-8 是免费的。只按字符数限制，攻击者
  用 64K 个汉字就能拿到 256 KiB。所以 `MaxRunes` 保持 pydantic 兼容（错误消息措辞
  与 `string_too_long` 类型不变），`MaxBytes` 作为真正的天花板。
- **错误语义**：超限仍然是 `string_too_long`，HTTP 状态码仍是 422，前端按 type 映射
  的逻辑无需改动；只是消息更可执行。

## 3. 具体上限

| 字段 | 位置 | 上限 |
| --- | --- | --- |
| `payload.text` | `POST /api/clips` | **64 KiB**（65536 字节 **且** 65536 字符） |
| `accessToken` | `/api/clips`、`/api/uploads/init` | 7..**128** 字符，且 ≤128 字节 |
| `token` | `POST /api/tokens/register` | 7..**128** 字符，且 ≤128 字节 |
| `environmentId` | 上述全部 | 1..64 字符，且 ≤64 字节 |
| `accessCode` | 上述全部 | 5..12 字符（字母或数字，校验不变） |
| `captchaToken` | 上述全部 | 1..4096 字符，且 ≤4096 字节 |
| `payload.file.name` / `filename` | `/api/clips`、`/api/uploads/init` | 1..255 字符，且 ≤255 字节 |
| `payload.file.type` / `mimeType` | 同上 | ≤255 字符，且 ≤255 字节 |
| `payload.file.dataUrl` | `POST /api/clips` | ≤**8 MiB** 字节（超出请走分片上传） |
| `requestId` | `POST /api/uploads/init` | 4..128 字符，且 ≤128 字节 |

超出 64 KiB 的文本不会被默默丢弃或截断，而是返回可执行的提示：

```json
{
  "detail": [{
    "type": "string_too_long",
    "loc": ["body", "payload", "text"],
    "msg": "文本片段最长 65536 字节（64 KiB，65536 字符），更长的内容请作为文件片段上传",
    "url": "https://errors.pydantic.dev/2.12/v/string_too_long"
  }]
}
```

## 4. 为什么是「文件片段」而不是放大上限

`/api/uploads/*` 已经具备分片上传、会话配额、磁盘水位门禁与落库总配额
（见 `docs/upload-quota.md`、`docs/clip-storage-quota.md`）。大文本继续走内联 JSON
只会绕开这些保护：分片路径有 `MAX_FILE_SIZE_BYTES` 与配额校验，且写入过程是流式的。
因此 64 KiB 之上的内容应作为文件片段上传（前端已 `new File([text], ...)` 走同一路径）。

## 5. 测试

`internal/schemas/limits_test.go`：

- `TestClipTextAtLimitIsAccepted` / `TestClipTextOverLimitIsRejected`：64 KiB 边界；
- `TestClipTextByteBoundCatchesMultibyte`：汉字文本（字符数未超、字节数超）；
- `TestAccessTokenMaxLength` / `TestAccessTokenStillHasMinimum`：token 上限与历史下限；
- `TestEnvironmentIDBound`、`TestTokenRegisterTokenBound`、`TestUploadInitSharesClipBounds`：
  确认分片上传与 token 注册接口不能绕过同一批上限；
- `TestFileClipDataURLBound`：内联 dataUrl 上限。

```
go test ./...
```

## 6. 兼容性

- 正常剪贴板内容（几 KB 以内）行为完全不变；
- 上限是编译期常量，未引入新的环境变量：这些值属于安全边界，不应由部署随手放宽；
- 需要更大的单个文件请调整 `MAX_FILE_SIZE_BYTES`（分片上传路径），而不是内联文本上限。
