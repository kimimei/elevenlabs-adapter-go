// Package apierr 统一 OpenAI 错误结构，并把上游原文翻译成「下一步改哪一行」。
//
// 原则：
//   - message 给机器/日志看（保持 OpenAI 结构不变）
//   - fix 给人看（一句话，动词开头，指向具体操作）
//   - 宁可说「去确认 X」，也不要猜一个可能错的结论
//
// 这个包不依赖任何 HTTP 框架：B 路线（内嵌 new-api 渠道）时可以直接复用。
package apierr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Err 是 OpenAI 错误体的内层结构。fix 是纯增量字段，老客户端只读 message，不受影响。
type Err struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Fix     string `json:"fix,omitempty"`
}

// Envelope 是 {"error": {...}}。
type Envelope struct {
	Error Err `json:"error"`
}

// Encode 序列化为响应体。ensure_ascii=False 的等价行为：中文原样输出。
//
// 必须关掉 HTML 转义：`json.Marshal` 会把 < > & 转成 \u003c / \u003e / \u0026，
// 于是 `'eleven-<name>' 形式的音色别名` 到了客户眼里变成
// `eleven-\u003cname\u003e`。Python 版不转义 —— 这不是排版问题，
// 是客户照着这句话去改配置时根本认不出那个占位符（M6 比对实测出来的）。
func Encode(message, code, fix string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Envelope{Error: Err{Message: message, Type: code, Code: code, Fix: fix}}); err != nil {
		return []byte(`{"error":{"message":"internal error","type":"internal_error","code":"internal_error"}}`)
	}
	// Encoder 会在末尾补一个 \n，响应体会平白多一字节。
	// 按字节比对时这就是一条差异，而且看上去像是「多了个换行」这种无意义的差异，
	// 排查时最容易被人当成噪声忽略掉。
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// 状态码兜底：上游没给可读信息时用。
var byStatus = map[int][2]string{
	400: {"请求参数不合法", "检查 model 是否为 eleven_* 引擎名、response_format 是否在 mp3/opus/wav/pcm 之内"},
	401: {"上游 Key 失效或未提供", "渠道密钥必须是 ElevenLabs 的 sk_ 开头 Key（不是中转站的 sk- 令牌）；到 ElevenLabs 后台重新复制一份，注意别带空格"},
	402: {"上游账户额度不足", "ElevenLabs 账户余额或套餐额度用尽；充值或换 Key 后重试"},
	403: {"上游拒绝访问", "该 Key 没有对应能力权限（如语音克隆需额外授权），或账户被限制"},
	404: {"上游找不到该资源", "voice_id 不存在或不属于该账户；先 GET /v1/models 与 /v1/voices 确认名字"},
	422: {"上游参数校验失败", "多为 voice_settings 或 output_format 不被该引擎支持；注意 eleven_v4 不接受 language_code=zh-TW"},
	429: {"上游并发或频率超限", "ElevenLabs 按套餐限制并发；降低并发、加退避重试，或升级套餐"},
	500: {"上游内部错误", "稍后重试；连续出现则查 ElevenLabs 状态页"},
	501: {"适配器不支持该功能", "去掉 stream=true（流式暂未实现）"},
	502: {"上游网络错误", "检查服务器出网与 DNS；重试一次"},
}

// 上游原文里的关键片段 -> 更精确的解释（优先于状态码兜底）。
var patterns = []struct {
	re      *regexp.Regexp
	message string
	fix     string
}{
	{regexp.MustCompile(`(?i)language_code|language code`),
		"该引擎不支持这个 language_code",
		"eleven_v4 只认 zh，不认 zh-TW；把 language 改成 zh，或改用 eleven_v3"},
	{regexp.MustCompile(`(?i)voice.*(not found|does not exist)|invalid.*voice`),
		"voice_id 不存在",
		"音色名写错，或该音色属于别人账户；用 /v1/voices 里的 voice_id 重试"},
	{regexp.MustCompile(`(?i)model.*(not found|does not exist|not supported)`),
		"模型不存在或当前账户不可用",
		"核对模型名（eleven_v3 / eleven_v4 / eleven_multilingual_v2 / eleven_flash_v2_5 / eleven_turbo_v2_5）"},
	{regexp.MustCompile(`(?i)quota|credit|insufficient`),
		"账户额度不足",
		"充值或换用其它 Key；在 ElevenLabs 后台查看剩余 credits"},
	{regexp.MustCompile(`(?i)concurren|rate limit|too many requests`),
		"并发超限",
		"降低并发数；批量任务请串行或加 1-3 秒间隔"},
	{regexp.MustCompile(`(?i)text.*(too long|exceed)|max.*characters`),
		"文本超过单次上限",
		"按 2000 字左右分段提交，再拼接音频"},
	{regexp.MustCompile(`(?i)permission|scope|unauthorized`),
		"Key 权限不足",
		"该 Key 缺少对应 API 权限；用具备 TTS 权限的 Key"},
}

// Explain 返回 (message, fix)。永不 panic。
func Explain(status int, upstreamText string) (string, string) {
	text := upstreamText
	if r := []rune(text); len(r) > 400 {
		text = string(r[:400])
	}
	for _, p := range patterns {
		if p.re.MatchString(text) {
			return p.message, p.fix
		}
	}
	if v, ok := byStatus[status]; ok {
		return v[0], v[1]
	}
	return fmt.Sprintf("上游返回 HTTP %d", status), "把这段错误原文发给服务方，附上 request_id"
}

// JoinLoc 把校验错误拼成 "字段: 说明"，与 Python 版的消息形态一致。
func JoinLoc(field, msg string) string {
	if field == "" {
		return msg
	}
	return strings.TrimSpace(field) + ": " + msg
}
