// Package config 集中管理 collect-ai 后端的所有配置
//
// 加载顺序(优先级从高到低):
//  1. 操作系统环境变量(必须以变量名直接匹配,如 PORT / BIGMODEL_API_KEY)
//  2. 项目根目录的 .env 文件(godotenv 加载,默认不覆盖已有 env)
//  3. config/config.yaml 文件(可选,支持 ${VAR} 占位符)
//  4. 代码内默认值(via viper.SetDefault)
//
// 关键点:
//   - 不读取 $HOME/.collect-ai 等用户目录,保证部署位置无关(server / docker / systemd 一致)
//   - 用 viper.BindEnv 显式绑定每个叶子 key,保证 env 一定覆盖默认值
//     (因为 viper.SetDefault 的优先级高于 AutomaticEnv,不能只靠 AutomaticEnv)
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"
)

// Config 全部配置 (env / yaml / 默认值)
type Config struct {
	// 服务
	Port          string `mapstructure:"PORT"`
	UploadDir     string `mapstructure:"UPLOAD_DIR"`
	MaxUploadMB   int    `mapstructure:"MAX_UPLOAD_MB"`
	PublicBaseURL string `mapstructure:"PUBLIC_BASE_URL"` // 用于生成图片 URL, 飞书 H5 用

	// PostgreSQL
	PGHost     string `mapstructure:"PG_HOST"`
	PGPort     int    `mapstructure:"PG_PORT"`
	PGUser     string `mapstructure:"PG_USER"`
	PGPassword string `mapstructure:"PG_PASSWORD"`
	PGDatabase string `mapstructure:"PG_DATABASE"`

	// BigModel
	BigModelAPIKey string `mapstructure:"BIGMODEL_API_KEY"`
	BigModelBase   string `mapstructure:"BIGMODEL_BASE"`
	OCRModel       string `mapstructure:"OCR_MODEL"` // hand_write / layout_parsing
	LLMModel       string `mapstructure:"LLM_MODEL"` // glm-4-flash

	// DeepSeek (2026-09-04 双引擎: 引擎2 视觉模型解析供货单)
	DeepseekAPIKey      string `mapstructure:"DEEPSEEK_API_KEY"`
	DeepseekBase        string `mapstructure:"DEEPSEEK_BASE"`
	DeepseekVisionModel string `mapstructure:"DEEPSEEK_VISION_MODEL"`

	// cube-agent-server
	AgentURL   string `mapstructure:"AGENT_URL"`
	AgentToken string `mapstructure:"AGENT_TOKEN"` // 可选
	// 统一数据源(默认 erp,可选 hbpos)
	//   透传到 cube-agent-server 的 ?datasource= 参数
	//   运行时可通过 /api/v1/datasource 切换
	DataSource string `mapstructure:"DATA_SOURCE"`

	// 解析行为
	OcrTimeoutSec int  `mapstructure:"OCR_TIMEOUT_SEC"`
	LlmTimeoutSec int  `mapstructure:"LLM_TIMEOUT_SEC"`
	UseLlm        bool `mapstructure:"USE_LLM"`
	FuzzyDistance int  `mapstructure:"FUZZY_DISTANCE"`

	// 并发限流
	MaxConcurrentParse int `mapstructure:"MAX_CONCURRENT_PARSE"` // 解析并发上限 (0=不限流, 默认 4)
	RateLimitWaitSec   int `mapstructure:"RATE_LIMIT_WAIT_SEC"`  // 客户端等待 semaphore 超时 (默认 30)

	// ============== 补货模块 (restock, 2026-09-02 精简) ==============
	RestockBranchNo string `mapstructure:"RESTOCK_BRANCH_NO"`

	// 3 次 cron 表达式 (HH:MM)
	DisplayRestockCronEve  string `mapstructure:"DISPLAY_RESTOCK_CRON_EVE"`
	DisplayRestockCronMorn string `mapstructure:"DISPLAY_RESTOCK_CRON_MORN"`
	DisplayRestockCronAft  string `mapstructure:"DISPLAY_RESTOCK_CRON_AFT"`
	DisplayRestockCubeName string `mapstructure:"DISPLAY_RESTOCK_CUBE_NAME"`
	DisplayRestockRetryMax int    `mapstructure:"DISPLAY_RESTOCK_RETRY_MAX"`
	DisplayRestockMaxPush  int    `mapstructure:"DISPLAY_RESTOCK_MAX_PUSH"`

	// 企微智能机器人 (长连接模式)
	WeComBotID     string `mapstructure:"WECOM_BOT_ID"`
	WeComBotSecret string `mapstructure:"WECOM_BOT_SECRET"`
	WeComWSURL     string `mapstructure:"WECOM_WS_URL"`
	WeComBindFile  string `mapstructure:"WECOM_BIND_FILE"`

	// ============== 鉴权 (2026-08-29) ==============
	// JWT
	JWTSecret          string `mapstructure:"JWT_SECRET"`
	AccessTokenTTLSec  int    `mapstructure:"ACCESS_TOKEN_TTL_SEC"`
	RefreshTokenTTLSec int    `mapstructure:"REFRESH_TOKEN_TTL_SEC"`

	// Dev 模式开关 (启用 /auth/dev-login)
	DevMode bool `mapstructure:"DEV_MODE"`

	// 企微 OAuth (H5 免登) — corpSecret 严禁进前端
	WeComCorpID     string `mapstructure:"WECOM_CORP_ID"`
	WeComAgentID    string `mapstructure:"WECOM_AGENT_ID"`
	WeComCorpSecret string `mapstructure:"WECOM_CORP_SECRET"`

	// Refresh cookie
	CookieDomain string `mapstructure:"COOKIE_DOMAIN"`
	CookieSecure bool   `mapstructure:"COOKIE_SECURE"`

	// ============== 业务字段映射 (2026-09-02 配置化) ==============
	//   YAML 路径,空 = 用 NewDefaultRegistry() 硬编码 fallback
	//   推荐: configs/mappings.yaml (项目根目录相对路径)
	MappingFile string `mapstructure:"MAPPING_FILE"`
}

