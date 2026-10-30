// Package textclean 在送上游合成前，去掉不影响朗读语义的字符。
//
// 设计铁律：默认只做「无损」清理。语义是否一致无法替客户担保，
// 所以默认档位只碰那些「删掉后人耳绝对听不出、意思绝对不变」的字符。
//
// 计费口径：清理本身不改变计费基数。默认 input 口径下，
// 清理掉的字符照样计入客户账单，但上游只收清理后的合成量 ——
// 那部分是上游成本节省，不是计费折扣。
//
// ⚠️ 与 Python 版的唯一实质差异：Go 的 regexp 是 RE2，**不支持反向引用**。
// Python 里的 `(\*\*|__)(.*?)\1` / `(\*|_)(.*?)\1` 必须拆成两条独立规则。
// 拆分后语义等价（见 clean_test.go 的对照用例），但规则条数从 9 条变 11 条。
package textclean

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// 清理档位
const (
	ModeOff       = "off"
	ModeSafe      = "safe"
	ModeNormalize = "normalize"
)

// 档位强弱顺序。请求级参数只允许「更强」，不允许「更弱」——
// 否则运维设了 safe 想兜底，客户传 off 就绕过去了。
var rank = map[string]int{ModeOff: 0, ModeSafe: 1, ModeNormalize: 2}

// Report 是清理报告，会同时写进账本与响应头。
type Report struct {
	Removed int      `json:"removed"`
	Rules   []string `json:"rules"`
	RawLen  int      `json:"raw_len"`
}

// 连续水平空白（半角空格、制表符、全角空格）折叠为单个半角空格。
// 朗读上完全等效，但能省掉一堆计费字符。
var hspaceRun = regexp.MustCompile("[ \t\u3000]{2,}")

// Markdown 语法标记（仅 normalize 档启用）。
//
// 注意第 4–7 条：Python 原版用反向引用 `(\*\*|__)(.*?)\1`，RE2 不支持，
// 因此拆成 **、__、*、_ 四条独立规则，顺序必须保持「先粗体后斜体」——
// 反过来会把 `**粗体**` 先按单星号吃掉一层，剩下的 `*` 留在正文里。
var mdRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`), "${1}"}, // 图片 -> alt
	{regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`), "${1}"},  // 链接 -> 文字
	{regexp.MustCompile(`(?m)^#{1,6}\s*`), ""},             // 标题井号
	{regexp.MustCompile(`\*\*(.*?)\*\*`), "${1}"},          // 粗体（*）
	{regexp.MustCompile(`__(.*?)__`), "${1}"},              // 粗体（_）
	{regexp.MustCompile(`\*(.*?)\*`), "${1}"},              // 斜体（*）
	{regexp.MustCompile(`_(.*?)_`), "${1}"},                // 斜体（_）
	{regexp.MustCompile("`{1,3}([^`]*)`{1,3}"), "${1}"},    // 行内代码
	{regexp.MustCompile(`(?m)^\s*[-*+]\s+`), ""},           // 无序列表符号
	{regexp.MustCompile(`(?m)^\s*>\s?`), ""},               // 引用符号
	{regexp.MustCompile(`(?m)^\s*[-*_]{3,}\s*$`), ""},      // 分隔线
}

// ResolveMode 决定本次请求实际使用哪个档位：取两者中更强的那个。
//
// 客户可以主动加强（safe -> normalize），但不能削弱运维设定的下限。
// 无法识别的值退回默认值，不报错 —— 为一个可选参数把请求打挂不值得。
func ResolveMode(requested, def string) string {
	if requested == "" {
		return def
	}
	req := strings.ToLower(strings.TrimSpace(requested))
	r, ok := rank[req]
	if !ok {
		return def
	}
	defRank, known := rank[def]
	if !known {
		defRank = 0 // 与 Python 的 _RANK.get(default, 0) 对齐
	}
	if r > defRank {
		return req
	}
	return def
}

// Clean 清理文本，返回 (结果, 报告)。
func Clean(text, mode string) (string, Report) {
	raw := text

	if mode == ModeOff {
		// 即便 off 也要规范化换行与首尾空白：上游对 \r\n 与尾随换行处理不一致
		out := strings.TrimSpace(NormalizeNewlines(raw))
		return out, Report{Removed: RuneLen(raw) - RuneLen(out), Rules: []string{"strip"}, RawLen: RuneLen(raw)}
	}

	rules := []string{}
	out := NormalizeNewlines(raw)

	before := RuneLen(out)
	out = strings.Map(func(r rune) rune {
		if isInvisible(r) {
			return -1
		}
		return r
	}, out)
	if RuneLen(out) != before {
		rules = append(rules, "invisible")
	}

	if mode == ModeNormalize {
		for _, rule := range mdRules {
			out = rule.re.ReplaceAllString(out, rule.repl)
		}
		rules = append(rules, "markdown")
	}

	before = RuneLen(out)
	out = hspaceRun.ReplaceAllString(out, " ")
	if RuneLen(out) != before {
		rules = append(rules, "collapse_space")
	}

	before = RuneLen(out)
	out = strings.TrimSpace(out)
	if RuneLen(out) != before {
		rules = append(rules, "strip")
	}

	return out, Report{Removed: RuneLen(raw) - RuneLen(out), Rules: rules, RawLen: RuneLen(raw)}
}

// Describe 把报告压成一行，用于响应头与日志。
func Describe(r Report) string {
	if r.Removed == 0 {
		return "none"
	}
	return itoa(r.Removed) + " chars removed by " + strings.Join(r.Rules, ",")
}

// NormalizeNewlines 把 \r\n / \r 统一为 \n。
// 上游对混合换行的停顿处理不稳定，统一后不同来源的文本表现一致。
func NormalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// isInvisible 判断是否为「删掉完全无感」的字符。
//
// 只收 U+200B(零宽空格) / U+2060(词连接符) / U+FEFF(BOM)，刻意不碰
// U+200C(ZWNJ) 与 U+200D(ZWJ) —— 它们参与 emoji 组合与部分语言的连写规则，
// 删掉会把 👨‍👩‍👧 拆成三个独立 emoji，语义直接崩坏。
func isInvisible(r rune) bool {
	switch r {
	case 0x200B, 0x2060, 0xFEFF:
		return true
	}
	if r < 0x09 {
		return true
	}
	if r == 0x0B || r == 0x0C || r == 0x7F {
		return true
	}
	return r >= 0x0E && r <= 0x1F
}

// RuneLen 是计费口径的字符数：Unicode 字符数，不是字节数。
//
// ⚠️ 全仓禁止用 len(s) 数字符。Go 的 len 返回字节数，中文一个字 3 字节，
// 用错会让 bill_chars 直接差 3 倍 —— 而且只在中文流量上发生。
func RuneLen(s string) int {
	return utf8.RuneCountInString(s)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
