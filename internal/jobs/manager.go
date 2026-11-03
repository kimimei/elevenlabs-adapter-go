package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kimimei/elevenlabs-adapter-go/internal/apierr"
	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
	"github.com/kimimei/elevenlabs-adapter-go/internal/pcm"
	"github.com/kimimei/elevenlabs-adapter-go/internal/textclean"
	"github.com/kimimei/elevenlabs-adapter-go/internal/upstream"
	"github.com/kimimei/elevenlabs-adapter-go/internal/voices"
)

// Settings 是任务模块的运行参数。与 config.Config 解耦，
// 这样测试可以只构造需要的那几项，不必摆一整套环境变量。
type Settings struct {
	Enabled      bool
	Workers      int
	MaxPending   int
	MaxChars     int
	TTLSeconds   int
	DefaultModel string
	TextClean    string
	Mock         bool
	Dir          string
}

// Reject 是被拒绝的请求，携带直接映射到 HTTP 的全部信息。
//
// 让 jobs 包自己决定状态码，而不是让 HTTP 层从错误文案里猜 ——
// 猜错就是 500 冒充 400，客户以为是服务故障而反复重试。
type Reject struct {
	Status  int
	Message string
	Code    string
	Fix     string
	Headers map[string]string
}

func (r *Reject) Error() string { return r.Message }

// codeInvalid 与 Python 版一致：jobs 端点除队列满/缺 Key 外，
// 一律用默认的 invalid_request_error（即使 HTTP 状态是 404）。
// M6 对账要比 error.code，这里差一个字符串就是一条差异。
const codeInvalid = "invalid_request_error"

func disabledReject(fix string) *Reject {
	return &Reject{Status: http.StatusNotFound, Message: "异步任务接口未开启", Code: codeInvalid, Fix: fix}
}

func notFoundReject(id string) *Reject {
	return &Reject{
		Status:  http.StatusNotFound,
		Message: "job_id 不存在或已过期：" + id,
		Code:    codeInvalid,
		Fix:     "确认 job_id 没抄错；结果在 ADAPTER_JOBS_TTL_SECONDS 后会被清理（默认 1800 秒）",
	}
}

// SubmitParams 是提交一个任务所需的全部输入。
type SubmitParams struct {
	Model          string
	Input          string
	Voice          string
	ResponseFormat string
	Speed          *float64
	TextClean      string // 请求级档位，只能比服务端默认更强
	APIKey         string
	ClientIP       string
	TokenName      string
}

// SubmitResult 是提交成功后的应答。
type SubmitResult struct {
	Job              *Job
	Position         int
	Depth            int
	EstimatedSeconds float64
	Workers          int
}

// AudioFile 是待下载的音频。
type AudioFile struct {
	Data        []byte
	ContentType string
	Format      string
	VoiceID     string
	ModelID     string
	Chars       int
}

// ListResult 是任务列表（运维视角）。
type ListResult struct {
	Total      int
	QueueDepth int
	Workers    int
	ByStatus   map[string]int
	Data       []map[string]any
}

// Manager 是任务表 + worker 池。
type Manager struct {
	st     Settings
	cat    *voices.Catalog
	ledger *billing.Ledger
	uc     *upstream.Client
	store  *Store

	baseCtx    context.Context
	baseCancel context.CancelFunc
	queue      chan string
	wg         sync.WaitGroup
	workers    int

	mu   sync.Mutex
	jobs map[string]*Job

	now func() time.Time
}

