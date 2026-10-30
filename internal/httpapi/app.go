package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
	"github.com/kimimei/elevenlabs-adapter-go/internal/config"
	"github.com/kimimei/elevenlabs-adapter-go/internal/jobs"
	"github.com/kimimei/elevenlabs-adapter-go/internal/upstream"
	"github.com/kimimei/elevenlabs-adapter-go/internal/voices"
)

// App 是全部依赖的组装点。显式注入，不用包级全局变量 ——
// 包级状态在测试里会互相污染，Python 版为此写了 conftest 的 monkeypatch 补丁。
type App struct {
	Cfg      *config.Config
	Catalog  *voices.Catalog
	Ledger   *billing.Ledger
	Upstream *upstream.Client
	Previews *upstream.PreviewCache
	Jobs     *jobs.Manager

	DemoLimiter *demoLimiter

	inflight atomic.Int64
}

// New 组装应用。
//
// 返回 error 的唯一来源是任务表：开启了异步任务却建不出可写目录时，
// 必须在启动就失败 —— 否则「重启不丢队列」只是一句口号。
func New(cfg *config.Config, cat *voices.Catalog, ledger *billing.Ledger, uc *upstream.Client) (*App, error) {
	// 音色列表为空会让试听 handler 索引越界（500）。配置解析已经兜了一层，
	// 这里再兜一层是因为 App 也可能被测试/嵌入方直接构造。
	if len(cfg.DemoVoices) == 0 {
		cfg.DemoVoices = []string{"zh-yingshi", "zh-boyin", "zh-pangbai"}
	}

	mgr, err := jobs.New(jobs.Settings{
		Enabled:      cfg.JobsEnabled,
		Workers:      cfg.JobsWorkers,
		MaxPending:   cfg.JobsMaxPending,
		MaxChars:     cfg.JobsMaxChars,
		TTLSeconds:   cfg.JobsTTLSeconds,
		DefaultModel: cfg.DefaultModel,
		TextClean:    cfg.TextClean,
		Mock:         cfg.Mock,
		Dir:          cfg.JobsDir,
	}, cat, ledger, uc)
	if err != nil {
		return nil, err
	}
	return &App{
		Cfg:         cfg,
		Catalog:     cat,
		Ledger:      ledger,
		Upstream:    uc,
		Previews:    upstream.NewPreviewCache(0),
		Jobs:        mgr,
		DemoLimiter: newDemoLimiter(cfg.DemoPerHour),
	}, nil
}

// randomID 生成 16 位十六进制请求 id。
func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// 关于来源 IP：ADAPTER_ALLOW_IPS 校验的是 TCP 对端地址（见 localAddr），
// **不读 X-Forwarded-For**。反代转发时这里看到的是反代自己的地址
// （同机 127.0.0.1、容器网关 172.17.0.1），而不是真实客户端 IP ——
// 这正是白名单该做的事（只允许自己的反代来找适配器）。
// 用 gin 的 c.ClientIP() 会信任 XFF，等于把白名单交给客户端自己填。

// resolveAPIKey 解析渠道密钥。
//
// 优先级：
//  1. Authorization: Bearer <key>（new-api / one-api 主路径）
//  2. x-api-key / api-key 头
//  3. 环境变量 ELEVENLABS_API_KEY
//  4. 本地 ~/.elevenlabs/config.json 明文 api_key（默认关闭）
func (a *App) resolveAPIKey(authorization, xAPIKey, apiKeyHeader string) string {
	for _, raw := range []string{authorization, xAPIKey, apiKeyHeader} {
		if k := normalizeKey(raw); k != "" {
			return k
		}
	}
	if k := os.Getenv("ELEVENLABS_API_KEY"); k != "" {
		return k
	}
	if !a.Cfg.AllowLocalKey {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".elevenlabs", "config.json"))
	if err != nil {
		return ""
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	key, _ := cfg["api_key"].(string)
	enc, _ := cfg["enc"].(string)
	if enc == "" {
		enc, _ = cfg["encrypted"].(string)
	}
	// key_manager v2 用 "enc": "dpapi" 标记加密；旧版明文无此字段
	if key != "" && enc == "" {
		return key
	}
	return ""
}

// normalizeKey 清洗渠道密钥：去空白、剥 Bearer、剥 sk- 外壳、多 key 取第一个。
//
// 中转站透传密钥的形态并不统一：
//   - new-api / one-api：Authorization: Bearer sk_xxx（ElevenLabs 原生 sk_ 下划线）
//   - 部分面板：多 key 用逗号分隔，或额外套一层 sk- 短横线前缀
//
// 只剥短横线 sk-（OpenAI 风格），保留下划线 sk_（ElevenLabs 原生）。
func normalizeKey(raw string) string {
	k := strings.TrimSpace(raw)
	if k == "" {
		return ""
	}
	if len(k) >= 7 && strings.EqualFold(k[:7], "bearer ") {
		k = strings.TrimSpace(k[7:])
	}
	if i := strings.Index(k, ","); i >= 0 {
		k = strings.TrimSpace(k[:i])
	}
	if strings.HasPrefix(k, "sk-") {
		k = k[3:]
	}
	return k
}

// tokenLabel 从中转站透传的 Key 里取个可读标签，只留前 6 位，不落完整密钥。
// 账本要能按客户/渠道聚合，但又不能变成密钥泄露的二次来源。
func tokenLabel(raw string) string {
	k := strings.TrimSpace(raw)
	if k == "" {
		return ""
	}
	if len(k) >= 7 && strings.EqualFold(k[:7], "bearer ") {
		k = strings.TrimSpace(k[7:])
	}
	if strings.HasPrefix(k, "sk-") {
		k = k[3:]
	}
	if r := []rune(k); len(r) > 6 {
		return string(r[:6]) + "…"
	}
	return k
}

// localAddr 把 RemoteAddr 拆成纯 IP。
func localAddr(remote string) string {
	if remote == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}
