package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/apierr"
	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
	"github.com/kimimei/elevenlabs-adapter-go/internal/pcm"
	"github.com/kimimei/elevenlabs-adapter-go/internal/textclean"
	"github.com/kimimei/elevenlabs-adapter-go/internal/upstream"
)

// 上游状态码 -> 对下游（OpenAI 语义）的状态码。
// 401 原样保留：中转站据此判定「渠道密钥失效」并自动禁用渠道，这是期望行为。
var upstreamStatusMap = map[int]int{
	401: 401,
	402: 403,
	403: 403,
	404: 404,
	422: 400,
	429: 429,
}

func normalizeStatus(code int) int {
	if v, ok := upstreamStatusMap[code]; ok {
		return v
	}
	if code >= 500 && code < 600 {
		return 502
	}
	return 400
}

// speechIn 是解析后的请求体。
type speechIn struct {
	Model          string
	Input          string
	Voice          string
	ResponseFormat string
	Speed          *float64
	Stream         bool
	StreamFormat   string
	TextClean      string
}

// handleSpeech 是 POST /v1/audio/speech。
//
// 计费基数有两个数，缺一不可：
//
//	rawChars  = 客户提交的原始长度（**未 strip**，与 new-api 的 RuneCount(input) 逐字相等）
//	            -> input 口径的计费依据
//	sentChars = 实际送上游合成的长度 -> **成本依据**（上游只收这个）
//
// 清理掉的字符（raw − sent）不再送上游，那部分**我方上游也不付钱**，
// 所以它是上游成本节省，**不是计费折扣**。两者混为一谈就会算错单位成本。
func (a *App) handleSpeech(c *gin.Context) {
	start := time.Now()
	rid := randomID()

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			a.tooLarge(c, a.Cfg.MaxBodyBytes+1)
			return
		}
		writeErr(c, http.StatusBadRequest, apierr.JoinLoc("body", "请求体读取失败"),
			"invalid_request_error", validationFix, nil)
		return
	}

	in, perr := parseSpeechBody(raw, a.Cfg.DefaultModel)
	if perr != nil {
		writeErr(c, http.StatusBadRequest, apierr.JoinLoc(perr.field, perr.message),
			"invalid_request_error", perr.fix, nil)
		return
	}

	authz := c.GetHeader("Authorization")
	xAPIKey := c.GetHeader("X-Api-Key")
	apiKeyHeader := c.GetHeader("Api-Key")

	cleanMode := textclean.ResolveMode(in.TextClean, a.Cfg.TextClean)
	text, report := textclean.Clean(in.Input, cleanMode)
	rawChars := report.RawLen
	sentChars := textclean.RuneLen(text)
	inChars := rawChars

	// 账本里的 model 字段：显式传了 voice 就记 voice（音色即模型的产品语义）
	modelName := in.Model
	if in.Voice != "" {
		modelName = in.Voice
	}
	voiceName := in.Voice
	clientIP := localAddr(c.Request.RemoteAddr)
	tokName := tokenLabel(firstNonEmpty(authz, xAPIKey, apiKeyHeader))
	cleanRules := strings.Join(report.Rules, ",")

	// 流式相关：isSSE 决定走哪条上游端点，streamIgnored 记录「客户要了流式但没给对参数」
	isSSE := in.StreamFormat == "sse"
	streamIgnored := in.Stream && !isSSE
	// 上游真实模型 id 在 ResolveVoiceAndModel 之后才拿到，闭包按引用读这个变量
	modelIDRec := ""

	// record 记一行账，返回应附加到响应的头。
	// 错误响应也必须带 X-Request-Id —— 客户说「这次失败还被扣钱了」时，
	// 没有 rid 就无从查起。
	record := func(status int, upCredits *int, upRID *string, errMsg string) map[string]string {
		entry := billing.Entry{
			RID:           rid,
			UpRID:         upRID,
			Model:         modelName,
			ModelID:       modelIDRec,
			Voice:         voiceName,
			Stream:        isSSE,
			StreamFormat:  in.StreamFormat,
			StreamIgnored: streamIgnored,
			InChars:       inChars,
			SentChars:     sentChars,
			UpCredits:     upCredits,
			Status:        status,
			ElapsedMS:     int(time.Since(start).Milliseconds()),
			ClientIP:      clientIP,
			TokenName:     tokName,
			Error:         errMsg,
			RawChars:      rawChars,
			CleanedChars:  sentChars,
			CleanRules:    cleanRules,
		}
		if err := a.Ledger.Record(entry); err != nil {
			log.Printf("billing log write failed (request %s): %v", rid, err)
		}
		return map[string]string{"X-Request-Id": rid}
	}

	// 判断的是清理后的结果：全是零宽字符/空白的 input 清理后为空，
	// 这种请求送给上游只会白扣费，直接拒绝。
	if text == "" {
		writeErr(c, http.StatusBadRequest, "input 不能为空", "invalid_request_error",
			"把要合成的文本放进 input 字段；纯空格、纯零宽字符也会被拒绝",
			record(400, nil, nil, "input 为空"))
		return
	}
	if a.Cfg.MaxChars > 0 && sentChars > a.Cfg.MaxChars {
		// 按实际送合成的量判断：上游的上限约束的是真正发过去的文本，
		// 用原始长度判断会让客户用不可见字符绕过限制。
		writeErr(c, http.StatusBadRequest,
			fmt.Sprintf("input 超长：%d 字符，单次上限 %d 字符，请分段提交", sentChars, a.Cfg.MaxChars),
			"invalid_request_error",
			"按 2000 字左右切分后逐段调用，再用音频拼接；或调大 ADAPTER_MAX_CHARS（注意上游单次也有上限）",
			record(400, nil, nil, "input 超长"))
		return
	}
	// stream=true 不再返回 501，改为「忽略该参数，按非流式返回」。
	//
	// 原因（2026-11-05）：大量 work 工具（Dify / n8n / 各类 Agent 框架）在调用
	// OpenAI 兼容 TTS 时会默认带 "stream": true。直接 501 会让接入方第一次调用就失败，
	// 且报错信息对他们没有指导意义 —— 他们只是照抄了 OpenAI 的默认参数。
	// 这类客户端拿到完整的 audio/mpeg 音频流同样能正常播放，丢的只是首包延迟。
	// 用 X-Stream-Ignored 头显式告知，避免「以为在流式、其实不是」这种静默差异。
	if streamIgnored {
		setHeader(c, "X-Stream-Ignored", "true")
		setHeader(c, "X-Stream-Reason", "stream_format is not sse; returning complete audio")
	}
	if in.StreamFormat != "audio" && in.StreamFormat != "sse" {
		writeErr(c, http.StatusBadRequest, "stream_format 仅支持 audio 或 sse",
			"invalid_request_error",
			`要边生成边播放就传 "stream_format":"sse"（并配 response_format=mp3）；只要整段音频就别传这个字段`,
			record(400, nil, nil, "stream_format 不支持"))
		return
	}
	if isSSE && in.ResponseFormat != "mp3" {
		// 与 Python 版一致：WAV 需要完整长度才能写 RIFF 头，伪造一个假头
		// 会让播放器读到 0 时长，比直接拒绝更难排查。
		writeErr(c, http.StatusBadRequest,
			"SSE 流式仅支持 mp3；WAV 需要完整长度，不能伪造 RIFF 头",
			"invalid_request_error",
			`把 response_format 改成 mp3（流式默认值），或去掉 stream_format 走非流式`,
			record(400, nil, nil, "流式格式不支持"))
		return
	}

	fmtSpec, ok := upstream.Formats[in.ResponseFormat]
	if !ok || fmtSpec == nil {
		writeErr(c, http.StatusBadRequest,
			fmt.Sprintf("response_format '%s' 不支持；ElevenLabs 仅支持 mp3/opus/wav/pcm", in.ResponseFormat),
			"invalid_request_error",
			"把 response_format 改成 mp3（最通用）或 opus/wav/pcm；aac/flac 上游不支持，不会静默降级",
			record(400, nil, nil, "response_format 不支持"))
		return
	}

	voiceID, modelID, extra, rerr := upstream.ResolveVoiceAndModel(in.Model, in.Voice, a.Catalog, a.Cfg.DefaultModel)
	if rerr != nil {
		// fix 文案必须与 Python 版逐字一致（M6 比对出来的）：
		// 少一句「不要传 tts-1 之外的自造名」，客户就会继续拿 tts-1 当模型名试。
		writeErr(c, http.StatusBadRequest, rerr.Error(), "invalid_request_error",
			`voice 传音色名（如 yun）或 15 位以上的 voice_id；model 传引擎名（eleven_v3）或已注册的别名，不要传 "tts-1" 之外的自造名`,
			record(400, nil, nil, "voice_id 非法"))
		return
	}
	voiceName = voiceID
	modelIDRec = modelID
	if voiceID == "" {
		writeErr(c, http.StatusBadRequest,
			"无法确定 voice_id：请在请求中传 voice（音色名或 voice_id），"+
				// 路径要写成 app/voices.json：只说 voices.json，客户在仓库根目录下找不到
				"或把模型配置为 'eleven-<name>' 形式的音色别名（见 app/voices.json）",
			"invalid_request_error",
			"补一个 voice 字段：{\"model\":\"eleven_v3\",\"voice\":\"yun\"}；model 用引擎名时 voice 是必填的",
			record(400, nil, nil, "无法确定 voice_id"))
		return
	}

	apiKey := a.resolveAPIKey(authz, xAPIKey, apiKeyHeader)
	if apiKey == "" && !a.Cfg.Mock {
		writeErr(c, http.StatusUnauthorized,
			"缺少 ElevenLabs API Key：请通过 Authorization: Bearer <key> 传入，或设置环境变量 ELEVENLABS_API_KEY",
			"authentication_error",
			"在请求头加 Authorization: Bearer sk_xxx；中转站场景下这个 Key 是渠道密钥，不是客户令牌",
			record(401, nil, nil, "缺少 API Key"))
		return
	}

	speedUsed, speedClamped := upstream.ClampSpeed(in.Speed)

	// 流式走独立路径：它不能复用下面的 body []byte 流程 —— 那个流程天然要
	// 把整段音频攒在内存里，攒完再发，正好把流式的意义抵消掉。
	if isSSE {
		a.handleSpeechStream(c, streamReq{
			RID: rid, Text: text, ModelID: modelID, VoiceID: voiceID, APIKey: apiKey,
			OutputFormat: fmtSpec.Output, Speed: in.Speed, Extra: extra,
			InChars: inChars, SentChars: sentChars, RawChars: rawChars,
			ModelName: modelName, VoiceName: voiceName, ClientIP: clientIP,
			TokenName: tokName, CleanMode: cleanMode, Removed: report.Removed,
			SpeedUsed: speedUsed, SpeedClamped: speedClamped,
			Mock: a.Cfg.Mock, Record: record,
		})
		return
	}

	var (
		body        []byte
		contentType string
		upCredits   *int
		upRID       *string
	)

	if a.Cfg.Mock {
		body = pcm.Silence(1.0, 24000)
		contentType = "audio/wav"
		setHeader(c, "X-Mock", "true")
		// 请求 mp3/opus 时这里会是 wav：明写出来，别让下游以为是适配器格式转换出问题
		setHeader(c, "X-Mock-Format", "wav")
		if in.ResponseFormat != "wav" && in.ResponseFormat != "pcm" {
			setHeader(c, "X-Mock-Format-Mismatch", in.ResponseFormat)
		}
	} else {
		up, uerr := a.Upstream.TextToSpeech(c.Request.Context(), text, modelID, voiceID, apiKey,
			fmtSpec.Output, in.Speed, extra)
		if uerr != nil {
			// 超时上游可能已扣费（这是最贵的一类失败），账本要留痕
			_, fix := apierr.Explain(502, uerr.Error())
			log.Printf("upstream call failed (request %s): %v", rid, uerr)
			writeErr(c, http.StatusBadGateway, "调用 ElevenLabs 失败："+uerr.Error(),
				"upstream_error", fix, record(502, nil, nil, uerr.Error()))
			return
		}
		upRID = firstHeader(up.Header, "request-id", "x-trace-id")
		if up.StatusCode != http.StatusOK {
			// 把上游原文翻译成一句可执行的话，同时保留原始 message 供排查
			_, fix := apierr.Explain(up.StatusCode, string(up.Body))
			status := normalizeStatus(up.StatusCode)
			errText := truncateRunes(string(up.Body), 200)
			log.Printf("upstream error (request %s): %d %s", rid, up.StatusCode, truncateRunes(string(up.Body), 500))
			writeErr(c, status, upstream.ErrorMessage(up), "upstream_error", fix,
				record(status, nil, upRID, errText))
			return
		}
		body = up.Body
		// OpenAI 的 wav 语义是「带 RIFF 容器的 WAV」，上游给的是裸 PCM
		if in.ResponseFormat == "wav" {
			body = pcm.Encode(body, 24000)
		}
		upCredits = upstream.ExtractCharacterCost(up.Header)
		contentType = fmtSpec.ContentType
	}

	// ---- 计费口径 ----
	// bill_chars = 向客户收费的数量（按 BillingMode 决定）
	// sent_chars = 实际送上游合成的字符数 = **成本依据**
	// up_credits = 上游真实扣的积分（拿不到则为 nil，绝不拿估算值顶替）
	//
	// 收钱的是 new-api（按 RuneCount(input) 扣 quota），我们只是记账。
	// 口径必须与 new-api 同源 —— 账本记 sent 而 new-api 记 raw，
	// 客户拿后台日志一对就发现差额，且差额方向固定为我方少收。
	billN, billSrc := billing.BillCharsByMode(a.Cfg.BillingMode, inChars, sentChars, upCredits, 200)
	setHeader(c, "X-Usage-Input-Characters", strconv.Itoa(billN))
	setHeader(c, "X-Usage-Source", billSrc)
	// 用 RuleHeader（纯 ASCII）—— HTTP 头只允许 latin-1，中文会把响应打挂
	setHeader(c, "X-Usage-Rule", billing.RuleHeader(a.Cfg.BillingMode, billN, inChars, upCredits, 200, billSrc))
	if upCredits != nil {
		setHeader(c, "X-Usage-Upstream-Credits", strconv.Itoa(*upCredits))
	}
	setHeader(c, "X-Request-Id", rid)
	setHeader(c, "X-ElevenLabs-Model", modelID)
	setHeader(c, "X-ElevenLabs-Voice", voiceID)
	// 清理透明化 + 价值可见：客户能自己核对「提交多少、合成多少」
	setHeader(c, "X-Text-Clean-Mode", cleanMode)
	if report.Removed > 0 {
		setHeader(c, "X-Text-Clean", textclean.Describe(report))
		setHeader(c, "X-Text-Raw-Chars", strconv.Itoa(rawChars))
		setHeader(c, "X-Text-Sent-Chars", strconv.Itoa(sentChars))
		// ⚠️ 语义随口径变：input 口径下这部分**仍然计入客户账单**（与 new-api 一致），
		// 但上游只收 sent —— 所以它是上游成本节省，不是计费折扣。
		saved := rawChars - sentChars
		if saved < 0 {
			saved = 0
		}
		setHeader(c, "X-Text-Saved-Chars", strconv.Itoa(saved))
	}
	if speedClamped && speedUsed != nil {
		setHeader(c, "X-Speed-Clamped", strconv.FormatFloat(*speedUsed, 'g', -1, 64))
	}

	record(200, upCredits, upRID, "")
	c.Data(http.StatusOK, contentType, body)
}

