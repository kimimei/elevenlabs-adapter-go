package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/apierr"
	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
	"github.com/kimimei/elevenlabs-adapter-go/internal/pcm"
	"github.com/kimimei/elevenlabs-adapter-go/internal/upstream"
)

// streamReq 是流式路径需要的一切。用结构体而不是十几个参数，
// 是因为这个 handler 后面还会加（比如按档位限速），参数列表已经开始失控。
type streamReq struct {
	RID, Text, ModelID, VoiceID, APIKey string
	OutputFormat                        string
	Speed                               *float64
	Extra                               upstream.Extra

	InChars, SentChars, RawChars int
	ModelName, VoiceName         string
	ClientIP, TokenName          string
	CleanMode                    string
	Removed                      int
	SpeedUsed                    *float64
	SpeedClamped                 bool
	Mock                         bool

	Record func(status int, upCredits *int, upRID *string, errMsg string) map[string]string
}

type deltaEvent struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

// sseFrame 组装一个 SSE 事件。
//
// 事件名（event:）和 JSON 里的 type 两处都写：经 new-api 转发时网关会把
// event: 行剥掉只留 data:，只写 type 的话直连客户端会丢事件名，只写 event:
// 的话经网关又全丢 —— 两边都写才同时活得下去。
func sseFrame(event string, payload any) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		// payload 全是本文件构造的简单结构，序列化失败属于程序员错误。
		// 退化成最小可用事件，绝不 panic 把整个连接打断。
		body = []byte(`{"type":"` + event + `","error":"encode_failed"}`)
	}
	out := make([]byte, 0, len(body)+len(event)+24)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, body...)
	out = append(out, '\n', '\n')
	return out
}

