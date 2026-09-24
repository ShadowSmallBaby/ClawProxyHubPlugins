// auth.go — 登录（WorkOS 设备码 / refreshToken 文件）、凭据刷新与 token 维护。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// Login 两种方式：device（WorkOS 设备码 → cline register）/ refresh_file（直接填 refreshToken）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "refresh_file":
		var c credential
		if err := json.Unmarshal([]byte(req.Form["content"]), &c); err != nil || c.RefreshToken == "" {
			// 容错：裸粘 refreshToken 字符串
			rt := strings.TrimSpace(req.Form["content"])
			if rt == "" {
				return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 refreshToken（JSON 或裸字符串）"}}, nil
			}
			c = credential{RefreshToken: rt}
		}
		return p.loginByRefresh(ctx, &c)

	case "device":
		return p.loginDevice(ctx, req)
	}
	return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
}

// loginByRefresh 用 refreshToken 换 accessToken 建档；失败按 401 报出。
func (p *plugin) loginByRefresh(ctx context.Context, c *credential) (*pb.LoginResult, error) {
	if err := p.refreshCred(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	return loginDone(c), nil
}

// loginDevice WorkOS 设备码授权：发起 → 用户浏览器授权 → 手动确认后 register。
// 两步协议：State 空 = 发起（返回 open_url），State 非空 = 轮询确认。
func (p *plugin) loginDevice(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		// 第一步：申请设备码，返回授权链接
		form := url.Values{"client_id": {workosClient}}
		httpResp, err := postForm(ctx, deviceAuth, form)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
		}
		defer httpResp.Body.Close()
		if httpResp.StatusCode != 200 {
			body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: fmt.Sprintf("device auth failed: HTTP %d %s", httpResp.StatusCode, shared.Truncate(string(body), 200))}}, nil
		}
		var d struct {
			DeviceCode              string `json:"device_code"`
			UserCode                string `json:"user_code"`
			VerificationURI         string `json:"verification_uri"`
			VerificationURIComplete string `json:"verification_uri_complete"`
			Interval                int    `json:"interval"`
			ExpiresIn               int    `json:"expires_in"`
		}
		if err := json.NewDecoder(httpResp.Body).Decode(&d); err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "device auth decode: " + err.Error()}}, nil
		}
		// device_code 与 cline register 一起走第二步
		state, _ := json.Marshal(map[string]string{"device_code": d.DeviceCode})
		authURL := shared.OrDefault(d.VerificationURIComplete, d.VerificationURI)
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url", Url: authURL,
			Prompt: map[string]string{
				"zh": fmt.Sprintf("已打开授权页，登录后输入代码 %s，完成后点击「我已完成授权」", d.UserCode),
				"en": fmt.Sprintf("Auth page opened; sign in with code %s, then confirm below", d.UserCode),
			},
			State: state,
			Wait:  false,
			Fields: []*pb.AuthField{{
				Name: "confirm", Label: map[string]string{"zh": "确认授权", "en": "Confirm"},
				Type: "confirm", Placeholder: "",
			}},
		}}, nil
	}

	// 第二步：用 device_code 换 WorkOS token，再 register 到 cline
	var s struct {
		DeviceCode string `json:"device_code"`
	}
	if json.Unmarshal(req.State, &s) != nil || s.DeviceCode == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "state 已失效，请重新发起"}}, nil
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {s.DeviceCode},
		"client_id":   {workosClient},
	}
	httpResp, err := postForm(ctx, authenticate, form)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
	}
	defer httpResp.Body.Close()
	var a struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	_ = json.NewDecoder(httpResp.Body).Decode(&a)
	if a.AccessToken == "" {
		msg := shared.OrDefault(a.ErrorDesc, shared.OrDefault(a.Error, "尚未完成授权，请先在浏览器完成登录"))
		if a.Error == "authorization_pending" || a.Error == "slow_down" {
			msg = "尚未完成授权，请先在浏览器完成登录"
		}
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: msg}}, nil
	}

	// register 到 cline 换自家凭据
	reg, err := p.registerCline(ctx, a.AccessToken, a.RefreshToken)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
	}
	return loginDone(reg), nil
}

