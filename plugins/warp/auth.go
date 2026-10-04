// auth.go — 设备授权两步 Login + Firebase 刷新 + client login 握手。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential 保存访问令牌、刷新令牌及设备标识。
type credential struct {
	AccessToken     string    `json:"access_token,omitempty"`
	AccessExpiresAt time.Time `json:"access_expires_at,omitempty"`
	RefreshToken    string    `json:"refresh_token"`
	DeviceID        string    `json:"device_id,omitempty"`
	RequestID       string    `json:"request_id,omitempty"`
	Email           string    `json:"email,omitempty"`

	proxyURL string `json:"-"`

	// 会话态（不持久化）
	lastLogin time.Time
}

func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	var c credential
	if err := json.Unmarshal(blob.GetBlob(), &c); err != nil {
		return nil, fmt.Errorf("invalid credential: %w", err)
	}
	if strings.TrimSpace(c.RefreshToken) == "" {
		return nil, fmt.Errorf("credential missing refresh_token")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return &c, nil
}

// ensureFresh 到期交由核心触发 Refresh 并持久化新凭据；请求内只发登录通知。
func (p *plugin) ensureFresh(ctx context.Context, c *credential) error {
	now := time.Now()
	if c.AccessToken == "" || now.After(c.AccessExpiresAt.Add(-tokenRefreshAhead)) {
		return shared.HTTPError{Code: 401, Message: "credential refresh required"}
	}
	if now.Sub(c.lastLogin) >= loginValidity {
		if err := p.clientLogin(ctx, c); err != nil {
			// 登录握手失败不致命（上游登录通知），继续用已认证请求
			if p.host != nil {
				p.host.Log("warn", "warp client login failed; continuing: "+err.Error())
			}
		}
	}
	return nil
}

// refreshJWT 刷新访问令牌并记录过期时间。
func (p *plugin) refreshJWT(ctx context.Context, c *credential) error {
	key, err := p.firebaseKey()
	if err != nil {
		return err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", firebaseURL(firebaseTokenURL, key), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := p.hcFor(c).Do(httpReq)
	if err != nil {
		return fmt.Errorf("firebase refresh: %w", err)
	}
	defer resp.Body.Close()
	raw := shared.ReadLimitedResp(resp, 1<<20)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("firebase refresh HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var parsed struct {
		IDToken      string      `json:"id_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    interface{} `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("decode firebase refresh response: %w", err)
	}
	jwt := strings.TrimSpace(parsed.IDToken)
	if jwt == "" {
		return fmt.Errorf("firebase refresh response missing id_token")
	}
	c.AccessToken = jwt
	c.AccessExpiresAt = firebaseExpiry(jwt, parsed.ExpiresIn)
	if rt := strings.TrimSpace(parsed.RefreshToken); rt != "" {
		c.RefreshToken = rt
	}
	c.lastLogin = time.Time{}
	return nil
}

// firebaseExpiry 从 JWT exp 或 expires_in 推到期；都缺省 55min。
func firebaseExpiry(jwt string, expiresIn interface{}) time.Time {
	if exp := jwtExpiry(jwt); !exp.IsZero() {
		return exp
	}
	seconds, _ := strconv.ParseFloat(fmt.Sprint(expiresIn), 64)
	if seconds > 0 {
		return time.Now().Add(time.Duration(seconds) * time.Second)
	}
	return time.Now().Add(55 * time.Minute)
}

// jwtExpiry 解析 JWT exp claim（失败回零值）。
func jwtExpiry(jwt string) time.Time {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64Decode(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func base64Decode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// clientLogin 发送携带设备标识的登录通知。
func (p *plugin) clientLogin(ctx context.Context, c *credential) error {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", warpLoginURL, nil)
	if err != nil {
		return err
	}
	warpHeaders(httpReq)
	httpReq.Header.Set("Authorization", "Bearer "+c.AccessToken)
	httpReq.Header.Set("X-Warp-Experiment-Id", c.DeviceID)
	httpReq.Header.Set("X-Warp-Experiment-Bucket", experimentBucket(c.DeviceID))
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Content-Length", "0")
	resp, err := p.hcFor(c).Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("warp login HTTP %d", resp.StatusCode)
	}
	c.lastLogin = time.Now()
	return nil
}

// experimentBucket 随设备 id 确定性派生的实验桶（同凭据恒同桶）。
func experimentBucket(deviceID string) string {
	sum := sha256.Sum256([]byte("warp-exp:" + deviceID))
	return hex.EncodeToString(sum[:])
}

// ---------- 设备授权登录（两步） ----------

// loginState 第一步签发的多步状态（device_code 不回传浏览器）。
type loginState struct {
	DeviceCode              string `json:"dc"`
	Interval                int    `json:"iv"`
	ExpiresAt               int64  `json:"exp"`
	VerificationURIComplete string `json:"vuri,omitempty"`
}

// Login 两步：State 空 = 发起设备授权给登录链接；State 非空 = 轮询换 token 建档。
// Callback=auto_wait：前端在插件本机（同机访问）会持续提交本步直至完成。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		return p.loginStart(ctx)
	}
	return p.loginPoll(ctx, req)
}

