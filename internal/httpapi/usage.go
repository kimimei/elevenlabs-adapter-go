package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
)

// handleUsage 用量看板。
//
// 数据全部来自本地账本，不调用上游、不消耗额度。
// **只返回聚合数字，不返回原文** —— 账本里有 token 前 6 位与 IP，
// 这些属于运营侧的排查信息，不该经由这个接口暴露给调用方。
//
// ⚠️ saved_chars 的语义随计费口径变（bill_source 可查当前口径）：
// input 口径（默认）下，客户按 in_chars 付费（与 new-api 一致），
// 而 saved_chars = in_chars − sent_chars 是**上游成本的节省**，**不是**计费差额。
// 把两者混为一谈会得出错误的单位成本。
func (a *App) handleUsage(c *gin.Context) {
	days := 30
	if q := c.Query("days"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			days = n
		}
	}
	if days < 1 {
		days = 1
	}
	if days > a.Cfg.BillingLogDays {
		days = a.Cfg.BillingLogDays
	}

	today := a.Ledger.Stats("")
	win := a.Ledger.WindowStats(days)
	byToken := a.Ledger.UsageByToken(days)

	totalIn, totalSent, totalSaved := 0, 0, 0
	callers := make([]gin.H, 0, len(byToken))
	for _, r := range byToken {
		totalIn += r.InChars
		totalSent += r.SentChars
		totalSaved += r.SavedChars
		callers = append(callers, gin.H{
			"caller":      r.Token,
			"requests":    r.Requests,
			"in_chars":    r.InChars,
			"sent_chars":  r.SentChars,
			"saved_chars": r.SavedChars,
			"saved_ratio": r.SavedRatio,
		})
	}

	note := "计费 = 客户提交的原始字符数（与 new-api 一致）；saved_chars 是其中被清理掉、仍计入账单的部分"
	if a.Cfg.BillingMode != billing.ModeInput {
		note = "计费 = 实际合成字符数；saved_chars 是清理掉的字符数，未计入账单"
	}

	c.PureJSON(http.StatusOK, gin.H{
		"window_days": days,
		"today": gin.H{
			"requests":           today.Requests,
			"succeeded":          today.Succeeded,
			"failed":             today.Failed,
			"bill_chars":         today.BillChars,
			"saved_chars":        today.SavedChars,
			"saved_ratio":        today.SavedRatio,
			"gross_margin_ratio": today.GrossMarginRatio,
			"credits_per_char":   today.CreditsPerChar,
			"up_cost_usd":        today.UpCostUSD,
			"revenue_usd":        today.RevenueUSD,
		},
		"window": gin.H{
			"requests":           win.Requests,
			"in_chars":           totalIn,
			"sent_chars":         totalSent,
			"saved_chars":        totalSaved,
			"saved_ratio":        ratioOrZero(totalSaved, totalIn),
			"gross_margin_ratio": win.GrossMarginRatio,
			"credits_per_char":   win.CreditsPerChar,
			"up_cost_usd":        win.UpCostUSD,
			"revenue_usd":        win.RevenueUSD,
		},
		"by_caller":    callers,
		"billing_mode": a.Cfg.BillingMode,
		"note":         note,
	})
}

// ratioOrZero 窗口级 saved_ratio：分母为 0 时返回 0（与 Python 版一致）。
func ratioOrZero(saved, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(int64(float64(saved)/float64(total)*10000+0.5)) / 10000
}
