package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
	"github.com/kimimei/elevenlabs-adapter-go/internal/config"
	"github.com/kimimei/elevenlabs-adapter-go/internal/upstream"
	"github.com/kimimei/elevenlabs-adapter-go/internal/voices"
)

var testPCM = bytes.Repeat([]byte{0x01, 0x00, 0x02, 0x00}, 200)

func fakeUpstream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("character-cost", "1")
	w.Header().Set("request-id", "up_req_1")
	w.WriteHeader(http.StatusOK)
	w.Write(testPCM)
}

type harness struct {
	app    *App
	engine *gin.Engine
	srv    *httptest.Server
	dir    string
}

func newHarness(t *testing.T, mutate func(*config.Config), upstreamHandler http.HandlerFunc) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	if upstreamHandler == nil {
		upstreamHandler = fakeUpstream
	}
	srv := httptest.NewServer(upstreamHandler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		APIBase:        srv.URL,
		DefaultModel:   "eleven_multilingual_v2",
		TimeoutSeconds: 10,
		MaxBodyBytes:   2 * 1024 * 1024,
		BillingMode:    config.BillingBaseInput,
		BillingLogDir:  dir,
		BillingLogDays: 30,
		TextClean:      "safe",
		Presets:        "none",
		MaxRetries:     0,
	}
	if mutate != nil {
		mutate(cfg)
	}

	cat, err := voices.Load("")
	if err != nil {
		t.Fatalf("加载模型目录失败: %v", err)
	}
	ledger := billing.NewLedger(cfg.BillingLogDir, cfg.BillingMode, cfg.BillingLogDays, false, 0, 0)
	t.Cleanup(func() { _ = ledger.Close() })

	uc := upstream.NewClient(cfg.APIBase, cfg.TimeoutSeconds, cfg.MaxRetries)
	app, err := New(cfg, cat, ledger, uc)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	return &harness{app: app, engine: app.Router(), srv: srv, dir: dir}
}

func (h *harness) post(t *testing.T, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk_test_key_1234")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	return rec
}

func (h *harness) lastRow(t *testing.T) map[string]any {
	t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatalf("读账本目录失败: %v", err)
	}
	var file string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			file = filepath.Join(h.dir, e.Name())
		}
	}
	if file == "" {
		t.Fatalf("账本目录里没有任何 .jsonl，说明根本没写入：%s", h.dir)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读账本失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
		t.Fatalf("账本行解析失败: %v\n原文: %s", err, lines[len(lines)-1])
	}
	return row
}

// SAMPLES 覆盖 new-api 口径最容易出偏差的形态。
// 这是整个迁移最硬的一条门禁：bill_chars 必须逐笔等于 utf8.RuneCountInString(input)。
var paritySamples = []string{
	"你好世界",
	"  你好  ",                    // 首尾空格（new-api 不 strip）
	"你好\n",                      // 尾随换行
	"\n你好",                      // 首部换行
	"你\u200b好\u200b世界",          // 零宽
	"\ufeff你好世界\ufeff",          // BOM
	"你    好",                    // 连续空格
	"第一段\r\n第二段",                // CRLF
	"👨\u200d👩\u200d👧一家人",        // emoji ZWJ
	"**粗体**和[链接](http://x.com)", // Markdown
	"a b c",
	"混排 text 混排 123",
	"𝔘𝔫𝔦𝔠𝔬𝔡𝔢 𝟄𝔢𝔠𝔥", // 代理对：UTF-16 长度 ≠ 字符数
}

func TestLedgerEqualsNewapiRuneCount(t *testing.T) {
	h := newHarness(t, nil, nil)
	for _, text := range paritySamples {
		t.Run(text, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"model": "eleven_multilingual_v2",
				"voice": "rachel",
				"input": text,
			})
			rec := h.post(t, string(body), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200，响应体：%s", rec.Code, rec.Body.String())
			}
			want := utf8.RuneCountInString(text)
			row := h.lastRow(t)
			if got := int(row["bill_chars"].(float64)); got != want {
				t.Fatalf("账本 bill_chars = %d，new-api 口径 = %d（原文 %q）", got, want, text)
			}
			if got := int(row["in_chars"].(float64)); got != want {
				t.Fatalf("账本 in_chars = %d，期望 %d", got, want)
			}
			if row["bill_source"] != "input" {
				t.Fatalf("bill_source = %v，期望 input", row["bill_source"])
			}
			if got := rec.Header().Get("X-Usage-Input-Characters"); got != itoa(want) {
				t.Fatalf("响应头 = %q，期望 %d", got, want)
			}
		})
	}
}

