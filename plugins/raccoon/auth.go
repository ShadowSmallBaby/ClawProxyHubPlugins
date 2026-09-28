// auth.go — 凭据解析、扫码登录轮询、Token 导入、续期。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential Raccoon 会话凭据：Bearer access_token（+ 可选 refresh_token）。
type credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"` // JWT exp 毫秒字符串
	DeviceID     string `json:"device_id,omitempty"`
	Label        string `json:"label,omitempty"`

	proxyURL string `json:"-"`
}

// credFrom 解析凭据 blob（含代理配置），blob 需含非空 access_token。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	if blob == nil || len(blob.GetBlob()) == 0 {
		return nil, fmt.Errorf("缺少 Raccoon 凭据，请先登录")
	}
	c := &credential{}
	if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
		return nil, fmt.Errorf("凭据解析失败: %w", err)
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return nil, fmt.Errorf("凭据缺少 access_token")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// decodeJwtExpMs 从 JWT 的 exp 声明取过期毫秒（无 exp/解析失败返回空）。
func decodeJwtExpMs(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return ""
	}
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", int64(claims.Exp*1000))
}

// base64URLDecode JWT payload 的 base64url 解码（无 padding）。
func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// Login 双方式：qrcode（扫码轮询）/ token_import（粘贴 token）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "token_import":
		return p.loginToken(ctx, req)
	case "qrcode":
		return p.loginQR(ctx, req)
	}
	return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "unknown auth method: " + req.MethodId}}, nil
}

// loginToken 贴 access_token（或完整 Bearer 头），建档前校验并拉积分。
func (p *plugin) loginToken(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	token := strings.TrimSpace(req.Form["access_token"])
	token = strings.TrimPrefix(token, "Bearer ")
	if token == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "access_token 不能为空"}}, nil
	}
	c := &credential{
		AccessToken:  token,
		RefreshToken: strings.TrimSpace(req.Form["refresh_token"]),
		Label:        "raccoon",
	}
	if exp := decodeJwtExpMs(token); exp != "" {
		c.ExpiresAt = exp
	}
	blob, _ := json.Marshal(c)
	prof, _ := p.GetProfile(ctx, &pb.CredentialBlob{Blob: blob})
	if prof != nil && !prof.Healthy {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "token 无效或已过期，请重新登录 Raccoon 后复制"}}, nil
	}
	return &pb.LoginResult{Blob: blob, Profile: prof}, nil
}

// loginQR 扫码登录：第一步生成 qrcode_code 返回承载页 URL（Wait=true 前端轮询），
// 后续步轮询登录会话状态，success 即建档。
func (p *plugin) loginQR(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		// 第一步：生成 code + 后台轮询
		code := shared.RandHex(16)
		// 微信登录承载页（公开页面，扫码后服务端把该 code 置为 success）
		pageURL := apiBase + "/login/mp?code=" + code + "&appname=" + "商汤小浣熊官网"
		sess := &qrSession{status: "pending", expireAt: time.Now().Add(qrTimeout)}
		p.mu.Lock()
		p.qrStates = map[string]*qrSession{code: sess} // 单条即可，覆盖旧的
		p.mu.Unlock()
		go p.pollQRLoop(code, sess)
		time.AfterFunc(qrTimeout, func() { p.expireQR(code) })
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url", Url: pageURL,
			Prompt: map[string]string{"zh": "已打开扫码页，扫码后此处自动完成", "en": "QR page opened; this step completes automatically after scanning"},
			State:  []byte(code),
			Wait:   true,
		}}, nil
	}

	// 后续步：轮询会话状态
	code := string(req.State)
	p.mu.Lock()
	sess := p.qrStates[code]
	p.mu.Unlock()
	if sess == nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "state 已失效，请重新发起扫码"}}, nil
	}
	p.mu.Lock()
	sessStatus, sessCred, sessErr := sess.status, sess.cred, sess.err
	p.mu.Unlock()
	switch sessStatus {
	case "done":
		p.expireQR(code)
		return p.loginDone(ctx, sessCred)
	case "failed":
		p.expireQR(code)
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: sessErr}}, nil
	}
	return &pb.LoginResult{Next: &pb.LoginNextStep{
		Action: "open_url",
		Prompt: map[string]string{"zh": "等待扫码完成...", "en": "Waiting for QR scan..."},
		State:  []byte(code),
		Wait:   true,
	}}, nil
}

