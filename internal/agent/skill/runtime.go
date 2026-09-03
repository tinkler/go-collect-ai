package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// InvokeSkillReq invoke_skill tool 入参
type InvokeSkillReq struct {
	// SkillName 必填,严格匹配 frontmatter.name
	SkillName string `json:"skill_name" jsonschema:"description=要激活的 skill 名(必填,严格匹配),required"`

	// Input 用户/上游对 skill 的输入(L2 上下文的一部分)
	Input string `json:"input,omitempty" jsonschema:"description=本次任务的输入文本,会被拼到 SKILL.md 正文后"`

	// Action 必填: "load" / "run_script" / "read_file"
	//   load: 读取 SKILL.md 全文(默认 L2)
	//   run_script: 跑 scripts/ 下某个脚本
	//   read_file: 读 references/ 或 assets/ 下某个文件(L3)
	Action string `json:"action" jsonschema:"description=动作(load|run_script|read_file),required,enum=load,enum=run_script,enum=read_file"`

	// Path run_script 时:scripts/foo.py(相对 skill root)
	//      read_file 时:references/bar.md 或 assets/x.json
	Path string `json:"path,omitempty" jsonschema:"description=资源相对路径(相对 skill root)"`

	// Args run_script 时:stdin JSON 字符串(可选)
	Args json.RawMessage `json:"args,omitempty" jsonschema:"description=run_script 时的 stdin JSON"`

	// ScriptTimeoutSec run_script 超时秒数(默认 30,最大 120)
	ScriptTimeoutSec int `json:"script_timeout_sec,omitempty" jsonschema:"description=脚本超时秒数(默认 30,最大 120)"`
}

// InvokeSkillResp invoke_skill tool 出参(JSON 字符串,直接给 LLM 看)
type InvokeSkillResp struct {
	// Skill 触发的 skill 名
	Skill string `json:"skill"`

	// Action 实际执行的动作
	Action string `json:"action"`

	// Body SKILL.md 正文(load 时返回)
	Body string `json:"body,omitempty"`

	// Output 脚本 stdout(run_script 时)
	Output string `json:"output,omitempty"`

	// Content 文件内容(read_file 时)
	Content string `json:"content,omitempty"`

	// Scripts 可用脚本清单(load 时附带)
	Scripts []string `json:"scripts,omitempty"`

	// References 可用参考清单(load 时附带)
	References []string `json:"references,omitempty"`

	// Assets 可用资源清单(load 时附带)
	Assets []string `json:"assets,omitempty"`

	// Truncated 是否被截断
	Truncated bool `json:"truncated,omitempty"`
}

// maxBodyChars 单次返回 body 上限(避免 LLM context 爆炸)
const maxBodyChars = 50_000

// maxScriptOutput run_script 单次 stdout 上限
const maxScriptOutput = 20_000

// maxReadChars read_file 单次上限
const maxReadChars = 50_000

// NewInvokeSkillTool 构造 invoke_skill tool 注入 trpc-agent-go
//   - 工具本质 = 把 store 里的 skill 暴露给 LLM 调
//   - store 为 nil 时,工具调用会返 error(不 panic)
func NewInvokeSkillTool(store *Store) tool.Tool {
	type req = InvokeSkillReq
	type resp = InvokeSkillResp
	fn := func(ctx context.Context, r req) (resp, error) {
		if store == nil {
			return resp{}, fmt.Errorf("invoke_skill: skill store 未初始化")
		}
		return runInvoke(ctx, store, r)
	}
	return function.NewFunctionTool(
		fn,
		function.WithName("invoke_skill"),
		function.WithDescription("按需加载一个 skill 的 SKILL.md 全文,或运行它的 scripts/ 脚本,或读 references/ 文件。Skill 通过 Agent Skills spec (Anthropic) 暴露。Use this tool when a task matches one of the skill descriptions listed in the system prompt."),
	)
}