// New 构造任务管理器：建存储、恢复历史任务、按需拉起 worker。
func New(st Settings, cat *voices.Catalog, ledger *billing.Ledger, uc *upstream.Client) (*Manager, error) {
	if st.Workers < 1 {
		st.Workers = 1
	}
	if st.TTLSeconds <= 0 {
		st.TTLSeconds = 1800
	}
	m := &Manager{
		st:     st,
		cat:    cat,
		ledger: ledger,
		uc:     uc,
		jobs:   map[string]*Job{},
		now:    time.Now,
	}

	// 通道容量：给了上限就按上限，没给（<=0 视为不限）就用一个足够大的固定值。
	// 真正拦住请求的是 pending 校验，通道只是投递管道。
	capacity := st.MaxPending
	if capacity <= 0 {
		capacity = 1024
	}
	m.queue = make(chan string, capacity)
	m.baseCtx, m.baseCancel = context.WithCancel(context.Background())

	// 没配目录且功能关闭时什么都不做：不碰文件系统，也不刷 WARN。
	// （单测里构造的 Settings 常常只有 Enabled=false 一项，不该因此产生噪声日志。）
	if st.Enabled || strings.TrimSpace(st.Dir) != "" {
		store, err := NewStore(st.Dir)
		if err != nil {
			// 开启了异步任务却没有可写目录 = 承诺的「重启不丢」是假的，
			// 这种配置错误必须在启动时就暴露，而不是等重启时才发现任务全没了。
			if st.Enabled {
				return nil, err
			}
			log.Printf("[WARN] 任务表不可用（%v）：历史任务记录无法读取", err)
		} else {
			m.store = store
			loaded, lerr := store.Load()
			if lerr != nil {
				log.Printf("[WARN] 任务表读取失败（%s）: %v", store.Dir(), lerr)
			}
			m.restore(loaded)
			if len(loaded) > 0 {
				log.Printf("[INFO] 任务表已恢复 %d 条（目录 %s）", len(loaded), store.Dir())
			}
		}
	}

	if st.Enabled {
		m.workers = st.Workers
		for i := 0; i < st.Workers; i++ {
			m.wg.Add(1)
			go m.worker()
		}
	}
	return m, nil
}

// Enabled 报告任务模块是否开启。
func (m *Manager) Enabled() bool { return m.st.Enabled }

// DisabledError 返回「未开启」的拒绝，供 HTTP 层在提交前判定。
func (m *Manager) DisabledError() error {
	return disabledReject("设置 ADAPTER_JOBS_ENABLED=1 后重启")
}

// Workers 是并发 worker 数（0 表示未开启）。
func (m *Manager) Workers() int { return m.workers }

// MaxPending 是队列上限（0 = 不限）。
func (m *Manager) MaxPending() int { return m.st.MaxPending }

// QueueDepth 返回排队中的任务数（先做过期清理，避免把已过期任务算进堆积）。
func (m *Manager) QueueDepth() int {
	m.purgeExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.depthLocked()
}

// Dir 是任务表目录。
func (m *Manager) Dir() string {
	if m.store == nil {
		return ""
	}
	return m.store.Dir()
}

// restore 处理重启后读回的任务。
//
// 铁律：**绝不自动续跑**。崩溃时状态为 running 的任务，上游很可能已经
// 合成完并扣了费，自动重跑就是二次扣费，客户还会拿到两份互为重复的音频。
// 所以这里只做一件事：把「永远 running」的假象改成客户能读懂的确定结局。
func (m *Manager) restore(loaded []*Job) {
	now := m.stamp()
	for _, j := range loaded {
		switch j.Status {
		case StatusQueued, StatusRunning:
			neverStarted := j.Status == StatusQueued
			j.Status = StatusFailed
			j.ErrorCode = CodeInterrupted
			j.NeverStarted = neverStarted
			if neverStarted {
				j.Error = "适配器重启，排队中的任务已中断（尚未开始合成）"
				j.Fix = "重新提交同一个任务即可；这一单没有产生上游费用"
			} else {
				j.Error = "适配器重启，任务在合成过程中中断"
				j.Fix = "重新提交前先确认：中断的那一单上游可能已按已完成部分扣费"
			}
			j.FinishedAt = &now
			m.jobs[j.ID] = j
			m.save(j)
		case StatusSucceeded:
			if m.store != nil && !m.store.HasAudio(j.ID) {
				j.Status = StatusFailed
				j.ErrorCode = CodeAudioMissing
				j.Error = "音频文件缺失，任务结果已不可用"
				j.Fix = "重新提交同一个任务；并检查 ADAPTER_JOBS_DIR 是否被清理过"
				j.FinishedAt = &now
				m.save(j)
			}
			m.jobs[j.ID] = j
		default:
			m.jobs[j.ID] = j
		}
	}
}

