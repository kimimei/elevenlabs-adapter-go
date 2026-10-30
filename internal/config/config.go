// Package config 读取配置项。
//
// 铁律：环境变量名与默认值必须与 Python 版逐一对应（见 CONTRACT.md §7）。
// 改名字等于把存量部署的 .env 变成静默失效 —— 值读不到就用默认值，
// 程序不报错，行为却变了，这类问题在生产上极难发现。
package config

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// 计费口径的两种基数，各有其存在理由，不可混用。
const (
	BillingBaseInput = "input"
	BillingBaseSent  = "chars"
)

// Config 是全部运行期配置。字段顺序与 .env.example 的分组保持一致，便于对照。
type Config struct {
	APIBase        string
	DefaultModel   string
	Mock           bool
	TimeoutSeconds float64
	MaxRetries     int
	MaxChars       int
	MaxBodyBytes   int64
	MaxConcurrency int
	AllowIPs       []string
	AllowLocalKey  bool
	Host           string
	Port           int
	Presets        string

	JobsEnabled    bool
	JobsWorkers    int
	JobsMaxPending int
	JobsMaxChars   int
	JobsTTLSeconds int
	JobsDir        string

	DemoEnabled  bool
	DemoMaxChars int
	DemoPerHour  int
	DemoVoices   []string
	DemoTrustXFF bool

	BillingMode     string
	BillingLogDir   string
	BillingLogDays  int
	BillingLogOff   bool
	UpCreditsPerUSD float64
	CharsPerUSD     float64

	TextClean string
}

// Load 从环境变量构造配置。任何非法值都退回默认值，绝不因为一个可选变量把服务打死。
func Load() *Config {
	return &Config{
		APIBase:        strings.TrimRight(envStr("ELEVENLABS_API_BASE", "https://api.elevenlabs.io/v1"), "/"),
		DefaultModel:   envStr("ELEVENLABS_DEFAULT_MODEL", "eleven_multilingual_v2"),
		Mock:           envBool("ELEVENLABS_MOCK"),
		TimeoutSeconds: envFloat("ELEVENLABS_TIMEOUT", 300),
		MaxRetries:     envInt("ELEVENLABS_MAX_RETRIES", 0),
		MaxChars:       envInt("ADAPTER_MAX_CHARS", 0),
		MaxBodyBytes:   int64(envInt("ADAPTER_MAX_BODY_BYTES", 2*1024*1024)),
		MaxConcurrency: envInt("ADAPTER_MAX_CONCURRENCY", 0),
		AllowIPs:       envList("ADAPTER_ALLOW_IPS"),
		AllowLocalKey:  envBool("ADAPTER_ALLOW_LOCAL_KEY"),
		Host:           envStr("ADAPTER_HOST", "127.0.0.1"),
		Port:           envInt("ADAPTER_PORT", 8123),
		Presets:        strings.TrimSpace(envStr("ADAPTER_PRESETS", "none")),

		JobsEnabled:    envBool("ADAPTER_JOBS_ENABLED"),
		JobsWorkers:    maxInt(1, envInt("ADAPTER_JOBS_WORKERS", 2)),
		JobsMaxPending: envInt("ADAPTER_JOBS_MAX_PENDING", 50),
		JobsMaxChars:   envInt("ADAPTER_JOBS_MAX_CHARS", 8000),
		JobsTTLSeconds: envInt("ADAPTER_JOBS_TTL_SECONDS", 1800),
		JobsDir:        envStr("ADAPTER_JOBS_DIR", "logs/jobs"),

		DemoEnabled:  envBool("ADAPTER_DEMO_ENABLED"),
		DemoMaxChars: envInt("ADAPTER_DEMO_MAX_CHARS", 20),
		DemoPerHour:  envInt("ADAPTER_DEMO_PER_HOUR", 20),
		DemoVoices: envListDefault("ADAPTER_DEMO_VOICES",
			[]string{"zh-yingshi", "zh-boyin", "zh-pangbai"}),
		DemoTrustXFF: envBool("ADAPTER_DEMO_TRUST_XFF"),

		BillingMode:     strings.ToLower(strings.TrimSpace(envStr("ADAPTER_BILLING_MODE", "input"))),
		BillingLogDir:   envStr("ADAPTER_BILLING_LOG_DIR", "logs/billing"),
		BillingLogDays:  envInt("ADAPTER_BILLING_LOG_DAYS", 30),
		BillingLogOff:   envBool("ADAPTER_BILLING_LOG_OFF"),
		UpCreditsPerUSD: envFloat("ADAPTER_UP_CREDITS_PER_USD", 0),
		CharsPerUSD:     envFloat("ADAPTER_CHARS_PER_USD", 0),

		TextClean: strings.ToLower(strings.TrimSpace(envStr("ADAPTER_TEXT_CLEAN", "safe"))),
	}
}

func envStr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(name string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// envListDefault 解析逗号分隔列表；解析为空（只有分隔符/空格）时回落到默认值。
//
// 直接返回空列表会让上层取 DEMO_VOICES[0] 时索引越界 ——
// 一个拼错的逗号就能把试听接口打成 500，而这在启动日志里必须看得见。
func envListDefault(name string, def []string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		log.Printf("[WARN] %s 解析为空（检查是否只有分隔符/空格），已回落到默认值：%s",
			name, strings.Join(def, ","))
		return def
	}
	return out
}

func envList(name string) []string {
	raw := os.Getenv(name)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
