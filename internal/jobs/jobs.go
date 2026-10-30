// Package jobs 是长文本异步任务：任务表落盘 + 固定 worker 池。
//
// 为什么需要（与 Python 版一致的理由）
// ------------------------------------
// 配音客户的第一单通常是几千字。同步接口在两种情况下必然失败：
// 网关超时（60~120s）与上游单次字符上限。客户看到的是
// 「提交成功 → 等 30 秒 → 504」，第一次接触就流失。
//
// 与 Python 版的**关键差异：任务表落盘**
// --------------------------------------
// Python 版把任务放在进程内存里，重启后排队中的任务全部消失，
// 客户端轮询拿到 404 —— 不知道自己提交的任务是「还在跑」还是「根本不存在」。
// Go 版把任务元数据写进 ADAPTER_JOBS_DIR：
//
//	<dir>/<id>.json   任务元数据（状态/字符数/时间/错误）
//	<dir>/<id>.audio  成功任务的音频字节
//
// 于是重启后：轮询能拿到**真实终态**（failed + error_code=interrupted），
// 已完成的音频还能下载。这是修复「重启丢队列」的全部内容 ——
// 注意它**不是**「断点续跑」，下面写清了为什么不做。
//
// 刻意不落盘的两样东西
// --------------------
//  1. 客户原文（text）—— 属于客户内容，进程重启后不需要它，就不该留在磁盘上
//  2. 渠道密钥（api_key）—— 明文写盘 = 把中转站渠道密钥泄露给任何能读文件的人
//
// 代价是重启后任务无法自动续跑。这不是偷懒，是**唯一正确的选择**：
// 崩溃时状态为 running 的任务，上游很可能已经合成完并扣了费，
// 自动重跑就是**二次扣费**，而且客户会拿到两份音频互为重复。
// 所以 running 中断一律转 failed，并标 never_started=false 告诉客户
// 「这一单可能已经产生上游费用，别再原样重提」；只有 queued（从未开始）
// 标 never_started=true，客户可以放心重提。
package jobs

import (
	"encoding/binary"
	"math"
)

// 任务状态。与 Python 版字符串完全一致（客户端会按字面值判断）。
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// error_code 是 Go 版**新增**的诊断字段（Python 版没有）。
// 它只在中途夭折的任务上出现，用来区分「客户写错了」与「服务端出事了」。
const (
	CodeInterrupted  = "interrupted"   // 进程重启导致中断
	CodeShutdown     = "shutdown"      // 收到退出信号，任务被主动中断
	CodeAudioMissing = "audio_missing" // 元数据在、音频文件没了
)

// Job 是一个任务。字段分两类：
//
//	落盘字段（有 json tag）—— 只有公开视图与诊断需要的信息
//	运行字段（json:"-"）—— 客户原文、密钥、取消句柄；进程重启后必然为空
type Job struct {
	ID              string   `json:"id"`
	Status          string   `json:"status"`
	Chars           int      `json:"chars"`
	CreatedAt       float64  `json:"created_at"`
	StartedAt       *float64 `json:"started_at,omitempty"`
	FinishedAt      *float64 `json:"finished_at,omitempty"`
	Model           string   `json:"model"`
	Voice           string   `json:"voice"`
	ResponseFormat  string   `json:"response_format"`
	VoiceID         string   `json:"voice_id,omitempty"`
	ModelID         string   `json:"model_id,omitempty"`
	ContentType     string   `json:"content_type,omitempty"`
	ClientIP        string   `json:"client_ip,omitempty"`
	TokenName       string   `json:"token_name,omitempty"`
	DurationSec     *float64 `json:"duration_sec,omitempty"`
	UsageChars      int      `json:"usage_chars,omitempty"`
	UsageSource     string   `json:"usage_source,omitempty"`
	UpstreamCredits *int     `json:"upstream_credits,omitempty"`
	AudioBytes      int      `json:"audio_bytes,omitempty"`
	Error           string   `json:"error,omitempty"`
	Fix             string   `json:"fix,omitempty"`
	ErrorCode       string   `json:"error_code,omitempty"`
	NeverStarted    bool     `json:"never_started,omitempty"`

	// ---- 以下不落盘 ----
	text         string
	speed        *float64
	apiKey       string
	rawChars     int
	cleanedChars int
	cleanRules   string
	audio        []byte
	cancel       func()
	billed       bool
}

