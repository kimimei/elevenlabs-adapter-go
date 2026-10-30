package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Response 是上游响应的快照。不直接透传 *http.Response，
// 是为了让调用方拿不到 Body 句柄 —— 忘记 Close 的泄漏在长跑服务里是慢性病。
type Response struct {
	StatusCode int
	Body       []byte
	Header     http.Header
}

// Client 是 ElevenLabs 上游客户端。
type Client struct {
	base       string
	timeout    time.Duration
	maxRetries int
	hc         *http.Client
}

// NewClient 构造客户端。超时由 per-request context 控制，不用 http.Client.Timeout ——
// 后者会把「读响应体」也算进去，长音频会在这里被砍断，且报错信息看不懂。
func NewClient(base string, timeoutSeconds float64, maxRetries int) *Client {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 300
	}
	if maxRetries < 0 {
		maxRetries = 0
	}
	return &Client{
		base:       base,
		timeout:    time.Duration(timeoutSeconds * float64(time.Second)),
		maxRetries: maxRetries,
		hc:         &http.Client{},
	}
}

// TextToSpeech 调用 POST /v1/text-to-speech/{voice_id}，返回响应快照。
//
// **超时一律不重试**（关键）：上游是「合成完才返回」，客户端超时只代表自己不等了，
// 上游那一侧通常已经合成完毕并扣了费。重试 = 再扣一次。实测 87% 的字符消耗
// 来自超时重试（3000 字 × 120s 超时 × 3 次 = 扣 3 次钱，只拿到 0 份音频）。
//
// 其余失败（网络中断 / 5xx / 429）仍按 maxRetries 重试：
// 这些情况下上游大概率没有完成合成，重试相对安全。
func (c *Client) TextToSpeech(ctx context.Context, text, modelID, voiceID, apiKey,
	outputFormat string, speed *float64, extra Extra) (*Response, error) {

	payload := BuildPayload(text, modelID, outputFormat, speed, extra)
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("请求体序列化失败: %w", err)
	}
	target := TTSURL(c.base, voiceID)

	attempts := c.maxRetries + 1
	var lastErr error

	for i := 0; i < attempts; i++ {
		reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
		req, rerr := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(buf))
		if rerr != nil {
			cancel()
			return nil, fmt.Errorf("构造上游请求失败: %w", rerr)
		}
		req.Header.Set("xi-api-key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "audio/mpeg")

		resp, derr := c.hc.Do(req)
		if derr != nil {
			cancel()
			if isTimeout(derr) {
				return nil, fmt.Errorf("上游超时 %.0fs（不重试：上游大概率已合成并扣费，重试等于再扣一次）",
					c.timeout.Seconds())
			}
			lastErr = derr
			if i == attempts-1 {
				return nil, derr
			}
			sleepBackoff(ctx, i)
			continue
		}

		body, berr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if berr != nil {
			lastErr = berr
			if i == attempts-1 {
				return nil, berr
			}
			sleepBackoff(ctx, i)
			continue
		}

		out := &Response{StatusCode: resp.StatusCode, Body: body, Header: resp.Header}
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return out, nil
		}
		if i == attempts-1 {
			return out, nil
		}
		sleepBackoff(ctx, i)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("上游调用未返回结果")
}

// ErrorMessage 提取上游错误体中的可读信息（detail 可能是对象或字符串）。
func ErrorMessage(resp *Response) string {
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return fmt.Sprintf("ElevenLabs 上游错误 HTTP %d", resp.StatusCode)
	}
	detail, ok := body["detail"]
	if !ok {
		detail = body
	}
	switch d := detail.(type) {
	case map[string]any:
		if m, ok := d["message"].(string); ok && m != "" {
			return "ElevenLabs: " + m
		}
		if s, ok := d["status"].(string); ok && s != "" {
			return "ElevenLabs: " + s
		}
		return fmt.Sprintf("ElevenLabs 上游错误 HTTP %d", resp.StatusCode)
	case string:
		return "ElevenLabs: " + d
	}
	return fmt.Sprintf("ElevenLabs 上游错误 HTTP %d", resp.StatusCode)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// sleepBackoff 指数退避 0.4s / 1.2s / 3.6s。ctx 取消时立即返回，不拖住 shutdown。
func sleepBackoff(ctx context.Context, attempt int) {
	d := time.Duration(400*float64(time.Millisecond)) * time.Duration(pow3(attempt))
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func pow3(n int) int {
	r := 1
	for i := 0; i < n; i++ {
		r *= 3
	}
	return r
}

// PreviewCache 缓存音色试听链接。上游这个接口有频率限制，且 preview_url 有效期很长。
type PreviewCache struct {
	mu   sync.Mutex
	ts   time.Time
	data map[string]string
	ttl  time.Duration
}

// NewPreviewCache 构造缓存，ttl 传 0 时用 1 小时。
func NewPreviewCache(ttl time.Duration) *PreviewCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &PreviewCache{ttl: ttl, data: map[string]string{}}
}

// FetchPreviews 返回 {voice_id: preview_url}。失败时返回上一次的缓存（可能为空）。
//
// 拿不到就当没有 —— 不让目录接口因为上游抖动而 500。
func (c *Client) FetchPreviews(ctx context.Context, apiKey string, cache *PreviewCache) map[string]string {
	if apiKey == "" || cache == nil {
		return map[string]string{}
	}
	cache.mu.Lock()
	if len(cache.data) > 0 && time.Since(cache.ts) < cache.ttl {
		out := copyMap(cache.data)
		cache.mu.Unlock()
		return out
	}
	cache.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.base+"/voices?page_size=100", nil)
	if err != nil {
		return map[string]string{}
	}
	req.Header.Set("xi-api-key", apiKey)
	resp, err := c.hc.Do(req)
	if err != nil {
		cache.mu.Lock()
		out := copyMap(cache.data)
		cache.mu.Unlock()
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cache.mu.Lock()
		out := copyMap(cache.data)
		cache.mu.Unlock()
		return out
	}
	var body struct {
		Voices []struct {
			VoiceID    string `json:"voice_id"`
			PreviewURL string `json:"preview_url"`
		} `json:"voices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		cache.mu.Lock()
		out := copyMap(cache.data)
		cache.mu.Unlock()
		return out
	}
	data := make(map[string]string, len(body.Voices))
	for _, v := range body.Voices {
		if v.VoiceID != "" {
			data[v.VoiceID] = v.PreviewURL
		}
	}
	cache.mu.Lock()
	cache.ts = time.Now()
	cache.data = data
	cache.mu.Unlock()
	return copyMap(data)
}

func copyMap(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
