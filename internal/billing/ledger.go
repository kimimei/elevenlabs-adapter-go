package billing

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry 是一个请求的记账输入。
type Entry struct {
	RID          string
	UpRID        *string
	Model        string
	Voice        string
	ModelID      string // 上游真实模型（eleven_v3…）。Model 可能只是音色别名，算成本只能用它
	Stream       bool   // 本次是否实际走了 SSE
	StreamFormat string // sse / audio
	// 客户端传了 stream:true 却没给 stream_format=sse：首包必然差，
	// 不单独标记就会被误判成适配器性能问题
	StreamIgnored bool
	InChars       int
	SentChars     int
	UpCredits     *int
	Status        int
	ElapsedMS     int
	ClientIP      string
	TokenName     string
	Error         string
	RawChars      int
	CleanedChars  int
	CleanRules    string
	Async         bool
}

// Row 是账本里的一行。字段顺序即 JSON 输出顺序，与 Python 版保持一致：
// M6 影子对账要把两边逐字段 diff，顺序一致能省掉一轮视觉噪声。
type Row struct {
	TS            int64   `json:"ts"`
	RID           string  `json:"rid"`
	UpRID         *string `json:"up_rid"`
	Model         *string `json:"model"`
	ModelID       *string `json:"model_id"`
	Voice         *string `json:"voice"`
	Stream        *bool   `json:"stream"`
	StreamFormat  *string `json:"stream_format"`
	StreamIgnored bool    `json:"stream_ignored"`
	InChars       int     `json:"in_chars"`
	SentChars     int     `json:"sent_chars"`
	BillChars     int     `json:"bill_chars"`
	BillSource    string  `json:"bill_source"`
	BillRule      string  `json:"bill_rule"`
	UpCredits     *int    `json:"up_credits"`
	Status        int     `json:"status"`
	MS            int     `json:"ms"`
	Mode          string  `json:"mode"`
	IP            *string `json:"ip"`
	Token         *string `json:"token"`
	Error         string  `json:"error,omitempty"`
	RawChars      int     `json:"raw_chars"`
	CleanedChars  int     `json:"cleaned_chars"`
	CleanRules    *string `json:"clean_rules"`
	Async         bool    `json:"async,omitempty"`
}

// Ledger 是按天切分的 JSONL 账本。
//
// 与 Python 版的差异：文件句柄按天缓存、复用，不再每写一行 open/close。
// 语义不变（追加写、失败不影响业务），但少一次 syscall，且天然按天对齐。
type Ledger struct {
	mu              sync.Mutex
	dir             string
	mode            string
	days            int
	off             bool
	upCreditsPerUSD float64
	charsPerUSD     float64
	files           map[string]*os.File
	lastCleanup     string
	now             func() time.Time
}

// NewLedger 构造账本。days<=0 时用 30 天。
//
// upCreditsPerUSD / charsPerUSD 缺省为 0 = 不算毛利率：
// 宁可返回 null，也不返回一个拿积分除字符的假数。
func NewLedger(dir, mode string, days int, off bool, upCreditsPerUSD, charsPerUSD float64) *Ledger {
	if days <= 0 {
		days = 30
	}
	return &Ledger{
		dir:             dir,
		mode:            mode,
		days:            days,
		off:             off,
		upCreditsPerUSD: upCreditsPerUSD,
		charsPerUSD:     charsPerUSD,
		files:           map[string]*os.File{},
		now:             time.Now,
	}
}

// Dir 返回账本目录，供 /v1/usage 展示。
func (l *Ledger) Dir() string { return l.dir }

// Mode 返回当前计费口径。
func (l *Ledger) Mode() string { return l.mode }

// Record 写一行账本。任何异常都只返回 error 由调用方记日志 ——
// 记账失败绝不能影响业务响应。
func (l *Ledger) Record(e Entry) error {
	if l.off {
		return nil
	}
	now := l.now()
	n, src := BillCharsByMode(l.mode, e.InChars, e.SentChars, e.UpCredits, e.Status)

	row := Row{
		TS:            now.Unix(),
		RID:           e.RID,
		UpRID:         e.UpRID,
		Model:         optStr(e.Model),
		ModelID:       optStr(e.ModelID),
		Voice:         optStr(e.Voice),
		Stream:        &e.Stream,
		StreamFormat:  optStr(e.StreamFormat),
		StreamIgnored: e.StreamIgnored,
		InChars:       e.InChars,
		SentChars:     e.SentChars,
		BillChars:     n,
		BillSource:    src,
		BillRule:      RuleText(l.mode, n, e.InChars, e.UpCredits, e.Status, src),
		UpCredits:     e.UpCredits,
		Status:        e.Status,
		MS:            e.ElapsedMS,
		Mode:          l.mode,
		IP:            optStr(e.ClientIP),
		Token:         optStr(e.TokenName),
		Error:         truncateRunes(e.Error, 200),
		RawChars:      e.RawChars,
		CleanedChars:  e.CleanedChars,
		CleanRules:    optStr(e.CleanRules),
		Async:         e.Async,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // Python 的 ensure_ascii=False 同样不转义 < > &
	if err := enc.Encode(row); err != nil {
		return fmt.Errorf("账本行序列化失败: %w", err)
	}

	day := now.Format("2006-01-02")
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.fileFor(day)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("账本写入失败: %w", err)
	}
	l.cleanupLocked(now)
	return nil
}