// registerCline 用 WorkOS token 换 cline 自家凭据。
func (p *plugin) registerCline(ctx context.Context, workosAccess, workosRefresh string) (*credential, error) {
	body, _ := json.Marshal(map[string]string{"accessToken": workosAccess, "refreshToken": workosRefresh})
	resp, err := postJSON(ctx, nil, apiBase+"/auth/register", map[string]string{"Content-Type": "application/json"}, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cline register failed: HTTP %d %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var e struct {
		Data struct {
			AccessToken  string          `json:"accessToken"`
			RefreshToken string          `json:"refreshToken"`
			ExpiresAt    any             `json:"expiresAt"`
			UserInfo     json.RawMessage `json:"userInfo"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Data.RefreshToken == "" {
		return nil, fmt.Errorf("cline register 响应缺少 refreshToken")
	}
	return &credential{
		RefreshToken: e.Data.RefreshToken,
		AccessToken:  e.Data.AccessToken,
		ExpiresAt:    parseExpiry(e.Data.ExpiresAt),
		User:         e.Data.UserInfo,
	}, nil
}

// refreshCred 刷新 accessToken（写回 cred）。
func (p *plugin) refreshCred(ctx context.Context, c *credential) error {
	body, _ := json.Marshal(map[string]string{"refreshToken": c.RefreshToken, "grantType": "refresh_token"})
	resp, err := postJSON(ctx, p.hc(c), apiBase+"/auth/refresh", map[string]string{"Content-Type": "application/json"}, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 {
		return fmt.Errorf("refreshToken 已失效，请重新登录")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("refresh failed: HTTP %d %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var e struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Data.AccessToken == "" {
		return fmt.Errorf("refresh 响应缺少 accessToken")
	}
	c.AccessToken = e.Data.AccessToken
	if e.Data.RefreshToken != "" {
		c.RefreshToken = e.Data.RefreshToken
	}
	c.ExpiresAt = parseExpiry(e.Data.ExpiresAt)
	return nil
}

// ensureToken accessToken 就绪（过期 / 缺失即刷新）。
func (p *plugin) ensureToken(ctx context.Context, c *credential) error {
	if c.AccessToken != "" && (c.ExpiresAt == 0 || time.Now().UnixMilli() < c.ExpiresAt-60000) {
		return nil
	}
	return p.refreshCred(ctx, c)
}

func loginDone(c *credential) *pb.LoginResult {
	blob, _ := json.Marshal(c)
	name := credentialName(c)
	return &pb.LoginResult{
		Blob: blob,
		Profile: &pb.AccountProfile{
			DisplayName: name, Healthy: true, Quota: map[string]string{},
		},
	}
}

func credentialName(c *credential) string {
	var u struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(c.User, &u)
	if u.Email != "" {
		return u.Email
	}
	return "cline-account"
}

// Refresh 刷新凭据并顺带拉余额（credits 快照）。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if err := p.refreshCred(ctx, c); err != nil {
		code := int32(503)
		if strings.Contains(err.Error(), "失效") {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	profile := &pb.AccountProfile{DisplayName: credentialName(c), Healthy: true, Quota: map[string]string{}}
	// 刷新成功后顺带拉余额，避免 credits 快照缺块（前端积分列读 CreditsJson）
	p.fetchBalance(ctx, c, profile)
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: profile}, nil
}

func postForm(ctx context.Context, rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return sdk.UpstreamClient("").Do(req)
}

// parseExpiry 上游 expiresAt 多形状（毫秒数 / RFC3339 字符串）→ unix 毫秒。
func parseExpiry(exp any) int64 {
	switch v := exp.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}
