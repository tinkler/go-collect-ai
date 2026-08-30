# 鉴权 (Auth) — collect-ai 后端

> 2026-08-29 加入, 配套前端 H5 (supermarket-ai) OAuth 免登流程
> 详细 API 契约: `F:\weixinapp\supermarket-ai\docs\auth-api-contract.md`

## TL;DR

```bash
# 1. 启动 (默认 dev 模式, JWT 用默认 secret)
cd F:\go\src\github.com\tinkler\collect-ai
go run ./cmd/server

# 2. dev 登录拿 owner token
python F:\weixinapp\supermarket-ai\get_token.py owner
#  → 输出 eyJhbGciOiJIUzI1NiIs...

# 3. 用 token 调业务
curl -H "Authorization: Bearer <token>" http://127.0.0.1:8089/api/v1/suppliers
```

## 架构

```
        ┌─────────────┐
        │ H5 浏览器    │
        └──────┬──────┘
               │ Authorization: Bearer <access>
               │ Cookie: refresh=<opaque>
               ▼
   ┌───────────────────────┐
   │ collect-ai (Gin)      │
   │  ┌────────────────┐   │
   │  │ AuthMiddleware │──┼──→ 401 if bad
   │  │ RequirePerm    │──┼──→ 403 if denied
   │  └────────────────┘   │
   │  ┌────────────────┐   │
   │  │ Handler        │   │
   │  └────────┬───────┘   │
   └───────────┼───────────┘
               ▼
       ┌──────────────┐
       │ PG: users/   │
       │ role_perms/  │
       │ auth_sessions│
       └──────────────┘
```

### Token 流程

| 步骤 | 说明 |
|---|---|
| 1. 登录 | `/auth/wecom/callback` 或 `/auth/dev-login` → 验企微 / 查 user → 签 access (15min HS256) + refresh (7d opaque) |
| 2. 访问业务 | `Authorization: Bearer <access>`, AuthMiddleware 验签 → 塞 ctx (user_id, tenant_id, role) |
| 3. 权限检查 | RequirePerm(perm) 查内存 role_permissions map, owner 用 `*` 通配 |
| 4. 续期 | access 过期 → 调 `/auth/refresh` (带 refresh cookie) → 验签 + 查 session (bcrypt) → 签新 access + 新 refresh (rotation) |
| 5. 登出 | `/auth/logout` → 软删 session + 清 cookie |

### 数据流

- **access token**: JWT HS256, claims `{sub, tid, role, iat, exp}`, 走 `Authorization: Bearer` 头
- **refresh token**: 形态 `rt.<jwt>`, 走 httpOnly + SameSite=Lax + Secure(prod) cookie
- **存库**: `auth_sessions.refresh_hash = bcrypt(sha256(refresh))` — bcrypt 限 72 字节, 所以先 SHA-256 缩短
- **rotation**: 每次 refresh 软删旧 session + 发新 session, 防重放
- **RBAC**: `role_permissions` 表启动时全量加载到内存 `sync.RWMutex` map, 4 个 role × 几十 perm, 内存可忽略

## 启动

### Dev 模式 (默认)

```bash
# .env 里默认 DEV_MODE=true, 不需要企微凭证
go run ./cmd/server
```

可用账号 (在 `users` 表 seed):

| user_id | name | role | 能做的 |
|---|---|---|---|
| `u_owner` | 梁老板(店主) | owner | 全部 (`*` 通配) |
| `u_manager` | 李店长 | manager | session:*, row:*, plan:*, inventory:* |
| `u_buyer` | 王采购 | buyer | session:*, row:*, plan:*, inventory:view |
| `u_cashier` | 陈收银 | cashier | session:read, plan:read (只读) |

dev 登录:

```bash
python F:\weixinapp\supermarket-ai\get_token.py owner     # 拿 owner token
python F:\weixinapp\supermarket-ai\get_token.py cashier   # 拿 cashier token (验证 403)
```

### 企微 OAuth (H5 免登)

1. **企微后台建自建应用**:
   - 我的企业 → 应用管理 → 自建 → 创建应用
   - 记下 `AgentId` 和 `Secret`

2. **H5 配置 OAuth 授权**:
   - 应用 → 网页授权及 JS-SDK → 设置可信域名
   - 配置回调 URL (`https://your-domain/auth/wecom-callback.html`)

