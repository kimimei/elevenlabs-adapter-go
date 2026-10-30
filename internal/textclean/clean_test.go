package textclean

import "testing"

func TestCleanSafeIsLossless(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		removed int
		rules   []string
	}{
		{"纯中文不动", "你好世界", "你好世界", 0, nil},
		{"首尾空白被 strip", "  你好  ", "你好", 4, []string{"collapse_space", "strip"}},
		{"零宽字符被删", "你\u200b好\u200b世界", "你好世界", 2, []string{"invisible"}},
		{"BOM 被删", "\ufeff你好", "你好", 1, []string{"invisible"}},
		// CRLF 归一为 LF：长度少 1，但换行规范化不是「规则」，
		// 所以 rules 为空、removed 记 1 —— 与 Python 版的 len(raw)-len(out) 一致
		{"CRLF 归一", "第一段\r\n第二段", "第一段\n第二段", 1, nil},
		{"制表符连续折叠", "a\t\t\tb", "a b", 2, []string{"collapse_space"}},
		{"全角空格折叠", "a\u3000\u3000b", "a b", 1, []string{"collapse_space"}},
		{"控制字符被删", "a\x00\x01b", "ab", 2, []string{"invisible"}},
		{"单个空格不动", "a b", "a b", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, report := Clean(tc.in, ModeSafe)
			if got != tc.want {
				t.Fatalf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if report.Removed != tc.removed {
				t.Fatalf("Removed = %d, want %d", report.Removed, tc.removed)
			}
			if len(report.Rules) != len(tc.rules) {
				t.Fatalf("Rules = %v, want %v", report.Rules, tc.rules)
			}
			for i := range tc.rules {
				if report.Rules[i] != tc.rules[i] {
					t.Fatalf("Rules = %v, want %v", report.Rules, tc.rules)
				}
			}
			if report.RawLen != len([]rune(tc.in)) {
				t.Fatalf("RawLen = %d, want %d", report.RawLen, len([]rune(tc.in)))
			}
		})
	}
}

// ZWJ 必须保留：删掉会把 👨‍👩‍👧 拆成三个独立 emoji，语义直接崩坏。
func TestCleanKeepsZWJ(t *testing.T) {
	in := "👨\u200d👩\u200d👧一家人"
	got, _ := Clean(in, ModeSafe)
	if got != in {
		t.Fatalf("ZWJ 被误删: got %q want %q", got, in)
	}
	if n := RuneLen(got); n != 8 {
		t.Fatalf("RuneLen = %d, want 8（5 个 emoji/ZWJ rune + 3 个汉字）", n)
	}
}

// RE2 不支持反向引用，Markdown 规则被拆成多条 —— 这里验证拆分后语义仍等价。
func TestCleanNormalizeMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**粗体**", "粗体"},
		{"__粗体__", "粗体"},
		{"*斜体*", "斜体"},
		{"_斜体_", "斜体"},
		{"**粗**和*斜*", "粗和斜"},
		{"`code`", "code"},
		{"# 标题", "标题"},
		{"### 三级标题", "三级标题"},
		{"[链接](http://x.com)", "链接"},
		{"![图](a.png)", "图"},
		{"- 列表项", "列表项"},
		{"> 引用", "引用"},
	}
	for _, tc := range cases {
		got, _ := Clean(tc.in, ModeNormalize)
		if got != tc.want {
			t.Errorf("Clean(%q, normalize) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanOffModeOnlyStrips(t *testing.T) {
	got, report := Clean("  你\u200b好  ", ModeOff)
	if got != "你\u200b好" {
		t.Fatalf("off 档不应删除零宽字符: got %q", got)
	}
	if report.Removed != 4 {
		t.Fatalf("Removed = %d, want 4", report.Removed)
	}
	if len(report.Rules) != 1 || report.Rules[0] != "strip" {
		t.Fatalf("Rules = %v, want [strip]", report.Rules)
	}
}

// 请求级参数只能加强，不能削弱服务端下限。
func TestResolveModeOnlyStrengthens(t *testing.T) {
	cases := []struct{ requested, def, want string }{
		{"", "safe", "safe"},
		{"normalize", "safe", "normalize"},
		{"off", "safe", "safe"}, // 不许削弱
		{"off", "normalize", "normalize"},
		{"safe", "normalize", "normalize"},
		{"garbage", "safe", "safe"}, // 无法识别退回默认
		{"  NORMALIZE  ", "safe", "normalize"},
	}
	for _, tc := range cases {
		if got := ResolveMode(tc.requested, tc.def); got != tc.want {
			t.Errorf("ResolveMode(%q, %q) = %q, want %q", tc.requested, tc.def, got, tc.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe(Report{Removed: 0}); got != "none" {
		t.Fatalf("Describe = %q, want none", got)
	}
	got := Describe(Report{Removed: 3, Rules: []string{"invisible", "strip"}})
	if got != "3 chars removed by invisible,strip" {
		t.Fatalf("Describe = %q", got)
	}
}