// fileFor 返回当天的文件句柄。day 变化时关掉旧句柄，只保留当天的。
func (l *Ledger) fileFor(day string) (*os.File, error) {
	if f, ok := l.files[day]; ok {
		return f, nil
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return nil, fmt.Errorf("账本目录创建失败: %w", err)
	}
	path := filepath.Join(l.dir, day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("账本打开失败: %w", err)
	}
	// 只保留当天句柄：跨零点后旧句柄不再需要，留着会攒出一堆 fd
	for d, old := range l.files {
		if d != day {
			old.Close()
			delete(l.files, d)
		}
	}
	l.files[day] = f
	return f, nil
}

// cleanupLocked 删除超过保留期的账本文件。每天最多跑一次，失败不影响主流程。
func (l *Ledger) cleanupLocked(now time.Time) {
	day := now.Format("2006-01-02")
	if l.lastCleanup == day {
		return
	}
	l.lastCleanup = day
	cutoff := now.AddDate(0, 0, -l.days)
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSuffix(name, ".jsonl"), time.Local)
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
}

// Close 关闭所有账本句柄。
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var firstErr error
	for d, f := range l.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(l.files, d)
	}
	return firstErr
}

// ---------------- 聚合 ----------------

// Aggregate 是一天的汇总，字段与 /v1/usage 的输出一一对应。
type Aggregate struct {
	Requests         int
	Succeeded        int
	Failed           int
	InChars          int
	BillChars        int
	UpCredits        int
	OkBillChars      int
	OkUpCredits      int
	OkSentChars      int
	SentChars        int
	SavedChars       int
	SavedRatio       *float64
	CreditsPerChar   *float64
	GrossMarginRatio *float64
	UpCostUSD        *float64
	RevenueUSD       *float64
	Path             string
}

// Stats 汇总某天账本（day 为空表示今天）。
//
// saved_chars = 原始输入 − 实际合成 = 清理掉的字符数。语义随口径变，别读错：
// input 口径下这部分**仍然计入客户账单**（与 new-api 一致），但上游只收 sent，
// 所以它是上游成本节省，不是计费折扣。chars 口径下它才是真正的「没向客户收的部分」。
func (l *Ledger) Stats(day string) Aggregate {
	if day == "" {
		day = l.now().Format("2006-01-02")
	}
	out := Aggregate{Path: filepath.Join(l.dir, day+".jsonl")}
	l.scanFiles([]string{out.Path}, &out)
	out.finalize(l.upCreditsPerUSD, l.charsPerUSD)
	return out
}

// WindowStats 聚合最近 N 天账本。
func (l *Ledger) WindowStats(days int) Aggregate {
	if days < 1 {
		days = 1
	}
	out := Aggregate{}
	paths := make([]string, 0, days)
	for i := 0; i < days; i++ {
		d := l.now().AddDate(0, 0, -i).Format("2006-01-02")
		paths = append(paths, filepath.Join(l.dir, d+".jsonl"))
	}
	l.scanFiles(paths, &out)
	out.finalize(l.upCreditsPerUSD, l.charsPerUSD)
	return out
}

// TokenAgg 是按客户令牌聚合的用量。
type TokenAgg struct {
	Token      string
	Requests   int
	InChars    int
	SentChars  int
	UpCredits  int
	SavedChars int
	SavedRatio float64
	LastSeen   int64
}

