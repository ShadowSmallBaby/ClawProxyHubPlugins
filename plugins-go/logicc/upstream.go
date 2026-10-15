// upstream:Logicc API 出站收敛(Bearer + 401 刷新重试 + 流式头)。
package logicc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// reqOpts 一次上游请求的可选参数。
type reqOpts struct {
	Method string
	Body   interface{} // 非 nil 时 JSON 编码并带 Content-Type
	Stream bool        // 流式:Accept/Stream-Response-Protocol 按 AI SDK v6 声明
}

// upstream 发一次 Logicc API 请求。401 时强制刷新凭据并重试一次(轮换后的 cookie 写回 blob)。
// 返回的 resp.Body 由调用方关闭;非 2xx 已读回 body 并转成带状态码的错误。
func (p *plugin) upstream(ctx context.Context, cred *credential, path string, opts reqOpts) (*http.Response, error) {
	resp, err := p.do(ctx, cred, path, opts)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		if _, rerr := p.refreshToken(ctx, cred, true); rerr != nil {
			return nil, rerr
		}
		return p.do(ctx, cred, path, opts)
	}
	return resp, err
}

// do 单次请求(内部),不处理 401 重试。
func (p *plugin) do(ctx context.Context, cred *credential, path string, opts reqOpts) (*http.Response, error) {
	token, err := p.token(ctx, cred)
	if err != nil {
		return nil, err
	}
	method := shared.OrDefault(opts.Method, "GET")
	var body io.Reader
	if opts.Body != nil {
		raw, err := json.Marshal(opts.Body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.apiBase()+"/"+strings.TrimLeft(path, "/"), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if opts.Stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Stream-Response-Protocol", "ai-sdk-v6")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if opts.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		label := strings.SplitN(strings.TrimLeft(path, "/"), "/", 2)[0]
		if label == "chats" {
			label = "聊天"
		}
		return nil, shared.HTTPError{Code: mapUpstreamStatus(resp.StatusCode),
			Message: fmt.Sprintf("上游 %s 请求失败(HTTP %d): %s", label, resp.StatusCode, shared.Truncate(strings.TrimSpace(string(raw)), 300))}
	}
	return resp, nil
}

// mapUpstreamStatus 上游状态码 → 信封错误码。
// 401/403 原样保留,让核心的凭据恢复链(刷新 → 换号)按预期工作;
// 402(欠费)与 429 归一为 429,触发核心的限流退避与换号。
func mapUpstreamStatus(status int) int32 {
	switch status {
	case 400, 413, 422:
		return 400
	case 401, 403:
		return int32(status)
	case 402, 429:
		return 429
	}
	return 502
}

// token 取可用凭据:仍有效的 JWT 直接复用,否则刷新。
func (p *plugin) token(ctx context.Context, cred *credential) (string, error) {
	// 对话中途换到的 JWT / Cookie 只在本进程有效(无法经 RPC 回写核心),先查进程内缓存。
	if token, ok := cachedToken(cred); ok {
		return token, nil
	}
	if jwtFresh(cred.Token) {
		storeToken(cred)
		return cred.Token, nil
	}
	return p.refreshToken(ctx, cred, false)
}
