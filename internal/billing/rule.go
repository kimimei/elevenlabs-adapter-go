package billing

import "fmt"

// RuleText 生成一句话的计费依据，写进账本（可用中文，账本是 UTF-8 文件）。
//
// ⚠️ 这个函数**不能**直接用于 HTTP 响应头：HTTP 头只允许 latin-1，
// 中文写进去会让整个响应崩掉（不是只丢那个头，是请求直接 500）。
// 给客户看的头用 RuleHeader。
func RuleText(mode string, n, rawChars int, upCredits *int, status int, source string) string {
	if status != 200 {
		return fmt.Sprintf("status=%d 未计费（失败请求不收费）", status)
	}
	if mode == ModeUpstream && upCredits != nil {
		return fmt.Sprintf("status=200 mode=upstream bill=character-cost=%d", n)
	}
	if mode == ModeUpstream && upCredits == nil {
		return fmt.Sprintf("status=200 mode=upstream 但上游未回传积分，退回 len(input)=%d", n)
	}
	var s string
	if source == SourceInput {
		// input 口径：与 new-api 同源，账单一字不差
		s = fmt.Sprintf("status=200 mode=input bill=len(input)=%d", n)
		if rawChars != n {
			s += fmt.Sprintf("（原始输入 %d 字符，已剔除不可朗读字符）", rawChars)
		}
	} else {
		// chars 口径：按实际合成量收费
		s = fmt.Sprintf("status=200 mode=chars bill=sent=%d", n)
		if rawChars != n {
			s += fmt.Sprintf("（原始输入 %d，已剔除不可朗读字符）", rawChars)
		}
	}
	if upCredits != nil {
		s += fmt.Sprintf(" (upstream=%d credits)", *upCredits)
	}
	return s
}

// RuleHeader 是给 HTTP 响应头用的纯 ASCII 版本。
//
// HTTP 头只能是 latin-1，任何非 ASCII 字符写入都会抛错，
// 把整个响应打挂（不是只丢那个头，是整个请求 500）。
func RuleHeader(mode string, n, rawChars int, upCredits *int, status int, source string) string {
	if status != 200 {
		return fmt.Sprintf("status=%d not-billed", status)
	}
	if mode == ModeUpstream && upCredits != nil {
		return fmt.Sprintf("status=200 mode=upstream bill=%d", n)
	}
	if mode == ModeUpstream && upCredits == nil {
		return fmt.Sprintf("status=200 mode=upstream no-credits fallback=%d", n)
	}
	var s string
	if source == SourceInput {
		s = fmt.Sprintf("status=200 mode=input bill=input=%d", n)
		if rawChars != n {
			s += fmt.Sprintf(" sent=%d", rawChars)
		}
	} else {
		s = fmt.Sprintf("status=200 mode=chars bill=sent=%d", n)
		if rawChars != n {
			s += fmt.Sprintf(" raw=%d", rawChars)
		}
	}
	if upCredits != nil {
		s += fmt.Sprintf(" upstream=%d", *upCredits)
	}
	return s
}
