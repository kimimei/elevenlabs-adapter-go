package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// streamingUpstream 返回一个"边写边刷"的假上游，用来验证适配器有没有把
// 每一块立刻转出去，而不是攒完再发。
func streamingUpstream(chunks []string, status int, cost string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		if cost != "" {
			w.Header().Set("character-cost", cost)
		}
		w.Header().Set("request-id", "up_stream_1")
		w.WriteHeader(status)
		if status != http.StatusOK {
			w.Write([]byte(`{"detail":{"message":"boom"}}`))
			return
		}
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			w.Write([]byte(c))
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// decodeSSE 把响应体拆成事件列表，并拼回原始音频。
func decodeSSE(t *testing.T, body string) (events []map[string]any, audio string, hasDone bool) {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
			t.Fatalf("SSE data 行不是合法 JSON: %v (%s)", err, line)
		}
		events = append(events, m)
		switch m["type"] {
		case "speech.audio.delta":
			s, _ := m["audio"].(string)
			raw, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				t.Fatalf("delta 的 audio 不是合法 base64: %v", err)
			}
			sb.Write(raw)
		case "speech.audio.done":
			hasDone = true
		}
	}
	return events, sb.String(), hasDone
}

const streamBody = `{"model":"eleven_multilingual_v2","voice":"21m00Tcm4TlvDq8ikWAM",` +
	`"input":"你好世界","response_format":"mp3","stream_format":"sse"}`

func TestStreamSSEStreamsAudioAndDone(t *testing.T) {
	h := newHarness(t, nil, streamingUpstream([]string{"AAA", "BBB", "CCC"}, 200, "3"))
	rec := h.post(t, streamBody, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 必须是 text/event-stream，得到 %q", ct)
	}
	// 经网关转发时 event: 行会被剥掉，适配器自己必须写出来
	if !strings.Contains(rec.Body.String(), "event: speech.audio.delta") {
		t.Fatalf("缺少 event: 行，经面板转发后客户端就分不清事件类型了")
	}

	_, audio, hasDone := decodeSSE(t, rec.Body.String())
	if audio != "AAABBBCCC" {
		t.Fatalf("拼回的音频不对：%q", audio)
	}
	if !hasDone {
		t.Fatalf("缺少 speech.audio.done，客户端无法判断流已结束")
	}

	row := h.lastRow(t)
	if row["stream"] != true {
		t.Fatalf("账本 stream 必须是 true，得到 %v", row["stream"])
	}
	if row["stream_format"] != "sse" {
		t.Fatalf("账本 stream_format 必须是 sse，得到 %v", row["stream_format"])
	}
	if row["model_id"] != "eleven_multilingual_v2" {
		t.Fatalf("账本 model_id 必须是上游真模型，得到 %v", row["model_id"])
	}
	// 成功时必须记上游真实积分，不能拿输入字符数顶替
	if cred, ok := row["up_credits"].(float64); !ok || int(cred) != 3 {
		t.Fatalf("up_credits 应为 3，得到 %v", row["up_credits"])
	}
	if status, _ := row["status"].(float64); int(status) != 200 {
		t.Fatalf("账本 status 应为 200，得到 %v", row["status"])
	}
}

func TestStreamSSEHitsStreamEndpoint(t *testing.T) {
	var gotPath string
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("character-cost", "1")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("X"))
	})
	h.post(t, streamBody, nil)

	// 打错端点（少 /stream）时上游照样 200，只是整段返回 —— 这种人畜无害的
	// 错误最难发现，所以把路径钉死
	if !strings.HasSuffix(gotPath, "/stream") {
		t.Fatalf("必须打 /text-to-speech/{id}/stream，实际路径 %q", gotPath)
	}
}