// Submit 提交一个长文本任务，立刻返回。
func (m *Manager) Submit(p SubmitParams) (*SubmitResult, error) {
	if !m.st.Enabled {
		return nil, disabledReject("中转站客户需要长文本时，在适配器上设置 ADAPTER_JOBS_ENABLED=1 后重启；" +
			"否则请按 2000 字分段调用同步接口")
	}

	// 与同步接口同一套规则：客户可加强（normalize），不能削弱服务端下限
	mode := textclean.ResolveMode(p.TextClean, m.st.TextClean)
	text, report := textclean.Clean(p.Input, mode)
	if text == "" {
		return nil, &Reject{
			Status:  http.StatusBadRequest,
			Message: "input 不能为空",
			Code:    "invalid_request_error",
			Fix:     "把要合成的文本放进 input 字段；纯空白/纯零宽字符也会被拒绝",
		}
	}
	sentChars := textclean.RuneLen(text)
	if m.st.MaxChars > 0 && sentChars > m.st.MaxChars {
		return nil, &Reject{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("input 超长：%d 字符，单任务上限 %d 字符", sentChars, m.st.MaxChars),
			Code:    "invalid_request_error",
			Fix:     "调大 ADAPTER_JOBS_MAX_CHARS，或分成多个任务提交",
		}
	}

	// chars 与账本 in_chars 同源：原始长度，**不 strip**（见 CONTRACT §8）。
	rawChars := report.RawLen
	now := m.stamp()

	m.purgeExpired()

	m.mu.Lock()
	depth := m.depthLocked()
	if m.st.MaxPending > 0 && depth >= m.st.MaxPending {
		m.mu.Unlock()
		return nil, &Reject{
			Status:  http.StatusTooManyRequests,
			Message: fmt.Sprintf("队列已满：排队中 %d / 上限 %d", depth, m.st.MaxPending),
			Code:    "rate_limit_error",
			Fix:     "稍后重试；或调大 ADAPTER_JOBS_MAX_PENDING",
			Headers: map[string]string{"Retry-After": "10"},
		}
	}

	j := &Job{
		ID:             newID(),
		Status:         StatusQueued,
		Chars:          rawChars,
		CreatedAt:      now,
		Model:          p.Model,
		Voice:          p.Voice,
		ResponseFormat: p.ResponseFormat,
		ClientIP:       p.ClientIP,
		TokenName:      p.TokenName,
		text:           text,
		speed:          p.Speed,
		apiKey:         p.APIKey,
		rawChars:       rawChars,
		cleanedChars:   sentChars,
		cleanRules:     strings.Join(report.Rules, ","),
	}
	m.jobs[j.ID] = j
	m.mu.Unlock()

	// 落盘失败不拒绝请求：任务在内存里还能跑完，客户的业务不该因为
	// 我们的磁盘问题被打断。但必须告警 —— 这一个任务重启后会丢。
	if err := m.save(j); err != nil {
		log.Printf("[ERROR] 任务落盘失败 id=%s: %v（该任务重启后会丢失）", j.ID, err)
	}

	select {
	case m.queue <- j.ID:
	default:
		// 理论上到不了：pending 已经按上限校验过。留个兜底而不是阻塞在这里。
		log.Printf("[WARN] 任务通道已满，id=%s 仍已受理（worker 会按队列顺序取）", j.ID)
	}

	log.Printf("[INFO] 任务已入队 %s：%d 字符，队列深度 %d，worker %d",
		j.ID, rawChars, depth+1, m.workers)

	return &SubmitResult{
		Job:              j,
		Position:         depth + 1,
		Depth:            depth + 1,
		EstimatedSeconds: estimateSeconds(rawChars),
		Workers:          m.workers,
	}, nil
}

// View 返回单个任务的对外视图。
func (m *Manager) View(id string) (map[string]any, *Reject) {
	m.purgeExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, notFoundReject(id)
	}
	return j.Public(m.stamp(), m.st.TTLSeconds), nil
}

