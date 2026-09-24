// auth.go — 登录（凭据文件 / 手机验证码 / 浏览器 OAuth 三段式）、令牌刷新。
package main

import (
	"context"
	"encoding/base64"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	p.ensureIdentity() // 登录走浏览器形态头，先刷新全局浏览器 UA
	switch req.MethodId {
	case "auth_file":
		cred, err := parseCred([]byte(req.Form["content"]))
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
		}
		blob, _ := json.Marshal(cred)
		return &pb.LoginResult{
			Blob: blob,
			Profile: &pb.AccountProfile{
				DisplayName: shared.OrDefault(cred.Account.Nickname, shared.OrDefault(cred.Account.UID, "workbuddy-account")),
				Healthy:     true, Quota: map[string]string{},
			},
		}, nil

	case "phone_otp":
		// 第一步：手机号 → 发送验证码
		if len(req.State) == 0 {
			phone := strings.TrimSpace(req.Form["phone"])
			if phone == "" {
				return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写手机号"}}, nil
			}
			if err := p.sendSMS(ctx, phone); err != nil {
				return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
			}
			return &pb.LoginResult{Next: &pb.LoginNextStep{
				Action: "input_form",
				Prompt: map[string]string{"zh": "验证码已发送，请输入收到的短信验证码", "en": "OTP sent; enter the code you received via SMS"},
				Fields: []*pb.AuthField{{
					Name: "code", Label: map[string]string{"zh": "验证码", "en": "SMS Code"}, Type: "text", Required: true,
				}},
				State: []byte("phone:" + phone),
			}}, nil
		}
		// 第二步：验证码 → 换取 token
		state := string(req.State)
		if !strings.HasPrefix(state, "phone:") {
			return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "登录状态已失效，请重新发起"}}, nil
		}
		phone := strings.TrimPrefix(state, "phone:")
		code := strings.TrimSpace(req.Form["code"])
		if code == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写验证码"}}, nil
		}
		at, rt, err := p.loginWithSMS(ctx, phone, code)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
		}

		// 构造 .info 形态凭据：AT 的 JWT 里取身份与过期
		cred := &credential{}
		cred.Auth.AccessToken, cred.Auth.RefreshToken = at, rt
		cred.Auth.Domain = "www.codebuddy.cn"
		claims := jwtClaims(at)
		if exp, ok := claims["exp"].(float64); ok {
			cred.Auth.ExpiresAt = int64(exp) * 1000
		}
		if name, ok := claims["preferred_username"].(string); ok && name != "" {
			cred.Account.Nickname = name
		}
		blob, _ := json.Marshal(cred)
		return &pb.LoginResult{
			Blob: blob,
			Profile: &pb.AccountProfile{
				DisplayName: shared.OrDefault(cred.Account.Nickname, phone), Healthy: true, Quota: map[string]string{},
			},
		}, nil
	case "oauth":
		return p.loginBrowserAuth(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "unknown auth method: "+req.MethodId)
}

// loginBrowserAuth 浏览器客户端授权三段式：
// state → open_url → 用户授权后插件侧轮询 token → 取账号资料 → .info 建档。
func (p *plugin) loginBrowserAuth(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		// 第一步：申请上游 state 与授权地址
		resp, err := postJSON(ctx, nil, upstreamBase+"/v2/plugin/auth/state?platform=workbuddy",
			browserAuthHeaders(""), map[string]interface{}{})
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
		}
		data, err := envelope(resp)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
		}
		var started struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &started); err != nil || started.State == "" || started.AuthURL == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "授权初始化响应不完整"}}, nil
		}

		// 上游 state 留在插件内存；回传前端的是我们签发的随机 state
		localState := shared.RandHex(16)
		p.mu.Lock()
		if p.authState == nil {
			p.authState = map[string]string{}
		}
		p.authState[localState] = started.State
		p.mu.Unlock()

		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url",
			Url:    withLoginParams(started.AuthURL),
			Prompt: map[string]string{"zh": "已打开浏览器授权页，完成登录后此处自动完成", "en": "Browser auth page opened; this step completes automatically after sign-in"},
			State:  []byte(localState),
			Wait:   true, // 前端轮询，无需用户手动确认
		}}, nil
	}

	// 后续步：前端轮询触发，每次 poll 一次上游（11217 = 用户尚未完成授权）
	localState := string(req.State)
	p.mu.Lock()
	upstreamState, valid := p.authState[localState]
	p.mu.Unlock()
	if !valid {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "授权会话已失效，请重新发起"}}, nil
	}

	grant, pending, err := p.pollToken(ctx, upstreamState)
	if err != nil {
		p.mu.Lock()
		delete(p.authState, localState)
		p.mu.Unlock()
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
	}
	if pending {
		// 用户还没在浏览器完成授权：让前端继续轮询（不带 url，避免重复打开浏览器）
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url",
			Prompt: map[string]string{"zh": "等待浏览器完成授权...", "en": "Waiting for browser authorization..."},
			State:  []byte(localState),
			Wait:   true,
		}}, nil
	}
	p.mu.Lock()
	delete(p.authState, localState)
	p.mu.Unlock()

	// 取账号资料（uid/nickname/enterprise）
	acct, err := p.fetchAuthAccount(ctx, upstreamState, grant.AccessToken)
	if err != nil {
		acct = map[string]string{} // 资料失败不阻塞建档
	}

	cred := &credential{}
	cred.Auth.AccessToken = grant.AccessToken
	cred.Auth.RefreshToken = grant.RefreshToken
	cred.Auth.ExpiresAt = grant.ReceivedAtMs + grant.ExpiresIn*1000
	cred.Auth.Domain = shared.OrDefault(grant.Domain, "www.codebuddy.cn")
	cred.Account.UID = acct["uid"]
	cred.Account.Nickname = acct["nickname"]
	cred.Account.EnterpriseID = acct["enterpriseId"]

	blob, _ := json.Marshal(cred)
	return &pb.LoginResult{
		Blob: blob,
		Profile: &pb.AccountProfile{
			DisplayName: shared.OrDefault(cred.Account.Nickname, shared.OrDefault(cred.Account.UID, "workbuddy-account")),
			Healthy:     true, Quota: map[string]string{},
		},
	}, nil
}

