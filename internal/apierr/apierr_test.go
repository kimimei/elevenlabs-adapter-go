package apierr

import (
	"encoding/json"
	"strings"
	"testing"
)

// Encode 必须与 Python 的 json.dumps(ensure_ascii=False) 行为一致。
//
// 这条测试是从 M6 影子对账里长出来的：Go 的 json.Marshal 默认把 < > & 转成
// \u003c / \u003e / \u0026，于是错误提示里的 `'eleven-<name>'` 到了客户眼里
// 变成 `eleven-\u003cname\u003e`，照着这句话去改配置根本认不出那个占位符。
// Python 版不转义 —— 客户在两周里会看到两种不同的报错，取决于打到哪个版本。
func TestEncodeKeepsAngleBracketsLiteral(t *testing.T) {
	body := Encode("把模型配置为 'eleven-<name>' 形式的别名，A & B", "invalid_request_error", "")
	s := string(body)
	if strings.Contains(s, `\u003c`) || strings.Contains(s, `\u0026`) {
		t.Fatalf("尖括号/与号被 HTML 转义了，客户会看到 \\u003c 这种字面量：%s", s)
	}
	if !strings.Contains(s, "<name>") {
		t.Fatalf("应保留原文 <name>，实际：%s", s)
	}
}

// 中文必须原样输出，不能变成 \u4f60\u597d。
func TestEncodeKeepsChineseLiteral(t *testing.T) {
	s := string(Encode("中文错误", "invalid_request_error", "改这一行"))
	if !strings.Contains(s, "中文错误") {
		t.Fatalf("中文被转义了：%s", s)
	}
}

// 结尾不能多一个换行：Encoder.Encode 会自动补 \n，
// 而 Python 的 json.dumps 不补。按字节比对时这一字节会被当成差异。
func TestEncodeHasNoTrailingNewline(t *testing.T) {
	if got := Encode("x", "invalid_request_error", ""); got[len(got)-1] == '\n' {
		t.Fatalf("响应体结尾多了换行：%q", got)
	}
}

// 结构必须能被标准 JSON 解析，且 error.code / error.type 都等于传入的 code
// （OpenAI 客户端两种读法都有）。
func TestEncodeStructure(t *testing.T) {
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
			Fix     string `json:"fix"`
		} `json:"error"`
	}
	if err := json.Unmarshal(Encode("msg", "rate_limit_error", "等 3 秒"), &out); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if out.Error.Code != "rate_limit_error" || out.Error.Type != "rate_limit_error" {
		t.Fatalf("type/code 不一致: %+v", out.Error)
	}
	if out.Error.Fix != "等 3 秒" {
		t.Fatalf("fix 丢了: %+v", out.Error)
	}
}