// RunInvokeForTest 导出 runInvoke(供 e2e / 集成测试直接调,不走 tool wrapper)
var RunInvokeForTest = runInvoke

func runInvoke(ctx context.Context, store *Store, r InvokeSkillReq) (InvokeSkillResp, error) {
	r.SkillName = strings.TrimSpace(r.SkillName)
	if r.SkillName == "" {
		return InvokeSkillResp{}, fmt.Errorf("skill_name 必填")
	}
	sk, ok := store.Get(r.SkillName)
	if !ok {
		// 列出可用 skill 帮助 LLM 自纠
		known := store.Names()
		return InvokeSkillResp{}, fmt.Errorf("skill %q 不存在;已知: %v", r.SkillName, known)
	}

	action := strings.TrimSpace(r.Action)
	if action == "" {
		action = "load"
	}

	switch action {
	case "load":
		return doLoad(sk, r), nil
	case "run_script":
		return doRunScript(ctx, sk, r)
	case "read_file":
		return doReadFile(sk, r)
	default:
		return InvokeSkillResp{}, fmt.Errorf("action %q 非法(仅 load/run_script/read_file)", action)
	}
}

func doLoad(sk *Skill, r InvokeSkillReq) InvokeSkillResp {
	body := sk.Body
	truncated := false
	if len(body) > maxBodyChars {
		body = body[:maxBodyChars]
		truncated = true
	}
	// 在末尾追加 input,给 LLM 一个完整的"指令 + 上下文"
	note := ""
	if strings.TrimSpace(r.Input) != "" {
		note = "\n\n---\n# 当前任务输入\n" + r.Input + "\n"
	}
	return InvokeSkillResp{
		Skill:     sk.Manifest.Name,
		Action:    "load",
		Body:      body + note,
		Scripts:   sk.Scripts,
		References: sk.References,
		Assets:    sk.Assets,
		Truncated: truncated,
	}
}

func doRunScript(ctx context.Context, sk *Skill, r InvokeSkillReq) (InvokeSkillResp, error) {
	if r.Path == "" {
		return InvokeSkillResp{}, fmt.Errorf("path 必填(scripts/ 下的脚本名,如 foo.py)")
	}
	// 安全:不允许 ../ 跳出 skill root
	rel, err := filepath.Rel(sk.Root, filepath.Join(sk.Root, r.Path))
	if err != nil || strings.HasPrefix(rel, "..") || strings.Contains(rel, ".."+string(filepath.Separator)) {
		return InvokeSkillResp{}, fmt.Errorf("path %q 非法,必须位于 %s 内", r.Path, sk.Root)
	}
	full := filepath.Join(sk.Root, r.Path)
	info, err := os.Stat(full)
	if err != nil {
		return InvokeSkillResp{}, fmt.Errorf("script %s 不存在: %w", full, err)
	}
	if info.IsDir() {
		return InvokeSkillResp{}, fmt.Errorf("%s 是目录,不是脚本", r.Path)
	}

	timeout := time.Duration(r.ScriptTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}

	// 派生子进程跑脚本:python foo.py / node foo.js / 直接执行
	cmd, err := buildScriptCmd(ctx, full, r.Args)
	if err != nil {
		return InvokeSkillResp{}, err
	}
	cmd.Dir = sk.Root
	cmd.Env = append(os.Environ(), "SKILL_NAME="+sk.Manifest.Name, "SKILL_ROOT="+sk.Root)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return InvokeSkillResp{}, fmt.Errorf("启动脚本失败: %w", err)
	}

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return InvokeSkillResp{}, fmt.Errorf("脚本被 ctx 取消: %w", ctx.Err())
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return InvokeSkillResp{
			Skill:  sk.Manifest.Name,
			Action: "run_script",
			Output: truncateStr(fmt.Sprintf("[timeout %s]\nstdout:\n%s\nstderr:\n%s", timeout, stdout.String(), stderr.String()), maxScriptOutput),
		}, fmt.Errorf("脚本超时(%s)", timeout)
	case err := <-doneCh:
		out := stdout.String()
		if err != nil {
			return InvokeSkillResp{
				Skill:  sk.Manifest.Name,
				Action: "run_script",
				Output: truncateStr(fmt.Sprintf("exit error: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr.String()), maxScriptOutput),
			}, err
		}
		return InvokeSkillResp{
			Skill:  sk.Manifest.Name,
			Action: "run_script",
			Output: truncateStr(out, maxScriptOutput),
		}, nil
	}
}

