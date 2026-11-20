package upstream

// 非标准 ElevenLabs 能力端点的通用上游调用。
//
// 与 TextToSpeech 的关键差异：**任何失败都不重试**。
// 这些端点同样按用量计费（对白按字符、IVC 按音色创建），
// 超时只代表调用方不等了，上游大概率已经完成并扣费。
// 重试等于再扣一次钱 —— 这条铁律对所有能力端点一视同仁。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// CallCapability 把原始请求体透传到一个 ElevenLabs 能力端点。
//
// pathSuffix 必须由调用方用已校验过的参数拼好（voice_id 走 PathEscape，
// 且只允许 [A-Za-z0-9_-]）：这里不做二次校验，避免出现两处规则漂移。
// contentType 原样透传，因为 forced-alignment / voices/add 是 multipart，
// 而 with-timestamps / text-to-dialogue 是 JSON。
func (c *Client) CallCapability(ctx context.Context, method, pathSuffix string,
	raw []byte, contentType, apiKey string) (*Response, error) {

	if pathSuffix == "" || pathSuffix[0] != '/' {
		return nil, errors.New("capability path suffix must start with /")
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, c.base+pathSuffix, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造能力端点请求失败: %w", err)
	}
	req.Header.Set("xi-api-key", apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		if isTimeout(err) {
			return nil, fmt.Errorf("能力端点超时 %.0fs（不重试：上游可能已完成并扣费）", c.timeout.Seconds())
		}
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		// 读响应体失败时上游已经完成，绝不重发请求。
		return nil, fmt.Errorf("读取能力端点响应失败（不重试）: %w", err)
	}
	return &Response{StatusCode: resp.StatusCode, Body: body, Header: resp.Header}, nil
}
