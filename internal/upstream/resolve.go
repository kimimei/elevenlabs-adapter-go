// Package upstream 负责与 ElevenLabs 交互：请求转换、鉴权转换、调用与重试。
//
// 这一层刻意不依赖任何 HTTP 框架，只吃 context + 结构化参数 ——
// B 路线（内嵌 new-api 渠道）时可以整段搬过去，只换一层调用壳。
package upstream

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/kimimei/elevenlabs-adapter-go/internal/voices"
)

// Format 是一个 response_format 对应的上游输出格式与下游 Content-Type。
type Format struct {
	Output      string
	ContentType string
}

// Formats 是 OpenAI response_format -> ElevenLabs output_format 的映射。
//
// ElevenLabs 枚举无 aac/flac，故这两项显式留空：宁可在入口报 400，
// 也不静默降级成 mp3 —— 客户按 mp3 写死了后续处理流程，降级比报错更难查。
var Formats = map[string]*Format{
	"mp3":  {"mp3_44100_128", "audio/mpeg"},
	"opus": {"opus_48000_128", "audio/ogg"},
	"wav":  {"pcm_24000", "audio/wav"},
	"pcm":  {"pcm_24000", "audio/L16; rate=24000"},
	"aac":  nil,
	"flac": nil,
}

// voice_id 会被拼进上游 URL 路径，必须严格校验：
// 一旦允许 "/"、".." 之类的字符，调用方就能把请求指到 api.elevenlabs.io 的任意路径上，
// 且用的是服务端配置的 Key（路径穿越 / 越权调用）。
var voiceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{10,64}$`)

// ElevenLabs voice_settings.speed 的实际接受区间。
const (
	SpeedMin = 0.7
	SpeedMax = 1.2
)

// Extra 是预设别名携带的默认配置。
type Extra struct {
	Lang          string
	VoiceSettings map[string]any
}

// ClampSpeed 把 OpenAI 语速区间(0.25–4.0)钳制到 ElevenLabs 可接受区间(0.7–1.2)。
// 返回 (实际下发值, 是否发生钳制)。上游对越界值直接报 400，
// 故此处不报错而是钳制，并通过响应头告知调用方。
func ClampSpeed(speed *float64) (*float64, bool) {
	if speed == nil {
		return nil, false
	}
	v := *speed
	if v < SpeedMin {
		out := SpeedMin
		return &out, true
	}
	if v > SpeedMax {
		out := SpeedMax
		return &out, true
	}
	return &v, false
}

// checkVoiceID voice_id 会被拼进上游 URL 路径，任何来源都必须过这一关。
func checkVoiceID(voiceID, source string) (string, error) {
	if voiceID != "" && !voiceIDRe.MatchString(voiceID) {
		shown := voiceID
		if len(shown) > 40 {
			shown = shown[:40]
		}
		// 收到值用单引号包（Python 的 %r 就是这样）。客户在两个版本间切换时，
		// 报错文本应当是同一句，不该因为「换了语言」而换个排版。
		return "", fmt.Errorf(
			"voice_id 不合法（来源：%s）：只允许字母、数字、下划线、连字符（10–64 位）。收到：'%s'",
			source, shown)
	}
	return voiceID, nil
}

// ResolveVoiceAndModel 返回 (voice_id, model_id, extra)。
// voice_id 可能为空（此时调用方应报 400）。
//
// 判定顺序：
//  1. body.voice 显式指定（音色名或 voice_id）
//  2. model 命中 aliases（模型名即音色，可含中文预设）
//  3. model 本身是 ElevenLabs 模型名（eleven_*）
//  4. model 命中 model_alias（OpenAI 兼容名）
func ResolveVoiceAndModel(model, voice string, cat *voices.Catalog, defaultModel string) (string, string, Extra, error) {
	var voiceID string
	var err error
	if voice != "" {
		voiceID, err = checkVoiceID(cat.VoiceToID(voice), "body.voice")
		if err != nil {
			return "", "", Extra{}, err
		}
	}

	if spec, ok := cat.Aliases[model]; ok {
		extra := Extra{Lang: spec.Lang, VoiceSettings: spec.VoiceSettings}
		picked := voiceID
		if picked == "" {
			picked = spec.VoiceID
		}
		// 别名分支提前返回，voice_id 必须在这里再校验一次。
		// voices.json 是仓内文件但可编辑 —— 一旦有人写进一个带斜杠的 voice_id，
		// 这条路径就成了绕过 body.voice 校验的口子。
		picked, err = checkVoiceID(picked, "aliases["+model+"]")
		if err != nil {
			return "", "", Extra{}, err
		}
		mid := spec.Model
		if mid == "" {
			mid = defaultModel
		}
		return picked, mid, extra, nil
	}

	if strings.HasPrefix(model, "eleven_") || model == "elevenlabs-tts" {
		return voiceID, model, Extra{}, nil
	}

	mid := defaultModel
	if m, ok := cat.ModelAlias[model]; ok {
		mid = m
	}
	return voiceID, mid, Extra{}, nil
}

// TTSURL 组装上游合成地址。voice_id 再 quote 一次作为双保险：
// 即使上游校验被绕过，也不让 voice_id 改写路径。
func TTSURL(base, voiceID string) string {
	return base + "/text-to-speech/" + url.PathEscape(voiceID)
}

// BuildPayload 组装上游请求体。
func BuildPayload(text, modelID, outputFormat string, speed *float64, extra Extra) map[string]any {
	payload := map[string]any{
		"text":          text,
		"model_id":      modelID,
		"output_format": outputFormat,
	}
	if extra.Lang != "" {
		payload["language"] = extra.Lang
	}
	settings := map[string]any{}
	for k, v := range extra.VoiceSettings {
		settings[k] = v
	}
	if clamped, _ := ClampSpeed(speed); clamped != nil {
		settings["speed"] = *clamped
	}
	if len(settings) > 0 {
		payload["voice_settings"] = settings
	}
	return payload
}

// ExtractCharacterCost 读上游回传的 character-cost —— 这是上游实际扣掉的额度，不是我们估的。
//
// 实测它与输入长度的关系是 cost = floor(len(text)/2)（中英文都一样），
// 即上游按半字符计费。自己用 len(input) 估会多报一倍，客户按这个数被扣钱就是双倍收费。
// 拿不到（老版本上游 / 代理剥头 / MOCK）时返回 nil，由调用方决定兜底。
func ExtractCharacterCost(h http.Header) *int {
	raw := strings.TrimSpace(h.Get("character-cost"))
	if raw == "" {
		return nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	n := int(f)
	if n < 0 {
		return nil
	}
	return &n
}
