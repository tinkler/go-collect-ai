# collect-ai 后端 (Go + Gin + PostgreSQL)

c/s 架构的 collect-ai 后端。OCR + LLM + SkuMatcher 全在后端, C# 客户端和飞书 H5 通过 REST API 调用。

## 功能

- **图片解析** (POST `/api/v1/parse` / `/api/v1/sessions`): 上传图片 → BigModel OCR → GLM-4 解析 → SkuMatcher 级联匹配 → 返回 rows
- **解析历史** (GET/DELETE `/api/v1/sessions`): 列/删解析会话
- **行编辑** (PUT/DELETE `/api/v1/sessions/:id/rows/:rowId`): 改/删某行
- **导出** (GET `/api/v1/sessions/:id/export`): TXT 导出 (排除 is_deleted + is_new)
- **模板管理**:
  - `GET /api/v1/templates?supplier=xxx&default=1&purchase=1` 飞书端用
  - `GET /api/v1/templates/all` C# 端管理
  - `POST /api/v1/templates/sync` C# 端整体同步
- **供应商/品牌**:
  - `GET /api/v1/suppliers` 拉所有供应商 (代理 agent)
  - (后续: 按品牌反查)

## 快速开始

### 1. 准备 PostgreSQL

用 Docker:
```powershell
docker run -d --name collectai-pg -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=collectai -p 5432:5432 -v collectai-pg-data:/var/lib/postgresql/data postgres:16-alpine
```

或装 PG 16 本地服务。

### 2. 配置

```powershell
cp .env.example .env
# 编辑 .env, 填 BIGMODEL_API_KEY
```