// 代理对按字符计，不是 UTF-16 码元，也不是字节。
func TestSurrogatePairCountedAsCharacters(t *testing.T) {
	text := "𝔘𝔫𝔦𝔠𝔬𝔡𝔢 𝟄𝔢𝔠𝔥"
	if n := utf8.RuneCountInString(text); n != 12 {
		t.Fatalf("样本本身变了：rune 数 = %d，期望 12", n)
	}
	if n := len([]byte(text)); n != 45 {
		t.Fatalf("样本本身变了：UTF-8 字节数 = %d，期望 45", n)
	}
	h := newHarness(t, nil, nil)
	body, _ := json.Marshal(map[string]any{"model": "eleven_multilingual_v2", "voice": "rachel", "input": text})
	rec := h.post(t, string(body), nil)
	if got := rec.Header().Get("X-Usage-Input-Characters"); got != "12" {
		t.Fatalf("X-Usage-Input-Characters = %q，期望 12（按字符计，不是字节/码元）", got)
	}
}

// 首尾空白不 strip：客户提交 6 个字符，new-api 就收 6；我们若 strip 成 4，逐笔对账稳定差 2。
func TestWhitespaceNotStripped(t *testing.T) {
	h := newHarness(t, nil, nil)
	body, _ := json.Marshal(map[string]any{"model": "eleven_multilingual_v2", "voice": "rachel", "input": "  你好  "})
	rec := h.post(t, string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Usage-Input-Characters"); got != "6" {
		t.Fatalf("计费字符 = %q，期望 6（不 strip）", got)
	}
	row := h.lastRow(t)
	if got := int(row["sent_chars"].(float64)); got != 2 {
		t.Fatalf("sent_chars = %d，期望 2（实际合成 2 字，这是成本依据）", got)
	}
}

// 回滚路径必须一直可用：改一个环境变量就能切回 sent 口径。
func TestCharsModeStillAvailableForRollback(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.BillingMode = config.BillingBaseSent }, nil)
	body, _ := json.Marshal(map[string]any{"model": "eleven_multilingual_v2", "voice": "rachel", "input": "你\u200b好\u200b世界"})
	rec := h.post(t, string(body), nil)
	if got := rec.Header().Get("X-Usage-Input-Characters"); got != "4" {
		t.Fatalf("chars 口径计费字符 = %q，期望 4", got)
	}
	if got := rec.Header().Get("X-Usage-Source"); got != "chars" {
		t.Fatalf("X-Usage-Source = %q，期望 chars", got)
	}
	row := h.lastRow(t)
	if got := row["bill_source"]; got != "chars" {
		t.Fatalf("账本 bill_source = %v，期望 chars", got)
	}
}

// HTTP 头只能是 latin-1：中文写进去会让整个响应打挂，不是只丢那个头。
func TestResponseHeadersAreLatin1(t *testing.T) {
	h := newHarness(t, nil, nil)
	body, _ := json.Marshal(map[string]any{"model": "eleven_multilingual_v2", "voice": "rachel", "input": "你\u200b好"})
	rec := h.post(t, string(body), nil)
	for k, vals := range rec.Header() {
		for _, v := range vals {
			for _, r := range v {
				if r > 0xFF {
					t.Fatalf("响应头 %s 含非 latin-1 字符 %q：%s", k, r, v)
				}
			}
		}
	}
	if rule := rec.Header().Get("X-Usage-Rule"); !strings.Contains(rule, "mode=input") {
		t.Fatalf("X-Usage-Rule = %q", rule)
	}
}

func TestMissingVoiceReturns400(t *testing.T) {
	h := newHarness(t, nil, nil)
	body, _ := json.Marshal(map[string]any{"model": "eleven_multilingual_v2", "input": "你好"})
	rec := h.post(t, string(body), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	var env map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v", err)
	}
	if env["error"]["code"] != "invalid_request_error" {
		t.Fatalf("error.code = %v", env["error"]["code"])
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("失败响应也必须带 X-Request-Id —— 客户说「失败还被扣钱」时没它查不了")
	}
	row := h.lastRow(t)
	if got := int(row["bill_chars"].(float64)); got != 0 {
		t.Fatalf("失败请求 bill_chars = %d，期望 0", got)
	}
	if row["bill_source"] != "failed" {
		t.Fatalf("bill_source = %v，期望 failed", row["bill_source"])
	}
}