func TestStreamSSERejectsNonMP3(t *testing.T) {
	h := newHarness(t, nil, nil)
	rec := h.post(t, `{"model":"eleven_multilingual_v2","voice":"21m00Tcm4TlvDq8ikWAM",`+
		`"input":"你好","response_format":"wav","stream_format":"sse"}`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("WAV+SSE 必须 400，得到 %d", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("拒绝时不能返回 SSE")
	}
}

func TestStreamSSEUpstreamErrorIsJSON(t *testing.T) {
	h := newHarness(t, nil, streamingUpstream(nil, http.StatusUnauthorized, ""))
	rec := h.post(t, streamBody, nil)

	// 关键：还没写 SSE 之前就发现上游 401，必须能正常返回 JSON 错误。
	// 一旦先 WriteHeader(200) 再发现错误，客户只会看到一个空流。
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("上游 401 应映射为 401，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("错误响应不能是 SSE")
	}
	row := h.lastRow(t)
	if status, _ := row["status"].(float64); int(status) != 401 {
		t.Fatalf("账本应记 401，得到 %v", row["status"])
	}
	if row["up_credits"] != nil {
		t.Fatalf("失败请求不应记积分，得到 %v", row["up_credits"])
	}
}

func TestNonStreamLedgerMarksAudio(t *testing.T) {
	h := newHarness(t, nil, nil)
	rec := h.post(t, `{"model":"eleven_multilingual_v2","voice":"21m00Tcm4TlvDq8ikWAM",`+
		`"input":"你好世界"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	row := h.lastRow(t)
	if row["stream"] != false {
		t.Fatalf("非流式请求 stream 必须是 false，得到 %v", row["stream"])
	}
	if row["stream_format"] != "audio" {
		t.Fatalf("非流式请求 stream_format 必须是 audio，得到 %v", row["stream_format"])
	}
	if row["model_id"] != "eleven_multilingual_v2" {
		t.Fatalf("model_id 缺失或不对：%v", row["model_id"])
	}
}

func TestStreamTrueWithoutFormatIsMarkedIgnored(t *testing.T) {
	h := newHarness(t, nil, nil)
	rec := h.post(t, `{"model":"eleven_multilingual_v2","voice":"21m00Tcm4TlvDq8ikWAM",`+
		`"input":"你好","stream":true}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("stream:true 应被忽略并正常返回，得到 %d", rec.Code)
	}
	if rec.Header().Get("X-Stream-Ignored") != "true" {
		t.Fatalf("必须回 X-Stream-Ignored:true，否则客户以为自己在流式")
	}
	row := h.lastRow(t)
	if row["stream_ignored"] != true {
		t.Fatalf("账本必须记 stream_ignored=true，得到 %v", row["stream_ignored"])
	}
	if row["stream"] != false {
		t.Fatalf("没有 stream_format=sse 就不算流式，得到 %v", row["stream"])
	}
}

// 上游中途断开时必须发 event: error —— 与 Python 版同名同载荷。
//
// 这条专门钉事件名：Go 版最初写的是 `speech.audio.error`，Python 版是 `error`。
// 名字不一致时，客户从 Python 切到 Go 不会报错，只会把半截音频当完整结果播出去 ——
// 属于「静默降级」里最难查的一类。
func TestStreamBrokenUpstreamEmitsPythonCompatibleError(t *testing.T) {
	h := newHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		// 声称 100 字节却只写 3 字节：客户端读流时拿到 unexpected EOF，
		// 正好模拟「上游写到一半断线」
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("AAA"))
	})
	rec := h.post(t, streamBody, nil)
	body := rec.Body.String()

	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("上游中断必须发 event: error，实际：%s", body)
	}
	if strings.Contains(body, "speech.audio.error") {
		t.Fatalf("不能自创 speech.audio.error：Python 版是 error，客户换实现会漏判失败")
	}
	if !strings.Contains(body, `"code":"upstream_error"`) {
		t.Fatalf("error 载荷缺 code=upstream_error：%s", body)
	}

	row := h.lastRow(t)
	if status, _ := row["status"].(float64); int(status) != 502 {
		t.Fatalf("上游中断应记 502，得到 %v", row["status"])
	}
}

// done 事件必须带 usage，且与 Python 版同构（客户端按 OpenAI TTS 习惯读它）
func TestStreamDoneCarriesUsage(t *testing.T) {
	h := newHarness(t, nil, streamingUpstream([]string{"AAA"}, 200, "3"))
	rec := h.post(t, streamBody, nil)
	events, _, _ := decodeSSE(t, rec.Body.String())

	var done map[string]any
	for _, e := range events {
		if e["type"] == "speech.audio.done" {
			done = e
		}
	}
	if done == nil {
		t.Fatalf("缺少 done 事件")
	}
	usage, ok := done["usage"].(map[string]any)
	if !ok {
		t.Fatalf("done 事件缺 usage：%v", done)
	}
	// "你好世界" = 4 个字符；Python 版把原始字符数放进 input_tokens/total_tokens
	if n, _ := usage["input_tokens"].(float64); int(n) != 4 {
		t.Fatalf("input_tokens 应为 4，得到 %v", usage["input_tokens"])
	}
	if n, _ := usage["total_tokens"].(float64); int(n) != 4 {
		t.Fatalf("total_tokens 应为 4，得到 %v", usage["total_tokens"])
	}
}