// List 返回任务列表（最近的在前）。
func (m *Manager) List(limit int) ListResult {
	m.purgeExpired()
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.stamp()
	out := ListResult{
		Total:      len(m.jobs),
		QueueDepth: m.depthLocked(),
		Workers:    m.workers,
		ByStatus:   map[string]int{},
		Data:       make([]map[string]any, 0, len(m.jobs)),
	}
	all := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out.ByStatus[j.Status]++
		all = append(all, j)
	}
	// 按创建时间倒序；同时间按 id 倒序，保证输出可复现
	sort.SliceStable(all, func(i, k int) bool {
		if all[i].CreatedAt != all[k].CreatedAt {
			return all[i].CreatedAt > all[k].CreatedAt
		}
		return all[i].ID > all[k].ID
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	for _, j := range all {
		out.Data = append(out.Data, j.Public(now, m.st.TTLSeconds))
	}
	return out
}

// AudioOf 取任务音频。没有音频返回 *Reject（404/409）。
func (m *Manager) AudioOf(id string) (*AudioFile, *Reject) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return nil, notFoundReject(id)
	}
	if j.Status != StatusSucceeded {
		status := j.Status
		m.mu.Unlock()
		return nil, &Reject{
			Status:  http.StatusConflict,
			Message: fmt.Sprintf("任务尚未完成（当前状态：%s）", status),
			Code:    "invalid_request_error",
			Fix:     "按 poll_url 轮询到 status=succeeded 再下载",
		}
	}
	af := &AudioFile{
		Data:        j.audio,
		ContentType: j.ContentType,
		Format:      j.ResponseFormat,
		VoiceID:     j.VoiceID,
		ModelID:     j.ModelID,
		Chars:       j.UsageChars,
	}
	if af.Chars <= 0 {
		af.Chars = j.Chars
	}
	m.mu.Unlock()

	// 重启后的任务内存里没有音频，从磁盘读。
	if len(af.Data) == 0 && m.store != nil {
		data, err := m.store.LoadAudio(id)
		if err != nil {
			return nil, &Reject{
				Status:  http.StatusNotFound,
				Message: "任务音频文件缺失：" + id,
				Code:    codeInvalid,
				Fix:     "重新提交该任务；并检查 ADAPTER_JOBS_DIR 是否被清理过",
			}
		}
		af.Data = data
	}
	if len(af.Data) == 0 {
		return nil, &Reject{
			Status:  http.StatusNotFound,
			Message: "任务音频已不可用：" + id,
			Code:    codeInvalid,
			Fix:     "结果已过期（ADAPTER_JOBS_TTL_SECONDS）或被清理，重新提交",
		}
	}
	return af, nil
}

// Cancel 取消任务。只有排队中/进行中的可以取消。
func (m *Manager) Cancel(id string) error {
	if !m.st.Enabled {
		return disabledReject("设置 ADAPTER_JOBS_ENABLED=1 后重启")
	}
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return notFoundReject(id)
	}
	if j.Terminal() {
		status := j.Status
		m.mu.Unlock()
		return &Reject{
			Status:  http.StatusConflict,
			Message: fmt.Sprintf("任务已结束（状态 %s），无法取消", status),
			Code:    "invalid_request_error",
			Fix:     "只有排队中的任务可以取消",
		}
	}
	running := j.Status == StatusRunning
	now := m.stamp()
	j.Status = StatusCancelled
	if j.FinishedAt == nil {
		j.FinishedAt = &now
	}
	j.audio = nil
	cancel := j.cancel
	m.mu.Unlock()

	m.save(j)
	if running && cancel != nil {
		// 中止在途的上游请求。注意：上游可能已经合成完并扣费，
		// 取消只保证「我们不再等它」，不保证「不花钱」。
		cancel()
	}
	return nil
}

// Shutdown 停止接收新任务并等待在途任务收尾。
func (m *Manager) Shutdown(ctx context.Context) error {
	m.baseCancel()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------- worker ----------------

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.baseCtx.Done():
			return
		case id := <-m.queue:
			m.process(id)
		}
	}
}