func doReadFile(sk *Skill, r InvokeSkillReq) (InvokeSkillResp, error) {
	if r.Path == "" {
		return InvokeSkillResp{}, fmt.Errorf("path 必填(references/ 或 assets/ 下的文件名)")
	}
	rel, err := filepath.Rel(sk.Root, filepath.Join(sk.Root, r.Path))
	if err != nil || strings.HasPrefix(rel, "..") || strings.Contains(rel, ".."+string(filepath.Separator)) {
		return InvokeSkillResp{}, fmt.Errorf("path %q 非法,必须位于 %s 内", r.Path, sk.Root)
	}
	// read_file 只允许读 references/ 和 assets/ 下的文件
	// scripts/ 下的脚本必须用 run_script 跑(防止 LLM 误读 .py 源码)
	relSlash := filepath.ToSlash(rel)
	if !strings.HasPrefix(relSlash, "references/") && !strings.HasPrefix(relSlash, "assets/") {
		return InvokeSkillResp{}, fmt.Errorf("path %q 不允许 read_file:只支持 references/ 和 assets/ 下的文件;scripts/ 下的脚本请用 action=run_script", r.Path)
	}
	full := filepath.Join(sk.Root, r.Path)
	data, err := os.ReadFile(full)
	if err != nil {
		return InvokeSkillResp{}, fmt.Errorf("读文件失败: %w", err)
	}
	content := string(data)
	truncated := false
	if len(content) > maxReadChars {
		content = content[:maxReadChars]
		truncated = true
	}
	return InvokeSkillResp{
		Skill:     sk.Manifest.Name,
		Action:    "read_file",
		Content:   content,
		Truncated: truncated,
	}, nil
}

// buildScriptCmd 根据扩展名挑解释器
//   .py → python(未找到则 python3)
//   .js → node
//   .sh → bash(Windows 上 Git Bash)
//   其它(可执行)→ 直接跑
func buildScriptCmd(ctx context.Context, full string, stdinPayload json.RawMessage) (*exec.Cmd, error) {
	ext := strings.ToLower(filepath.Ext(full))

	var cmd *exec.Cmd
	switch ext {
	case ".py":
		if _, err := exec.LookPath("python"); err == nil {
			cmd = exec.CommandContext(ctx, "python", full)
		} else if _, err := exec.LookPath("python3"); err == nil {
			cmd = exec.CommandContext(ctx, "python3", full)
		} else {
			return nil, fmt.Errorf("找不到 python 解释器")
		}
	case ".js", ".mjs":
		if _, err := exec.LookPath("node"); err != nil {
			return nil, fmt.Errorf("找不到 node")
		}
		cmd = exec.CommandContext(ctx, "node", full)
	case ".sh":
		if _, err := exec.LookPath("bash"); err == nil {
			cmd = exec.CommandContext(ctx, "bash", full)
		} else if _, err := exec.LookPath("sh"); err == nil {
			cmd = exec.CommandContext(ctx, "sh", full)
		} else {
			return nil, fmt.Errorf("找不到 bash/sh")
		}
	default:
		cmd = exec.CommandContext(ctx, full)
	}

	if len(stdinPayload) > 0 && string(stdinPayload) != "null" {
		cmd.Stdin = strings.NewReader(string(stdinPayload))
	}
	return cmd, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... [truncated]"
}
