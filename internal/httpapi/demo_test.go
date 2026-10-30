package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kimimei/elevenlabs-adapter-go/internal/config"
)

// doReq 发一个请求。演示接口有 GET 与 POST 两类，harness 只封装了同步语音那条路径。
func doReq(h *harness, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	return rec
}

func demoHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	return newHarness(t, func(cfg *config.Config) {
		cfg.Mock = true
		cfg.DemoEnabled = true
		cfg.DemoMaxChars = 20
		cfg.DemoPerHour = 20
		cfg.DemoVoices = []string{"zh-yingshi", "zh-boyin"}
		if mutate != nil {
			mutate(cfg)
		}
	}, nil)
}

func TestDemoDisabledReturns404(t *testing.T) {
	h := demoHarness(t, func(cfg *config.Config) { cfg.DemoEnabled = false })
	rec := doReq(h, http.MethodPost, "/v1/demo/speech", `{"text":"你好"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Fix     string `json:"fix"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v", err)
	}
	if body.Error.Message == "" {
		t.Fatalf("错误体缺少 message")
	}
}

// 试听页靠这个接口决定「显示试听按钮」还是「提示未开启」，
// 字段名写错就会让页面永远显示成未开启。
func TestDemoConfigShape(t *testing.T) {
	h := demoHarness(t, nil)
	rec := doReq(h, http.MethodGet, "/v1/demo/config", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var out struct {
		Enabled  bool `json:"enabled"`
		MaxChars int  `json:"max_chars"`
		PerHour  int  `json:"per_hour"`
		Voices   []struct {
			Name  string `json:"name"`
			Label string `json:"label"`
		} `json:"voices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !out.Enabled || out.MaxChars != 20 || out.PerHour != 20 {
		t.Fatalf("配置回显不对: %+v", out)
	}
	if len(out.Voices) != 2 {
		t.Fatalf("音色数 = %d，期望 2", len(out.Voices))
	}
	if out.Voices[1].Name != "zh-boyin" || out.Voices[1].Label != "播音女声" {
		t.Fatalf("音色标签未从目录取到: %+v", out.Voices[1])
	}
}

func TestDemoMockTruncatesToMaxChars(t *testing.T) {
	h := demoHarness(t, func(cfg *config.Config) { cfg.DemoMaxChars = 5 })
	rec := doReq(h, http.MethodPost, "/v1/demo/speech", `{"text":"你好世界你好世界"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Demo-Chars"); got != "5" {
		t.Fatalf("X-Demo-Chars = %q，期望 5（按 DemoMaxChars 截断）", got)
	}
	if got := rec.Header().Get("X-Mock"); got != "true" {
		t.Fatalf("X-Mock = %q，期望 true", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "audio/wav") {
		t.Fatalf("Content-Type = %q，期望 audio/wav", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "RIFF") {
		t.Fatalf("mock 音频应为带 RIFF 头的 WAV")
	}
}

func TestDemoRateLimit(t *testing.T) {
	h := demoHarness(t, func(cfg *config.Config) { cfg.DemoPerHour = 2 })
	for i := 0; i < 2; i++ {
		rec := doReq(h, http.MethodPost, "/v1/demo/speech", `{"text":"你好"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次应放行，got %d", i+1, rec.Code)
		}
	}
	rec := doReq(h, http.MethodPost, "/v1/demo/speech", `{"text":"你好"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次状态码 = %d，期望 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3600" {
		t.Fatalf("Retry-After = %q，期望 3600", got)
	}
}

// 反向代理后面所有人都是 127.0.0.1，限额会变成全站共享 20 次。
// 这个开关就是给「确认前面是自己的 nginx」的场景用的。
func TestDemoTrustXFFSplitsBuckets(t *testing.T) {
	h := demoHarness(t, func(cfg *config.Config) {
		cfg.DemoPerHour = 1
		cfg.DemoTrustXFF = true
	})
	first := doReq(h, http.MethodPost, "/v1/demo/speech", `{"text":"你好"}`,
		map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.1"})
	if first.Code != http.StatusOK {
		t.Fatalf("首个请求应放行，got %d", first.Code)
	}
	same := doReq(h, http.MethodPost, "/v1/demo/speech", `{"text":"你好"}`,
		map[string]string{"X-Forwarded-For": "203.0.113.7"})
	if same.Code != http.StatusTooManyRequests {
		t.Fatalf("同一 IP 第二次状态码 = %d，期望 429", same.Code)
	}
	other := doReq(h, http.MethodPost, "/v1/demo/speech", `{"text":"你好"}`,
		map[string]string{"X-Forwarded-For": "198.51.100.9"})
	if other.Code != http.StatusOK {
		t.Fatalf("不同 IP 应各自计数，got %d", other.Code)
	}
}

func TestDemoRejectsOverlongText(t *testing.T) {
	h := demoHarness(t, nil)
	body, _ := json.Marshal(map[string]string{"text": strings.Repeat("啊", 201)})
	rec := doReq(h, http.MethodPost, "/v1/demo/speech", string(body), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
}

func TestDemoRejectsUnknownVoiceByFallback(t *testing.T) {
	h := demoHarness(t, nil)
	// 传一个不在白名单里的音色：不能报错，也不能真的用它 —— 回落到第一个
	rec := doReq(h, http.MethodPost, "/v1/demo/speech",
		`{"text":"你好","voice":"../../../etc/passwd"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Demo-Chars"); got != "2" {
		t.Fatalf("X-Demo-Chars = %q，期望 2", got)
	}
}

func TestDemoEmptyTextRejected(t *testing.T) {
	h := demoHarness(t, nil)
	for _, body := range []string{`{"text":"   "}`, `{}`, `{"text":123}`} {
		rec := doReq(h, http.MethodPost, "/v1/demo/speech", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s 状态码 = %d，期望 400", body, rec.Code)
		}
	}
}

// 试听用的是服务端 Key（公众端点不接受客户自带 Key），
// 所以没配 Key 时必须 500 并说清「配哪一项」，而不是笼统的「上游失败」。
func TestDemoWithoutServerKey(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "")
	h := demoHarness(t, func(cfg *config.Config) { cfg.Mock = false })
	rec := doReq(h, http.MethodPost, "/v1/demo/speech", `{"text":"你好"}`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ELEVENLABS_API_KEY") {
		t.Fatalf("错误体应指出要配置 ELEVENLABS_API_KEY，got %s", rec.Body.String())
	}
}

func TestDemoLabelFallsBackToName(t *testing.T) {
	h := demoHarness(t, func(cfg *config.Config) { cfg.DemoVoices = []string{"not-a-real-voice"} })
	rec := doReq(h, http.MethodGet, "/v1/demo/config", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var out struct {
		Voices []struct {
			Label string `json:"label"`
		} `json:"voices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Voices) != 1 || out.Voices[0].Label != "not-a-real-voice" {
		t.Fatalf("未知音色的 label 应回落到名字本身: %+v", out.Voices)
	}
}
