// upstream.go — HTTP client、网关/管理面请求头与管理面调用（会话过期自动重登）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

var proxyClients sync.Map // proxyURL → *http.Client

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

// gatewayHeaders /v1 网关请求头（api_key）。ua：对话用核心下发（路由 > 全局 > 客户端），目录用 modelsUA；空则不设置。
func gatewayHeaders(cred *credential, ua string) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + cred.APIKey,
		"Content-Type":  "application/json",
	}
	if ua != "" {
		h["User-Agent"] = ua
	}
	return h
}

// managementHeaders 管理面请求头：优先系统访问令牌，否则用站点会话（都带 New-Api-User + 浏览器 UA）。
func managementHeaders(cred *credential, site *siteConfig) map[string]string {
	if cred.AccessToken == "" {
		return cred.session().headers(site)
	}
	h := map[string]string{
		"Authorization": "Bearer " + cred.AccessToken,
		"Content-Type":  "application/json",
		"User-Agent":    site.BrowserUA,
	}
	if cred.UserID > 0 {
		h["New-Api-User"] = strconv.Itoa(cred.UserID)
	}
	return h
}

func (p *plugin) do(ctx context.Context, cred *credential, method, rawURL string, headers map[string]string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return p.hc(cred).Do(req)
}

// apiEnvelope New API 管理面信封 {success, message, data}。
type apiEnvelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// managementJSON 管理面调用 → data；无系统令牌时靠会话，会话过期/被拒且有密码则自动重登一次。
func (p *plugin) managementJSON(ctx context.Context, cred *credential, site *siteConfig, method, path string) (json.RawMessage, error) {
	canRelogin := cred.AccessToken == "" && cred.Password != ""
	if canRelogin && cred.sessionStale() {
		if err := p.relogin(ctx, cred, site); err != nil {
			return nil, err
		}
		canRelogin = false
	}
	data, err := p.managementOnce(ctx, cred, site, method, path)
	if _, isAuth := err.(*authError); isAuth && canRelogin {
		if rerr := p.relogin(ctx, cred, site); rerr != nil {
			return nil, rerr
		}
		data, err = p.managementOnce(ctx, cred, site, method, path)
	}
	return data, err
}

// relogin 账号密码重登刷新会话（写回 cred；持久化由 Refresh 回传 Blob 完成）。
func (p *plugin) relogin(ctx context.Context, cred *credential, site *siteConfig) error {
	auth, err := p.loginPassword(ctx, site, cred.Username, cred.Password)
	if err != nil {
		return &authError{msg: "重新登录失败: " + err.Error()}
	}
	cred.applySession(auth)
	return nil
}

func (p *plugin) managementOnce(ctx context.Context, cred *credential, site *siteConfig, method, path string) (json.RawMessage, error) {
	resp, err := p.do(ctx, cred, method, site.BaseURL+path, managementHeaders(cred, site), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env apiEnvelope
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		msg := shared.Truncate(string(raw), 200)
		if json.Unmarshal(raw, &env) == nil && env.Message != "" {
			msg = env.Message
		}
		return nil, &authError{msg: fmt.Sprintf("management auth failed: HTTP %d %s", resp.StatusCode, msg)}
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("upstream non-json (HTTP %d): %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	if !env.Success {
		return nil, &apiError{message: env.Message}
	}
	return env.Data, nil
}

type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

// apiError 管理面业务拒绝（success=false），保留原始 message 供按文案分支（如「今日已签到」）。
type apiError struct{ message string }

func (e *apiError) Error() string { return e.message }
