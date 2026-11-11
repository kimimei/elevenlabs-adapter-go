package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// /v1/usage 必须能回答「有多少请求走了流式、各档位的真实单价是多少」。
//
// 这不是锦上添花：流式与非流式在客户端是完全不同的接入方式，
// 上线后如果看板说不出流式占比，就既无法评估这次改动的覆盖面，
// 也无法单独对流式路径定 SLA。Go 版最初只补了账本字段、没补聚合，
// 结果是账本里查得到、看板里看不见 —— 等于没做。
func TestUsageReportsStreamSplitAndPerModelCost(t *testing.T) {
	h := newHarness(t, nil, streamingUpstream([]string{"AAA", "BBB"}, 200, "6"))
	auth := map[string]string{"Authorization": "Bearer sk_streamtest"}

	// 同一 token 下，一笔整段 + 一笔流式
	h.post(t, `{"model":"eleven_multilingual_v2","voice":"21m00Tcm4TlvDq8ikWAM",`+
		`"input":"你好世界"}`, auth)
	h.post(t, streamBody, auth)

	rec := doReq(h, http.MethodGet, "/v1/usage", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/usage 期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Today struct {
			Requests      int      `json:"requests"`
			SSERequests   int      `json:"sse_requests"`
			AudioRequests int      `json:"audio_requests"`
			SSERatio      *float64 `json:"sse_ratio"`
			ByModel       map[string]struct {
				Requests       int      `json:"requests"`
				SentChars      int      `json:"sent_chars"`
				CreditsPerChar *float64 `json:"credits_per_char"`
			} `json:"by_model"`
		} `json:"today"`
		Window struct {
			SSERequests int `json:"sse_requests"`
		} `json:"window"`
		ByCaller []struct {
			Caller      string   `json:"caller"`
			SSERequests int      `json:"sse_requests"`
			SSERatio    float64  `json:"sse_ratio"`
			Models      []string `json:"models"`
		} `json:"by_caller"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 usage 失败: %v（body=%s）", err, rec.Body.String())
	}

	if out.Today.SSERequests != 1 || out.Today.AudioRequests != 1 {
		t.Fatalf("today 流式拆分不对：sse=%d audio=%d，期望 1/1",
			out.Today.SSERequests, out.Today.AudioRequests)
	}
	if out.Today.SSERatio == nil || *out.Today.SSERatio != 0.5 {
		t.Fatalf("sse_ratio 期望 0.5，得到 %v", out.Today.SSERatio)
	}

	m, ok := out.Today.ByModel["eleven_multilingual_v2"]
	if !ok {
		t.Fatalf("by_model 缺少 eleven_multilingual_v2，实际键：%v", keysOf(out.Today.ByModel))
	}
	if m.Requests != 2 {
		t.Errorf("by_model.requests = %d，期望 2", m.Requests)
	}
	// 两笔各 4 字符（"你好世界"），上游每次回 6 积分 → 6/4 = 1.5
	if m.CreditsPerChar == nil || *m.CreditsPerChar != 1.5 {
		t.Errorf("by_model.credits_per_char 期望 1.5，得到 %v", m.CreditsPerChar)
	}

	if out.Window.SSERequests != 1 {
		t.Errorf("window.sse_requests = %d，期望 1", out.Window.SSERequests)
	}
	if len(out.ByCaller) != 1 {
		t.Fatalf("by_caller 期望 1 条，得到 %d", len(out.ByCaller))
	}
	if out.ByCaller[0].SSERequests != 1 || out.ByCaller[0].SSERatio != 0.5 {
		t.Errorf("by_caller 流式拆分不对：sse=%d ratio=%v",
			out.ByCaller[0].SSERequests, out.ByCaller[0].SSERatio)
	}
	if len(out.ByCaller[0].Models) != 1 || out.ByCaller[0].Models[0] != "eleven_multilingual_v2" {
		t.Errorf("by_caller.models 期望 [eleven_multilingual_v2]，得到 %v", out.ByCaller[0].Models)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
