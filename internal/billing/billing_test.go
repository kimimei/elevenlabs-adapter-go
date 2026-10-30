package billing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func intp(n int) *int { return &n }

func TestBillCharsByMode(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		in, sent int
		credits  *int
		status   int
		wantN    int
		wantSrc  string
	}{
		{"input 口径按原始长度", ModeInput, 100, 80, intp(48), 200, 100, SourceInput},
		{"chars 口径按合成量", ModeChars, 100, 80, intp(48), 200, 80, SourceChars},
		{"upstream 口径按上游积分", ModeUpstream, 100, 80, intp(48), 200, 48, SourceUpstream},
		{"失败请求一律 0", ModeInput, 100, 80, intp(48), 502, 0, SourceFailed},
		{"空输入记 0", ModeInput, 0, 0, nil, 200, 0, SourceInput},
		{"chars 口径空合成记 0", ModeChars, 10, 0, nil, 200, 0, SourceChars},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, src := BillCharsByMode(tc.mode, tc.in, tc.sent, tc.credits, tc.status)
			if n != tc.wantN || src != tc.wantSrc {
				t.Fatalf("got (%d, %s), want (%d, %s)", n, src, tc.wantN, tc.wantSrc)
			}
		})
	}
}

// upstream 口径但上游没回传积分时，按 Python 版行为退回 len(input)。
func TestUpstreamModeWithoutCreditsFallsBack(t *testing.T) {
	n, src := BillCharsByMode(ModeUpstream, 42, 30, nil, 200)
	if n != 42 {
		t.Fatalf("n = %d, want 42（退回 input 口径）", n)
	}
	if src != SourceInput {
		t.Fatalf("src = %s, want input", src)
	}
	// 但响应头要如实说明「退回」
	h := RuleHeader(ModeUpstream, n, 42, nil, 200, src)
	want := "status=200 mode=upstream no-credits fallback=42"
	if h != want {
		t.Fatalf("RuleHeader = %q, want %q", h, want)
	}
}

// 中文绝不能进 HTTP 头：latin-1 限制会让整个响应打挂。
func TestRuleHeaderIsASCII(t *testing.T) {
	got := RuleHeader(ModeInput, 100, 120, intp(48), 200, SourceInput)
	for _, r := range got {
		if r > 0x7F {
			t.Fatalf("RuleHeader 含非 ASCII 字符 %q：%s", r, got)
		}
	}
	// 账本侧（中文）与响应头（ASCII）必须是两个函数，不能互相顶替
	if txt := RuleText(ModeInput, 100, 120, intp(48), 200, SourceInput); txt == got {
		t.Fatal("RuleText 与 RuleHeader 不应相同：账本可以中文，HTTP 头不行")
	}
}

func TestLedgerRecordAndStats(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir, ModeInput, 30, false, 4545.45, 12500)
	defer l.Close()

	if err := l.Record(Entry{
		RID: "r1", Model: "eleven_v3", Voice: "yun",
		InChars: 100, SentChars: 80, UpCredits: intp(48), Status: 200,
		ElapsedMS: 7000, ClientIP: "203.0.113.9", TokenName: "kamy",
		RawChars: 100, CleanedChars: 80, CleanRules: "invisible",
	}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}
	if err := l.Record(Entry{
		RID: "r2", InChars: 50, SentChars: 50, Status: 502,
		Error: "upstream boom", RawChars: 50, CleanedChars: 50,
	}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	st := l.Stats("")
	if st.Requests != 2 || st.Succeeded != 1 || st.Failed != 1 {
		t.Fatalf("requests/succeeded/failed = %d/%d/%d", st.Requests, st.Succeeded, st.Failed)
	}
	if st.BillChars != 100 {
		t.Fatalf("bill_chars = %d, want 100（失败请求不计费）", st.BillChars)
	}
	if st.SavedChars != 20 {
		t.Fatalf("saved_chars = %d, want 20", st.SavedChars)
	}
	// in_chars 含失败行（100+50=150），saved 只算成功行（20）—— 与 Python 版一致
	if st.SavedRatio == nil || *st.SavedRatio != 0.1333 {
		t.Fatalf("saved_ratio = %v, want 0.1333", st.SavedRatio)
	}
	// 上游真实单价：积分/字符，同量纲
	if st.CreditsPerChar == nil || *st.CreditsPerChar != 0.6 {
		t.Fatalf("credits_per_char = %v, want 0.6", st.CreditsPerChar)
	}
	// 毛利率必须两边都折美元；未配单价时必须返回 nil（宁可不给，不给错数）
	if st.GrossMarginRatio == nil {
		t.Fatal("配了单价却没算毛利率")
	}
	upCost := 48 / 4545.45
	revenue := 100 / 12500.0
	want := 1 - upCost/revenue
	if diff := *st.GrossMarginRatio - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("gross_margin_ratio = %v, want ≈ %v", *st.GrossMarginRatio, want)
	}

	noPrice := NewLedger(dir, ModeInput, 30, false, 0, 0)
	defer noPrice.Close()
	if g := noPrice.Stats(""); g.GrossMarginRatio != nil {
		t.Fatalf("未配单价时毛利率应为 nil，got %v", *g.GrossMarginRatio)
	}
}

func TestUsageByTokenSortsBySavedChars(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir, ModeInput, 30, false, 0, 0)
	defer l.Close()

	l.Record(Entry{RID: "a", InChars: 100, SentChars: 90, Status: 200, TokenName: "small"})
	l.Record(Entry{RID: "b", InChars: 100, SentChars: 20, Status: 200, TokenName: "big"})
	l.Record(Entry{RID: "c", InChars: 100, SentChars: 50, Status: 500, TokenName: "failed"})

	rows := l.UsageByToken(1)
	if len(rows) != 2 {
		t.Fatalf("只有成功请求应聚合，got %d 行", len(rows))
	}
	if rows[0].Token != "big" {
		t.Fatalf("应按节省量降序，首行 = %s", rows[0].Token)
	}
	if rows[0].SavedChars != 80 || rows[1].SavedChars != 10 {
		t.Fatalf("saved = %d / %d", rows[0].SavedChars, rows[1].SavedChars)
	}
}

// 并发写账本是真实的竞态面：多个请求同时落盘，一旦行被截断或交错，
// 整天的对账数据就废了（而且是静默废掉 —— 后续解析只会跳过坏行）。
// 本机没有 gcc，-race 跑不了，所以用「行完整 + 行数精确」来兜住这条底线。
func TestLedgerConcurrentWritesKeepLinesIntact(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir, ModeInput, 30, false, 0, 0)
	defer l.Close()

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := l.Record(Entry{
				RID: fmt.Sprintf("r%03d", i), InChars: 10, SentChars: 10,
				Status: 200, TokenName: "tester",
			}); err != nil {
				t.Errorf("并发写入失败: %v", err)
			}
		}(i)
	}
	wg.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var file string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			file = filepath.Join(dir, e.Name())
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != n {
		t.Fatalf("行数 = %d，期望 %d（丢行或合并行说明追加写没被串行化）", len(lines), n)
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("第 %d 行不是完整 JSON（被截断/交错）: %v\n%q", i, err, line)
		}
	}
}
