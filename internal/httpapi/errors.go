// Package httpapi 是 OpenAI 兼容的 HTTP 层。
//
// 这一层与 Python 版一一对应（main.py + demo.py + jobs.py 的同步部分），
// 但刻意做得「薄」：所有判断都在这里，所有计算都在 internal/* 里 ——
// 将来走 B 路线（内嵌 new-api 渠道）时，这块整层替换，下面不动。
package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/apierr"
)

// writeErr 输出 OpenAI 错误结构，并带上指定响应头。
//
// 用 PureJSON 而不是 JSON：gin 的 JSON 会做 HTML 转义（& < > 变成 \u0026 等），
// Python 的 ensure_ascii=False 不转义。客户拿响应体做字符串匹配时会发现差异。
func writeErr(c *gin.Context, status int, message, code, fix string, headers map[string]string) {
	for k, v := range headers {
		setHeader(c, k, v)
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(status)
	c.Writer.Write(apierr.Encode(message, code, fix))
}

// addHeaders 批量设置响应头。
func addHeaders(c *gin.Context, headers map[string]string) {
	for k, v := range headers {
		setHeader(c, k, v)
	}
}

// setHeader 直接写入底层 header map，绕过 Go 的 canonical 化。
//
// 为什么较真：gin 的 c.Header() 走 http.Header.Set()，会把键名 canonical 化，
// `X-ElevenLabs-Voice` 变成 `X-Elevenlabs-Voice`。HTTP 头本大小写不敏感，
// 客户端用 Header.Get() 读不受影响；但 M6 影子对账要拿两边逐头 diff，
// 大小写不一致会让 22 个头全部显示为「不同」，噪声淹没真差异。
func setHeader(c *gin.Context, key, value string) {
	c.Writer.Header()[key] = []string{value}
}

// parseErr 是请求体校验失败，携带足够信息直接回 400。
type parseErr struct {
	field   string
	message string
	fix     string
}

func (p *parseErr) Error() string { return p.field + ": " + p.message }

// validationFix 与 Python 版一致的通用修复提示。
const validationFix = "检查请求体字段类型：input 必须是非空字符串，" +
	"speed 需在 0.25–4.0，stream 需为布尔值"

// isBodyTooLarge 判断 gin/http 的错误是否来自 MaxBytesReader。
func isBodyTooLarge(err error) bool {
	if err == nil {
		return false
	}
	if errorsAsMaxBytes(err) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}

func errorsAsMaxBytes(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// hasHeader 判断 Content-Type 是否为 JSON（gin 的 ShouldBindJSON 不校验这个）。
func isJSONContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	return strings.HasPrefix(ct, "application/json")
}

// statusText 便于日志。
func statusText(code int) string {
	if t := http.StatusText(code); t != "" {
		return t
	}
	return "unknown"
}