// ---------------- 请求体解析 ----------------
//
// 不用结构体 + encoding/json 直接反序列化，是因为 pydantic 在 lax 模式下会做类型强制：
// speed 传字符串 "1.0"、stream 传 "true" 或 1，Python 版都是接受的。
// Go 的 encoding/json 会直接报错 —— 升级即故障，存量客户会突然全量 400。

// defaultModel 是这个请求在「字段缺失」时该用的引擎。
// 注意：不能在这里写死引擎名 —— 曾经硬编码 eleven_multilingual_v2，
// 结果 config 里把默认改成 eleven_v3 也不生效，因为字段缺失时解析层就填好了，
// 根本走不到 ResolveVoiceAndModel 的 defaultModel 分支。
// 传空串表示「交给下游按配置决定」，保持与显式传 model 一致的语义。
func parseSpeechBody(raw []byte, defaultModel string) (*speechIn, *parseErr) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &parseErr{field: "", message: "JSON 解析失败：" + err.Error(), fix: validationFix}
	}

	out := &speechIn{
		Model:          defaultModel,
		ResponseFormat: "mp3",
	}

	if v, ok := m["model"]; ok && !isNull(v) {
		s, ok := asString(v)
		if !ok {
			return nil, &parseErr{field: "model", message: "Input should be a valid string", fix: validationFix}
		}
		out.Model = s
	}

	v, ok := m["input"]
	if !ok {
		return nil, &parseErr{field: "input", message: "Field required", fix: validationFix}
	}
	if isNull(v) {
		return nil, &parseErr{field: "input", message: "Input should be a valid string", fix: validationFix}
	}
	s, ok := asString(v)
	if !ok {
		return nil, &parseErr{field: "input", message: "Input should be a valid string", fix: validationFix}
	}
	out.Input = s

	if vv, ok := m["voice"]; ok && !isNull(vv) {
		s, ok := asString(vv)
		if !ok {
			return nil, &parseErr{field: "voice", message: "Input should be a valid string", fix: validationFix}
		}
		out.Voice = s
	}

	if vv, ok := m["response_format"]; ok && !isNull(vv) {
		s, ok := asString(vv)
		if !ok {
			return nil, &parseErr{field: "response_format", message: "Input should be a valid string", fix: validationFix}
		}
		out.ResponseFormat = s
	}

	if vv, ok := m["speed"]; ok && !isNull(vv) {
		f, ok := asFloat(vv)
		if !ok {
			return nil, &parseErr{field: "speed", message: "Input should be a valid number", fix: validationFix}
		}
		if f < 0.25 {
			return nil, &parseErr{field: "speed", message: "Input should be greater than or equal to 0.25", fix: validationFix}
		}
		if f > 4.0 {
			return nil, &parseErr{field: "speed", message: "Input should be less than or equal to 4", fix: validationFix}
		}
		out.Speed = &f
	}

	if vv, ok := m["stream"]; ok && !isNull(vv) {
		b, ok := asBool(vv)
		if !ok {
			return nil, &parseErr{field: "stream", message: "Input should be a valid boolean", fix: validationFix}
		}
		out.Stream = b
	}

	// stream_format 才是真正控制流式的字段（与 OpenAI 一致，也与 new-api 的
	// AudioRequest.StreamFormat 对齐）。缺省 audio。
	if vv, ok := m["stream_format"]; ok && !isNull(vv) {
		s, ok := asString(vv)
		if !ok {
			return nil, &parseErr{field: "stream_format", message: "Input should be a valid string", fix: validationFix}
		}
		out.StreamFormat = s
	}
	if out.StreamFormat == "" {
		out.StreamFormat = "audio"
	}

	if vv, ok := m["text_clean"]; ok && !isNull(vv) {
		s, ok := asString(vv)
		if !ok {
			return nil, &parseErr{field: "text_clean", message: "Input should be a valid string", fix: validationFix}
		}
		out.TextClean = s
	}

	return out, nil
}

func isNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func asString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func asFloat(raw json.RawMessage) (float64, bool) {
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

func asBool(raw json.RawMessage) (bool, bool) {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n != 0, true
	}
	return false, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstHeader(h http.Header, names ...string) *string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			vv := v
			return &vv
		}
	}
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
