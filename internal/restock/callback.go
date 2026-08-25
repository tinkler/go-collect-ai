package restock

import (
	"encoding/xml"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// CallbackHandler 企微回调 HTTP handler
//   GET  /wecom/callback  → URL 验证
//   POST /wecom/callback  → 业务事件(目前只处理 button click)
type CallbackHandler struct {
	WeCom *WeCom
	Store *Store
}

// CallbackVerifyResponse URL 验证成功响应(XML)
type CallbackVerifyResponse struct {
	XMLName xml.Name `xml:"xml"`
	Encrypt string   `xml:"Encrypt"`
	MsgSig  string   `xml:"MsgSignature"`
	TimeStamp string `xml:"TimeStamp"`
	Nonce    string   `xml:"Nonce"`
}

func (h *CallbackHandler) VerifyURL(c *gin.Context) {
	signature := c.Query("msg_signature")
	timestamp := c.Query("timestamp")
	nonce := c.Query("nonce")
	echostr := c.Query("echostr")

	plain, err := h.WeCom.VerifyURL(signature, timestamp, nonce, echostr)
	if err != nil {
		log.Printf("[restock] VerifyURL failed: %v", err)
		c.String(400, "verify failed: %v", err)
		return
	}
	c.String(200, plain)
}

// CallbackEvent 企微消息 envelope(XML)
type CallbackEvent struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	CreateTime int64    `xml:"CreateTime"`
	MsgType    string   `xml:"MsgType"`
	Event      string   `xml:"Event"`
	// 模板卡片事件专用
	TaskID    string `xml:"TaskId"`
	CardType  string `xml:"CardType"`
	ResponseCode string `xml:"ResponseCode"`
	AgentID   string `xml:"AgentID"`
}

// OnEvent POST 业务消息
func (h *CallbackHandler) OnEvent(c *gin.Context) {
	signature := c.Query("msg_signature")
	timestamp := c.Query("timestamp")
	nonce := c.Query("nonce")

	// 企微发的 body 是 XML,内层 <Encrypt>...</Encrypt>
	bodyBs, err := c.GetRawData()
	if err != nil {
		c.String(400, "read body: %v", err)
		return
	}

	var env struct {
		Encrypt string `xml:"Encrypt"`
	}
	if err := xml.Unmarshal(bodyBs, &env); err != nil {
		log.Printf("[restock] callback body parse: %v body=%s", err, Truncate(string(bodyBs), 200))
		c.String(400, "bad xml")
		return
	}

	plain, err := h.WeCom.DecryptEvent(signature, timestamp, nonce, env.Encrypt)
	if err != nil {
		log.Printf("[restock] callback decrypt: %v", err)
		c.String(400, "decrypt: %v", err)
		return
	}

	var evt CallbackEvent
	if err := xml.Unmarshal(plain, &evt); err != nil {
		log.Printf("[restock] callback event parse: %v", err)
		c.String(400, "event parse")
		return
	}

	log.Printf("[restock] callback: msgtype=%s event=%s task_id=%s user=%s",
		evt.MsgType, evt.Event, evt.TaskID, evt.FromUserName)

	// 只处理 template_card_event (按钮点击)
	if evt.Event != "template_card_event" || evt.TaskID == "" {
		c.String(200, "success")
		return
	}

	// 解析 button key
	//   企微回调里 button 的 key 在 CardType / ResponseCode 之外的字段
	//   实际企微文档里是 <Button> 节点,这里简化:从 task_id 反推 (DONE / SHORT)
	//   真实接入时按企微回调 XML 完整解析
	kind, taskID := parseButtonKey(evt)
	if taskID == "" {
		taskID = evt.TaskID
	}

	if kind == "" {
		log.Printf("[restock] unknown button key for task=%s, skip", taskID)
		c.String(200, "success")
		return
	}

	ctx := c.Request.Context()
	// 写反馈
	fb := &Feedback{
		TaskID:       taskID,
		FeedbackType: kind,
		FeedbackUser: evt.FromUserName,
		FeedbackTime: time.Now(),
	}
	if err := h.Store.InsertFeedback(ctx, fb); err != nil {
		log.Printf("[restock] insert feedback: %v", err)
		c.String(500, "db")
		return
	}

	// 改 task 状态
	newStatus := TaskStatusAcked
	if kind == FeedbackShort {
		newStatus = TaskStatusShort
	}
	if err := h.Store.UpdateStatus(ctx, taskID, newStatus); err != nil {
		log.Printf("[restock] update status: %v", err)
	}

	log.Printf("[restock] feedback recorded: task=%s kind=%s user=%s", taskID, kind, evt.FromUserName)
	c.String(200, "success")
}

// parseButtonKey 从 event XML 里解析 (kind, taskID)
//   企微 template_card_event 的 button key 实际在子节点 <Button><Key>DONE|restock-xxx</Key></Button>
//   这里做个兼容:如果顶层 TaskID 含 "|" 就直接切
func parseButtonKey(evt CallbackEvent) (kind, taskID string) {
	// 简化:实际 XML 解析时, button key 在嵌套节点,这里用 TaskID 含 "|" 兜底
	if strings.Contains(evt.TaskID, "|") {
		parts := strings.SplitN(evt.TaskID, "|", 2)
		return parts[0], parts[1]
	}
	return "", evt.TaskID
}
