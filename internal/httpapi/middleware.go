package httpapi

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 白名单豁免路径。健康检查必须豁免：白名单填错时唯一的自查手段就是 /healthz，
// 把它也拦住等于让人瞎填（实测踩过：配错后连健康检查都 403，无法判断服务死活）。
var exemptPaths = map[string]bool{"/healthz": true}

// guard 是三道前置闸门：来源 IP 白名单 + 并发上限 + 请求体上限（含 chunked）。
//
// 没有这几道闸门，把 8123 暴露到公网等于把上游 Key 借给全互联网：
// 任何人都能 POST 一段超长文本，既刷额度也打内存。
func (a *App) guard() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		if len(a.Cfg.AllowIPs) > 0 && !exemptPaths[path] {
			host := localAddr(c.Request.RemoteAddr)
			if !containsStr(a.Cfg.AllowIPs, host) {
				writeErr(c, http.StatusForbidden,
					"来源 IP 不在白名单（ADAPTER_ALLOW_IPS）："+host,
					"permission_error",
					"填**发起连接的地址**，不是真实客户端 IP：反代转发时这里看到的是反代的地址"+
						"（同机 127.0.0.1、容器网关 172.17.0.1 / 172.18.0.1）",
					nil)
				c.Abort()
				return
			}
		}

		// --- 并发闸门 ---
		// 上游按并发限流。放 200 个请求同时进来，ElevenLabs 会挨个返回 429，
		// 客户看到的是「随机失败」。在入口就挡住并说清楚并发上限，体验差别很大。
		if maxConc := a.Cfg.MaxConcurrency; maxConc > 0 {
			n := int(a.inflight.Load())
			if n >= maxConc {
				writeErr(c, http.StatusTooManyRequests,
					fmt.Sprintf("并发已满：当前 %d / 上限 %d。队列已满，请稍后重试。", n, maxConc),
					"rate_limit_error",
					"客户端加退避重试（1-3 秒）；或调大 ADAPTER_MAX_CONCURRENCY；批量任务请串行提交",
					map[string]string{
						"Retry-After":           "3",
						"X-Adapter-Concurrency": fmt.Sprintf("%d/%d", n, maxConc),
					})
				c.Abort()
				return
			}
			a.inflight.Add(1)
			defer a.inflight.Add(-1)
		}

		// --- 请求体上限 ---
		// 只看 Content-Length 是能被绕过的：客户端用 Transfer-Encoding: chunked
		// 逐块发送时压根不带 Content-Length，检查直接跳过，超大请求照样进解析。
		// 所以无 Content-Length 时改用 MaxBytesReader，边收边判，超了立刻掐断。
		cl := c.Request.ContentLength
		if cl > a.Cfg.MaxBodyBytes {
			a.tooLarge(c, cl)
			c.Abort()
			return
		}
		if cl < 0 {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, a.Cfg.MaxBodyBytes)
		}

		c.Next()
	}
}

// tooLarge 输出 413，并给出「改用异步任务」的具体出路。
func (a *App) tooLarge(c *gin.Context, size int64) {
	writeErr(c, http.StatusRequestEntityTooLarge,
		fmt.Sprintf("请求体过大：%d 字节，上限 %d 字节。长文本请用异步任务接口。",
			size, a.Cfg.MaxBodyBytes),
		"invalid_request_error",
		"短文本按 2000 字左右切分分次调用；长文本改用 POST /v1/audio/speech/jobs"+
			"（提交后轮询），或调大 ADAPTER_MAX_BODY_BYTES（默认 2 MiB）",
		nil)
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