// leaves 列出所有需要 BindEnv 的叶子 key
// 配合 BindEnv 强制让 env 覆盖 SetDefault
// (按字段名直接 match env var, 不加前缀以保持向后兼容)
var leaves = []string{
	"PORT", "UPLOAD_DIR", "MAX_UPLOAD_MB", "PUBLIC_BASE_URL",
	"PG_HOST", "PG_PORT", "PG_USER", "PG_PASSWORD", "PG_DATABASE",
	"BIGMODEL_API_KEY", "BIGMODEL_BASE", "OCR_MODEL", "LLM_MODEL",
	// 双引擎引擎2 (2026-09-04): DeepSeek 视觉模型, 必须 BindEnv 否则 SetDefault("") 会压住 OS env
	"DEEPSEEK_API_KEY", "DEEPSEEK_BASE", "DEEPSEEK_VISION_MODEL",
	"AGENT_URL", "AGENT_TOKEN", "DATA_SOURCE",
	"OCR_TIMEOUT_SEC", "LLM_TIMEOUT_SEC", "USE_LLM", "FUZZY_DISTANCE",
	"MAX_CONCURRENT_PARSE", "RATE_LIMIT_WAIT_SEC",

	// restock (2026-09-02 精简后)
	"RESTOCK_BRANCH_NO",
	"DISPLAY_RESTOCK_CRON_EVE", "DISPLAY_RESTOCK_CRON_MORN", "DISPLAY_RESTOCK_CRON_AFT",
	"DISPLAY_RESTOCK_CUBE_NAME", "DISPLAY_RESTOCK_RETRY_MAX", "DISPLAY_RESTOCK_MAX_PUSH",
	"WECOM_BOT_ID", "WECOM_BOT_SECRET", "WECOM_WS_URL", "WECOM_BIND_FILE",

	// auth
	"JWT_SECRET", "ACCESS_TOKEN_TTL_SEC", "REFRESH_TOKEN_TTL_SEC",
	"DEV_MODE",
	"WECOM_CORP_ID", "WECOM_AGENT_ID", "WECOM_CORP_SECRET",
	"COOKIE_DOMAIN", "COOKIE_SECURE",

	// 业务字段映射配置 (2026-09-02)
	"MAPPING_FILE",
}

