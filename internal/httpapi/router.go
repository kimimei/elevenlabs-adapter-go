package httpapi

import (
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/buildinfo"
	"github.com/kimimei/elevenlabs-adapter-go/internal/web"
)

// Router 组装路由。
func (a *App) Router() *gin.Engine {
	r := gin.New()
	r.RedirectTrailingSlash = false
	r.Use(gin.Recovery())
	r.Use(a.guard())

	r.POST("/v1/audio/speech", a.handleSpeech)

	// 长文本异步任务。路由总是注册，开关只影响「能不能提交」——
	// 这样已生成的任务在关掉开关后仍然查得到、下载得到。
	r.POST("/v1/audio/speech/jobs", a.handleJobSubmit)
	r.GET("/v1/audio/speech/jobs", a.handleJobList)
	r.GET("/v1/audio/speech/jobs/:id", a.handleJobGet)
	r.GET("/v1/audio/speech/jobs/:id/audio", a.handleJobAudio)
	r.DELETE("/v1/audio/speech/jobs/:id", a.handleJobCancel)

	// 在线试听（免注册，服务端 Key，四道闸门见 demo.go）
	r.GET("/v1/demo/config", a.handleDemoConfig)
	r.POST("/v1/demo/speech", a.handleDemoSpeech)

	r.GET("/v1/models", a.handleModels)
	r.GET("/v1/presets", a.handlePresets)
	r.GET("/v1/voices", a.handleVoices)
	r.GET("/v1/usage", a.handleUsage)
	r.GET("/healthz", a.handleHealthz)

	// ElevenLabs 独占能力端点。默认关闭（ADAPTER_CAPABILITIES_ENABLED=0）：
	// 这些端点的计费契约与 /v1/audio/speech 不同，必须在隔离实例上显式开启。
	// /v1/capabilities 始终可读，用于声明实现状态与验证边界。
	if a.Cfg.CapabilitiesEnabled {
		r.POST("/v1/text-to-speech/:voiceId/with-timestamps", a.capabilityHandler("with-timestamps"))
		r.POST("/v1/text-to-dialogue", a.capabilityHandler("text-to-dialogue"))
		r.POST("/v1/forced-alignment", a.capabilityHandler("forced-alignment"))
		r.POST("/v1/voices/add", a.capabilityHandler("voices-add"))
	}
	r.GET("/v1/capabilities", a.handleCapabilities)

	// 试听 + 配置生成器单页（纯静态，不依赖 Key）。
	// 静态页里的 API 调用签名必须与 Python 版完全一致，否则页面某个按钮会 404。
	if st := web.FS(); st != nil {
		if _, err := fs.Stat(st, "index.html"); err == nil {
			r.GET("/", func(c *gin.Context) {
				data, err := fs.ReadFile(st, "index.html")
				if err != nil {
					c.String(http.StatusInternalServerError, "index.html 读取失败")
					return
				}
				c.Data(http.StatusOK, "text/html; charset=utf-8", data)
			})
			r.StaticFS("/static", http.FS(st))
		}
	}
	return r
}

// handleHealthz 健康检查。
//
// 必须是「什么都不依赖」的：上游挂了、账本目录不可写，它都应该照常返回 ok。
// 它下面是运维在白名单配错时唯一能自救的入口 —— 所以 middleware 里给了豁免。
//
// jobs 段只在开启时出现（与 Python 版一致）。队列深度放进健康检查是刻意的：
// 接了监控的话，「任务堆积」能在告警里看见，而不是等客户来问「我的单子卡住了」。
func (a *App) handleHealthz(c *gin.Context) {
	out := gin.H{
		"status":  "ok",
		"version": buildinfo.Version,
		"mock":    a.Cfg.Mock,
		"catalog": a.Catalog.EngineIDs,
		"demo":    a.Cfg.DemoEnabled,
	}
	if a.Cfg.JobsEnabled {
		out["jobs"] = gin.H{
			"enabled":     true,
			"queue_depth": a.Jobs.QueueDepth(),
			"max_pending": a.Jobs.MaxPending(),
			"workers":     a.Jobs.Workers(),
		}
	}
	c.PureJSON(http.StatusOK, out)
}
