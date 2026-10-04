# Super Clipboard 后端 · Golang 移植版

本目录是 [`pixia1234/super-clipboard`](https://github.com/pixia1234/super-clipboard) 中
`/backend`（FastAPI + SQLite）的 **Go 语言等价实现**：功能等价、API 兼容、磁盘数据格式一致。
原有 Python 代码不再需要，前端（Vite + React）无需任何修改即可直接对接本服务。

```
Python (FastAPI + uvicorn + sqlite3 + httpx + pydantic)
        │  port
        ▼
Go 1.23 (net/http + modernc.org/sqlite + google/uuid，纯标准库路由，无 cgo)
```

---

## 开发协作指南（给 AI Agent / 新贡献者）

- 中文：[`AGENT.md`](./agent.md)
- English: [`AGENT_EN.md`](./AGENT_EN.md)

> 建议在做任何跨层或行为变更前先阅读该指南，避免破坏 API/数据兼容性。

## 1. 文件对应关系

| Python 源文件 | Go 文件 | 说明 |
| --- | --- | --- |
| `backend/__main__.py` | `backend/main.go` | 进程入口：加载配置 → 打开数据库 → 启动 HTTP 服务 |
| `backend/config.py` | `internal/config/config.go`<br>`internal/config/dotenv.go` | `Settings`（`SUPER_CLIPBOARD_` 前缀 + `.env`）、字段校验器、启动时建目录 |
| `backend/models.py` | `internal/models/models.go` | `StoredFile`、`Clip` 及 `is_expired` / `reached_download_limit` / `is_active` |
| `backend/repository.py` | `internal/repository/repository.go` | SQLite schema、迁移、token 注册/校验、片段增删查改、下载计数、清理 |
| `backend/schemas.py` | `internal/schemas/schemas.go`<br>`internal/schemas/validation.go` | pydantic 请求/响应模型与校验（含 FastAPI 422 错误结构） |
| `backend/storage.py` | `internal/storage/storage.go` | data URL 解析、base64 解码、落盘命名规则 |
| `backend/utils.py` | `internal/utils/utils.go` | 直链 HTML 渲染、`base_url`、验证码校验、客户端 IP |
| `backend/main.py` | `internal/api/server.go`（应用/路由/中间件/生命周期）<br>`internal/api/handlers.go`（各端点）<br>`internal/api/router.go`（Starlette 语义路由）<br>`internal/api/cors.go`（CORSMiddleware）<br>`internal/api/respond.go`（JSON/HTML/PlainText/FileResponse） | FastAPI 应用整体 |
| `backend/tests/test_clips.py` | `internal/api/api_test.go` | 6 个原始回归用例全部移植，并额外补充边界用例 |
| （pydantic 内建异常） | `internal/apperr/apperr.go` | `ValueError` / `HTTPException` 的 Go 等价类型 |

## 2. 运行

```bash
cd backend

# 开发运行（等价于 python -m backend）
go run .

# 指定端口 / 数据目录（等价于 export SUPER_CLIPBOARD_APP_PORT=5174）
SUPER_CLIPBOARD_APP_PORT=5174 \
SUPER_CLIPBOARD_DATABASE_PATH=./storage/clipboard.db \
SUPER_CLIPBOARD_FILE_STORAGE_DIR=./storage/files \
go run .

# 回归测试（等价于 pytest backend/tests）
go test ./...

# 生产构建
go build -o bin/super-clipboard .
./bin/super-clipboard
```

默认监听 `0.0.0.0:5173`，与 Python 版一致。生产部署时先 `npm run build` 生成 `dist/`，
再在仓库根目录启动本服务即可一体化托管前端静态资源（`/`、`/static`、`/assets`）。
Vite 开发服务器场景下把后端改到 5174，再 `BACKEND_PORT=5174 npm run dev`。

## 3. 环境变量（与 Python 版完全一致）

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SUPER_CLIPBOARD_DATABASE_PATH` | `backend/storage/clipboard.db` | SQLite 数据库文件 |
| `SUPER_CLIPBOARD_FILE_STORAGE_DIR` | `backend/storage/files` | 上传文件目录 |
| `SUPER_CLIPBOARD_APP_HOST` | `0.0.0.0` | 监听地址 |
| `SUPER_CLIPBOARD_APP_PORT` | `5173` | 监听端口 |
| `SUPER_CLIPBOARD_DEFAULT_MAX_DOWNLOADS` | `10` | 未指定时的下载次数上限 |
| `SUPER_CLIPBOARD_MAX_ALLOWED_DOWNLOADS` | `500` | 允许的最大下载次数 |
| `SUPER_CLIPBOARD_CLEANUP_INTERVAL_SECONDS` | `300` | 自动销毁任务间隔 |
| `SUPER_CLIPBOARD_MAX_FILE_SIZE_BYTES` | `52428800`（50MB） | 文件体积上限 |
| `SUPER_CLIPBOARD_TOKEN_EXPIRY_HOURS` | `720` | 持久 Token 有效期 |
| `SUPER_CLIPBOARD_STATIC_ROOT` | `dist` | 前端构建产物目录 |
| `SUPER_CLIPBOARD_CAPTCHA_PROVIDER` | 空 | `turnstile` 或 `recaptcha` |
| `SUPER_CLIPBOARD_CAPTCHA_SECRET` | 空 | 验证码 Secret |
| `SUPER_CLIPBOARD_CAPTCHA_SITE_KEY` | 空 | 下发给前端的 site key |
| `SUPER_CLIPBOARD_CAPTCHA_BYPASS_TOKEN` | 空 | 仅测试用的直通 Token |
| `SUPER_CLIPBOARD_CAPTCHA_TIMEOUT_SECONDS` | `6.0` | 验证码校验超时 |
| `SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES` | `10737418240`（10GiB） | 活动上传会话已预留字节上限，`0` = 不限 |
| `SUPER_CLIPBOARD_MAX_ACTIVE_UPLOAD_SESSIONS` | `1000` | 并发活动上传会话上限，`0` = 不限 |
| `SUPER_CLIPBOARD_MIN_FREE_DISK_BYTES` | `1073741824`（1GiB） | 可用磁盘水位，低于则拒绝写入，`0` = 不限 |
| `SUPER_CLIPBOARD_STORED_TOTAL_QUOTA_BYTES` | `10737418240`（10GiB） | 已保存片段文件总大小上限（`SUM(clips.file_size)`），`0` = 不限 |

同时支持工作目录下的 `.env` 文件；真实环境变量优先级高于 `.env`（与 pydantic-settings 一致）。

## 4. HTTP API（路径、方法、状态码、响应体均保持一致）

| 方法与路径 | 说明 | 成功 | 失败 |
| --- | --- | --- | --- |
| `GET /healthz` | 健康检查 `{ok, timestamp}` | 200 | — |
| `GET /` | 有 `dist/index.html` 则返回前端，否则返回 `{name, ok}` | 200 | — |
| `GET /static/{path}` `GET /assets/{path}` | 前端静态资源（目录存在时才挂载） | 200 | 404 `Not Found` |
| `GET /api/clips?environmentId=` | 列出当前设备的片段（先执行一次清理） | 200 `{items:[...]}` | 400 `environmentId 缺失` / 422 |
| `POST /api/clips` | 创建文本或文件片段 | 201 `ClipResponse` | 400 / 409 / 422 / 500 / 507 `存储配额不足` |
| `GET /api/clips/{clip_id}?environmentId=` | 读取片段 | 200 | 404 `片段未找到`、404 `片段已过期或达到下载次数` |
| `GET /api/clips/code/{access_code}` | 按短码读取片段 | 200 | 404 `直链不存在或已过期` |
| `DELETE /api/clips/{clip_id}?environmentId=` | 删除片段及其文件 | 200 `{ok:true}` | 404 `片段未找到` |
| `POST /api/clips/{clip_id}/download?environmentId=` | 计数 +1，达上限即销毁 | 200 `{clip, removed}` | 404 `片段未找到`、410 `片段已过期或销毁` |
| `GET /api/clips/{clip_id}/file?environmentId=` | 下载文件（`Content-Disposition: attachment`） | 200 | 404 / 410 `文件已丢失`、410 `文件已过期或销毁` |
| `POST /api/tokens/register` | 注册/续期持久 Token | 200 `TokenRegisterResponse` | 409 `持久 Token 已被其他设备占用，请稍后重试` |
| `GET /api/config` | 下发验证码配置 | 200 `{captchaProvider, captchaSiteKey}` | — |
| `GET /{access_code}` | 直链：文本渲染 HTML，文件直接下载 | 200 | 404 `直链不存在或已过期`、410 `文件已丢失` / `文件数据缺失` |
| `GET /{access_code}/raw` | 直链原始内容（VNC/SSH 场景） | 200 `text/plain` 或文件流 | 同上 |

直链标识支持三种形式：`54321`（短码）、`persistToken`（≥7 位 Token）、
`ownerId.54321`（设备限定短码，见 `_parse_identifier`）。

### 错误体格式

* 业务错误（原 `HTTPException`）：`{"detail": "中文提示"}`
* 参数校验错误（原 pydantic `RequestValidationError`）：`422 {"detail": [{"type": "...", "loc": ["body","field"], "msg": "...", "input": ..., "url": "..."}]}`
* 方法不允许：`405 {"detail": "Method Not Allowed"}` + `Allow` 响应头
* 未匹配路由：`404 {"detail": "Not Found"}`
* 结尾斜杠（如 `/api/clips/`）：`307` 重定向到规范路径（Starlette `redirect_slashes`）
* 未捕获异常：`500 Internal Server Error`（`text/plain`）

CORS 与原版一致：`allow_origins=["*"] / allow_methods=["*"] / allow_headers=["*"]`，
预检请求返回 `200 OK`、`Access-Control-Allow-Origin: *`、`Access-Control-Max-Age: 600`。

## 5. 数据兼容性

SQLite 表结构、索引、字段名与 Python 版逐字一致（`clips` / `tokens`，含 `owner_id` 的
`ALTER TABLE` 迁移逻辑），时间戳仍以 **秒级 UNIX 整数** 存储，上传文件仍写入
`SUPER_CLIPBOARD_FILE_STORAGE_DIR/<UTC 时间戳 20 位><扩展名>`。
因此可以直接把 Python 版的数据卷挂到本服务上继续运行，也可以反向回滚。

## 6. 已知差异（有意为之）

1. `main.py` 中 `GET /api/clips/code/{access_code}` 的失效分支写成了
   `raise HTTPException(status代码=404, ...)`（Python 会抛 `TypeError` → 500）。
   Go 版按显然的本意返回 **404 `直链不存在或已过期`**。
2. 空字符串环境变量视为“未设置”（Python 会因类型校验失败而无法启动），
   便于 docker-compose 里留空的 `SUPER_CLIPBOARD_CAPTCHA_*`。
3. 数据库以 WAL 模式打开并设置 `busy_timeout=10s`，写操作仍由互斥锁串行化
   （等价于 Python 的 `threading.Lock`），数据库文件格式不变。
4. `Content-Type` 由 Go 的 `mime` 包推断，静态 HTML 会带上 `; charset=utf-8`。

## 7. 测试

```bash
go test ./...          # 全部包
go test ./internal/api # 移植自 backend/tests/test_clips.py 的端到端用例
go test -race ./...    # 竞态检测
```

已移植的原始用例：`test_create_and_fetch_text_clip`、`test_file_clip_download_limit`、
`test_token_direct_access`、`test_token_register_conflict`、
`test_captcha_required_when_enabled`、`test_config_endpoint`。
另补充：路由 405/404/307、CORS、静态资源托管、体积超限、非法 base64、
422 校验结构、token 生命周期、清理任务、并发计数等用例。

## 8. Docker

```bash
# 纯后端镜像（不含前端构建）
docker build -t super-clipboard-go ./backend

docker run -d --name super-clipboard \
  -p 5173:5173 \
  -v clipboard-data:/data \
  -e SUPER_CLIPBOARD_DATABASE_PATH=/data/clipboard.db \
  -e SUPER_CLIPBOARD_FILE_STORAGE_DIR=/data/files \
  -e SUPER_CLIPBOARD_CAPTCHA_PROVIDER=turnstile \
  -e SUPER_CLIPBOARD_CAPTCHA_SECRET=<your_turnstile_secret> \
  -e SUPER_CLIPBOARD_CAPTCHA_SITE_KEY=<your_turnstile_site_key> \
  super-clipboard-go
```
