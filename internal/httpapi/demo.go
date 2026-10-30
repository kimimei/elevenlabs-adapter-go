package httpapi

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/pcm"
	"github.com/kimimei/elevenlabs-adapter-go/internal/textclean"
	"github.com/kimimei/elevenlabs-adapter-go/internal/upstream"
)

// 在线试听（Demo）—— 免注册、有护栏。
//
// 为什么要有护栏：这个端点用的是**服务端配置的 Key**，等于把额度公开。
// 没有限制的话，一条 for 循环就能把一个月额度刷光。四道闸门：
//
//  1. 默认关闭（ADAPTER_DEMO_ENABLED=1 才开）
//  2. 单次文本 ≤ ADAPTER_DEMO_MAX_CHARS（默认 20，够听音色，不够白嫖配音）
//  3. 每 IP 每小时 N 次（默认 20）
//  4. 只允许目录里指定的几个音色（不允许传任意 voice_id）
//
// ⚠️ 刻意**不写账本**：账本的作用是跟 new-api 逐笔对账，而试听用的是服务端
// 自己的 Key，没有 new-api 侧的对应记录。混进去会让 L1 对账凭空多出一批
// 「有计费无日志」的行，把真正的漏记淹掉。
type demoLimiter struct {
	mu      sync.Mutex
	hits    map[string][]int64
	perHour int
	now     func() time.Time
}

func newDemoLimiter(perHour int) *demoLimiter {
	if perHour < 1 {
		perHour = 1
	}
	return &demoLimiter{hits: map[string][]int64{}, perHour: perHour, now: time.Now}
}

