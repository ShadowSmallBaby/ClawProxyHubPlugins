// upstream.go — Raccoon HTTP：API 常量 + 业务请求 helper（Bearer + 归属头）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

const (
	apiBase = "https://xiaohuanxiong.com"

	authPrefix   = "/api/web/auth/v1"
	llmPrefix    = "/api/web/llm/v2"
	pointsPrefix = "/api/web/points/v1"
	desktopPfx   = "/api/web/desktop/v1"

	// 桌面端身份常量
	defaultUA = "Raccoon Work/1.0.35 (Windows)"

	qrPollInterval = 2 * time.Second
	qrTimeout      = 3 * time.Minute
)

// bizRequest 发一次业务请求并拆信封（{ok,code,message,data}，code===0 成功）。
// X-Client-Platform 对 desktop/v1/login/points/grant 必需。
func (p *plugin) bizRequest(ctx context.Context, cred *credential, method, fullURL string, body []byte) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	// 个人账号为空串；客户端总是发送该头
	req.Header.Set("X-Org-Code", "")
	req.Header.Set("X-Raccoon-Language", "zh")
	req.Header.Set("X-Client-Platform", p.clientPlatform())
	req.Header.Set("X-Client-Version", p.clientVersion())
	req.Header.Set("User-Agent", defaultUA)
	if cred.DeviceID != "" {
		req.Header.Set("X-Client-Device-ID", cred.DeviceID)
	}

	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env struct {
		Code    float64                `json:"code"`
		Message string                 `json:"message"`
		Details string                 `json:"details"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("响应解析失败: HTTP %d", resp.StatusCode)
	}
	code := env.Code
	if code == 0 && resp.StatusCode >= 400 {
		code = float64(resp.StatusCode)
	}
	if code != 0 {
		msg := env.Message
		if msg == "" {
			msg = env.Details
		}
		if msg == "" {
			msg = fmt.Sprintf("业务码 %v", code)
		}
		return nil, &bizError{code: int64(code), msg: msg}
	}
	return env.Data, nil
}

// bizError 业务信封错误（code 非 0）。
type bizError struct {
	code int64
	msg  string
}

func (e *bizError) Error() string { return fmt.Sprintf("%s（%d）", e.msg, e.code) }

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hc(cred *credential) *http.Client {
	return sdk.UpstreamClient(cred.proxyURL)
}

// postUpstream 带业务头 POST（对话走 llm 前缀，不带 X-Client-Platform 也无碍）。
func (p *plugin) postUpstream(ctx context.Context, cred *credential, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("X-Org-Code", "")
	req.Header.Set("X-Raccoon-Language", "zh")
	req.Header.Set("User-Agent", defaultUA)
	if cred.DeviceID != "" {
		req.Header.Set("X-Client-Device-ID", cred.DeviceID)
	}
	return p.hc(cred).Do(req)
}

var _ = shared.Truncate