3. **填 .env**:
   ```env
   DEV_MODE=false
   WECOM_CORP_ID=ww1234567890abcdef
   WECOM_AGENT_ID=1000002
   WECOM_CORP_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
   JWT_SECRET=<32 字符以上强随机串, 不要用默认>
   COOKIE_DOMAIN=your-domain.com
   COOKIE_SECURE=true   # prod 必须 https
   ```

4. **前端** (已就绪):
   - 用户打开 H5 → 企微 SDK 拉 code → POST `/auth/wecom/callback`
   - 后端用 corpSecret 换 userid → upsert user (默认 cashier) → 签 token + 写 cookie
   - 后续业务请求带 access token, 401 后调 `/auth/refresh`

### 生产 checklist

- [ ] `DEV_MODE=false` (禁用 dev-login)
- [ ] `JWT_SECRET` ≥32 字符, **不要用默认 dev 值** (启动会 WARN 但不阻断)
- [ ] `WECOM_CORP_SECRET` 走 secrets manager, **不要进 git / .env.example**
- [ ] `COOKIE_SECURE=true` (必须 https)
- [ ] `COOKIE_DOMAIN` 设为实际域名 (不带协议, 例 `example.com`)
- [ ] nginx 反代转发 `Set-Cookie` 头 (默认会, 但注意 `proxy_pass_header Set-Cookie` 和 `proxy_hide_header` 配置)
- [ ] 启用 CORS 白名单 (现在反射 Origin, 严格生产环境改白名单)

## 添加新角色 / 权限

### 加一个新 perm

```sql
INSERT INTO role_permissions (role, perm) VALUES
  ('manager', 'mynew:action'),
  ('buyer',   'mynew:action')
ON CONFLICT DO NOTHING;
```

改完 SQL 后, **重启服务** (目前不热加载)。如果想热加载: 调 `auth.Store.ReloadRolePerms(ctx)`。

### 加一个新 role

```sql
-- 1. 加用户
INSERT INTO users (id, name, role, tenant_id, source)
VALUES ('u_alice', 'Alice', 'supervisor', 't_dev', 'dev')
ON CONFLICT (id) DO NOTHING;

-- 2. 加权限
INSERT INTO role_permissions (role, perm) VALUES
  ('supervisor', 'session:read'),
  ('supervisor', 'plan:read')
ON CONFLICT DO NOTHING;

-- 3. router 里给需要的路由加 RequirePerm
authed.GET("/sessions", auth.RequirePerm("session:read"), h.ListSessions)
```

### 临时禁用某 role

```sql
UPDATE users SET status='disabled' WHERE id='u_cashier';
```

下次 `/auth/dev-login` 返 403 `{"code":"FORBIDDEN","message":"user disabled"}`。

## Cookie / CORS / Nginx

### Cookie

- 名称: `refresh`
- `HttpOnly; SameSite=Lax; Path=/; Max-Age=604800` (7d)
- 失败时 (refresh 过期 / 验签失败) 后端 `Set-Cookie: refresh=; Max-Age=0` 立刻清

### CORS

- 当前实现: 反射 `Origin` 头, 允许 `GET,POST,PUT,DELETE,OPTIONS` + `Content-Type,Authorization,X-Requested-With` + `Access-Control-Allow-Credentials: true`
- **生产建议改白名单** (改 `router.go` 里 CORS 中间件, 例: `if origin == "https://h5.example.com" { ... } else { origin = "" }`)
- 企微 H5 同源场景不需要 CORS, 主要是 dev (8089 后端 vs 8090 前端) 需要

### Nginx 反代 (生产)

```nginx
location /api/ {
    proxy_pass http://127.0.0.1:8089;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    # 重要: 透传 Set-Cookie 头 (默认会, 但确认下)
    proxy_pass_header Set-Cookie;

    # 超时: OCR/LLM 类可能跑 2min
    proxy_read_timeout 300s;
    proxy_send_timeout 300s;
}
```

`PUBLIC_BASE_URL` 设成 `https://your-domain.com`, 这样 `parse_session.image_urls` 生成的图片 URL 就能被前端 / 企微 H5 访问。

## API 速查

