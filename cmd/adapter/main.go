// cmd/adapter 是适配器入口：只做装配，不做业务。
//
// 装配顺序有意为之：配置 -> 模型目录 -> 账本 -> 上游客户端 -> 路由。
// 前面任何一步失败都直接 fatal —— 带着半套依赖启动的服务，
// 会在第一个真实请求上以「无法确定 voice_id」这种误导性错误暴露问题。
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kimimei/elevenlabs-adapter-go/internal/billing"
	"github.com/kimimei/elevenlabs-adapter-go/internal/config"
	"github.com/kimimei/elevenlabs-adapter-go/internal/httpapi"
	"github.com/kimimei/elevenlabs-adapter-go/internal/upstream"
	"github.com/kimimei/elevenlabs-adapter-go/internal/voices"
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("")

	cfg := config.Load()

	cat, err := voices.Load("")
	if err != nil {
		log.Fatalf("模型目录加载失败: %v", err)
	}

	ledger := billing.NewLedger(cfg.BillingLogDir, cfg.BillingMode, cfg.BillingLogDays,
		cfg.BillingLogOff, cfg.UpCreditsPerUSD, cfg.CharsPerUSD)
	defer func() {
		if err := ledger.Close(); err != nil {
			log.Printf("账本关闭失败: %v", err)
		}
	}()

	client := upstream.NewClient(cfg.APIBase, cfg.TimeoutSeconds, cfg.MaxRetries)
	app, err := httpapi.New(cfg, cat, ledger, client)
	if err != nil {
		log.Fatalf("任务模块初始化失败: %v", err)
	}

	if cfg.Mock {
		log.Printf("[WARN] ELEVENLABS_MOCK=1：不调用 ElevenLabs 上游，返回本地生成的占位音频（仅用于链路验证）")
	}
	log.Printf("[INFO] 标准引擎 %d 个：%s", len(cat.EngineIDs), strings.Join(cat.EngineIDs, ", "))

	enabled, unknown := cat.ParsePacks(cfg.Presets)
	parts := make([]string, 0, len(cat.PackIDs()))
	for _, pid := range cat.PackIDs() {
		state := "off"
		if enabled[pid] {
			state = "on"
		}
		parts = append(parts, pid+"="+state)
	}
	if len(parts) == 0 {
		parts = append(parts, "无")
	}
	log.Printf("[INFO] 补充包：%s（ADAPTER_PRESETS=%s，未启用的包只是不列出，仍可调用）",
		strings.Join(parts, ", "), cfg.Presets)
	if len(unknown) > 0 {
		log.Printf("[WARN] ADAPTER_PRESETS 中有无法识别的包：%s（可用：%s）",
			strings.Join(unknown, ","), strings.Join(cat.PackIDs(), ","))
	}
	log.Printf("[INFO] 计费口径=%s 账本=%s 保留=%d天 上游超时=%.0fs 重试=%d",
		cfg.BillingMode, cfg.BillingLogDir, cfg.BillingLogDays, cfg.TimeoutSeconds, cfg.MaxRetries)
	if cfg.JobsEnabled {
		log.Printf("[INFO] 异步任务已开启：worker=%d 队列上限=%d 单任务上限=%d 字符 TTL=%ds 任务表=%s",
			app.Jobs.Workers(), cfg.JobsMaxPending, cfg.JobsMaxChars, cfg.JobsTTLSeconds, app.Jobs.Dir())
	} else {
		log.Printf("[INFO] 异步任务未开启（ADAPTER_JOBS_ENABLED=0）；长文本请按 2000 字分段调用同步接口")
	}
	if cfg.DemoEnabled {
		log.Printf("[INFO] 在线试听已开启：单次 %d 字符、每 IP 每小时 %d 次、音色 %s、信任 XFF=%v",
			cfg.DemoMaxChars, cfg.DemoPerHour, strings.Join(cfg.DemoVoices, ","), cfg.DemoTrustXFF)
	}

	gin.SetMode(gin.ReleaseMode)
	engine := app.Router()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	srv := &http.Server{
		Addr:    addr,
		Handler: engine,
		// 上游是「合成完才返回」，长文本动辄上百秒，写超时必须留足；
		// 这里不设 WriteTimeout，交给上游 context 控制（Python 版同样不设）。
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[INFO] listening on http://%s （mock=%v）", addr, cfg.Mock)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("[INFO] 收到退出信号，开始优雅关闭…")

	// 给在途请求 30 秒：一段 3000 字的合成可能正在上游跑，
	// 此时硬杀进程等于让客户白付一次钱却拿不到音频。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[WARN] 优雅关闭超时: %v", err)
	}
	// 先停 HTTP（不再受理新任务），再停 worker：
	// 在途任务会被中断并落成 failed(error_code=shutdown)，而不是留个「永远 running」。
	if err := app.Jobs.Shutdown(ctx); err != nil {
		log.Printf("[WARN] 任务模块关闭超时: %v", err)
	}
	log.Printf("[INFO] 已退出")
}