// pydantic 在 lax 模式下把 "2.0" 这个字符串强制成 float；Go 的 encoding/json 会报错。
// 升级即故障，所以必须兼容。
func TestSpeedStringCoercedAndClamped(t *testing.T) {
	h := newHarness(t, nil, nil)
	body := `{"model":"eleven_multilingual_v2","voice":"rachel","input":"你好","speed":"2.0"}`
	rec := h.post(t, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("字符串 speed 应被接受，状态码 = %d，响应：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Speed-Clamped"); got != "1.2" {
		t.Fatalf("X-Speed-Clamped = %q，期望 1.2", got)
	}
}

func TestSpeedOutOfOpenAIRangeRejected(t *testing.T) {
	h := newHarness(t, nil, nil)
	body := `{"model":"eleven_multilingual_v2","voice":"rachel","input":"你好","speed":9}`
	rec := h.post(t, body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
}

func TestStreamReturns501(t *testing.T) {
	h := newHarness(t, nil, nil)
	body, _ := json.Marshal(map[string]any{
		"model": "eleven_multilingual_v2", "voice": "rachel", "input": "你好", "stream": true,
	})
	rec := h.post(t, string(body), nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501", rec.Code)
	}
}

func TestUnsupportedFormatRejected(t *testing.T) {
	h := newHarness(t, nil, nil)
	body, _ := json.Marshal(map[string]any{
		"model": "eleven_multilingual_v2", "voice": "rachel", "input": "你好", "response_format": "aac",
	})
	rec := h.post(t, string(body), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("aac 应被拒绝（不静默降级），状态码 = %d", rec.Code)
	}
}

func TestWavGetsRIFFContainer(t *testing.T) {
	h := newHarness(t, nil, nil)
	body, _ := json.Marshal(map[string]any{
		"model": "eleven_multilingual_v2", "voice": "rachel", "input": "你好", "response_format": "wav",
	})
	rec := h.post(t, string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if got := rec.Body.Bytes()[:4]; string(got) != "RIFF" {
		t.Fatalf("wav 响应缺 RIFF 容器，前 4 字节 = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestMockModeDoesNotCallUpstream(t *testing.T) {
	called := false
	h := newHarness(t, func(c *config.Config) { c.Mock = true }, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	body, _ := json.Marshal(map[string]any{"model": "eleven_multilingual_v2", "voice": "rachel", "input": "你好"})
	rec := h.post(t, string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if called {
		t.Fatal("mock 模式不应调用上游")
	}
	if rec.Header().Get("X-Mock") != "true" {
		t.Fatalf("X-Mock = %q", rec.Header().Get("X-Mock"))
	}
	// 请求 mp3 时返回的是 wav，必须明写出来
	if rec.Header().Get("X-Mock-Format-Mismatch") != "mp3" {
		t.Fatalf("X-Mock-Format-Mismatch = %q，期望 mp3", rec.Header().Get("X-Mock-Format-Mismatch"))
	}
	if got := rec.Body.Bytes()[:4]; string(got) != "RIFF" {
		t.Fatalf("mock 音频应为 WAV，前 4 字节 = %q", got)
	}
}

// 上游 422 归一成 400；401 必须原样保留（面板据此禁用渠道）。
func TestUpstreamStatusMapping(t *testing.T) {
	cases := []struct{ upstream, want int }{
		{http.StatusUnprocessableEntity, http.StatusBadRequest},
		{http.StatusUnauthorized, http.StatusUnauthorized},
		{http.StatusPaymentRequired, http.StatusForbidden},
		{http.StatusInternalServerError, http.StatusBadGateway},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		code := tc.upstream
		h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"detail":{"message":"upstream says no"}}`))
		})
		body, _ := json.Marshal(map[string]any{"model": "eleven_multilingual_v2", "voice": "rachel", "input": "你好"})
		rec := h.post(t, string(body), nil)
		if rec.Code != tc.want {
			t.Fatalf("上游 %d 映射成 %d，期望 %d", tc.upstream, rec.Code, tc.want)
		}
	}
}

func TestModelsOnlyListsEnginesByDefault(t *testing.T) {
	h := newHarness(t, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Data) != len(h.app.Catalog.EngineIDs) {
		t.Fatalf("默认模型数 = %d，期望只含 %d 个标准引擎", len(out.Data), len(h.app.Catalog.EngineIDs))
	}
	for _, d := range out.Data {
		if d.ID == "tts-1" {
			t.Fatal("默认不应列出 compat 包里的 tts-1")
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
