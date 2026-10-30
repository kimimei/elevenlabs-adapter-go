package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kimimei/elevenlabs-adapter-go/internal/config"
)

func jobsHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Mock = true // 不碰真上游
		cfg.JobsEnabled = true
		cfg.JobsWorkers = 1
		cfg.JobsMaxPending = 10
		cfg.JobsMaxChars = 8000
		cfg.JobsTTLSeconds = 60
		cfg.JobsDir = dir
	}, nil)
	// 必须先停 worker 再让 TempDir 被删：worker 还在后台写任务表时删目录，
	// 报错是「The directory is not empty」—— 看上去像测试写错了，
	// 实际是后台 goroutine 与清理撞车。t.Cleanup 后进先出，所以这里晚于
	// newHarness 内部的 TempDir 注册，会先执行。
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.app.Jobs.Shutdown(ctx)
	})
	return h
}

// 提交响应里的 status 必须是 "queued"，不能是 "running"。
//
// M6 影子对账实测出来的：这一行原来读的是 `res.Job.Status`（共享状态），
// 而 worker 可能在响应写出之前就把它改成 running。于是同一个请求，
// 快机器回 running、慢机器回 queued —— 同一个 202，两次看到的答案不同。
//
// 而且 running 会让客户以为已经在合成，然后盯着一个不动的进度等。
func TestJobSubmitAlwaysReturnsQueued(t *testing.T) {
	h := jobsHarness(t)
	// 连发几次：单次测试可能刚好赶在 worker 之前，多次才暴露竞态。
	for i := 0; i < 6; i++ {
		rec := doReq(h, http.MethodPost, "/v1/audio/speech/jobs",
			`{"model":"zh-boyin","input":"提交响应的状态必须是排队","response_format":"mp3"}`, nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("状态码 = %d，期望 202；body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Status string `json:"status"`
			Chars  int    `json:"chars"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON: %v", err)
		}
		if out.Status != "queued" {
			t.Fatalf("第 %d 次提交的 status = %q，期望 queued（读共享状态会产生竞态）", i+1, out.Status)
		}
	}
}

// 任务关闭时提交应 404 + invalid_request_error（与 Python 版逐字一致）。
func TestJobSubmitDisabled(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Mock = true; cfg.JobsEnabled = false }, nil)
	rec := doReq(h, http.MethodPost, "/v1/audio/speech/jobs",
		`{"model":"zh-boyin","input":"关掉了"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Error.Code != "invalid_request_error" {
		t.Fatalf("error.code = %q，期望 invalid_request_error", out.Error.Code)
	}
}
