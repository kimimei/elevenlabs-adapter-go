package httpapi

// ElevenLabs 独占能力端点：字符级时间戳、原生多角色对白、强制对齐、IVC 克隆。
//
// 为什么单独一层：这些端点的请求/响应形状都不是 OpenAI 兼容的，
// 不能塞进 /v1/audio/speech 的 handler，但**计费、账本、红线必须同源**：
//   1. 字符数一律 utf8.RuneCountInString（len() 是字节，中文差 3 倍）；
//   2. 中文绝不进 HTTP 头；
//   3. 任何失败都不重试（上游合成完才返回，重试 = 再扣一次钱）；
//   4. MOCK 返回确定性夹具并显式标注，绝不把夹具伪装成真实 alignment。
//
// 真实上游调用路径已经具备，但本轮**未发起任何真实 ElevenLabs 调用**：
// /v1/capabilities 里 verification 明确写 mock-only。要验证真实能力，
// 需要单独批准一次小样本调用。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
	"github.com/kimimei/elevenlabs-adapter-go/internal/pcm"
)

const capabilityVersion = "go-capabilities-20261113.1"

var capabilityKinds = []string{
	"with-timestamps", "text-to-dialogue", "forced-alignment", "voices-add",
}

// handleCapabilities 声明 Go 侧真实具备的能力与验证边界。
func (a *App) handleCapabilities(c *gin.Context) {
	c.PureJSON(http.StatusOK, gin.H{
		"layer":        "go-adapter-capabilities",
		"version":      capabilityVersion,
		"enabled":      a.Cfg.CapabilitiesEnabled,
		"mock":         a.Cfg.Mock,
		"verification": "mock-only; the upstream call paths are implemented but NO live ElevenLabs request has been issued in this environment",
		"endpoints": gin.H{
			"POST /v1/text-to-speech/{voice_id}/with-timestamps": "upstream /text-to-speech/{voice_id}/with-timestamps; JSON {text,...}; audio_base64 + real alignment when live",
			"POST /v1/text-to-dialogue":                          "upstream /v1/text-to-dialogue; inputs[] each with voice_id/text; single upstream call, billed by total rune count",
			"POST /v1/forced-alignment":                          "upstream /forced-alignment; multipart audio+text; billed by the text field's rune count",
			"POST /v1/voices/add":                                "upstream /voices/add (IVC); multipart samples; billed per request with 0 characters (voice creation, not synthesis)",
		},
		"billing": gin.H{
			"ledger":              "one billing.Entry per capability call, same ledger as /v1/audio/speech",
			"rune_count":          "utf8.RuneCountInString over text / concatenated inputs[].text",
			"ivc_character_count": "0 (voice creation is not text synthesis)",
			"retry":               "never, including timeouts and 5xx",
		},
		"mock_fixture": gin.H{
			"alignment": "intentionally null; never fabricated",
			"headers":   "X-Mock-Capability + X-Capability-Verified: mock-only",
		},
		"red_lines": []string{
			"no retry after timeout", "no Chinese text in HTTP headers",
			"no fabricated alignment", "no ElevenLabs master key stored on the server",
		},
	})
}

// capabilityText 提取可计费文本。
//
// 计费基数必须精确：拿不到文本就明确 400，绝不用「请求体长度」这种
// 会把 multipart 边界和音频字节一起算进去的模糊口径。
func capabilityText(kind string, raw []byte, contentType string) (string, error) {
	// IVC 是创建音色，不合成文本：按请求记一次账、字符数为 0。
	if kind == "voices-add" {
		return "", nil
	}

	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		if kind == "text-to-dialogue" {
			var inputs []struct {
				Text string `json:"text"`
			}
			if v, ok := m["inputs"]; ok && json.Unmarshal(v, &inputs) == nil && len(inputs) > 0 {
				var sb strings.Builder
				for _, it := range inputs {
					sb.WriteString(it.Text)
				}
				return sb.String(), nil
			}
			return "", errors.New("inputs[] with per-segment text is required for billing")
		}
		if v, ok := m["text"]; ok {
			var t string
			if json.Unmarshal(v, &t) == nil && t != "" {
				return t, nil
			}
		}
	}

	// forced-alignment 上游是 multipart：文本在名为 text 的 part 里。
	if strings.Contains(strings.ToLower(contentType), "multipart/form-data") {
		_, params, err := mime.ParseMediaType(contentType)
		if err == nil && params["boundary"] != "" {
			mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
			for {
				p, e := mr.NextPart()
				if e == io.EOF {
					break
				}
				if e != nil {
					return "", fmt.Errorf("multipart parse failed: %w", e)
				}
				if p.FormName() == "text" {
					b, _ := io.ReadAll(io.LimitReader(p, 2<<20))
					p.Close()
					return string(b), nil
				}
				p.Close()
			}
		}
	}
	return "", errors.New("cannot determine billable text; send JSON {text:...} or a multipart 'text' part")
}

func capabilityPath(kind, voiceID string) (string, error) {
	switch kind {
	case "with-timestamps":
		if voiceID == "" || strings.ContainsAny(voiceID, "/?#\\") || utf8.RuneCountInString(voiceID) > 64 {
			return "", errors.New("invalid voice_id in capability path")
		}
		return "/text-to-speech/" + url.PathEscape(voiceID) + "/with-timestamps", nil
	case "text-to-dialogue":
		return "/text-to-dialogue", nil
	case "forced-alignment":
		return "/forced-alignment", nil
	case "voices-add":
		return "/voices/add", nil
	}
	return "", errors.New("unknown capability kind")
}