// Load 加载配置
// 流程:
//  1. 加载 .env(godotenv 默认不覆盖已存在的 OS 环境变量)
//  2. 加载 config.yaml(可选)
//  3. BindEnv 所有叶子 key(env 覆盖任何其他来源)
//  4. 用 SetDefault 提供兜底值
func Load() (*Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working dir: %w", err)
	}

	// 1. 加载 .env (项目根目录)
	//    godotenv.Load 默认不覆盖已存在的 OS 环境变量
	//    → 语义: OS env > .env
	envPath := filepath.Join(cwd, ".env")
	if _, statErr := os.Stat(envPath); statErr == nil {
		if err := godotenv.Load(envPath); err != nil {
			return nil, fmt.Errorf("load .env: %w", err)
		}
	}

	// 2. viper 初始化 + 加载 config.yaml (可选)
	v := viper.New()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(filepath.Join(cwd, "config"))
	v.AddConfigPath(cwd)

	// 不读用户目录: 部署位置无关
	// (历史版本曾读 $HOME/.collect-ai, 移除以保证 docker / systemd 行为一致)

	if err := v.ReadInConfig(); err != nil {
		// 配置文件可选: 找不到不报错
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config.yaml: %w", err)
		}
	}

	// 3. BindEnv 每个叶子 key,让 OS env 显式覆盖默认
	//    (SetDefault 优先级高于 AutomaticEnv, 必须显式 BindEnv 才能让 env 生效)
	for _, k := range leaves {
		_ = v.BindEnv(k)
	}

	// 4. 默认值 (env / yaml 都没设时生效)
	v.SetDefault("PORT", "8089")
	v.SetDefault("UPLOAD_DIR", "./uploads")
	v.SetDefault("MAX_UPLOAD_MB", 16)
	v.SetDefault("PUBLIC_BASE_URL", "")

	v.SetDefault("PG_HOST", "127.0.0.1")
	v.SetDefault("PG_PORT", 5432)
	v.SetDefault("PG_USER", "postgres")
	v.SetDefault("PG_PASSWORD", "postgres")
	v.SetDefault("PG_DATABASE", "collectai")

	v.SetDefault("BIGMODEL_API_KEY", "")
	v.SetDefault("BIGMODEL_BASE", "https://open.bigmodel.cn/api/paas/v4")
	v.SetDefault("OCR_MODEL", "hand_write")
	v.SetDefault("LLM_MODEL", "glm-4-flash")

	// DeepSeek (2026-09-04 双引擎)
	v.SetDefault("DEEPSEEK_API_KEY", "")
	v.SetDefault("DEEPSEEK_BASE", "https://api.deepseek.com")
	v.SetDefault("DEEPSEEK_VISION_MODEL", "deepseek-v4-flash-vision-exp")

	v.SetDefault("AGENT_URL", "http://127.0.0.1:8088")
	v.SetDefault("AGENT_TOKEN", "")
	// 数据源(2026-08-31 简化):启动后即固定,不可运行时切换,默认 hbpos(用户当前部署)
	v.SetDefault("DATA_SOURCE", "hbpos")

	v.SetDefault("OCR_TIMEOUT_SEC", 60)
	v.SetDefault("LLM_TIMEOUT_SEC", 60)
	v.SetDefault("USE_LLM", true)
	v.SetDefault("FUZZY_DISTANCE", 2)

	v.SetDefault("MAX_CONCURRENT_PARSE", 4)
	v.SetDefault("RATE_LIMIT_WAIT_SEC", 30)

	// restock 默认值
	v.SetDefault("RESTOCK_BRANCH_NO", "")
	v.SetDefault("RESTOCK_ROP_FACTOR", 1.5)
	v.SetDefault("RESTOCK_OUT_DAYS", 7)
	v.SetDefault("RESTOCK_OUT_PROMO_BOOST", 1.3)
	v.SetDefault("RESTOCK_SAFETY_MIN", 5)
	v.SetDefault("RESTOCK_DAILY_AVG_W_OLD", 0.4)
	v.SetDefault("RESTOCK_DAILY_AVG_W_7D", 0.4)
	v.SetDefault("RESTOCK_DAILY_AVG_W_30D", 0.2)
	v.SetDefault("RESTOCK_PUSH_FLOOR_MIN_INTERVAL_MIN", 30)
	v.SetDefault("RESTOCK_PUSH_OFFICE_P0_MIN_MIN", 15)
	v.SetDefault("RESTOCK_PUSH_OFFICE_P1_MIN_MIN", 60)
	v.SetDefault("RESTOCK_PUSH_OFFICE_P2_MIN_MIN", 360)
	v.SetDefault("RESTOCK_MAX_PUSH_PER_TICK", 20)
	v.SetDefault("RESTOCK_ESCALATE_P2_TO_P1_HOURS", 24)
	v.SetDefault("RESTOCK_ESCALATE_P1_TO_P0_HOURS", 12)
	v.SetDefault("RESTOCK_CRON_HOURLY", "0 7-21 * * *")
	v.SetDefault("RESTOCK_CRON_AGGREGATE", "0 30 21 * * *")
	v.SetDefault("RESTOCK_CRON_LLM_PLAN", "0 0 1,7,13,19 * * *")

	// 陈列补货新版 (2026-08-30 起)
	//   cron 是 5 字段 "分 时 日 月 周",parseHHMM 拿 parts[0]=mm, parts[1]=hh
	v.SetDefault("DISPLAY_RESTOCK_CRON_EVE", "07:00")  // 07:00 tick
	v.SetDefault("DISPLAY_RESTOCK_CRON_MORN", "12:00") // 12:00 tick
	v.SetDefault("DISPLAY_RESTOCK_CRON_AFT", "20:30")  // 20:30 tick
	v.SetDefault("DISPLAY_RESTOCK_CUBE_NAME", "display_restock_window")
	v.SetDefault("DISPLAY_RESTOCK_RETRY_MAX", 3)
	v.SetDefault("DISPLAY_RESTOCK_MAX_PUSH", 30)
	v.SetDefault("WECOM_BOT_ID", "")
	v.SetDefault("WECOM_BOT_SECRET", "")
	v.SetDefault("WECOM_WS_URL", "wss://openws.work.weixin.qq.com")
	v.SetDefault("WECOM_BIND_FILE", "./wecom_bindings.yaml")

	// auth 默认
	//   JWT_SECRET 故意给一个明显 dev 值, 启动时 main 会校验
	//   强制 ≥32 字符, 满足 HS256 最低要求
	v.SetDefault("JWT_SECRET", "dev-secret-change-me-in-prod-32chars")
	v.SetDefault("ACCESS_TOKEN_TTL_SEC", 900)     // 15 min
	v.SetDefault("REFRESH_TOKEN_TTL_SEC", 604800) // 7 days
	v.SetDefault("DEV_MODE", true)                // 默认开 dev-login, 生产手动关
	v.SetDefault("WECOM_CORP_ID", "")
	v.SetDefault("WECOM_AGENT_ID", "")
	v.SetDefault("WECOM_CORP_SECRET", "")
	v.SetDefault("COOKIE_DOMAIN", "127.0.0.1")
	v.SetDefault("COOKIE_SECURE", false)

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// 兜底: bool 字段如果 yaml / .env 写了 "true"/"false" 字符串,
	// viper 应该自动转,但保险起见手动处理
	cfg.UseLlm = parseBoolEnv("USE_LLM", cfg.UseLlm)
	cfg.DevMode = parseBoolEnv("DEV_MODE", cfg.DevMode)
	cfg.CookieSecure = parseBoolEnv("COOKIE_SECURE", cfg.CookieSecure)

	return cfg, nil
}

// parseBoolEnv 在 os.Getenv 命中时强制解析 bool
// (防止 .env 里 USE_LLM=true 被反序列化成空结构体)
func parseBoolEnv(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off", "":
			return false
		}
	}
	return fallback
}

func (c *Config) PGDSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		c.PGUser, c.PGPassword, c.PGHost, c.PGPort, c.PGDatabase)
}
