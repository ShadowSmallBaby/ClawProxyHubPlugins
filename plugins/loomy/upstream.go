// upstream.go — Loomy HTTP：聊天 SSE 建流（含瞬时过载重试）+ 额度/身份 GET。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	baseURL    = "https://loomy.xunfei.cn"
	chatAPI    = baseURL + "/web/api/chat/completions"
	apiPoints  = "/web/api/auth/points-summary"
	apiMe      = "/web/api/auth/me"
	apiProfile = "/web/api/auth/profile"

	// defaultUserAgent 桌面浏览器形态 UA（可经设置 user_agent 覆盖）。
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

	maxAttempts = 3
)

// transientMarkers Loomy 用 200 + JSON 体报的可重试过载信号（非 HTTP 错误码）。
var transientMarkers = []string{"conversation_busy", "nonterminal run", "900000", "Athena"}

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（SSE 流式：不限制总时长，由 sdk 统一构造）。
func (p *plugin) hc(cred *credential) *http.Client {
	key := ""
	if cred != nil {
		key = cred.proxyURL
	}
	if c, ok := proxyClients.Load(key); ok {
		return c.(*http.Client)
	}
	c := sdk.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
}

// loomyErr 带 HTTP 状态码的错误（用于会话失效 / 限流 / 502 分类）。
type loomyErr struct {
	status int
	msg    string
}

func (e *loomyErr) Error() string { return e.msg }

func loomyErrf(status int, format string, a ...interface{}) *loomyErr {
	return &loomyErr{status: status, msg: fmt.Sprintf(format, a...)}
}

// statusOf 取错误携带的 HTTP 状态码，非 loomyErr 归 502。
func statusOf(err error) int {
	var le *loomyErr
	if errors.As(err, &le) {
		return le.status
	}
	return 502
}

// isAuthErr 判断是否鉴权失效（401/403 或含未认证标记）——不跨账号重试，提示重登。
func isAuthErr(err error) bool {
	if err == nil {
		return false
	}
	if s := statusOf(err); s == 401 || s == 403 {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "UNAUTHENTICATED") || strings.Contains(msg, "请先登录")
}

// isTransient 可重试的瞬时过载：429/409/425/5xx，或体内含过载标记。
func isTransient(status int, body string) bool {
	if status == 409 || status == 425 || status == 429 || status >= 500 {
		return true
	}
	for _, m := range transientMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// chatHeaders 聊天请求头（Cookie 绝不落日志）。
func (p *plugin) chatHeaders(cred *credential) map[string]string {
	return map[string]string{
		"Accept":       "*/*",
		"Content-Type": "application/json",
		"Origin":       baseURL,
		"Referer":      baseURL + "/web",
		"Cookie":       cred.Cookie,
		"User-Agent":   p.userAgentStr(),
	}
}

// openStream POST 聊天端点并重试瞬时过载（同账号，字节未下发前）。
// 成功 = HTTP 200 + text/event-stream；Loomy 的失败以 200+JSON 或非 200 出现，转错误。
func (p *plugin) openStream(ctx context.Context, cred *credential, payload map[string]interface{}) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	var last error = loomyErrf(502, "Loomy 服务不可用")
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", chatAPI, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, v := range p.chatHeaders(cred) {
			req.Header.Set(k, v)
		}
		resp, err := p.hc(cred).Do(req)
		if err != nil {
			last = loomyErrf(502, "请求失败: %v", err)
		} else if resp.StatusCode == 200 && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			return resp, nil
		} else {
			text := string(shared.ReadLimitedResp(resp, 8192))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				// 成功码但非 SSE = Loomy 以 JSON 体报错（Athena 900000 等）。
				last = loomyErrf(502, "Loomy 返回错误: %s", shared.Truncate(text, 300))
			} else {
				last = loomyErrf(resp.StatusCode, "Loomy HTTP %d: %s", resp.StatusCode, shared.Truncate(text, 300))
			}
			if !isTransient(resp.StatusCode, text) {
				return nil, last
			}
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
	}
	return nil, last
}

// postJSON POST 业务接口（first-login 等写端点）；401/403 → 鉴权错误。
func (p *plugin) postJSON(ctx context.Context, cred *credential, path string, body map[string]interface{}) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+path, bytes.NewBufferString(jsonEncode(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cred.Cookie)
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Referer", baseURL+"/web")
	req.Header.Set("User-Agent", p.userAgentStr())

	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := shared.ReadLimitedResp(resp, 64*1024)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, loomyErrf(resp.StatusCode, "HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	if resp.StatusCode != 200 {
		return nil, loomyErrf(resp.StatusCode, "HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}
	return obj, nil
}

// jsonEncode 简单对象序列化（任务上报体只有空对象/简单键值）。
func jsonEncode(body map[string]interface{}) string {
	if len(body) == 0 {
		return "{}"
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// getJSON GET 额度 / 身份接口；401/403 → 鉴权错误。
func (p *plugin) getJSON(ctx context.Context, cred *credential, path string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cred.Cookie)
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Referer", baseURL+"/web")
	req.Header.Set("User-Agent", p.userAgentStr())

	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := shared.ReadLimitedResp(resp, 64*1024)
	if resp.StatusCode != 200 {
		return nil, loomyErrf(resp.StatusCode, "HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}
	return obj, nil
}
