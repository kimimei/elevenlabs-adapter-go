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