// handleSpeechStream 处理 stream_format=sse。
//
// 与 Python 版保持同一套语义（客户在两个实现之间切换时行为必须一致）：
//   - 先打上游拿响应头，**非 200 时还能正常返回 JSON 错误**；一旦开始写 SSE，
//     状态码就固定成 200 了，再出错只能发 error 事件；
//   - 账本记 200（正常结束）/ 499（客户端断开）/ 502（上游中断）；
//   - 成功时记上游真实积分，失败记 nil —— 绝不拿输入字符数冒充。
func (a *App) handleSpeechStream(c *gin.Context, r streamReq) {
	// 先把能算的头算完：SSE 一旦开始刷字节，Header 就改不动了
	billN, billSrc := billing.BillCharsByMode(a.Cfg.BillingMode, r.InChars, r.SentChars, nil, 200)
	setHeader(c, "X-Request-Id", r.RID)
	setHeader(c, "X-ElevenLabs-Model", r.ModelID)
	setHeader(c, "X-ElevenLabs-Voice", r.VoiceID)
	setHeader(c, "X-Usage-Input-Characters", strconv.Itoa(billN))
	setHeader(c, "X-Usage-Source", billSrc)
	setHeader(c, "X-Text-Clean-Mode", r.CleanMode)
	if r.Removed > 0 {
		setHeader(c, "X-Text-Raw-Chars", strconv.Itoa(r.RawChars))
		setHeader(c, "X-Text-Sent-Chars", strconv.Itoa(r.SentChars))
	}
	if r.SpeedClamped && r.SpeedUsed != nil {
		setHeader(c, "X-Speed-Clamped", strconv.FormatFloat(*r.SpeedUsed, 'g', -1, 64))
	}

	var (
		upCredits *int
		upRID     *string
		src       io.Reader
	)

	if r.Mock {
		// Mock 造的是 WAV（静音），不能谎称 mp3：明写出来，别让下游以为是格式转换坏了
		setHeader(c, "X-Mock", "true")
		setHeader(c, "X-Mock-Format", "wav")
		src = &sliceReader{buf: pcm.Silence(1.0, 24000)}
	} else {
		conn, err := a.Upstream.TextToSpeechStream(c.Request.Context(), r.Text, r.ModelID,
			r.VoiceID, r.APIKey, r.OutputFormat, r.Speed, r.Extra)
		if err != nil {
			// 还没写出任何字节，可以正常返回 502 JSON
			_, fix := apierr.Explain(502, err.Error())
			log.Printf("upstream stream call failed (request %s): %v", r.RID, err)
			writeErr(c, http.StatusBadGateway, "调用 ElevenLabs 失败："+err.Error(),
				"upstream_error", fix, r.Record(502, nil, nil, err.Error()))
			return
		}
		defer conn.Close()

		if conn.StatusCode != http.StatusOK {
			status := normalizeStatus(conn.StatusCode)
			_, fix := apierr.Explain(conn.StatusCode, string(conn.ErrorBody))
			errText := truncateRunes(string(conn.ErrorBody), 200)
			log.Printf("upstream stream error (request %s): %d %s", r.RID, conn.StatusCode,
				truncateRunes(string(conn.ErrorBody), 500))
			writeErr(c, status, upstream.ErrorMessageBytes(conn.StatusCode, conn.ErrorBody),
				"upstream_error", fix, r.Record(status, nil, nil, errText))
			return
		}
		upRID = firstHeader(conn.Header, "request-id", "x-trace-id")
		upCredits = upstream.ExtractCharacterCost(conn.Header)
		if upCredits != nil {
			setHeader(c, "X-Usage-Upstream-Credits", strconv.Itoa(*upCredits))
		}
		src = conn.Body
	}

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	// 别让反向代理给这个响应套 gzip 或攒包，否则每一块都会被压着等
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.WriteHeaderNow()

	w := c.Writer
	buf := make([]byte, 32*1024)
	outcome := 200
	failure := ""

	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			frame := sseFrame("speech.audio.delta", deltaEvent{
				Type:  "speech.audio.delta",
				Audio: base64.StdEncoding.EncodeToString(buf[:n]),
			})
			if _, werr := w.Write(frame); werr != nil {
				// 客户端断开：这是最常见的情况，不是服务端错误，记 499 而不是 502
				outcome, failure = 499, "客户端断开流式连接"
				break
			}
			w.Flush()
		}
		if rerr == io.EOF {
			// done 带上 usage，与 Python 版的载荷逐字段一致：客户端在两种实现之间
			// 切换时，按 OpenAI TTS 习惯读 usage 的代码不该突然拿到空对象。
			w.Write(sseFrame("speech.audio.done", map[string]any{
				"type": "speech.audio.done",
				"usage": map[string]any{
					"input_tokens":        r.RawChars,
					"output_tokens":       0,
					"total_tokens":        r.RawChars,
					"input_token_details": map[string]any{"text_tokens": r.RawChars, "audio_tokens": 0},
				},
			}))
			w.Flush()
			break
		}
		if rerr != nil {
			outcome = 502
			failure = "上游流中断：" + rerr.Error()
			log.Printf("upstream stream broken (request %s): %v", r.RID, rerr)
			// 事件名用 "error" 而不是 "speech.audio.error"：Python 版就是 "error"，
			// 客户换实现时不该因为事件名不同而漏判失败。详细原因进日志，不进响应体
			// （与 Python 一致：对端只需知道"这段音频不完整"）。
			w.Write(sseFrame("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"message": "上游音频流中断", "type": "upstream_error", "code": "upstream_error"},
			}))
			w.Flush()
			break
		}
	}

	r.Record(outcome, upCredits, upRID, failure)
}

// sliceReader 把一段内存包成 io.Reader，仅供 Mock 路径使用。
// 用它而不是 bytes.NewReader，是为了让「Mock 也走同一条读循环」这件事更显式。
type sliceReader struct {
	buf []byte
	pos int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.buf) {
		return 0, io.EOF
	}
	end := s.pos + len(p)
	if end > len(s.buf) {
		end = len(s.buf)
	}
	n := copy(p, s.buf[s.pos:end])
	s.pos += n
	return n, nil
}