// loginStart 发起设备授权（warp-agent-cli client）。
func (p *plugin) loginStart(ctx context.Context) (*pb.LoginResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", warpDeviceAuthURL, strings.NewReader(url.Values{"client_id": {warpAgentCLIClientID}}.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "请求设备码失败: " + err.Error()}}, nil
	}
	defer resp.Body.Close()
	raw := shared.ReadLimitedResp(resp, 64<<10)
	if resp.StatusCode != http.StatusOK {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: fmt.Sprintf("设备授权 HTTP %d", resp.StatusCode)}}, nil
	}
	var auth struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil || auth.DeviceCode == "" || auth.VerificationURI == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "设备授权响应不完整"}}, nil
	}
	if auth.Interval < 1 {
		auth.Interval = 2
	}
	if auth.ExpiresIn < 1 {
		auth.ExpiresIn = 600
	}
	state, _ := json.Marshal(loginState{
		DeviceCode:              auth.DeviceCode,
		Interval:                auth.Interval,
		ExpiresAt:               time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second).Unix(),
		VerificationURIComplete: auth.VerificationURIComplete,
	})
	return &pb.LoginResult{Next: &pb.LoginNextStep{
		Action: "open_url",
		Url:    shared.OrDefault(auth.VerificationURIComplete, auth.VerificationURI),
		Prompt: map[string]string{
			"zh": fmt.Sprintf("请在浏览器打开并输入代码 %s 完成授权", auth.UserCode),
			"en": fmt.Sprintf("Open the link and enter code %s to authorize", auth.UserCode),
		},
		State: state,
		Wait:  true,
	}}, nil
}

// loginPoll 轮询授权结果并交换刷新令牌。
func (p *plugin) loginPoll(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	var st loginState
	if json.Unmarshal(req.State, &st) != nil || st.DeviceCode == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "登录状态失效，请重新发起"}}, nil
	}
	if st.ExpiresAt > 0 && time.Now().Unix() > st.ExpiresAt {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "设备授权已过期，请重新发起"}}, nil
	}
	client := &http.Client{Timeout: 20 * time.Second}
	form := url.Values{
		"client_id":   {warpAgentCLIClientID},
		"device_code": {st.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", warpDeviceTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "轮询设备授权失败: " + err.Error()}}, nil
	}
	defer resp.Body.Close()
	raw := shared.ReadLimitedResp(resp, 64<<10)
	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(raw, &token)
	if strings.TrimSpace(token.Error) == "authorization_pending" {
		// 仍在等待浏览器确认：原样回传状态，前端继续轮询
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url", Wait: true, State: req.State,
			Prompt: map[string]string{"zh": "等待浏览器授权中…", "en": "Waiting for browser authorization…"},
		}}, nil
	}
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(token.AccessToken) == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: fmt.Sprintf("设备授权失败（HTTP %d）", resp.StatusCode)}}, nil
	}
	// 交换授权令牌，取得可续期凭据。
	refreshToken, err := p.exchangeFirebaseCustomToken(ctx, token.AccessToken)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "Firebase 交换失败: " + err.Error()}}, nil
	}
	c := &credential{RefreshToken: refreshToken, DeviceID: shared.RandUUID(), RequestID: shared.RandUUID()}
	// 实证：换一次 JWT 验证凭据可用并补邮箱
	if err := p.refreshJWT(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "凭据校验失败: " + err.Error()}}, nil
	}
	c.Email = jwtEmail(c.AccessToken)
	blob, _ := json.Marshal(c)
	name := shared.OrDefault(c.Email, "warp-account")
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}},
	}, nil
}

// exchangeFirebaseCustomToken 将授权令牌交换为刷新令牌。
func (p *plugin) exchangeFirebaseCustomToken(ctx context.Context, accessToken string) (string, error) {
	key, err := p.firebaseKey()
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]interface{}{"returnSecureToken": true, "token": accessToken})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", firebaseURL(firebaseCustomTokenURL, key), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw := shared.ReadLimitedResp(resp, 64<<10)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || strings.TrimSpace(parsed.RefreshToken) == "" {
		return "", fmt.Errorf("响应缺少 refreshToken")
	}
	return strings.TrimSpace(parsed.RefreshToken), nil
}

// ---------- Refresh / 凭据工具 ----------

// jwtEmail 解析 JWT email claim。
func jwtEmail(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64Decode(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.Email)
}