// allow 记一次命中，返回 (是否放行, 剩余次数)。
func (l *demoLimiter) allow(ip string) (bool, int) {
	now := l.now().UnixNano()
	cutoff := now - int64(time.Hour)

	l.mu.Lock()
	defer l.mu.Unlock()

	keep := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t > cutoff {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.perHour {
		l.hits[ip] = keep
		return false, 0
	}
	keep = append(keep, now)
	l.hits[ip] = keep

	// 顺手清掉没请求过的 IP：dict 只增不减的话，扫一遍来源 IP 就能把内存撑起来
	if len(l.hits) > 1024 {
		for k, v := range l.hits {
			if len(v) == 0 || now-v[len(v)-1] >= int64(time.Hour) {
				delete(l.hits, k)
			}
		}
	}
	return true, l.perHour - len(keep)
}

// demoErrBody 与 Python 版 demo 的错误体一致：只有 message 与 fix，**没有 type/code**。
//
// 这是刻意保留的不一致（Python 的 demo 用另一套 _err）。加字段看起来更规范，
// 但 M6 逐字段 diff 会把每个 demo 错误都标成差异。
type demoErrBody struct {
	Error struct {
		Message string `json:"message"`
		Fix     string `json:"fix,omitempty"`
	} `json:"error"`
}

func writeDemoErr(c *gin.Context, status int, message, fix string, headers map[string]string) {
	var body demoErrBody
	body.Error.Message = message
	body.Error.Fix = fix
	data, err := json.Marshal(body)
	if err != nil {
		data = []byte(`{"error":{"message":"internal error"}}`)
	}
	for k, v := range headers {
		setHeader(c, k, v)
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(status)
	c.Writer.Write(data)
}

// GET /v1/demo/config —— 试听页读的配置（打开时就能知道开没开、还剩几次）。
func (a *App) handleDemoConfig(c *gin.Context) {
	voices := make([]gin.H, 0, len(a.Cfg.DemoVoices))
	for _, name := range a.Cfg.DemoVoices {
		voices = append(voices, gin.H{"name": name, "label": a.demoLabel(name)})
	}
	c.PureJSON(http.StatusOK, gin.H{
		"enabled":   a.Cfg.DemoEnabled,
		"max_chars": a.Cfg.DemoMaxChars,
		"per_hour":  a.Cfg.DemoPerHour,
		"voices":    voices,
	})
}

func (a *App) demoLabel(name string) string {
	if spec, ok := a.Catalog.Aliases[name]; ok && spec.Label != "" {
		return spec.Label
	}
	return name
}

// POST /v1/demo/speech —— 合成一小段试听音频。
func (a *App) handleDemoSpeech(c *gin.Context) {
	if !a.Cfg.DemoEnabled {
		writeDemoErr(c, http.StatusNotFound, "在线试听未开启", "在 .env 设置 ADAPTER_DEMO_ENABLED=1", nil)
		return
	}

	ip := a.demoPeerIP(c)
	ok, left := a.DemoLimiter.allow(ip)
	if !ok {
		writeDemoErr(c, http.StatusTooManyRequests, "试听次数已用完",
			"每 IP 每小时 "+strconv.Itoa(a.Cfg.DemoPerHour)+" 次，稍后再试",
			map[string]string{"Retry-After": "3600"})
		return
	}

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeDemoErr(c, http.StatusBadRequest, "请求体读取失败", "检查请求体是否合法 JSON", nil)
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		writeDemoErr(c, http.StatusBadRequest, "JSON 解析失败："+err.Error(), "body 必须是 JSON", nil)
		return
	}
	textVal, has := m["text"]
	if !has || isNull(textVal) {
		writeDemoErr(c, http.StatusBadRequest, "text 不能为空", `body 加 text，例如 {"text":"你好"}`, nil)
		return
	}
	text, okStr := asString(textVal)
	if !okStr {
		writeDemoErr(c, http.StatusBadRequest, "text 必须是字符串", `body 加 text，例如 {"text":"你好"}`, nil)
		return
	}
	reqVoice := ""
	if v, has := m["voice"]; has && !isNull(v) {
		if s, okv := asString(v); okv {
			reqVoice = s
		}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		writeDemoErr(c, http.StatusBadRequest, "text 不能为空", "填一句要试听的话（不超过 "+strconv.Itoa(a.Cfg.DemoMaxChars)+" 个字符）", nil)
		return
	}
	// 与 pydantic 的 max_length=200 对齐：超长直接拒，不静默截断。
	// （状态码用 400 而不是 pydantic 的 422：422 的报错体是框架内部结构，
	// 与其它端点风格不一致，客户更难读懂 —— 见 CONTRACT 差异表）
	if n := textclean.RuneLen(text); n > 200 {
		writeDemoErr(c, http.StatusBadRequest,
			"text 太长：最多 200 字符（实际 "+strconv.Itoa(n)+"）", "试听只需要一两句话", nil)
		return
	}
	if n := textclean.RuneLen(text); n > a.Cfg.DemoMaxChars {
		text = string([]rune(text)[:a.Cfg.DemoMaxChars])
	}

	voice := a.Cfg.DemoVoices[0]
	for _, v := range a.Cfg.DemoVoices {
		if v == reqVoice {
			voice = v
			break
		}
	}

	voiceID, modelID, extra, rerr := upstream.ResolveVoiceAndModel(voice, "", a.Catalog, a.Cfg.DefaultModel)
	if rerr != nil {
		writeDemoErr(c, http.StatusBadRequest, rerr.Error(), "换一个音色名重试", nil)
		return
	}
	if voiceID == "" {
		writeDemoErr(c, http.StatusBadRequest, "无法确定 voice_id："+voice,
			"ADAPTER_DEMO_VOICES 里填的是 voices.json 中存在的音色名", nil)
		return
	}

	chars := strconv.Itoa(textclean.RuneLen(text))

	if a.Cfg.Mock {
		// ⚠️ 所有头必须在 c.Data 之前设置：c.Data 会立刻把状态行和头部刷出去，
		// 之后再 setHeader 是写进一个已经发出的响应，浏览器与 curl 都收不到。
		setHeader(c, "X-Mock", "true")
		setHeader(c, "X-Demo-Chars", chars)
		c.Data(http.StatusOK, "audio/wav", pcm.Silence(1.0, 24000))
		return
	}

	key := a.resolveAPIKey("", "", "")
	if key == "" {
		writeDemoErr(c, http.StatusInternalServerError, "服务端未配置 Key", "设置 ELEVENLABS_API_KEY 后重启", nil)
		return
	}

	up, uerr := a.Upstream.TextToSpeech(c.Request.Context(), text, modelID, voiceID, key,
		"mp3_44100_128", nil, extra)
	if uerr != nil {
		log.Printf("[WARN] 试听调用上游失败（voice=%s）: %v", voice, uerr)
		writeDemoErr(c, http.StatusBadGateway, "上游调用失败，稍后再试", "若持续失败，联系服务方", nil)
		return
	}
	if up.StatusCode != http.StatusOK {
		// 不把上游原文回给公众端：可能含账户/配额等内部信息。细节只进服务端日志。
		log.Printf("[WARN] 试听上游返回 %d: %s", up.StatusCode, truncateRunes(string(up.Body), 300))
		writeDemoErr(c, http.StatusBadGateway, "上游返回 "+strconv.Itoa(up.StatusCode), "稍后重试", nil)
		return
	}

	setHeader(c, "X-Demo-Chars", chars)
	setHeader(c, "X-Demo-Quota-Left", strconv.Itoa(left))
	setHeader(c, "Cache-Control", "no-store")
	c.Data(http.StatusOK, "audio/mpeg", up.Body)
}

// demoPeerIP 取试听的来源 IP。
//
// 默认取 TCP 对端地址（与 Python 版一致）。但适配器通常挂在 nginx 后面，
// 那样所有人看到的都是 127.0.0.1 —— 20 次/小时的限额变成**全站共享**，
// 第 21 个访客起就点不动了，而告警上什么都看不出来。
// 所以给了 ADAPTER_DEMO_TRUST_XFF 这个显式开关：只有确认前面是自己的反代时才开。
func (a *App) demoPeerIP(c *gin.Context) string {
	if a.Cfg.DemoTrustXFF {
		if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if rip := strings.TrimSpace(c.GetHeader("X-Real-Ip")); rip != "" {
			return rip
		}
	}
	return localAddr(c.Request.RemoteAddr)
}