// qrSession 一次扫码会话的状态：pending → done（凭据就绪）/ failed。
type qrSession struct {
	status   string
	cred     *credential
	err      string
	expireAt time.Time
}

// expireQR 超时/完成后作废会话。
func (p *plugin) expireQR(code string) {
	p.mu.Lock()
	delete(p.qrStates, code)
	p.mu.Unlock()
}

// pollQRLoop 后台轮询 login_with_qrcode_code：2s 一次，任何异常降级 pending
//（网络抖动不应中断登录流程），success 缺 token 视为未完成（否则空凭据卡死）。
func (p *plugin) pollQRLoop(code string, sess *qrSession) {
	ticker := time.NewTicker(qrPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		if time.Now().After(sess.expireAt) {
			p.mu.Lock()
			if sess.status == "pending" {
				sess.status, sess.err = "failed", "扫码超时，请重试"
			}
			p.mu.Unlock()
			return
		}
		token, refresh, ok := p.pollQROnce(code)
		if !ok {
			continue
		}
		c := &credential{AccessToken: token, RefreshToken: refresh, Label: "raccoon"}
		if exp := decodeJwtExpMs(token); exp != "" {
			c.ExpiresAt = exp
		}
		p.mu.Lock()
		sess.status, sess.cred = "done", c
		p.mu.Unlock()
		return
	}
}

// loginDone 登录完成：凭据 + 档案。
func (p *plugin) loginDone(ctx context.Context, c *credential) (*pb.LoginResult, error) {
	blob, _ := json.Marshal(c)
	prof, _ := p.GetProfile(ctx, &pb.CredentialBlob{Blob: blob})
	return &pb.LoginResult{Blob: blob, Profile: prof}, nil
}

// pollQROnce 单次轮询；code 已 success 且带 token 时返回凭据。
func (p *plugin) pollQROnce(code string) (token, refresh string, ok bool) {
	client := sdk.UpstreamClient("")
	body := mustJSON(map[string]string{"qrcode_code": code})
	req, err := http.NewRequest("POST", apiBase+authPrefix+"/login_with_qrcode_code", strings.NewReader(string(body)))
	if err != nil {
		return "", "", false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var env struct {
		Code    float64 `json:"code"`
		Data    *struct {
			Status       string `json:"status"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Code != 0 || env.Data == nil {
		return "", "", false
	}
	if env.Data.Status != "success" || env.Data.AccessToken == "" {
		return "", "", false
	}
	return env.Data.AccessToken, env.Data.RefreshToken, true
}

// Refresh 用 refresh_token 换新凭据；服务端只回新 access_token 时保留旧 refresh
//（否则续期一次就把账号变成不可续期）；401 = refresh_token 失效。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if c.RefreshToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "没有 refresh_token，请重新登录"}}, nil
	}
	client := sdk.UpstreamClient(c.proxyURL)
	body := mustJSON(map[string]string{"refresh_token": c.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, "POST", apiBase+authPrefix+"/refresh", strings.NewReader(string(body)))
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: err.Error()}}, nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: err.Error()}}, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var env struct {
		Code    float64 `json:"code"`
		Message string  `json:"message"`
		Data    *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: "refresh 响应解析失败"}}, nil
	}
	if resp.StatusCode == 401 || env.Code == 200003 {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "登录态已过期，请重新登录"}}, nil
	}
	if env.Code != 0 || env.Data == nil || env.Data.AccessToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: "续期失败：" + env.Message}}, nil
	}
	c.AccessToken = env.Data.AccessToken
	if env.Data.RefreshToken != "" {
		c.RefreshToken = env.Data.RefreshToken
	}
	if exp := decodeJwtExpMs(c.AccessToken); exp != "" {
		c.ExpiresAt = exp
	}
	blob, _ := json.Marshal(c)
	profile := &pb.AccountProfile{DisplayName: "raccoon", Healthy: true, Quota: map[string]string{}}
	p.fetchQuota(ctx, c, profile)
	return &pb.RefreshResult{Blob: blob, Profile: profile}, nil
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
