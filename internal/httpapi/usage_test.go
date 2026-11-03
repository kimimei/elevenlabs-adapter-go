package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
)

// usage 的 window.requests 与 window.in_chars 必须**同源**（都来自 by_token，只含成功请求）。
//
// 这条口径不是审美问题，是可观测性问题：
// 早期实现里 requests 取自 win.Requests（含失败请求），in_chars 取自 by_token 求和（不含失败）。
// 于是出现"window 有 6 次请求但 in_chars=0"这种自相矛盾的读数 ——
// 看起来像"有流量但没字符"，排查时会往"字符统计坏了"方向找，真因却在另一个字段上。
//
// 失败请求的次数看 today.failed，那才是它们的家。
func TestUsageWindowRequestsMatchesInCharsSource(t *testing.T) {
	// 混合场景：1 笔成功（有 token）、2 笔失败（无 token，token 为 null）
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("character-cost", "1")
		w.WriteHeader(http.StatusOK)
		w.Write(testPCM)
	})

	// 成功请求（走完整链路，会记账）
	h.post(t, `{"model":"eleven_multilingual_v2","input":"成功样本。","voice":"yun"}`,
		map[string]string{"Authorization": "Bearer sk_testtoken"})

	// 直接往账本塞两笔失败记录（模拟 400/500 场景，token 为 null）
	for i := 0; i < 2; i++ {
		err := h.app.Ledger.Record(billing.Entry{
			RID:       "fail-rid",
			Model:     "eleven_multilingual_v2",
			InChars:   10,
			SentChars: 10,
			Status:    400,
			Error:     "无法确定 voice_id",
			RawChars:  10,
		})
		if err != nil {
			t.Fatalf("写入失败记录出错: %v", err)
		}
	}

	rec := doReq(h, http.MethodGet, "/v1/usage", "", nil)
	var out struct {
		Today struct {
			Requests  int `json:"requests"`
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
		} `json:"today"`
		Window struct {
			Requests int `json:"requests"`
			InChars  int `json:"in_chars"`
		} `json:"window"`
		ByCaller []struct {
			Caller   string `json:"caller"`
			Requests int    `json:"requests"`
			InChars  int    `json:"in_chars"`
		} `json:"by_caller"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 usage 响应失败: %v（body=%s）", err, rec.Body.String())
	}

	// today 应当把 3 笔都算上（1 成功 + 2 失败）
	if out.Today.Requests != 3 {
		t.Errorf("today.requests = %d，期望 3（1 成功 + 2 失败）", out.Today.Requests)
	}
	if out.Today.Succeeded != 1 {
		t.Errorf("today.succeeded = %d，期望 1", out.Today.Succeeded)
	}
	if out.Today.Failed != 2 {
		t.Errorf("today.failed = %d，期望 2", out.Today.Failed)
	}

	// window 只含成功请求 —— 与 in_chars 同源
	if out.Window.Requests != 1 {
		t.Errorf("window.requests = %d，期望 1（只含成功请求，与 in_chars 同源）", out.Window.Requests)
	}
	if out.Window.InChars <= 0 {
		t.Errorf("window.in_chars = %d，期望 >0（有成功请求就该有字符数）", out.Window.InChars)
	}

	// 关键断言：requests 与 in_chars 必须来自同一批记录。
	// 失败请求有次数没字符，所以只要 window.requests > by_caller 求和，就说明口径混了。
	sumReq := 0
	for _, c := range out.ByCaller {
		sumReq += c.Requests
	}
	if out.Window.Requests != sumReq {
		t.Errorf("window.requests(%d) 与 by_caller 求和(%d) 不一致 —— 口径混用了",
			out.Window.Requests, sumReq)
	}
}
