package jobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
	"github.com/kimimei/elevenlabs-adapter-go/internal/upstream"
	"github.com/kimimei/elevenlabs-adapter-go/internal/voices"
)

func baseSettings(dir string) Settings {
	return Settings{
		Enabled:      true,
		Workers:      1,
		MaxPending:   10,
		MaxChars:     8000,
		TTLSeconds:   1800,
		DefaultModel: "eleven_multilingual_v2",
		TextClean:    "safe",
		Mock:         true,
		Dir:          dir,
	}
}

// newMgr 造一个管理器。ledger 每次新建（独立临时目录），
// jobs 目录由调用方给，便于测「重启」。
func newMgr(t *testing.T, st Settings, baseURL string) (*Manager, string) {
	t.Helper()
	cat, err := voices.Load("")
	if err != nil {
		t.Fatalf("模型目录加载失败: %v", err)
	}
	ledgerDir := t.TempDir()
	ledger := billing.NewLedger(ledgerDir, billing.ModeInput, 30, false, 0, 0)
	m, err := New(st, cat, ledger, upstream.NewClient(baseURL, 5, 0))
	if err != nil {
		t.Fatalf("任务管理器构造失败: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m.Shutdown(ctx)
		_ = ledger.Close()
	})
	return m, ledgerDir
}

func submitSample(t *testing.T, m *Manager, input string) *SubmitResult {
	t.Helper()
	res, err := m.Submit(SubmitParams{
		Model:          "zh-boyin", // 音色别名：能唯一定位 音色 + 引擎
		Input:          input,
		ResponseFormat: "wav",
		APIKey:         "sk_test_abcdef",
		ClientIP:       "127.0.0.1",
		TokenName:      "sk_tes…",
	})
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	return res
}

func waitTerminal(t *testing.T, m *Manager, id string) *Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// 必须走 Snapshot：lookup 返回的是活指针，锁已释放，
		// 直接读 Status 会和 worker 的写入撞上（-race 会报 DATA RACE）。
		if j, ok := m.Snapshot(id); ok && j.Terminal() {
			return &j
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, ok := m.Snapshot(id)
	if !ok {
		t.Fatalf("任务 %s 消失了", id)
	}
	t.Fatalf("任务 %s 未在 5s 内进入终态（当前 %s）", id, j.Status)
	return nil
}

func waitStatus(t *testing.T, m *Manager, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := m.Snapshot(id); ok && j.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, ok := m.Snapshot(id)
	got := "<消失>"
	if ok {
		got = j.Status
	}
	t.Fatalf("任务 %s 未进入 %s（当前 %s）", id, want, got)
}

