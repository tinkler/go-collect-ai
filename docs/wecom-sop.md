# 企业微信智能机器人申请 SOP（restock 模块用）

> 适用版本:collect-ai 2026-08-26 起
> 模块路径:`internal/restock/`
> 前置:已有企业微信管理后台账号

---

## 一、为什么不能用群机器人 Webhook

| 渠道 | 能否接收员工反馈 | 能否加按钮 | 用法 |
|---|---|---|---|
| **群机器人 Webhook** | ❌ 不能 | ❌ 不能 | 只适合单向通知 |
| **应用消息（智能机器人）** | ✅ 能（回调 URL） | ✅ 能（button_interaction） | ✅ 本模块用这个 |

本模块要员工点击"已补货 / 缺货/异常"按钮，**必须用智能机器人**。

---

## 二、申请步骤

### 1. 登录管理后台

URL: https://work.weixin.qq.com/wework_admin/

### 2. 创建智能机器人应用

- 我的企业 → 应用管理 → 创建应用
- 应用类型选 **"智能机器人"**（不是"自建应用"也不是"群机器人"）
- 应用名称示例:`超市 AI 助手`
- 应用 logo: 准备 200x200 PNG
- 应用简介:`智能补货调度与员工反馈`
- 可见范围:添加「卖场员工组」「办公室员工组」两个部门/标签

**记录 3 个关键值**（写入 `.env`）:

| 项 | env 变量 | 样例 |
|---|---|---|
| 企业 ID (CorpID) | `WECOM_CORP_ID` | `ww1234567890abcdef` |
| 应用 AgentId | `WECOM_AGENT_ID` | `1000002` |
| 应用 Secret | `WECOM_AGENT_SECRET` | `XxXxXxXxXxXxXxXxXxXxXxXxXxXxXxXx` |

### 3. 申请模板卡片权限

智能机器人默认没开通 `template_card.button_interaction` 类型，要走审批。

- 应用详情 → 模板卡片 → 申请权限
- 申请理由示例:
  > 用于向卖场员工群推送补货任务卡片,员工点击"已补货"或"缺货/异常"按钮反馈,数据回调至业务服务器。需要 button_interaction 卡片类型。

审批通常 1-2 个工作日。

### 4. 配置接收消息服务器

- 智能机器人 → 接收消息 → 设置 API 接收
- URL:`https://your-domain.com/wecom/callback`
  - **必须 HTTPS + 公网可访问 + ICP 备案**
  - 开发期可临时用内网穿透:`ngrok http 8089` → 拿到 `https://xxx.ngrok.io`
- Token:自己生成 32 位字符串(随机字母数字)
  - 写入 `WECOM_CALLBACK_TOKEN`
- EncodingAESKey:点"随机生成"(43 位 base64)
  - 写入 `WECOM_CALLBACK_AES_KEY`
- 点"保存"前**先保证 collect-ai 服务已起,且 `/wecom/callback` GET 端点能返回 echostr 验证成功**

### 5. IP 白名单

- 智能机器人 → 企业可信 IP
- 添加 collect-ai 服务器的公网 IP(不是 127.0.0.1)
- 多服务器时全部加上

### 6. 建 2 个群并拿到 chat_id

**chat_id 怎么拿**（管理后台不显示,必须用 API 创建时返回）:

#### 方式 A: 用本服务提供的端点(推荐,一键建好两个群)

**前置**: 你需要一个企微 userid 作为群主(owner)。获取方法:
- 企微管理后台 → 通讯录 → 选中一个用户 → 详情页 URL 上有 `userid=xxx` 字段
- 或者你自己的企微 → 「我」页面 → 个人信息里有你的 userid

**建群端点**: `POST http://127.0.0.1:8089/api/v1/restock/wecom/chat`

```powershell
# 建卖场群
$body = @{
    name    = "卖场补货群"
    owner   = "ZhangSan"          # ← 换成你的 userid
    userlist = @()                # ← 先空,群建好后手动拉人
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8089/api/v1/restock/wecom/chat" -Body $body -ContentType "application/json"

# 响应:
# { "ok": true, "chat_id": "wrOdJLCQAAbCdEfGhIjKlMnOpQrStUvWx", "name": "卖场补货群", "hint": "..." }
```

把 `chat_id` 写入 `.env` 的 `WECOM_FLOOR_CHAT_ID`。

同样的方法建办公室群,把 chat_id 写入 `WECOM_OFFICE_CHAT_ID`。

#### 方式 B: 用企微原生 API

```
POST https://qyapi.weixin.qq.com/cgi-bin/appchat/create?access_token={token}
Content-Type: application/json
{"name":"卖场补货群","owner":"ZhangSan","userlist":[]}
```

返回 `chatid` 字段。

#### 群建好后,手动拉人

企微里打开群 → 右上角 → 群机器人 / 群管理 → 添加成员

#### ⚠️ 常见错

| errcode | 原因 | 解决 |
|---|---|---|
| `86003` | owner 不在应用可见范围 | 把这个人加进应用的可见部门 |
| `86004` | 群名含敏感词 | 换个名字 |
| `60011` | 没有该应用的权限 | 应用详情 → 权限管理 → 群应用权限开通 |

### 7. 验证回调连通

```powershell
# 本地起 collect-ai (需要 .env 已配)
go run ./cmd/server

# ngrok 暴露
ngrok http 8089
# 拿到 https://xxxx.ngrok.io

# 浏览器访问(企微后台配置 URL 时会用)
curl "https://xxxx.ngrok.io/wecom/callback?msg_signature=test&timestamp=1234567890&nonce=abc&echostr=test"
# 此时应该返回 400 (signature 校验失败,说明服务可达)
```

### 8. 首次配置回调 URL 验证

