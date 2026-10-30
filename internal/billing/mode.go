// Package billing 是计费账本：每个请求一行 JSONL，用来做三层对账。
//
// 为什么适配器要自己记账 —— new-api 的 logs 表只有一个 prompt_tokens 字段，
// 装不下「客户付了多少」和「上游扣了多少」两个数。靠时间戳去猜配对，并发时必然配错，
// 失败请求更是压根没有 new-api 日志（而这恰恰是最烧钱的部分）。
//
// 三层对账各解决一个问题：
//
//	L1  适配器 bill_chars  vs  new-api logs.prompt_tokens   -> 中转站漏记/重复记
//	L2  适配器 up_credits  vs  上游 /v1/history            -> 上游扣费异常 / Key 被挪用
//	L3  bill_chars × 售价  vs  up_credits × 成本           -> 实际毛利率
//
// 设计取舍：失败请求也记；追加写不阻塞响应；按天切分便于 grep 与清理。
package billing

// 计费口径。input 是默认值，原因不是「对客户好」，而是它**与真实扣费一致**。
const (
	ModeInput    = "input"
	ModeChars    = "chars"
	ModeUpstream = "upstream"

	SourceFailed   = "failed"
	SourceInput    = "input"
	SourceChars    = "chars"
	SourceUpstream = "upstream"
)

// BillCharsByMode 算出「向客户收费的字符数」。
//
// 口径必须与 new-api 同源，否则账对不上。new-api 的 TTS 计费取 prompt_tokens，
// 值是原始请求体 input 字段的 Unicode 字符数（Go 侧 utf8.RuneCountInString）。
// 收钱的是 new-api，我们的账本只是记录 —— 口径不一致时，先怀疑自己。
//
// 失败请求一律计 0：new-api 对非 200 响应不扣费，账本若仍计费，
// 两边永远对不上，只能靠「容差」自欺欺人。
func BillCharsByMode(mode string, inChars, sentChars int, upCredits *int, status int) (int, string) {
	if status != 200 {
		return 0, SourceFailed
	}
	if mode == ModeUpstream && upCredits != nil {
		return *upCredits, SourceUpstream
	}
	if mode == ModeChars {
		// 按实际合成量：这个数字客户能自己数出来（去掉不可见字符后的长度）
		if sentChars <= 0 {
			return 0, SourceChars
		}
		return sentChars, SourceChars
	}
	// 默认 input：与 new-api 同源。清理掉的字符同样计费，
	// 但那是上游成本的节省，不是计费折扣。
	if inChars <= 0 {
		return 0, SourceInput
	}
	return inChars, SourceInput
}
