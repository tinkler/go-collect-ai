package wecomchat

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// =====================================================================
// ChatRouter — 用途分发的核心
//
// 职责:
//   1) 维护内存 binding cache (chat_id -> purpose), 跟 PG wecom_chat_binding 同步
//   2) wcm.OnMessage 进来后, 查 purpose 决定走哪个 handler
//   3) 出站查询: 给 promo_alert / owner cron 一个"查目的 chat_id"接口
//   4) Reload() 热更新: 调一次即从 PG 重新加载
//
// 设计: cache 只读,所有写走 PG,Reload 重建
//  - 优点: 实现简单,无并发风险
//  - 缺点: 每次 reload 全量 load — 几百行数据可忽略
// =====================================================================

// ChatRouter 用途路由
type ChatRouter struct {
	store   *Store
	pool    *pgxpool.Pool
	mu      sync.RWMutex
	cache   map[string]*ChatBinding // chat_id -> binding (含 enabled=false)
	reload  time.Time               // 上次 reload 时间
	llmExec LLMExecutor             // 注入的 LLM 处理器 (agent.Bridge.Handle)
	// 出站多结果时优先取哪条 (按顺序) — 同一 purpose 可配多个群, 推时都推
	// 当前不实现去重, 配几条就推几条
}

// NewRouter 构造
func NewRouter(store *Store, pool *pgxpool.Pool) *ChatRouter {
	return &ChatRouter{
		store: store,
		pool:  pool,
		cache: make(map[string]*ChatBinding),
	}
}

// Reload 从 PG 重新加载 cache
func (r *ChatRouter) Reload(ctx context.Context) error {
	all, err := r.store.ListBindings(ctx, "")
	if err != nil {
		return fmt.Errorf("load bindings: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]*ChatBinding, len(all))
	for _, b := range all {
		// copy 一份避免外部修改影响 cache
		cp := *b
		r.cache[b.ChatID] = &cp
	}
	r.reload = time.Now()
	log.Printf("[wecomchat] reloaded %d bindings (purpose: %v)", len(all), summarizePurposes(all))
	return nil
}

// summarizePurposes 日志用汇总
func summarizePurposes(bs []*ChatBinding) map[string]int {
	out := map[string]int{}
	for _, b := range bs {
		if b.Enabled {
			out[b.Purpose]++
		}
	}
	return out
}

// ResolveChat 查 purpose
//   找不到时返回 ("log", nil) — 等同"未配置, 仅记录"
//   找到但 enabled=false 时返回 ("", binding) — 调用方应跳过
func (r *ChatRouter) ResolveChat(chatID string) (purpose string, b *ChatBinding) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if b, ok := r.cache[chatID]; ok {
		return b.Purpose, b
	}
	return "log", nil
}

// ListByPurpose 给 cron 用: 拿所有 enabled 且 purpose=X 的 chat_id
func (r *ChatRouter) ListByPurpose(purpose string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []string{}
	for _, b := range r.cache {
		if b.Purpose == purpose && b.Enabled {
			out = append(out, b.ChatID)
		}
	}
	return out
}

// FirstChatByPurpose 拿第一个 enabled 的 (向后兼容单 chat_id 的旧调用)
func (r *ChatRouter) FirstChatByPurpose(purpose string) string {
	list := r.ListByPurpose(purpose)
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

// Stats 用于 status 接口
func (r *ChatRouter) Stats() (loaded int, lastReload time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache), r.reload
}

// =====================================================================
// Dispatcher — 把 wecom 消息按 purpose 路由
//
// 设计:
//   - LLMExecutor 由 main.go 注入 (默认 nil = 不接 LLM,只记录)
//   - 注入后,purpose=agent|fee 的消息送 LLM
//   - 其它 purpose 仅记录 (出站单向的 promo_alert/owner 不应在群里收消息)
//
// 频控 / 串行 / 截断 / 重试: 都委托给 LLMExecutor (即 agent.Bridge)
// 这里只做"该不该送 LLM"的判定
// =====================================================================

// LLMExecutor 抽象 (由 main.go 注入 *agent.Bridge.Handle)
type LLMExecutor interface {
	Handle(chatID, userID, text string)
}

// SetLLMExecutor 注入 LLM 执行器
func (r *ChatRouter) SetLLMExecutor(exec LLMExecutor) {
	r.llmExec = exec
}

// ChatRouter 增量字段
//
//	llmExec LLMExecutor
//	（已在上面 struct 中定义,这里仅说明）
//
// Handle 主入口: wecom.Client.OnMessage 注册这个
//
//	逻辑:
//	  1) 查 purpose
//	  2) enabled=false → 静默
//	  3) purpose=agent|fee + llmExec != nil → 调 llmExec.Handle
//	  4) 其它 → 打 log, 不响应
func (r *ChatRouter) Handle(chatID, userID, text string) {
	purpose, b := r.ResolveChat(chatID)
	preview := truncate(text, 80)
	if b == nil {
		log.Printf("[wecomchat] msg chat=%s user=%s purpose=log (unbound) text=%q",
			chatID, userID, preview)
		// 自动建一个 unbound 行 (purpose=log, first_seen=now) 方便 UI 发现
		// 失败不阻断
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := r.store.TouchFirstSeen(ctx, chatID); err != nil {
				log.Printf("[wecomchat] TouchFirstSeen err: %v", err)
			}
		}()
		return
	}
	if !b.Enabled {
		log.Printf("[wecomchat] msg chat=%s user=%s purpose=%s DISABLED (drop) text=%q",
			chatID, userID, purpose, preview)
		return
	}
	switch purpose {
	case "agent", "fee":
		if r.llmExec == nil {
			log.Printf("[wecomchat] msg chat=%s user=%s purpose=%s LLM not ready (drop) text=%q",
				chatID, userID, purpose, preview)
			return
		}
		log.Printf("[wecomchat] msg chat=%s user=%s purpose=%s → LLM text=%q",
			chatID, userID, purpose, preview)
		r.llmExec.Handle(chatID, userID, text)
	default:
		// log / office / floor / promo_alert / owner / other: 出站为主,收消息仅记
		log.Printf("[wecomchat] msg chat=%s user=%s purpose=%s (log only) text=%q",
			chatID, userID, purpose, preview)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
