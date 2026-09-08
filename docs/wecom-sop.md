# 企业微信智能机器人接入 SOP（**长连接模式**）

> 适用版本: collect-ai 2026-08-28 起
> 模块路径: `internal/restock/`
> 长连接 SDK 自实现,无外部依赖

---

## 一、为什么是长连接,不是 HTTP 回调

企业微信智能机器人是 2024 年推出的**新型应用**,只支持 WebSocket 长连接模式,不支持 HTTP 回调。本模块用自实现 WS 客户端(无依赖)。

| 模式 | 凭证 | 接消息 | 发消息 |
|---|---|---|---|
| **长连接**(本模块) | BotID + Secret(2 个) | WS 收 `aibot_event_callback` 帧 | WS 发 `aibot_send_msg` 帧 |
| HTTP 回调(已废弃) | AgentID + AgentSecret + Token + AESKey(5 个) | HTTP POST 回调 | HTTP POST |

---

## 二、申请步骤

### 1. 创建智能机器人(API 模式 + 长连接)

- 企微工作台 → 智能机器人 → 创建机器人
- 类型选「**手动创建**」
- 拉到页面底部 → 「**API 模式创建**」
- API 配置页 → 连接方式选「**使用长连接**」
- 保存后页面生成 **Bot ID** + **Secret**(**Secret 只显示一次,务必保存!**)

### 2. 配置 .env

```ini
WECOM_BOT_ID=aib64EeZo2VaAZ0zP-AcEB1MgZvlY01sXQr
WECOM_BOT_SECRET=1d8KUej9lBx5tLKetfSBGlSXGNHdgNd4PnIaVGVMDgq
WECOM_WS_URL=wss://openws.work.weixin.qq.com    # 默认值,一般不用改
WECOM_BIND_FILE=./wecom_bindings.json           # 群 chat_id 绑定持久化
```

### 3. 启动服务,长连接自动建立

```powershell
go run .\cmd\server
```

日志看到:
```
[wecom] dialing wss://openws.work.weixin.qq.com ...
[wecom] ws handshake ok, key=...
[wecom] subscribed, bot_id=...
```
说明长连接 OK。

### 4. 在企微手动建 2 个群(关键差异: **没有 API 建群**)

⚠️ **长连接模式没有「创建群」API**,chat_id 必须在群里发消息后由企微推过来。

- 企微 → 群聊 → 创建群
- **群 1: 卖场补货群** → 拉机器人(超市 AI 助手)进群
- **群 2: 办公室管理群** → 拉机器人进群

### 5. 触发 chat_id 自动发现

让**任意成员**(包括你自己)在 2 个群里**@机器人 发任意一句话**。

收到后长连接会:
```
[wecom] NEW chat discovered: wrOdJLCQAAbCdEfGhIjKlMnOpQrStUvWx (use POST /api/v1/restock/wecom/chats/bind to bind role)
```

也可以查已发现的 chat_id:
```powershell
curl http://127.0.0.1:8089/api/v1/restock/wecom/chats
```

返回:
```json
{
  "chats": [
    {"chat_id":"wrXXX...","role":"","first_seen":"2026-08-28T13:00:00Z"}
  ],
  "count": 1,
  "connected": true
}
```

### 6. 人工绑定 role(floor / office)

```powershell
# 卖场群
$body = @{ chat_id = "wrXXX..."; role = "floor" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8089/api/v1/restock/wecom/chats/bind" -Body $body -ContentType "application/json"

# 办公室群
$body = @{ chat_id = "wrYYY..."; role = "office" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8089/api/v1/restock/wecom/chats/bind" -Body $body -ContentType "application/json"
```

绑定会**持久化到 `wecom_bindings.json`**,服务重启后自动加载。

### 7. 测试推送

```powershell
$body = @{ role = "floor"; text = "🛒 测试消息" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8089/api/v1/restock/wecom/chats/test" -Body $body -ContentType "application/json"
```

群里应立即收到测试消息。

---

## 三、长连接技术细节

### 协议
- URL: `wss://openws.work.weixin.qq.com`
- 握手后发 `aibot_subscribe` 携带 bot_id + secret
- 每 30 秒发 `{"cmd":"ping"}` 保活
- 自动重连(指数退避 2s → 60s)

### 收事件类型
- `aibot_msg_callback` — 文本消息
- `aibot_event_callback`:
  - `enter_chat` — 用户进入单聊
  - `template_card_event` — **按钮点击** ← 补货反馈用这个
  - `feedback_event` — 反馈事件
  - `disconnected_event` — 新连接踢旧连接

### 按钮 key 格式
模块用 `"DONE|<task_id>"` / `"SHORT|<task_id>"` 编码,长连接回调时:
```json
{
  "cmd": "aibot_event_callback",
  "body": {
    "event": {
      "eventtype": "template_card_event",
      "template_card_event": {
        "card_type": "button_interaction",
        "event_key": "DONE|restock-0001-ITEM-001",
        "task_id": "..."
      }
    }
  }
}
```
→ 自动解析写入 `restock_feedback` 表 + 改 task status。

### 频率限制
- **30 条/分钟/会话**
- 1000 条/小时/会话

补货 cron 单 tick 推 ≤ 20 条(`RESTOCK_MAX_PUSH_PER_TICK` 配),不会撞。

### 主动发消息前置条件
- 用户必须**先在会话里发过消息**(24 小时内),机器人才能主动推
- 单聊: 24 小时内有过消息
- 群聊: 群里有人 @ 机器人过

---

## 四、常见坑

| 现象 | 原因 | 解决 |
|---|---|---|
| 日志 `ws handshake failed` | 网络问题 / 防火墙挡了 443 | 测试 `Test-NetConnection openws.work.weixin.qq.com -Port 443` |
| 日志 `subscribe errcode=xxxxx` | bot_id / secret 错 | 重新复制 WECOM_BOT_SECRET(**Secret 只显示一次**)|
| 群里发消息没反应 | 1) 机器人没被 @ 2) 长连接没起 3) 飞书 SDK 占用 | @机器人 + 看日志 `NEW chat discovered` |
| 主动发消息 200 错误 "no chat_id for role" | chat_id 没绑定 | 调 `/api/v1/restock/wecom/chats/bind` |
| 主动发消息 200 错误 "用户没在会话发过消息" | 24h 内没人 @ 机器人 | 让人在群里 @ 机器人发句话 |
| 长连接频繁断开 | 心跳 25s 太接近 30s 边界 / 网络抖动 | 看日志 `disconnected_event`,会自动重连 |

---

## 五、API 端点速查

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/restock/wecom/chats` | 列出已发现/已绑定的所有 chat_id + 长连接状态 |
| POST | `/api/v1/restock/wecom/chats/bind` | `{chat_id, role: floor/office/""}` 绑定 |
| POST | `/api/v1/restock/wecom/chats/test` | `{chat_id 或 role, text}` 测试发消息 |
| GET | `/api/v1/restock/tasks` | 查 task 列表 |
| POST | `/api/v1/restock/cron/tick` | 手动跑一次 cron tick(测试用)|

---

## 六、安全注意

- **Secret 不能进 git**(在 .gitignore)
- `wecom_bindings.json` 也不要进 git(包含群 ID 信息)
- 服务端只接受长连接推送,无外部 HTTP 暴露
- 按钮 key 含 `task_id`,泄露后能看到内部 SKU 编号(风险低,内部用)