func (m *Manager) process(id string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ERROR] 任务 %s panic: %v\n%s", id, r, debug.Stack())
			if j := m.lookup(id); j != nil {
				m.finalize(j, func(jj *Job, _ float64) {
					jj.Status = StatusFailed
					jj.Error = "内部错误：panic"
					jj.Fix = "查适配器日志里 job " + id + " 那一行栈"
				})
				m.bill(j, http.StatusBadGateway, nil, nil, fmt.Sprint(r))
			}
		}
	}()

	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.Status != StatusQueued {
		// 已取消 / 已过期 / 重复投递：直接跳过，不记账
		m.mu.Unlock()
		return
	}
	started := m.stamp()
	j.Status = StatusRunning
	j.StartedAt = &started
	ctx, cancel := context.WithCancel(m.baseCtx)
	j.cancel = cancel
	text, speed, key := j.text, j.speed, j.apiKey
	reqModel, reqVoice := j.Model, j.Voice
	rawChars, cleanedChars := j.rawChars, j.cleanedChars
	format := j.ResponseFormat
	m.mu.Unlock()
	defer cancel()

	// 必须先把 running 落盘，再去调上游。
	//
	// 漏了这一步的后果实测过：进程被 kill -9 后磁盘上还是 queued，
	// 重启后按「从未开始」恢复（never_started=true），于是告诉客户
	// 「这一单没产生上游费用，放心重提」—— 而上游其实已经被调用过、可能已扣费。
	// 一句错误的安慰比一句警告贵得多。
	if err := m.save(j); err != nil {
		log.Printf("[ERROR] 任务状态落盘失败 id=%s: %v（崩溃后会被误判为未开始）", j.ID, err)
	}

	// 本地校验失败：根本没碰到上游，不记账（与 Python 版一致）。
	// 留给账本的是「真正花了钱」的请求，不是客户写错字段的噪声。
	localFail := func(msg, fix string) {
		m.finalize(j, func(jj *Job, _ float64) {
			jj.Status = StatusFailed
			jj.Error = msg
			jj.Fix = fix
		})
	}

	voiceID, modelID, extra, rerr := upstream.ResolveVoiceAndModel(reqModel, reqVoice, m.cat, m.st.DefaultModel)
	if rerr != nil {
		localFail(rerr.Error(), "voice 传音色名（如 yun）或 10 位以上的 voice_id；model 传引擎名（eleven_v3）")
		return
	}
	if voiceID == "" {
		localFail("无法确定 voice_id：请求里要带 voice 字段",
			`body 加 voice，例如 {"model":"eleven_v3","voice":"yun"}`)
		return
	}
	spec, ok := upstream.Formats[format]
	if !ok || spec == nil {
		localFail(fmt.Sprintf("response_format '%s' 不支持", format), "改成 mp3 / opus / wav / pcm")
		return
	}

	var (
		body        []byte
		contentType string
		upCredits   *int
		upRID       *string
	)

	if m.st.Mock {
		body = pcm.Silence(1.0, 24000)
		contentType = "audio/wav"
	} else {
		up, uerr := m.uc.TextToSpeech(ctx, text, modelID, voiceID, key, spec.Output, speed, extra)
		if uerr != nil {
			if ctx.Err() != nil {
				m.markInterrupted(j, uerr)
				return
			}
			_, fix := apierr.Explain(http.StatusBadGateway, uerr.Error())
			log.Printf("[WARN] 任务 %s 调用上游失败: %v", id, uerr)
			m.finalize(j, func(jj *Job, _ float64) {
				jj.Status = StatusFailed
				jj.Error = "调用 ElevenLabs 失败：" + uerr.Error()
				jj.Fix = fix
			})
			// 超时/网络中断的上游可能已扣费，必须留痕
			m.bill(j, http.StatusBadGateway, nil, nil, uerr.Error())
			return
		}
		upRID = firstHeader(up.Header, "request-id", "x-trace-id")
		if up.StatusCode != http.StatusOK {
			if ctx.Err() != nil {
				m.markInterrupted(j, fmt.Errorf("上游返回 %d 前被中断", up.StatusCode))
				return
			}
			_, fix := apierr.Explain(up.StatusCode, string(up.Body))
			msg := upstream.ErrorMessage(up)
			log.Printf("[WARN] 任务 %s 上游返回 %d: %s", id, up.StatusCode, truncateRunes(string(up.Body), 300))
			m.finalize(j, func(jj *Job, _ float64) {
				jj.Status = StatusFailed
				jj.Error = msg
				jj.Fix = fix
			})
			// 失败任务同样要记 —— 那才是白扣钱的凭据
			m.bill(j, up.StatusCode, nil, upRID, truncateRunes(string(up.Body), 200))
			return
		}
		body = up.Body
		// OpenAI 的 wav 语义是「带 RIFF 容器的 WAV」，上游给的是裸 PCM
		if format == "wav" {
			body = pcm.Encode(body, 24000)
		}
		upCredits = upstream.ExtractCharacterCost(up.Header)
		contentType = spec.ContentType
	}

	// 计费口径与同步接口一致：bill_chars 按模式，up_credits 是上游真值
	billN, billSrc := billing.BillCharsByMode(m.ledger.Mode(), rawChars, cleanedChars, upCredits, http.StatusOK)
	dur := audioSeconds(body, format)

	m.mu.Lock()
	if j.Status == StatusCancelled {
		// 取消与完成撞在一起：听客户的。音频丢弃，按取消记账。
		j.audio = nil
		m.mu.Unlock()
		m.bill(j, 499, nil, nil, "cancelled")
		return
	}
	j.audio = body
	j.AudioBytes = len(body)
	j.ContentType = contentType
	j.VoiceID = voiceID
	j.ModelID = modelID
	j.DurationSec = dur
	j.UsageChars = billN
	j.UsageSource = billSrc
	j.UpstreamCredits = upCredits
	m.mu.Unlock()

	// 顺序：音频落盘 -> 记账 -> 翻状态。
	//
	// 三条都必须在 succeeded 之前完成，否则会出现「客户看到 succeeded，但
	// 音频还没落盘 / 账本还没这一笔」的窗口 —— 前者重启后任务变 audio_missing，
	// 后者是已交付未记账的白条，事后都无从补。
	if err := m.saveAudio(j.ID, body); err != nil {
		// 落盘失败：内存里还有，本次运行内仍可下载；
		// 重启后该任务会转 failed(audio_missing) —— 这比给一份残缺音频好
		log.Printf("[WARN] 任务音频落盘失败 id=%s: %v", j.ID, err)
	}
	m.bill(j, http.StatusOK, upCredits, upRID, "")

	now := m.stamp()
	m.mu.Lock()
	if j.Status == StatusCancelled {
		// 记账与取消撞在一起：客户取消优先，音频丢弃（账本如实留着这次上游消耗）
		j.audio = nil
		m.mu.Unlock()
		m.save(j)
		return
	}
	j.Status = StatusSucceeded
	j.FinishedAt = &now
	m.mu.Unlock()

	if err := m.save(j); err != nil {
		log.Printf("[ERROR] 任务落盘失败 id=%s: %v", j.ID, err)
	}
}