func ledgerRows(t *testing.T, dir string) []map[string]any {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读账本目录失败: %v", err)
	}
	rows := []map[string]any{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读账本失败: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("账本行不是合法 JSON: %v", err)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// waitLedgerRows 等到账本里出现至少 n 行。
//
// 取消路径上「状态先落定、worker 后记账」，所以读账本必须允许短暂延迟 ——
// 这不是测试放水，而是真实时序：Cancel 立刻回 200，记账由 worker 收尾。
func waitLedgerRows(t *testing.T, dir string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows := ledgerRows(t, dir)
		if len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			return rows
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubmitSucceedsAndBillsRawChars(t *testing.T) {
	m, ledgerDir := newMgr(t, baseSettings(t.TempDir()), "http://127.0.0.1:1")
	res := submitSample(t, m, "  你好  ")

	if res.Job.Chars != 6 {
		t.Fatalf("chars = %d，期望 6（原始长度，不 strip）", res.Job.Chars)
	}
	if res.Position != 1 || res.Depth != 1 {
		t.Fatalf("队列位置 = %d/%d，期望 1/1", res.Position, res.Depth)
	}
	if res.EstimatedSeconds < 3 {
		t.Fatalf("estimated_seconds = %v，下限应为 3", res.EstimatedSeconds)
	}

	j := waitTerminal(t, m, res.Job.ID)
	if j.Status != StatusSucceeded {
		t.Fatalf("状态 = %s，期望 succeeded（error=%s）", j.Status, j.Error)
	}
	if j.AudioBytes < 1000 {
		t.Fatalf("音频字节 = %d，1 秒 24k 16bit 应在 48000 左右", j.AudioBytes)
	}
	if j.DurationSec == nil || *j.DurationSec < 0.9 || *j.DurationSec > 1.1 {
		t.Fatalf("duration_sec = %v，期望约 1.0", j.DurationSec)
	}
	// 计费口径：input 口径下 bill_chars = 原始 6，sent_chars = 清理后 2
	if j.UsageChars != 6 {
		t.Fatalf("usage_chars = %d，期望 6", j.UsageChars)
	}
	if j.UsageSource != billing.SourceInput {
		t.Fatalf("usage_source = %s，期望 input", j.UsageSource)
	}

	rows := ledgerRows(t, ledgerDir)
	if len(rows) != 1 {
		t.Fatalf("账本行数 = %d，期望 1（异步任务同样要记账）", len(rows))
	}
	row := rows[0]
	if got := row["rid"]; got != "job:"+res.Job.ID {
		t.Fatalf("rid = %v，期望 job:%s", got, res.Job.ID)
	}
	if got := row["async"]; got != true {
		t.Fatalf("async = %v，期望 true —— 没有这个标记就无法把异步流量与同步流量分开对账", got)
	}
	if got := row["in_chars"]; got != float64(6) {
		t.Fatalf("in_chars = %v，期望 6（未 strip）", got)
	}
	if got := row["sent_chars"]; got != float64(2) {
		t.Fatalf("sent_chars = %v，期望 2（清理后送合成）", got)
	}
	if got := row["bill_chars"]; got != float64(6) {
		t.Fatalf("bill_chars = %v，期望 6", got)
	}

	af, rj := m.AudioOf(res.Job.ID)
	if rj != nil {
		t.Fatalf("下载音频失败: %s", rj.Message)
	}
	if len(af.Data) < 44 || string(af.Data[:4]) != "RIFF" {
		t.Fatalf("音频不是 WAV 容器（wav 语义要求带 RIFF 头）")
	}
	if af.Chars != 6 {
		t.Fatalf("下载头里的字符数 = %d，期望 6", af.Chars)
	}
}

// 这个测试是整个 M4 存在的理由：Python 版重启后 job 直接消失，客户端轮询 404。
func TestJobTableSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m1, _ := newMgr(t, baseSettings(dir), "http://127.0.0.1:1")
	res := submitSample(t, m1, "重启后还要能下载")
	first := waitTerminal(t, m1, res.Job.ID)
	if first.Status != StatusSucceeded {
		t.Fatalf("第一次执行未成功: %s %s", first.Status, first.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = m1.Shutdown(ctx)
	cancel()

	// 模拟进程重启：新的管理器读同一个目录
	m2, _ := newMgr(t, baseSettings(dir), "http://127.0.0.1:1")
	view, rj := m2.View(res.Job.ID)
	if rj != nil {
		t.Fatalf("重启后查不到任务（这正是 Python 版的缺陷）: %s", rj.Message)
	}
	if view["status"] != StatusSucceeded {
		t.Fatalf("重启后状态 = %v，期望 succeeded", view["status"])
	}
	if view["duration_sec"] == nil {
		t.Fatalf("重启后 duration_sec 丢了")
	}
	af, rj := m2.AudioOf(res.Job.ID)
	if rj != nil {
		t.Fatalf("重启后下载不到音频: %s", rj.Message)
	}
	if len(af.Data) != first.AudioBytes {
		t.Fatalf("重启后音频字节 = %d，期望 %d", len(af.Data), first.AudioBytes)
	}
}

func TestQueuedJobBecomesInterruptedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	// 模拟「刚入队就被 kill」：磁盘上留下一条 queued
	if err := store.Save(&Job{
		ID: "aaaa1111", Status: StatusQueued, Chars: 12, CreatedAt: 1.0,
		Model: "zh-boyin", ResponseFormat: "mp3",
	}); err != nil {
		t.Fatalf("写任务失败: %v", err)
	}

	m, _ := newMgr(t, baseSettings(dir), "http://127.0.0.1:1")
	j, ok := m.Snapshot("aaaa1111")
	if !ok {
		t.Fatalf("任务没有恢复出来")
	}
	if j.Status != StatusFailed {
		t.Fatalf("状态 = %s，期望 failed（不能留一个永远 queued 的幽灵）", j.Status)
	}
	if j.ErrorCode != CodeInterrupted {
		t.Fatalf("error_code = %q，期望 interrupted", j.ErrorCode)
	}
	if !j.NeverStarted {
		t.Fatalf("never_started 应为 true：排队中断的这单没碰过上游，重提安全")
	}
	view, _ := m.View("aaaa1111")
	if view["error_code"] != CodeInterrupted || view["never_started"] != true {
		t.Fatalf("对外视图缺少中断标记: %v", view)
	}
}

// 最危险的恢复场景：任务在合成中被 kill。上游很可能已经扣了费，
// 所以绝不能自动重跑 —— 那会是二次扣费。
func TestRunningJobNotResumedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ID3fake-audio"))
	}))
	defer srv.Close()

	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := store.Save(&Job{
		ID: "bbbb2222", Status: StatusRunning, Chars: 5000, CreatedAt: 1.0,
		Model: "zh-boyin", ResponseFormat: "mp3",
	}); err != nil {
		t.Fatalf("写任务失败: %v", err)
	}

	st := baseSettings(dir)
	st.Mock = false
	m, _ := newMgr(t, st, srv.URL)

	// 给 worker 一点时间：如果它错误地把任务当成可执行，这里会看到上游被调用
	time.Sleep(150 * time.Millisecond)

	j, ok := m.Snapshot("bbbb2222")
	if !ok {
		t.Fatalf("恢复后的任务 bbbb2222 不见了")
	}
	if j.Status != StatusFailed || j.ErrorCode != CodeInterrupted {
		t.Fatalf("状态 = %s/%s，期望 failed/interrupted", j.Status, j.ErrorCode)
	}
	if j.NeverStarted {
		t.Fatalf("never_started 必须为 false：这单可能已被上游扣费，客户需知情")
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Fatalf("重启后向上游发起了 %d 次请求 —— 未完成的合成重跑就是二次扣费", n)
	}
}