// tokenGrant 授权轮询拿到的凭据。
type tokenGrant struct {
	AccessToken      string
	RefreshToken     string
	ExpiresIn        int64
	RefreshExpiresIn int64
	ReceivedAtMs     int64
	Domain           string
}

// pollToken 轮询一次授权 token；pending=true 表示用户尚未完成授权。
func (p *plugin) pollToken(ctx context.Context, upstreamState string) (*tokenGrant, bool, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		upstreamBase+"/v2/plugin/auth/token?state="+url.QueryEscape(upstreamState), nil)
	for k, v := range browserAuthHeaders("") {
		req.Header.Set(k, v)
	}
	resp, err := sdk.UpstreamClient("").Do(req)
	if err != nil {
		return nil, false, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e struct {
		Code    int             `json:"code"`
		Message string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, false, fmt.Errorf("授权轮询响应非法（HTTP %d）", resp.StatusCode)
	}
	if e.Code == 11217 {
		return nil, true, nil // 用户尚未完成授权
	}
	if e.Code != 0 {
		return nil, false, fmt.Errorf("授权轮询返回错误码 %d: %s", e.Code, e.Message)
	}
	var g struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresIn        int64  `json:"expiresIn"`
		RefreshExpiresIn int64  `json:"refreshExpiresIn"`
		Domain           string `json:"domain"`
	}
	if err := json.Unmarshal(e.Data, &g); err != nil || g.AccessToken == "" {
		return nil, false, fmt.Errorf("授权轮询响应缺少 accessToken")
	}
	return &tokenGrant{
		AccessToken: g.AccessToken, RefreshToken: g.RefreshToken,
		ExpiresIn: g.ExpiresIn, RefreshExpiresIn: g.RefreshExpiresIn,
		ReceivedAtMs: time.Now().UnixMilli(), Domain: g.Domain,
	}, false, nil
}

// fetchAuthAccount 授权后取账号资料（白名单字段）。
func (p *plugin) fetchAuthAccount(ctx context.Context, upstreamState, accessToken string) (map[string]string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		upstreamBase+"/v2/plugin/login/account?state="+url.QueryEscape(upstreamState), nil)
	for k, v := range browserAuthHeaders(accessToken) {
		req.Header.Set(k, v)
	}
	resp, err := sdk.UpstreamClient("").Do(req)
	if err != nil {
		return nil, err
	}
	data, err := envelope(resp)
	if err != nil {
		return nil, err
	}
	var acct struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	}
	if err := json.Unmarshal(data, &acct); err != nil {
		return nil, err
	}
	return map[string]string{
		"uid": acct.UID, "nickname": acct.Nickname, "enterpriseId": acct.EnterpriseID,
	}, nil
}

// browserAuthHeaders 授权三段请求的公共头。
func browserAuthHeaders(accessToken string) map[string]string {
	h := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"User-Agent":   clientUA(),
		"X-Domain":     "copilot.tencent.com",
		"X-Product":    "SaaS",
		"X-IDE-Type":   "WorkBuddy",
		"X-IDE-Name":   "WorkBuddy",
		"X-Request-ID": shared.RandHex(16),
	}
	if accessToken != "" {
		h["Authorization"] = "Bearer " + accessToken
	}
	return h
}