// mockCapability 返回确定性夹具。alignment 一律 null —— 真实字符时间戳
// 只能来自真实上游，夹具绝不伪造。
func mockCapability(kind string, chars int, rid string) []byte {
	wav := pcm.Silence(1.0, 24000)
	b64 := base64.StdEncoding.EncodeToString(wav)
	reason := "MOCK fixture: real alignment/timings require a live upstream call; nothing is fabricated"
	var out gin.H
	switch kind {
	case "with-timestamps":
		out = gin.H{
			"audio_base64": b64, "alignment": nil, "normalized_alignment": nil,
			"mock": true, "reason": reason,
			"usage": gin.H{"input_characters": chars},
		}
	case "text-to-dialogue":
		out = gin.H{
			"audio_base64": b64, "mock": true, "reason": reason,
			"usage": gin.H{"input_characters": chars},
		}
	case "forced-alignment":
		out = gin.H{
			"alignment": nil, "normalized_alignment": nil,
			"mock": true, "reason": reason,
			"usage": gin.H{"input_characters": chars},
		}
	case "voices-add":
		out = gin.H{
			"voice_id": "mock-ivc-" + rid, "mock": true,
			"reason": "MOCK fixture: no voice was actually created upstream",
			"usage":  gin.H{"input_characters": 0},
		}
	default:
		out = gin.H{"mock": true, "reason": "unknown capability"}
	}
	b, _ := json.Marshal(out)
	return b
}

// capabilityHandler 生成一个能力端点 handler：统一鉴权、rune 计费、
// 零重试上游转发（或 MOCK 夹具）、一行账本、显式验证状态声明。
func (a *App) capabilityHandler(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		rid := randomID()
		setHeader(c, "X-Request-Id", rid)
		setHeader(c, "X-Capability", kind)
		setHeader(c, "X-Capability-No-Retry", "true")

		if !a.Cfg.CapabilitiesEnabled {
			writeErr(c, http.StatusNotImplemented,
				"capability endpoints are disabled (ADAPTER_CAPABILITIES_ENABLED=0)",
				"capability_unavailable", "set ADAPTER_CAPABILITIES_ENABLED=1 on the isolated instance", nil)
			return
		}

		status := http.StatusOK
		errMsg := ""
		chars := 0
		recorded := false
		tokLabel := tokenLabel(firstNonEmpty(c.GetHeader("Authorization"),
			c.GetHeader("X-Api-Key"), c.GetHeader("Api-Key")))
		record := func() {
			if recorded {
				return
			}
			recorded = true
			entry := billing.Entry{
				RID: rid, Model: "capability:" + kind, Voice: c.Param("voiceId"),
				InChars: chars, SentChars: chars, RawChars: chars, CleanedChars: chars,
				Status: status, ElapsedMS: int(time.Since(start).Milliseconds()),
				ClientIP: localAddr(c.Request.RemoteAddr), TokenName: tokLabel,
				Error: errMsg,
			}
			if err := a.Ledger.Record(entry); err != nil {
				log.Printf("capability billing write failed (request %s): %v", rid, err)
			}
		}

		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			status = http.StatusBadRequest
			errMsg = "capability request body read failed"
			record()
			writeErr(c, status, errMsg, "invalid_request_error", "check request body size", nil)
			return
		}
		text, terr := capabilityText(kind, raw, c.GetHeader("Content-Type"))
		if terr != nil {
			status = http.StatusBadRequest
			errMsg = terr.Error()
			record()
			writeErr(c, status, "cannot determine billable text: "+errMsg,
				"invalid_request_error",
				"send JSON {text:...} / inputs[].text, or a multipart part named 'text'", nil)
			return
		}
		chars = utf8.RuneCountInString(text) // 铁律：中文按 rune，不用 len()

		apiKey := a.resolveAPIKey(c.GetHeader("Authorization"),
			c.GetHeader("X-Api-Key"), c.GetHeader("Api-Key"))
		if apiKey == "" && !a.Cfg.Mock {
			status = http.StatusUnauthorized
			errMsg = "missing ElevenLabs API key for capability endpoint"
			record()
			writeErr(c, status, errMsg, "authentication_error",
				"the gateway injects a channel key; clients never send a master key", nil)
			return
		}

		suffix, serr := capabilityPath(kind, c.Param("voiceId"))
		if serr != nil {
			status = http.StatusBadRequest
			errMsg = serr.Error()
			record()
			writeErr(c, status, errMsg, "invalid_request_error", "check voice_id", nil)
			return
		}

		var body []byte
		ct := "application/json; charset=utf-8"
		if a.Cfg.Mock {
			body = mockCapability(kind, chars, rid)
			setHeader(c, "X-Mock-Capability", kind)
			setHeader(c, "X-Capability-Verified", "mock-only")
		} else {
			up, uerr := a.Upstream.CallCapability(c.Request.Context(), http.MethodPost,
				suffix, raw, c.GetHeader("Content-Type"), apiKey)
			if uerr != nil {
				status = http.StatusBadGateway
				errMsg = uerr.Error()
				record()
				writeErr(c, status, "capability upstream call failed (NO RETRY): "+errMsg,
					"upstream_error", "a timeout may already have been billed; never retry this request", nil)
				return
			}
			if up.StatusCode != http.StatusOK {
				status = normalizeStatus(up.StatusCode)
				errMsg = truncateRunes(string(up.Body), 200)
			}
			body = up.Body
			if v := up.Header.Get("Content-Type"); v != "" {
				ct = v
			}
			setHeader(c, "X-Capability-Verified", "upstream-live-path")
		}

		setHeader(c, "X-Usage-Input-Characters", strconv.Itoa(chars))
		setHeader(c, "X-Usage-Source", "capability-rune-count")
		record()
		c.Data(status, ct, body)
	}
}