- 企微后台 → 接收消息服务器 → 点"保存"
- 企微会发一个 GET 请求到 URL,带 `msg_signature / timestamp / nonce / echostr`
- collect-ai 的 `VerifyURL` handler 校验签名 + 解密 → 返回明文 echostr
- 企微后台显示"配置成功"

---

## 三、.env 模板

```ini
# ============== 补货模块(restock)==============
RESTOCK_BRANCH_NO=0001

# 触发 / 水位
RESTOCK_ROP_FACTOR=1.5
RESTOCK_OUT_DAYS=7
RESTOCK_OUT_PROMO_BOOST=1.3
RESTOCK_SAFETY_MIN=5
RESTOCK_DAILY_AVG_W_OLD=0.4
RESTOCK_DAILY_AVG_W_7D=0.4
RESTOCK_DAILY_AVG_W_30D=0.2

# 节流
RESTOCK_PUSH_FLOOR_MIN_INTERVAL_MIN=30
RESTOCK_PUSH_OFFICE_P0_MIN_MIN=15
RESTOCK_PUSH_OFFICE_P1_MIN_MIN=60
RESTOCK_PUSH_OFFICE_P2_MIN_MIN=360
RESTOCK_MAX_PUSH_PER_TICK=20

# 静默升级
RESTOCK_ESCALATE_P2_TO_P1_HOURS=24
RESTOCK_ESCALATE_P1_TO_P0_HOURS=12

# cron(标准 5 段:分 时 日 月 周)
RESTOCK_CRON_HOURLY=0 7-21 * * *       # 每小时 7-21 点整点
RESTOCK_CRON_AGGREGATE=30 21 * * *     # 每天 21:30 聚合 need_purchase
RESTOCK_CRON_LLM_PLAN=0 1,7,13,19 * * *  # 每天 4 次批量 LLM 算补货量

# cube 名(空 = 默认)
RESTOCK_CUBE_SALES=sales_yesterday
RESTOCK_CUBE_INVENTORY=inventory_current
RESTOCK_CUBE_PROMOTION=promotion_plan_7d

# LLM
RESTOCK_LLM_ENABLED=true
RESTOCK_LLM_PLAN_ENABLED=true
RESTOCK_LLM_MODEL=glm-4-flash
RESTOCK_LLM_PLAN_CACHE_HRS=6

# ============== 企微智能机器人 ==============
WECOM_CORP_ID=ww1234567890abcdef
WECOM_AGENT_ID=1000002
WECOM_AGENT_SECRET=YOUR_SECRET_HERE
WECOM_CALLBACK_TOKEN=YOUR_32_CHAR_TOKEN
WECOM_CALLBACK_AES_KEY=YOUR_43_CHAR_BASE64
WECOM_FLOOR_CHAT_ID=wrXXXXXXXXXXXXXXXXXX
WECOM_OFFICE_CHAT_ID=wrYYYYYYYYYYYYYYYYYY
```

---

## 四、常见坑

| 坑 | 现象 | 解决 |
|---|---|---|
| URL 配置一直失败 | "URL 校验失败" | 1) 服务没起 2) 防火墙挡了 3) URL 是 HTTP 不是 HTTPS |
| 配置成功后回调不响应 | 员工点按钮无反应 | 1) IP 没加白名单 2) 服务异常挂掉 3) 没解密成功(EncodingAESKey 配错) |
| 模板卡片发不出去 | "没有 button 权限" | 没申请 button_interaction 权限,要去企微后台申请 |
| 群消息发不出 | "chatid 不存在" | chat_id 错了,确认是 `wr` 开头的那串 |
| access_token 频繁失效 | 推送偶发 40001 | 进程内缓存正常,检查是否多实例各自刷新 |
| 签名校验失败 | "signature mismatch" | 1) Token 配错 2) 系统时间不准(差 > 5min) |

---

## 五、企微 API 调用速查

### 取 access_token

```
GET https://qyapi.weixin.qq.com/cgi-bin/gettoken
    ?corpid={corp_id}&corpsecret={agent_secret}
```

响应:`{"access_token":"...","expires_in":7200}`

### 发群应用消息

```
POST https://qyapi.weixin.qq.com/cgi-bin/appchat/send?access_token={token}
Content-Type: application/json

{
  "chatid": "wrXXXXX",
  "msgtype": "template_card",
  "template_card": {
    "card_type": "button_interaction",
    "main_title": {"title": "...", "desc": "..."},
    "horizontal_content_list": [...],
    "task_id": "restock-0001-001",
    "button_list": [
      {"text": "✅ 已补货", "style": 1, "key": "DONE|restock-0001-001"},
      {"text": "❌ 缺货/异常", "style": 2, "key": "SHORT|restock-0001-001"}
    ]
  }
}
```

完整文档:https://developer.work.weixin.qq.com/document/path/90236

### 接收消息回调

```
POST https://your-domain.com/wecom/callback
Query: msg_signature / timestamp / nonce
Body: <xml><Encrypt>BASE64_AES_CIPHERTEXT</Encrypt></xml>
```

解密后得到:
```xml
<xml>
  <ToUserName>...</ToUserName>
  <FromUserName>USER_ID</FromUserName>
  <CreateTime>1700000000</CreateTime>
  <MsgType>event</MsgType>
  <Event>template_card_event</Event>
  <TaskId>restock-0001-001</TaskId>
  <CardType>button_interaction</CardType>
  <ResponseCode>DONE|restock-0001-001</ResponseCode>  <!-- 按钮 key -->
</xml>
```

> 注: 实际企微回调 XML 结构请以官方文档为准(本模块 `callback.go` 的解析做了简化兜底,真实接入时建议按完整 XML 解析)