// UsageByToken 按客户令牌聚合最近 N 天用量。只统计成功请求。
func (l *Ledger) UsageByToken(days int) []TokenAgg {
	if days < 1 {
		days = 1
	}
	agg := map[string]*TokenAgg{}
	for i := 0; i < days; i++ {
		day := l.now().AddDate(0, 0, -i).Format("2006-01-02")
		path := filepath.Join(l.dir, day+".jsonl")
		l.forEachRow(path, func(r *Row) {
			if r.Status != 200 {
				return
			}
			tok := "<anonymous>"
			if r.Token != nil && *r.Token != "" {
				tok = *r.Token
			}
			a, ok := agg[tok]
			if !ok {
				a = &TokenAgg{Token: tok}
				agg[tok] = a
			}
			a.Requests++
			a.InChars += r.InChars
			a.SentChars += r.SentChars
			if r.UpCredits != nil {
				a.UpCredits += *r.UpCredits
			}
			if r.InChars > r.SentChars {
				a.SavedChars += r.InChars - r.SentChars
			}
			if r.TS > a.LastSeen {
				a.LastSeen = r.TS
			}
		})
	}

	rows := make([]TokenAgg, 0, len(agg))
	for _, a := range agg {
		if a.InChars > 0 {
			a.SavedRatio = round4(float64(a.SavedChars) / float64(a.InChars))
		}
		rows = append(rows, *a)
	}
	// 按节省量降序；同值时按 token 升序，保证输出可复现
	// （Go 的 map 遍历无序，Python 靠插入序，这里用显式 tie-break 取代隐式行为）
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].SavedChars != rows[j].SavedChars {
			return rows[i].SavedChars > rows[j].SavedChars
		}
		return rows[i].Token < rows[j].Token
	})
	return rows
}

func (l *Ledger) scanFiles(paths []string, out *Aggregate) {
	for _, p := range paths {
		l.forEachRow(p, func(r *Row) {
			out.Requests++
			ok := r.Status == 200
			if ok {
				out.Succeeded++
			} else {
				out.Failed++
			}
			out.InChars += r.InChars
			out.BillChars += r.BillChars
			if r.UpCredits != nil {
				out.UpCredits += *r.UpCredits
			}
			if !ok {
				return
			}
			out.OkBillChars += r.BillChars
			if r.UpCredits != nil {
				out.OkUpCredits += *r.UpCredits
			}
			// 清理掉的字符：原始输入 - 实际合成
			out.SentChars += r.SentChars
			out.OkSentChars += r.SentChars
			if r.InChars > r.SentChars {
				out.SavedChars += r.InChars - r.SentChars
			}
		})
	}
}

func (out *Aggregate) finalize(upCreditsPerUSD, charsPerUSD float64) {
	if out.InChars > 0 && out.SavedChars > 0 {
		v := round4(float64(out.SavedChars) / float64(out.InChars))
		out.SavedRatio = &v
	}
	// 上游真实单价：积分 / 字符。同量纲，有意义。
	if out.OkSentChars > 0 && out.OkUpCredits > 0 {
		v := round4(float64(out.OkUpCredits) / float64(out.OkSentChars))
		out.CreditsPerChar = &v
	}
	// 毛利率必须先把两边都换成美元，否则是拿积分除字符 —— 量纲不同，数学上没有意义。
	// 只算成功请求：失败请求不收费但上游可能扣了钱，混进来会把毛利率算歪
	// （那是「白扣」损失，不是毛利）。
	if out.OkBillChars > 0 && out.OkUpCredits > 0 && upCreditsPerUSD > 0 && charsPerUSD > 0 {
		upCost := float64(out.OkUpCredits) / upCreditsPerUSD
		revenue := float64(out.OkBillChars) / charsPerUSD
		if revenue > 0 {
			m := round4(1 - upCost/revenue)
			c := roundN(upCost, 6)
			r := roundN(revenue, 6)
			out.GrossMarginRatio = &m
			out.UpCostUSD = &c
			out.RevenueUSD = &r
		}
	}
}

// forEachRow 逐行读取账本。坏行直接跳过 —— 一条脏数据不该让整个用量看板归零。
func (l *Ledger) forEachRow(path string, fn func(*Row)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var r Row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		fn(&r)
	}
}

// upCreditsPerUSD / charsPerUSD 在构造时读取，运行期不热更新 ——
// 单价改了要重启，这是刻意的：毛利率算到一半换单价，会让同一天的报表前后矛盾。
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func truncateRunes(s string, n int) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func round4(f float64) float64 { return roundN(f, 4) }

func roundN(f float64, n int) float64 {
	pow := 1.0
	for i := 0; i < n; i++ {
		pow *= 10
	}
	return float64(int64(f*pow+sign(f)*0.5)) / pow
}

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

// itoa 仅供测试与报文拼装使用。
func itoa(n int) string { return strconv.Itoa(n) }
