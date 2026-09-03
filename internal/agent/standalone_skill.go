// Package agent - standalone skill store helper (Phase A, 2026-09-02)
//
// 场景: OCR 解析需要 skills/ocr-purchase/SKILL.md 的 prompt 模板,但 LLM Agent 可能未启用
//   (用户可能只想用 OCR 解析,不要 Agent 聊天).
//
//   - agent.NewRunner 必须 cfg.Enabled=true 且 APIKey 非空才创建 LLM Agent
//   - 但 skill store 加载是 NewRunner 里的事,所以 agentRunner=nil 时就没有 skill store
//   - 这里提供 NewStandaloneSkillStore: 不创建 LLM Agent, 只 Load skills 一次
package agent

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/tinkler/collect-ai/internal/agent/skill"
)

// NewStandaloneSkillStore 不创建 Runner/LLM,只 Load skill 一次
//   extraRoots: 逗号分隔的额外根 (env COLLECTAI_SKILL_ROOTS)
//   不启动 watcher (无需热更新,启动一次够用; hot reload 是给 agent 聊天的)
//
// 失败: 返回 error, 不 panic; 调用方决定是否降级
func NewStandaloneSkillStore(extraRoots string) (*skill.Store, error) {
	wd, _ := os.Getwd()
	roots := skill.RootsFromEnvOrDefault(wd, extraRoots)

	store := skill.NewStore()
	store.SetRoots(roots)

	result, err := skill.Load(roots)
	if err != nil {
		return nil, fmt.Errorf("skill 加载失败: %w", err)
	}
	store.Replace(result.Skills)
	if msg := result.FormatErrors(); msg != "" {
		log.Printf("[standalone-skill] %s", strings.TrimSpace(msg))
	}
	log.Printf("[standalone-skill] 加载 %d 个 skill from %d 个 root (no watcher)", len(result.Skills), len(result.Roots))
	for _, sk := range result.Skills {
		log.Printf("[standalone-skill]   - %s [%s]", sk.Manifest.Name, sk.Source)
	}
	return store, nil
}
