package httpapi

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/apierr"
	"github.com/kimimei/elevenlabs-adapter-go/internal/jobs"
)

// POST /v1/audio/speech/jobs —— 提交长文本任务，立刻返回 job_id。
//
// 为什么必须异步：网关超时 60~120 秒，几千字的合成远超这个时间。
// 客户看到「提交成功 → 等 30 秒 → 504」，第一次接触就流失。
func (a *App) handleJobSubmit(c *gin.Context) {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			a.tooLarge(c, a.Cfg.MaxBodyBytes+1)
			return
		}
		writeErr(c, http.StatusBadRequest, apierr.JoinLoc("body", "请求体读取失败"),
			"invalid_request_error", validationFix, nil)
		return
	}

	in, perr := parseSpeechBody(raw, a.Cfg.DefaultModel)
	if perr != nil {
		writeErr(c, http.StatusBadRequest, apierr.JoinLoc(perr.field, perr.message),
			"invalid_request_error", perr.fix, nil)
		return
	}

	apiKey := a.resolveAPIKey(c.GetHeader("Authorization"), c.GetHeader("X-Api-Key"), c.GetHeader("Api-Key"))
	if apiKey == "" && !a.Cfg.Mock {
		writeErr(c, http.StatusUnauthorized, "缺少 ElevenLabs API Key",
			"authentication_error",
			"在请求头加 Authorization: Bearer sk_xxx；中转站场景下这个 Key 是渠道密钥，不是客户令牌",
			nil)
		return
	}

	res, serr := a.Jobs.Submit(jobs.SubmitParams{
		Model:          in.Model,
		Input:          in.Input,
		Voice:          in.Voice,
		ResponseFormat: in.ResponseFormat,
		Speed:          in.Speed,
		TextClean:      in.TextClean,
		APIKey:         apiKey,
		ClientIP:       localAddr(c.Request.RemoteAddr),
		TokenName:      tokenLabel(firstNonEmpty(c.GetHeader("Authorization"), c.GetHeader("X-Api-Key"), c.GetHeader("Api-Key"))),
	})
	if serr != nil {
		writeJobErr(c, serr)
		return
	}

	pollURL := "/v1/audio/speech/jobs/" + res.Job.ID
	setHeader(c, "Location", pollURL)
	c.PureJSON(http.StatusAccepted, gin.H{
		"id": res.Job.ID,
		// 固定 queued，不读 res.Job.Status。
		//
		// 读共享状态会得到非确定的结果：worker 可能已经在响应写出之前把状态改成
		// running，于是同一个请求，快机器回 running、慢机器回 queued。
		// 202 的语义是「已受理」，这一刻的事实就是排队 —— 客户拿到 running
		// 只会以为已经开始合成，然后盯着一个不动的进度等。
		"status":            jobs.StatusQueued,
		"chars":             res.Job.Chars,
		"poll_url":          pollURL,
		"estimated_seconds": res.EstimatedSeconds,
		"queue_position":    res.Position,
		"queue_depth":       res.Depth,
		"workers":           res.Workers,
	})
}

// GET /v1/audio/speech/jobs —— 任务列表（运维视角：队列深度与失败原因）。
func (a *App) handleJobList(c *gin.Context) {
	if !a.Jobs.Enabled() {
		writeJobErr(c, a.Jobs.DisabledError())
		return
	}
	out := a.Jobs.List(50)
	c.PureJSON(http.StatusOK, gin.H{
		"object":      "list",
		"total":       out.Total,
		"queue_depth": out.QueueDepth,
		"workers":     out.Workers,
		"by_status":   out.ByStatus,
		"data":        out.Data,
	})
}

// GET /v1/audio/speech/jobs/:id —— 轮询状态。
func (a *App) handleJobGet(c *gin.Context) {
	if !a.Jobs.Enabled() {
		writeJobErr(c, a.Jobs.DisabledError())
		return
	}
	view, rj := a.Jobs.View(c.Param("id"))
	if rj != nil {
		writeJobErr(c, rj)
		return
	}
	c.PureJSON(http.StatusOK, view)
}

// GET /v1/audio/speech/jobs/:id/audio —— 下载结果音频。
//
// 这里刻意**不检查** ADAPTER_JOBS_ENABLED：任务一旦生成，结果就该能取走。
// 开关只控制「能不能提交新任务」，不该让已完成的结果突然取不到。
func (a *App) handleJobAudio(c *gin.Context) {
	af, rj := a.Jobs.AudioOf(c.Param("id"))
	if rj != nil {
		writeJobErr(c, rj)
		return
	}
	setHeader(c, "X-Usage-Input-Characters", strconv.Itoa(af.Chars))
	setHeader(c, "Content-Disposition", `attachment; filename="`+c.Param("id")+"."+af.Format+`"`)
	setHeader(c, "Cache-Control", "no-store")
	if af.VoiceID != "" {
		setHeader(c, "X-ElevenLabs-Voice", af.VoiceID)
	}
	if af.ModelID != "" {
		setHeader(c, "X-ElevenLabs-Model", af.ModelID)
	}
	contentType := af.ContentType
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	c.Data(http.StatusOK, contentType, af.Data)
}

// DELETE /v1/audio/speech/jobs/:id —— 取消排队中的任务。
func (a *App) handleJobCancel(c *gin.Context) {
	id := c.Param("id")
	if err := a.Jobs.Cancel(id); err != nil {
		writeJobErr(c, err)
		return
	}
	c.PureJSON(http.StatusOK, gin.H{"id": id, "status": jobs.StatusCancelled})
}

// writeJobErr 把 jobs 包的 Reject 映射成 HTTP 响应。
func writeJobErr(c *gin.Context, err error) {
	var rj *jobs.Reject
	if errors.As(err, &rj) {
		writeErr(c, rj.Status, rj.Message, rj.Code, rj.Fix, rj.Headers)
		return
	}
	log.Printf("[ERROR] 任务接口内部错误: %v", err)
	writeErr(c, http.StatusInternalServerError, "内部错误："+err.Error(),
		"internal_error", "查看适配器日志", nil)
}