完整配置层级和覆盖顺序见 [配置](#配置) 章节。

### 3. 跑

```powershell
go mod tidy
go run ./cmd/server
# 监听 :8089
```

### 4. 验证

```powershell
curl http://127.0.0.1:8089/api/v1/health
# {"status":"ok","ts":...}
```

## 配置

### 配置层级

**优先级:操作系统环境变量 > `.env` 文件 > `config/config.yaml` > 代码内默认值**

| 来源 | 说明 | 典型用途 |
|------|------|----------|
| 操作系统环境变量 | 由 systemd / Docker / 终端 shell 设置 | 生产密钥、运行时调参 |
| `.env` 文件 | 项目根目录,`godotenv` 加载,默认不覆盖已存在的环境变量 | 开发默认值、密钥占位 |
| `config/config.yaml` | 可选的 YAML 配置文件 (按需创建) | 集中存放多套环境的非敏感配置 |
| 代码内 `SetDefault` | 兜底值 | 合理默认 |

### 关键配置项

参考 `.env.example` 列出 22 个变量,按域分组:

```ini
# --- 服务 ---
PORT=8089                        # HTTP 监听端口
UPLOAD_DIR=./uploads             # 上传图片本地存储目录
MAX_UPLOAD_MB=16                 # 单图上限 MB
PUBLIC_BASE_URL=                 # 公网 base URL (飞书 H5 用),留空=纯本地

# --- PostgreSQL ---
PG_HOST=127.0.0.1
PG_PORT=5432
PG_USER=postgres
PG_PASSWORD=postgres
PG_DATABASE=collectai

# --- BigModel 智谱 (OCR + LLM) ---
BIGMODEL_API_KEY=                # 必填,去 https://bigmodel.cn 申请
BIGMODEL_BASE=https://open.bigmodel.cn/api/paas/v4
OCR_MODEL=hand_write             # hand_write / layout_parsing
LLM_MODEL=glm-4-flash            # glm-4-flash / glm-4-plus

# --- cube-agent-server (SKU 库) ---
AGENT_URL=http://127.0.0.1:8088
AGENT_TOKEN=                     # 可选,无则空

# --- 解析行为 ---
OCR_TIMEOUT_SEC=60               # OCR HTTP 超时
LLM_TIMEOUT_SEC=180              # LLM HTTP 超时 (30+ 行盘点单要 60-90s)
USE_LLM=true                     # 走 LLM 解析 (false=纯 OCR)
FUZZY_DISTANCE=2                 # SkuMatcher 模糊匹配编辑距离

# --- 并发限流 ---
MAX_CONCURRENT_PARSE=4           # 同时解析上限 (0=不限流)
RATE_LIMIT_WAIT_SEC=30           # 客户端等 semaphore 超时
```

### 环境变量覆盖示例

Linux / macOS bash:
```bash
export PORT=9090
export BIGMODEL_API_KEY=sk-real-key
./bin/collect-ai-server
```

Windows PowerShell:
```powershell
$env:PORT = "9090"
$env:BIGMODEL_API_KEY = "sk-real-key"
.\bin\collect-ai-server.exe
```

Docker:
```yaml
environment:
  - PORT=8089
  - BIGMODEL_API_KEY=sk-real-key
```

systemd:`EnvironmentFile=/opt/collect-ai/.env` 自动加载。

### 关键设计

- **不读取 `$HOME/.collect-ai`**:历史版本曾读用户目录,但容器 / systemd 用户的 `HOME` 不可靠,现已移除。配置位置完全由项目目录决定。
- **godotenv 默认不覆盖已有 env**:保证 systemd `EnvironmentFile=` / Docker `environment:` / shell 临时 export 都能压过 `.env` 默认值。
- **viper 显式 `BindEnv`**:因为 `viper.SetDefault` 优先级高于 `AutomaticEnv`,必须用 `BindEnv` 显式绑定每个叶子 key,环境变量才能真正生效。代码见 `internal/config/config.go`。
- **`config/config.yaml` 可选**:找不到不会报错,适合把多套环境的非敏感配置抽出来。

### 加载顺序单元测试

`internal/config/config_test.go` 覆盖 5 个场景,确保覆盖顺序不被未来重构破坏:

```powershell
go test ./internal/config/... -v
# 5/5 PASS
```

## 部署

### 方式一:本地直接运行 (开发 / 自用)

适合开发调试和小规模场景。

```powershell
go run ./cmd/server
# 前台运行, Ctrl+C 退出
```

### 方式二:Docker Compose (推荐,含 PG + cube-agent)

完整 c/s 架构一键启动,详见 [`DEPLOY.md`](./DEPLOY.md):

```bash
cd /opt/collect-ai
cp .env.example .env
nano .env   # 填 BIGMODEL_API_KEY
docker compose up -d
# 启动: collectai-pg / collectai-server / collectai-cube-agent
```

### 方式三:Linux systemd (常驻进程,机器重启自启)

适合裸机部署(无 Docker)、需要 systemd 接管生命周期的场景。

#### 1. 准备部署目录

```bash
# 假设部署到 /opt/collect-ai
sudo mkdir -p /opt/collect-ai
sudo mkdir -p /opt/collect-ai/uploads
sudo mkdir -p /var/log/collect-ai

# 传文件
sudo cp bin/collect-ai-server /opt/collect-ai/collect-ai-server
sudo cp .env /opt/collect-ai/.env
sudo cp -r migrations /opt/collect-ai/   # 可选 (现在自动 migrate)
```

#### 2. 创建非 root 用户

```bash
sudo useradd --system --shell /usr/sbin/nologin --home /opt/collect-ai collect-ai
sudo chown -R collect-ai:collect-ai /opt/collect-ai
sudo chown -R collect-ai:collect-ai /var/log/collect-ai
```

#### 3. 创建 systemd unit

`/etc/systemd/system/collect-ai.service`:

```ini
[Unit]
Description=collect-ai Server (OCR + LLM + SkuMatcher backend)
Documentation=https://github.com/tinkler/collect-ai
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=collect-ai
Group=collect-ai
WorkingDirectory=/opt/collect-ai

# 加载 .env 文件, 系统环境变量会覆盖 .env 同名键
EnvironmentFile=/opt/collect-ai/.env

# 启动命令
ExecStart=/opt/collect-ai/collect-ai-server

# 日志
StandardOutput=journal
StandardError=journal
SyslogIdentifier=collect-ai

# 文件描述符 (PG 连接池 + 上传并发需要)
LimitNOFILE=65535

# 自动重启
Restart=always
RestartSec=5
StartLimitIntervalSec=60
StartLimitBurst=10

# 优雅关停 (给 PG 连接池时间 flush)
TimeoutStopSec=15
KillMode=mixed
KillSignal=SIGTERM

# 安全加固
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/collect-ai/uploads /opt/collect-ai/.env

[Install]
WantedBy=multi-user.target
```

#### 4. 启用并启动

```bash
# 重新加载 systemd (改 unit 文件后必跑)
sudo systemctl daemon-reload

# 开机自启 + 立即启动
sudo systemctl enable --now collect-ai

# 看状态
sudo systemctl status collect-ai
```

#### 5. 日常管理命令

```bash
# 看实时日志
sudo journalctl -u collect-ai -f
sudo journalctl -u collect-ai -n 200 --no-pager   # 最近 200 行

# 重启 / 停 / 起
sudo systemctl restart collect-ai
sudo systemctl stop collect-ai
sudo systemctl start collect-ai

# 改 .env 后重载 (systemd 不会自动重读 EnvironmentFile)
sudo systemctl restart collect-ai

# 看完整状态 (含最近日志)
sudo systemctl status collect-ai

# 临时加 / 覆盖单个环境变量 (写到 override 文件)
sudo systemctl edit collect-ai
# 写入:
#   [Service]
#   Environment="PORT=9090"
#   Environment="BIGMODEL_API_KEY=sk-new-key"
sudo systemctl daemon-reload
sudo systemctl restart collect-ai
```

#### 6. 验证

```bash
# 健康检查
curl http://localhost:8089/api/v1/health
# 期望: {"status":"ok","ts":...}

# 看进程
ps aux | grep collect-ai-server | grep -v grep

# 看监听端口
ss -tlnp | grep 8089
```

#### 7. 升级

```bash
# 停服
sudo systemctl stop collect-ai

# 覆盖二进制 (.env 保留)
sudo cp collect-ai-server /opt/collect-ai/collect-ai-server
sudo chown collect-ai:collect-ai /opt/collect-ai/collect-ai-server

# 起服
sudo systemctl start collect-ai
sudo systemctl status collect-ai
```

#### 8. 完整架构示例

```
┌───────────────────────  Linux server  ────────────────────────┐
│                                                              │
│  ┌──────────────────────────┐                                │
│  │  collect-ai.service      │  systemd 管理                 │
│  │  /opt/collect-ai/        │  User=collect-ai              │
│  │   collect-ai-server ◀────┼── systemd                     │
│  │   .env                   │                                │
│  │   uploads/               │                                │
│  └────────┬─────────────────┘                                │
│           │                                                  │
│           │ :8089                                            │
│           ▼                                                  │
│  ┌──────────────────────────┐    ┌─────────────────────────┐ │
│  │  PostgreSQL 16           │    │  cube-agent-server      │ │
│  │  (本机服务 or Docker)     │    │  (本机服务 or Docker)    │ │
│  │  :5432                   │    │  :8088                   │ │
│  └──────────────────────────┘    └─────────────────────────┘ │
│                                                              │
│  Win7 C 端 (collect-ai.exe) ──HTTP──▶ http://<server>:8089   │
│  飞书 H5 (Vue 单页)         ──HTTP──▶ http://<server>:8089   │
└──────────────────────────────────────────────────────────────┘
```

### 方式四:Windows NSSM (Windows 后台服务)

参考 [cube-agent-server README](https://github.com/tinkler/cube-agent-server) 里的 NSSM 用法,把 `collect-ai-server.exe` 注册成 Windows 服务。

## 依赖

- `gin` HTTP
- `pgx/v5/pgxpool` PG 连接池
- `bigmodel` 智谱 BigModel API (OCR + chat)
- `agent` cube-agent-server (拉 SKU)

## 目录

```
cmd/server/             # 入口
internal/
  api/
    handler/            # HTTP handlers
    router.go           # 路由
  config/               # viper 配置加载
  model/                # 数据类型
  parser/
    bigmodel/           # BigModel OCR + LLM 客户端
    agent/              # cube-agent-server 客户端
    matcher/            # SkuMatcher (5 级级联)
    parser.go           # 主解析流程
    ocr_service.go      # OCR 行解析 + 合并行拆分
  store/
    pg.go               # 连接池 + Migrate
    session.go          # 会话仓库
    template.go         # 模板仓库
migrations/             # SQL 迁移 (代码内)
uploads/                # 上传图片 (gitignore)
```

## API 速查

| 方法 | 路径 | 说明 |
|------|------|------|
| GET  | `/api/v1/health` | 健康检查 |
| GET  | `/api/v1/suppliers` | 拉供应商列表 |
| GET  | `/api/v1/templates?supplier=X&default=1&purchase=1` | 飞书端: 拉某供应商的采购+默认模板 |
| GET  | `/api/v1/templates/all` | C# 端: 拉全部模板 |
| POST | `/api/v1/templates/sync` | C# 端: 整体同步 templates.json → PG |
| POST | `/api/v1/parse?supplier=X&mode=inventory` | 上传图片 → OCR+LLM+匹配 (不存库) |
| POST | `/api/v1/sessions?supplier=X&mode=purchase&template_id=Y&source=feishu` | 上传图片 + 存库 |
| GET  | `/api/v1/sessions?supplier=X&limit=50` | 列表 (按 created_at 倒序) |
| GET  | `/api/v1/sessions/:id` | 单条详情 |
| DELETE | `/api/v1/sessions/:id` | 整条删 |
| GET  | `/api/v1/sessions/:id/export` | TXT 导出 |
| PUT  | `/api/v1/sessions/:id/rows/:rowId` | 改某行 (body: `{"matched_barcode":"...","matched_name":"...","qty":5}`) |
| DELETE | `/api/v1/sessions/:id/rows/:rowId` | 软删某行 (is_deleted=true) |
| GET  | `/uploads/:path` | 静态图片 (本地) |

## 关键设计

### 解析流程
```
[Client] POST /api/v1/sessions multipart
  ↓
[Handler] 收图 + 调 Parser.ParseImageBytes
  ↓
[OcrClient] BigModel /files/ocr (hand_write) → words_result
  ↓
[Parser] OcrService.ParseOcrResponse
   1) 按 top 分行 (自适应行高阈值)
   2) SplitMergedLines (A: 多 block barcode 切; B: 单 block 文本多 barcode 切)
  ↓
[LlmClient] BigModel /chat/completions (glm-4-flash)
   - Default prompt: 8 列盘点单 / 采购单
   - 客户端预拆合并行后再调 LLM
  ↓
[LlmService.ParseLlmJson] 解析 + 客户端二次过滤
   - 标题/表头/孤立/签名/合并残留 全部 skip
  ↓
[AgentClient] cube-agent-server /v1/load → 供应商 SKU
  ↓
[SkuMatcher] 5 级级联 (barcode → name → fuzzy → substring → IsNew)
  ↓
[Handler] 存 PG + 返回 SessionDetail
```

### 模板同步
- C# 端 templates.json 是 source of truth
- 每次修改后, C# 端点 "同步模板→后端" 按钮 → POST /api/v1/templates/sync (整体覆盖)
- 飞书端只看到 `Mode=purchase` 且 `IsDefault=true` 的模板

### 导出规则
- 排除 `is_deleted=true` 和 `is_new=true` 行
- TXT 格式: `barcode<TAB>qty<TAB>unitPrice(可选)` + CRLF
- 头注释: `# session_id / supplier / created_at / note`
- 盘点模式 + 存在 `stock_mismatch=true` 行 → 返回 409 (拒绝)

## 已知限制

- 后端只读 cube-agent-server (拉 SKU), 不写
- 图片存本地文件系统, 不上云 OSS
- 库存/价格同步靠 agent /v1/load 实时拉, 不缓存
- 单 PG 实例 (无读写分离)