func TestCancelRunningJobAbortsUpstream(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	aborted := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 必须先把 body 读完：Go 的 http.Server 只有在 body 被消费后才会启动
		// 后台读来感知客户端断开。handler 不读 body 时 r.Context() 永远不会被取消，
		// 于是「取消有没有真的中止上游」这件事就测不出来（会假失败）。
		_, _ = io.ReadAll(r.Body)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			// 客户端真的断开了：上游那条在途请求确实被中止
			aborted <- struct{}{}
		case <-release:
			w.WriteHeader(http.StatusOK)
		}
	}))
	// 不用 defer srv.Close()：清理顺序必须是「先放行 handler 再关服务」，
	// 否则 handler 卡着会让 Close 等满超时（写成 defer 会先 Close 再 close(release)）。
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		srv.Close()
	})

	st := baseSettings(dir)
	st.Mock = false
	m, ledgerDir := newMgr(t, st, srv.URL)

	res := submitSample(t, m, "这一单会在合成途中被取消")

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatalf("上游一直没被调用")
	}

	// running 必须已落盘。漏了这一步，进程被强杀后重启会把它当成「从未开始」，
	// 于是告诉客户「这一单没产生上游费用，放心重提」—— 而上游已经调用过了。
	raw, rerr := os.ReadFile(filepath.Join(dir, res.Job.ID+".json"))
	if rerr != nil {
		t.Fatalf("元数据没落盘: %v", rerr)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("元数据不是合法 JSON: %v", err)
	}
	if meta["status"] != StatusRunning {
		t.Fatalf("磁盘上的状态 = %v，期望 running（否则崩溃恢复会把已扣费的任务说成没花钱）", meta["status"])
	}

	if err := m.Cancel(res.Job.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	j := waitTerminal(t, m, res.Job.ID)
	if j.Status != StatusCancelled {
		t.Fatalf("状态 = %s，期望 cancelled", j.Status)
	}

	// 只把状态改成 cancelled 而不管在途请求，等于「客户以为停了，我们还在付钱」。
	select {
	case <-aborted:
	case <-time.After(3 * time.Second):
		t.Fatalf("取消没有真正中止上游请求（只改了状态）")
	}

	rows := waitLedgerRows(t, ledgerDir, 1)
	if len(rows) != 1 {
		t.Fatalf("账本行数 = %d，期望 1（取消也要留痕：上游可能已扣费）", len(rows))
	}
	if got := rows[0]["status"]; got != float64(499) {
		t.Fatalf("账本 status = %v，期望 499", got)
	}
	if got := rows[0]["bill_chars"]; got != float64(0) {
		t.Fatalf("取消的任务 bill_chars = %v，期望 0（没交付就不能收钱）", got)
	}

	// 已终态的任务不能再取消
	if err := m.Cancel(res.Job.ID); err == nil {
		t.Fatalf("已结束的任务应拒绝取消")
	}
}