// withLoginParams 给上游 authUrl 补登录页参数（version + loginSessionId）。
func withLoginParams(authURL string) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return authURL
	}
	q := u.Query()
	if q.Get("version") == "" {
		q.Set("version", "5.5.4")
	}
	if q.Get("loginSessionId") == "" {
		q.Set("loginSessionId", shared.RandUUID())
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// sendSMS POST /v2/plugin/login/send-sms（浏览器形态头）。
func (p *plugin) sendSMS(ctx context.Context, phone string) error {
	resp, err := postJSON(ctx, nil, loginBase+pathSendSMS, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json, text/plain, */*",
		"Origin":       loginBase, "Referer": loginBase + "/",
		"User-Agent": browserUA(),
	}, map[string]interface{}{"phone": phone})
	if err != nil {
		return err
	}
	_, err = envelope(resp)
	return err
}

// loginWithSMS POST /v2/plugin/login/token → (accessToken, refreshToken)。
func (p *plugin) loginWithSMS(ctx context.Context, phone, code string) (string, string, error) {
	resp, err := postJSON(ctx, nil, loginBase+pathLoginTok, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json, text/plain, */*",
		"Origin":       loginBase, "Referer": loginBase + "/",
		"User-Agent": browserUA(),
	}, map[string]interface{}{
		"login_method": "phone", "phone": phone, "sms_code": code,
	})
	if err != nil {
		return "", "", err
	}
	data, err := envelope(resp)
	if err != nil {
		return "", "", err
	}
	var tok struct {
		AccessToken   string `json:"accessToken"`
		Access_token  string `json:"access_token"`
		RefreshToken  string `json:"refreshToken"`
		Refresh_token string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return "", "", err
	}
	at, rt := shared.OrDefault(tok.AccessToken, tok.Access_token), shared.OrDefault(tok.RefreshToken, tok.Refresh_token)
	if at == "" || rt == "" {
		return "", "", fmt.Errorf("响应缺少 accessToken/refreshToken")
	}
	return at, rt, nil
}

// jwtClaims 解析 JWT payload（不验签，仅取身份字段）。
func jwtClaims(token string) map[string]interface{} {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return map[string]interface{}{}
	}
	payload := parts[1]
	if pad := 4 - len(payload)%4; pad != 4 {
		payload += strings.Repeat("=", pad)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return map[string]interface{}{}
	}
	var claims map[string]interface{}
	if json.Unmarshal(raw, &claims) != nil {
		return map[string]interface{}{}
	}
	return claims
}

// Refresh 用 refreshToken 刷新 accessToken，并顺带拉积分与动态块。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if cred.Auth.RefreshToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "没有 refreshToken，请重新导入凭据"}}, nil
	}
	headers := p.headers(cred, true)
	headers["X-Refresh-Token"] = cred.Auth.RefreshToken
	headers["X-Auth-Refresh-Source"] = "plugin"

	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+pathRefresh, headers, map[string]interface{}{})
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: err.Error()}}, nil
	}
	data, err := envelope(resp)
	if err != nil {
		code := int32(503)
		if _, isAuth := err.(*authError); isAuth {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	var newAuth struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		ExpiresIn        int64  `json:"expiresIn"`
		RefreshExpiresIn int64  `json:"refreshExpiresIn"`
		Domain           string `json:"domain"`
	}
	if err := json.Unmarshal(data, &newAuth); err != nil || newAuth.AccessToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: "refresh 响应缺少 accessToken"}}, nil
	}
	cred.Auth.AccessToken = newAuth.AccessToken
	if newAuth.RefreshToken != "" {
		cred.Auth.RefreshToken = newAuth.RefreshToken
	}
	if newAuth.ExpiresAt == 0 && newAuth.ExpiresIn > 0 {
		cred.Auth.ExpiresAt = nowMillis() + newAuth.ExpiresIn*1000
	} else if newAuth.ExpiresAt > 0 {
		cred.Auth.ExpiresAt = newAuth.ExpiresAt
	}
	if newAuth.Domain != "" {
		cred.Auth.Domain = newAuth.Domain
	}
	blob, _ := json.Marshal(cred)
	profile := &pb.AccountProfile{
		DisplayName: shared.OrDefault(cred.Account.Nickname, cred.Account.UID), Healthy: true, Quota: map[string]string{},
	}
	// 刷新成功后顺带拉积分与动态块（与 GetProfile 同一套组装），避免快照缺块
	if credits := p.fetchCredits(ctx, cred); credits != "" {
		profile.CreditsJson = credits
		var c struct {
			Total     string `json:"total"`
			Used      string `json:"used"`
			Remaining string `json:"remaining"`
		}
		if json.Unmarshal([]byte(credits), &c) == nil {
			profile.Quota["total_credits"] = c.Total
			profile.Quota["used_credits"] = c.Used
			profile.Quota["credits"] = c.Remaining
		}
	}
	profile.Sections = append(profile.Sections, p.profileSections(ctx, cred, profile.CreditsJson)...)
	return &pb.RefreshResult{Blob: blob, Profile: profile}, nil
}