// markInterrupted 处理「调用被中断」：区分客户取消与服务关闭。
func (m *Manager) markInterrupted(j *Job, cause error) {
	m.mu.Lock()
	if j.Status == StatusCancelled {
		m.mu.Unlock()
		m.bill(j, 499, nil, nil, "cancelled")
		return
	}
	now := m.stamp()
	j.Status = StatusFailed
	j.Error = "任务被中断：" + cause.Error()
	j.Fix = "重新提交；若上游已完成合成，本次可能已产生费用"
	j.ErrorCode = CodeShutdown
	j.audio = nil
	j.FinishedAt = &now
	m.mu.Unlock()

	m.save(j)
	m.bill(j, http.StatusBadGateway, nil, nil, cause.Error())
}

// finalize 把任务推进到终态。返回 false 表示任务已被客户取消，本次不得覆盖。
func (m *Manager) finalize(j *Job, mutate func(*Job, float64)) bool {
	now := m.stamp()
	m.mu.Lock()
	if j.Status == StatusCancelled {
		m.mu.Unlock()
		return false
	}
	mutate(j, now)
	j.FinishedAt = &now
	m.mu.Unlock()
	m.save(j)
	return true
}

// bill 记一行账。同一任务只记一次（取消与失败可能同时想记）。
func (m *Manager) bill(j *Job, status int, credits *int, upRID *string, errMsg string) {
	if m.ledger == nil {
		return
	}
	m.mu.Lock()
	if j.billed {
		m.mu.Unlock()
		return
	}
	j.billed = true
	snap := *j
	m.mu.Unlock()

	elapsed := 0
	if snap.StartedAt != nil {
		elapsed = int((m.stamp() - *snap.StartedAt) * 1000)
	}
	entry := billing.Entry{
		RID:          "job:" + snap.ID,
		UpRID:        upRID,
		Model:        snap.Model,
		Voice:        firstNonEmpty(snap.VoiceID, snap.Voice),
		InChars:      snap.rawChars,
		SentChars:    snap.cleanedChars,
		UpCredits:    credits,
		Status:       status,
		ElapsedMS:    elapsed,
		ClientIP:     snap.ClientIP,
		TokenName:    snap.TokenName,
		Error:        errMsg,
		RawChars:     snap.rawChars,
		CleanedChars: snap.cleanedChars,
		CleanRules:   snap.cleanRules,
		Async:        true,
	}
	if err := m.ledger.Record(entry); err != nil {
		log.Printf("[ERROR] 任务账本写入失败 id=%s: %v", snap.ID, err)
	}
}