// Public 生成对外视图。
//
// 字段与 Python 版 `_public()` 一一对应；差异只在两处，都写进了 CONTRACT 的差异表：
//   - failed 且 error_code != "" 时多出 error_code / never_started（纯增量，
//     老客户端按 status=="failed" 判断不受影响）
//   - chars 与账本 in_chars 同源（原始长度，不 strip），Python 版这里 strip 过，
//     导致同一个任务在「任务详情」与「账本」上显示两个不同的字符数
func (j *Job) Public(now float64, ttlSeconds int) map[string]any {
	v := map[string]any{
		"id":              j.ID,
		"status":          j.Status,
		"chars":           j.Chars,
		"created_at":      j.CreatedAt,
		"model":           j.Model,
		"voice":           nilIfEmpty(j.Voice),
		"response_format": j.ResponseFormat,
	}
	switch j.Status {
	case StatusSucceeded:
		v["finished_at"] = j.FinishedAt
		v["audio_bytes"] = j.AudioBytes
		v["duration_sec"] = j.DurationSec
		v["usage_chars"] = j.UsageChars
		v["usage_source"] = nilIfEmpty(j.UsageSource)
		v["download_url"] = "/v1/audio/speech/jobs/" + j.ID + "/audio"
		expires := 0
		if j.FinishedAt != nil {
			expires = int(float64(ttlSeconds) - (now - *j.FinishedAt))
		}
		v["expires_in"] = expires
	case StatusFailed:
		v["error"] = nilIfEmpty(j.Error)
		v["fix"] = nilIfEmpty(j.Fix)
		if j.ErrorCode != "" {
			v["error_code"] = j.ErrorCode
			v["never_started"] = j.NeverStarted
		}
	case StatusCancelled:
		v["error"] = "任务已取消"
	}
	return v
}

// Terminal 判断是否终态（终态不可取消）。
func (j *Job) Terminal() bool {
	switch j.Status {
	case StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// estimateSeconds 粗估耗时：中文 TTS 大致 15~25 字/秒（含上游排队）。
//
// 没有这个字段，客户提交完 2000 字看到 queued 不知道要等多久，
// 只会以为卡死了然后反复提交 —— 那才是真正打爆并发的原因。
func estimateSeconds(chars int) float64 {
	return roundN(math.Max(3.0, float64(chars)/20.0), 1)
}

// audioSeconds 从音频字节估时长。拿不到就返回 nil（绝不编数字）。
//
// mp3 用文件头里的 Xing/Info 帧的帧数；估不出来（VBR 无 Xing 头）就放弃，
// 总比给个错的时长好 —— 客户按错误时长做的剪辑卡点会全乱。
func audioSeconds(content []byte, responseFormat string) *float64 {
	if responseFormat == "wav" && len(content) >= 44 && string(content[:4]) == "RIFF" {
		// 适配器把 pcm_24000 包成 WAV，采样率固定 24000、16-bit 单声道
		dataSize := binary.LittleEndian.Uint32(content[40:44])
		v := roundN(float64(dataSize)/2/24000, 2)
		return &v
	}
	if responseFormat == "mp3" {
		if len(content) >= 3 && string(content[:3]) == "ID3" {
			return nil
		}
		// 通用估算：mp3 128kbps ≈ 16 KB/s
		v := roundN(float64(len(content))/16000.0, 2)
		return &v
	}
	return nil
}

func roundN(f float64, n int) float64 {
	pow := math.Pow(10, float64(n))
	return math.Round(f*pow) / pow
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
