// Config 加载顺序测试
//
// 验证: OS env > .env > config.yaml > defaults
//
// 注意: godotenv 默认不覆盖已存在的 OS 环境变量,
//       viper.SetDefault 的优先级高于 AutomaticEnv,
//       所以必须用 BindEnv 显式绑定每个叶子 key
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/joho/godotenv"
)

// withEnv 在测试期间设置/还原环境变量
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	old := map[string]*string{}
	for k := range kv {
		if v, ok := os.LookupEnv(k); ok {
			old[k] = &v
		} else {
			old[k] = nil
		}
	}
	for k, v := range kv {
		_ = os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for k, v := range old {
			if v == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *v)
			}
		}
	})
}

// withCWD 切到临时目录, 写一个 .env, 测试完恢复
func withCWD(t *testing.T, envContent string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if envContent != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestLoad_DefaultsWhenNothing(t *testing.T) {
	withCWD(t, "") // 没 .env
	// 显式清空可能干扰测试的 env
	for _, k := range []string{"PORT", "PG_HOST", "PG_PORT", "MAX_CONCURRENT_PARSE", "USE_LLM", "BIGMODEL_API_KEY"} {
		_ = os.Unsetenv(k)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "8089" {
		t.Errorf("Port default: got %q, want 8089", cfg.Port)
	}
	if cfg.PGHost != "127.0.0.1" {
		t.Errorf("PGHost default: got %q, want 127.0.0.1", cfg.PGHost)
	}
	if cfg.PGPort != 5432 {
		t.Errorf("PGPort default: got %d, want 5432", cfg.PGPort)
	}
	if cfg.MaxConcurrentParse != 4 {
		t.Errorf("MaxConcurrentParse default: got %d, want 4", cfg.MaxConcurrentParse)
	}
	if !cfg.UseLlm {
		t.Error("UseLlm default: got false, want true")
	}
}

func TestLoad_DotEnvOverridesDefaults(t *testing.T) {
	withCWD(t, "PORT=9091\nPG_HOST=from-dotenv\nPG_PORT=6543\nBIGMODEL_API_KEY=from-dotenv\nMAX_CONCURRENT_PARSE=8\nUSE_LLM=false\n")
	// 清空可能干扰的 OS env
	for _, k := range []string{"PORT", "PG_HOST", "PG_PORT", "BIGMODEL_API_KEY", "MAX_CONCURRENT_PARSE", "USE_LLM"} {
		_ = os.Unsetenv(k)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "9091" {
		t.Errorf("Port from .env: got %q, want 9091", cfg.Port)
	}
	if cfg.PGHost != "from-dotenv" {
		t.Errorf("PGHost from .env: got %q", cfg.PGHost)
	}
	if cfg.PGPort != 6543 {
		t.Errorf("PGPort from .env: got %d", cfg.PGPort)
	}
	if cfg.BigModelAPIKey != "from-dotenv" {
		t.Errorf("BigModelAPIKey from .env: got %q", cfg.BigModelAPIKey)
	}
	if cfg.MaxConcurrentParse != 8 {
		t.Errorf("MaxConcurrentParse from .env: got %d", cfg.MaxConcurrentParse)
	}
	if cfg.UseLlm {
		t.Error("UseLlm from .env: got true, want false")
	}
}

func TestLoad_EnvOverridesDotEnv(t *testing.T) {
	withCWD(t, "PORT=9091\nPG_HOST=from-dotenv\nMAX_CONCURRENT_PARSE=8\n")
	withEnv(t, map[string]string{
		"PORT":               "7777",
		"PG_HOST":            "from-os-env",
		"MAX_CONCURRENT_PARSE": "32",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "7777" {
		t.Errorf("Port: OS env should win, got %q", cfg.Port)
	}
	if cfg.PGHost != "from-os-env" {
		t.Errorf("PGHost: OS env should win, got %q", cfg.PGHost)
	}
	if cfg.MaxConcurrentParse != 32 {
		t.Errorf("MaxConcurrentParse: OS env should win, got %d", cfg.MaxConcurrentParse)
	}
}

// 验证 godotenv 不会用 .env 覆盖已存在的 OS env
func TestGodotenv_DoesNotOverwriteExistingEnv(t *testing.T) {
	withCWD(t, "BIGMODEL_API_KEY=from-dotenv\n")
	_ = os.Setenv("BIGMODEL_API_KEY", "from-os-env")
	defer os.Unsetenv("BIGMODEL_API_KEY")

	if err := godotenv.Load(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("BIGMODEL_API_KEY"); got != "from-os-env" {
		t.Errorf("godotenv 应该不覆盖已存在的 env, got %q", got)
	}
}

// 验证: 不读取 $HOME/.collect-ai
// (历史 bug: 容器/服务用户 HOME 不可靠)
func TestLoad_NoUserHomeConfig(t *testing.T) {
	// 即便 HOME 指向一个含 .collect-ai/config.yaml 的目录, 也不应被读取
	home := t.TempDir()
	confDir := filepath.Join(home, ".collect-ai")
	_ = os.MkdirAll(confDir, 0o755)
	_ = os.WriteFile(filepath.Join(confDir, "config.yaml"), []byte("PORT: 12345\n"), 0o644)
	_ = os.Setenv("HOME", home)
	t.Cleanup(func() { _ = os.Unsetenv("HOME") })

	withCWD(t, "")
	// 清空可能干扰的 env
	_ = os.Unsetenv("PORT")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port == "12345" {
		t.Errorf("不应从 $HOME/.collect-ai 读 config, 但 Port=%q", cfg.Port)
	}
}