func TestQueueFullRejects(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	st := baseSettings(t.TempDir())
	st.Mock = false
	st.Workers = 1
	st.MaxPending = 1
	m, _ := newMgr(t, st, srv.URL)

	// 第一个占住唯一的 worker，第二个进队列（深度 1）。
	// 必须先等第一单真的进入 running：worker 还没取走它时队列深度是 1，
	// 这时候提交第二单会被判成「队列已满」——那是测试自己的时序问题。
	first, err := m.Submit(SubmitParams{Model: "zh-boyin", Input: "第一单", ResponseFormat: "mp3", APIKey: "k"})
	if err != nil {
		t.Fatalf("第一单应受理: %v", err)
	}
	waitStatus(t, m, first.Job.ID, StatusRunning)
	if _, err := m.Submit(SubmitParams{Model: "zh-boyin", Input: "第二单", ResponseFormat: "mp3", APIKey: "k"}); err != nil {
		t.Fatalf("第二单应受理: %v", err)
	}
	_, err = m.Submit(SubmitParams{Model: "zh-boyin", Input: "第三单", ResponseFormat: "mp3", APIKey: "k"})
	if err == nil {
		t.Fatalf("队列已满时应拒绝")
	}
	rj, ok := err.(*Reject)
	if !ok || rj.Status != http.StatusTooManyRequests {
		t.Fatalf("期望 429，got %#v", err)
	}
	if rj.Headers["Retry-After"] == "" {
		t.Fatalf("429 必须带 Retry-After，否则客户端会立刻重试，把队列压得更死")
	}
}

func TestDisabledManagerRejects(t *testing.T) {
	st := baseSettings(t.TempDir())
	st.Enabled = false
	m, _ := newMgr(t, st, "http://127.0.0.1:1")

	_, err := m.Submit(SubmitParams{Model: "zh-boyin", Input: "你好", ResponseFormat: "mp3"})
	rj, ok := err.(*Reject)
	if !ok || rj.Status != http.StatusNotFound {
		t.Fatalf("未开启时应返回 404 Reject，got %#v", err)
	}
	if err := m.Cancel("whatever"); err == nil {
		t.Fatalf("未开启时取消失败应返回错误")
	}
}

func TestTTLPurgeRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	st := baseSettings(dir)
	st.TTLSeconds = 60
	m, _ := newMgr(t, st, "http://127.0.0.1:1")

	res := submitSample(t, m, "到期后应被清理")
	waitTerminal(t, m, res.Job.ID)

	meta := filepath.Join(dir, res.Job.ID+".json")
	audio := filepath.Join(dir, res.Job.ID+".audio")
	if _, err := os.Stat(audio); err != nil {
		t.Fatalf("成功后音频应落盘: %v", err)
	}

	// 时钟拨快一小时
	m.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, rj := m.View(res.Job.ID); rj == nil {
		t.Fatalf("过期任务应查不到")
	}
	for _, p := range []string{meta, audio} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 应被清理（不清理的话，长跑实例会被音频字节吃光内存/磁盘）", p)
		}
	}
}

func TestListReportsQueueDepthAndStatuses(t *testing.T) {
	m, _ := newMgr(t, baseSettings(t.TempDir()), "http://127.0.0.1:1")
	a := submitSample(t, m, "第一条")
	waitTerminal(t, m, a.Job.ID)

	out := m.List(50)
	if out.Total != 1 {
		t.Fatalf("total = %d，期望 1", out.Total)
	}
	if out.ByStatus[StatusSucceeded] != 1 {
		t.Fatalf("by_status 未统计成功数: %v", out.ByStatus)
	}
	if out.QueueDepth != 0 {
		t.Fatalf("queue_depth = %d，期望 0", out.QueueDepth)
	}
}