// ---------------- 内部工具 ----------------

// lookup 在锁内取出任务指针，**返回时锁已释放**。
//
// ⚠️ 拿到的指针指向活结构体，worker 随时在改它。所以：
//   - 只允许把它交给「自己会加锁」的内部方法（finalize / bill / save / Cancel 等）；
//   - **绝不允许**在锁外读它的字段。那种读没有任何共同锁，就是 data race。
//
// 这不是理论风险：CI 的 -race job 真抓到过 —— process() 在锁内写 Status，
// 而测试拿到 lookup 的结果后直接调 Terminal() 读同一个字段，当场报
// WARNING: DATA RACE。只读场景请一律用 Snapshot。
func (m *Manager) lookup(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// Snapshot 返回任务的一份副本，可以安全地在锁外读。
//
// 与 save / bill 里的 `snap := *j` 是同一套做法：复制必须在锁内完成，
// 锁外只碰副本。副本之间不共享写入 —— 但其中的 audio 切片仍是同一块底层数组，
// 只读没问题，别就地改它。
func (m *Manager) Snapshot(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// save 落盘。取快照后解锁再写 —— 序列化活结构体会和并发写字段打架。
func (m *Manager) save(j *Job) error {
	if m.store == nil {
		return nil
	}
	m.mu.Lock()
	snap := *j
	m.mu.Unlock()
	return m.store.Save(&snap)
}

func (m *Manager) saveAudio(id string, data []byte) error {
	if m.store == nil {
		return nil
	}
	return m.store.SaveAudio(id, data)
}

func (m *Manager) depthLocked() int {
	n := 0
	for _, j := range m.jobs {
		if j.Status == StatusQueued {
			n++
		}
	}
	return n
}

// purgeExpired 清掉超过 TTL 的已完成任务。
//
// 不清的话，长跑实例会把每条任务的音频字节一直攒着 ——
// 一个 5000 字的任务结果就是几 MB 到几十 MB。
func (m *Manager) purgeExpired() {
	now := m.stamp()
	expired := make([]string, 0, 8)

	m.mu.Lock()
	for id, j := range m.jobs {
		if j.FinishedAt == nil {
			continue
		}
		if now-*j.FinishedAt > float64(m.st.TTLSeconds) {
			delete(m.jobs, id)
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()

	if len(expired) == 0 || m.store == nil {
		return
	}
	for _, id := range expired {
		if err := m.store.Delete(id); err != nil {
			log.Printf("[WARN] 过期任务清理失败 id=%s: %v", id, err)
		}
	}
}

// stamp 返回秒级浮点时间。用 float 而不是整数：Python 版返回的就是 time.time()，
// M6 双跑对账时 1 秒精度的差异会被当成真差异。
func (m *Manager) stamp() float64 {
	return float64(m.now().UnixNano()) / 1e9
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func firstHeader(h http.Header, names ...string) *string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			vv := v
			return &vv
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