### 公开 (不需 token)

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/v1/health` | 健康检查 |
| POST | `/api/v1/auth/wecom/callback` | 企微 OAuth 登录 |
| POST | `/api/v1/auth/dev-login?as_user=u_xxx` | dev 登录 (仅 DevMode=true) |
| POST | `/api/v1/auth/refresh` | 用 refresh cookie 换新 access |
| POST | `/api/v1/auth/logout` | 撤销 session + 清 cookie (需 token) |
| GET | `/api/v1/auth/me` | 当前 user 信息 (需 token) |

### 业务 (需 token + perm)

完整 perm 列表见 `role_permissions` 表 seed。下面是 router 里挂的 perm:

| 路径 | perm |
|---|---|
| `GET /suppliers`, `/suppliers/by-brand` | `session:read` |
| `GET /templates`, `/templates/all` | `session:read` |
| `POST /templates/sync` | `admin` (仅 owner) |
| `POST /parse` | `session:create` |
| `POST /rematch` | `session:update` |
| `GET /sessions`, `/sessions/:id`, `/sessions/:id/export` | `session:read` |
| `POST /sessions` | `session:create` |
| `DELETE /sessions/:id` | `session:delete` |
| `PUT /sessions/:id/rows/:rowId` | `row:update` |
| `DELETE /sessions/:id/rows/:rowId` | `row:delete` |
| `GET /purchase-plans` | `plan:read` |
| `GET /datasources`, `/products/search` | `session:read` |
| `GET /datasource`, `POST /datasource` | `admin` |
| `GET /restock/tasks`, `/restock/need-purchase`, `/restock/llm/plan` | `plan:read` |
| `POST /restock/cron/tick` | `admin` |
| `/restock/wecom/chats/*` (GET/POST) | `admin` |

owner role 的 `*` 通配, 所有 perm 自动通过。

## 错误格式

```json
{
  "code": "FORBIDDEN",
  "message": "user u_cashier lacks perm session:create",
  "detail": { "required": "session:create", "user_id": "u_cashier", "role": "cashier" }
}
```

| HTTP | code | 含义 |
|---|---|---|
| 400 | `MISSING_CODE` / `BAD_REQUEST` | 缺参数 / 格式错 |
| 401 | `UNAUTHORIZED` | 无 / 失效 access token |
| 401 | `REFRESH_EXPIRED` | refresh cookie 失效 (前端跳登录) |
| 403 | `FORBIDDEN` | token 有效但权限不够 |
| 404 | `NOT_FOUND` / `USER_NOT_FOUND` | 资源不存在 |
| 500 | `INTERNAL` | 后端错误 |
| 502 | `WECOM_API_ERROR` | 企微 API 调用失败 |

## 已知限制 (本期 out of scope)

- 角色管理 UI (改 SQL)
- 租户管理 (单 tenant `t_dev`)
- 多企业 (单 corpId)
- 密码登录 / 短信验证 / 2FA
- 全设备登出 (只清当前 session)
- 角色权限热加载 (改 SQL 后需重启)
- 用户管理 API (只能 SQL 改 users 表)
- 审计日志表已建但暂未写入 (后续可加 middleware)

## 测试

```bash
# 单元测试
go test ./internal/auth/...

# 端到端冒烟 (鉴权)
python F:\weixinapp\supermarket-ai\smoke_auth.py
#  → 8/8 PASS (1 health + 7 auth 步骤)

# 老业务冒烟 (带 token, 走 SKIP_UPLOAD 跳过慢 OCR)
SKIP_UPLOAD=1 python F:\weixinapp\supermarket-ai\smoke.py
#  → 8 步全跑 (0-7 + 静态图失败因为 PUBLIC_BASE_URL 空)
```

## 代码结构

```
internal/
├── auth/                     # 本期新增
│   ├── jwt.go                # HS256 sign / parse
│   ├── bcrypt.go             # sha256 + bcrypt (绕 72 字节限制)
│   ├── rand.go               # crypto/rand hex
│   ├── store.go              # pgx users / role_permissions / auth_sessions
│   ├── wecom.go              # 企微 access_token 缓存 + getuserinfo
│   ├── service.go            # 业务编排 (WeComCallback, DevLogin, Refresh, Logout, Me)
│   ├── middleware.go         # AuthMiddleware, RequirePerm, cookie helpers
│   ├── handler.go            # gin HTTP handler
│   ├── errors.go             # 错误码 + apiError
│   └── auth_test.go          # 单元测试
├── config/config.go          # + 9 个 auth 字段
├── store/pg.go               # + 4 张表 + 2 个 seed
└── api/router.go             # + 鉴权路由 + middleware 包裹
```
