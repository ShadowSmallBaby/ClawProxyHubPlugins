// workbuddy 插件 — 腾讯 WorkBuddy / CodeBuddy 客户端反代。
// 上游：copilot.tencent.com，OpenAI 兼容（/v2/chat/completions，仅流式）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	upstreamBase = "https://copilot.tencent.com"
	loginBase    = "https://www.workbuddy.cn"
	pathChat     = "/v2/chat/completions"
	pathRefresh  = "/v2/plugin/auth/token/refresh"
	pathSendSMS  = "/v2/plugin/login/send-sms"
	pathLoginTok = "/v2/plugin/login/token"
	// 浏览器形态 UA 内置默认：插件登录接口在 www.workbuddy.cn，不是客户端头（核心全局浏览器 UA 非空时覆盖）
	defaultBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/138.0.7204.251 Safari/537.36"
	// 客户端标识默认值：可被插件设置覆盖（user_agent / ide_version）
	defaultUserAgent  = "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"
	defaultIDEVersion = "5.5.4"
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	authState    map[string]string // 我们签发的 state → 上游 state（上游 state 不回传前端）
	settingsJSON []byte            // 插件设置缓存（30s，客户端标识懒刷新用）
	settingsAt   time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// 当前生效的客户端标识（ensureIdentity 刷新；telemetry 等包级代码读取）。
var (
	identMu        sync.RWMutex
	identUA        = defaultUserAgent
	identIDEVer    = defaultIDEVersion
	identBrowserUA = defaultBrowserUA
)

func clientUA() string { identMu.RLock(); defer identMu.RUnlock(); return identUA }

func clientIDEVersion() string { identMu.RLock(); defer identMu.RUnlock(); return identIDEVer }

// browserUA 浏览器形态 UA：核心全局浏览器 UA（sdk.SettingBrowserUserAgent）非空则用它，否则内置。
func browserUA() string { identMu.RLock(); defer identMu.RUnlock(); return identBrowserUA }

// versionFromUA 从 UA 提取版本号（首个 "/" 后到空格前的段）。
func versionFromUA(ua string) string {
	i := strings.Index(ua, "/")
	if i < 0 {
		return ""
	}
	rest := ua[i+1:]
	if j := strings.IndexAny(rest, " /"); j > 0 {
		return rest[:j]
	}
	return rest
}

// ensureIdentity 懒刷新客户端标识：读插件设置（30s 缓存），
// user_agent 覆盖 UA；ide_version 留空则从 UA 解析；核心注入的全局浏览器 UA 覆盖内置浏览器 UA。
func (p *plugin) ensureIdentity() {
	p.mu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < 30*time.Second
	p.mu.Unlock()
	if fresh {
		return
	}
	ua, ver, bua := defaultUserAgent, "", defaultBrowserUA
	if p.host != nil {
		if raw := p.host.Settings("workbuddy"); len(raw) > 0 {
			var cfg map[string]string
			if json.Unmarshal(raw, &cfg) == nil {
				if cfg["user_agent"] != "" {
					ua = cfg["user_agent"]
				}
				ver = cfg["ide_version"]
				if v := strings.TrimSpace(cfg[sdk.SettingBrowserUserAgent]); v != "" {
					bua = v
				}
			}
		}
	}
	if ver == "" {
		ver = versionFromUA(ua)
	}
	if ver == "" {
		ver = defaultIDEVersion
	}
	identMu.Lock()
	identUA, identIDEVer, identBrowserUA = ua, ver, bua
	identMu.Unlock()
	p.mu.Lock()
	p.settingsJSON, p.settingsAt = []byte("cached"), time.Now()
	p.mu.Unlock()
}

// ---------- 凭据 blob（桌面端 .info 文件形态） ----------

type credential struct {
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	} `json:"account"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c, err := parseCred(blob.GetBlob())
	if err != nil {
		return nil, err
	}
	if pr := blob.GetProxy(); pr != nil && pr.GetHost() != "" {
		u := &url.URL{Scheme: shared.OrDefault(pr.GetScheme(), "http"), Host: fmt.Sprintf("%s:%d", pr.GetHost(), pr.GetPort())}
		if pr.GetUsername() != "" {
			u.User = url.UserPassword(pr.GetUsername(), pr.GetPassword())
		}
		c.proxyURL = u.String()
	}
	return c, nil
}

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hc(cred *credential) *http.Client {
	if cred == nil || cred.proxyURL == "" {
		return shared.UpstreamClient("")
	}
	if c, ok := proxyClients.Load(cred.proxyURL); ok {
		return c.(*http.Client)
	}
	u, err := url.Parse(cred.proxyURL)
	if err != nil {
		return shared.UpstreamClient("")
	}
	c := shared.UpstreamClient(u.String())
	proxyClients.Store(cred.proxyURL, c)
	return c
}

func parseCred(blob []byte) (*credential, error) {
	var c credential
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("invalid credential: %w", err)
	}
	if c.Auth.AccessToken == "" {
		return nil, fmt.Errorf("credential missing auth.accessToken")
	}
	return &c, nil
}

// headers WorkBuddy 客户端伪装头。
func (p *plugin) headers(cred *credential, auth bool) map[string]string {
	p.ensureIdentity()
	domain := cred.Auth.Domain
	if domain == "" {
		domain = "www.codebuddy.cn"
	}
	h := map[string]string{
		"Content-Type":                "application/json",
		"Accept":                      "application/json",
		"X-User-Id":                   cred.Account.UID,
		"X-Enterprise-Id":             cred.Account.EnterpriseID,
		"X-Tenant-Id":                 cred.Account.EnterpriseID,
		"X-Domain":                    domain,
		"User-Agent":                  clientUA(),
		"X-IDE-Type":                  "WorkBuddy",
		"X-IDE-Name":                  "WorkBuddy",
		"X-IDE-Version":               clientIDEVersion(),
		"X-Private-Data":              "false",
		"X-Product":                   "SaaS",
		"x-stainless-arch":            "x64",
		"x-stainless-lang":            "js",
		"x-stainless-os":              "Windows",
		"x-stainless-package-version": "6.25.0",
		"x-stainless-retry-count":     "0",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v22.21.1",
		"X-Agent-Intent":              "craft",
		"X-Agent-Purpose":             "conversation_topic",
	}
	if auth {
		h["Authorization"] = "Bearer " + cred.Auth.AccessToken
	}
	return h
}

func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body interface{}) (*http.Response, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = shared.UpstreamClient("")
	}
	return client.Do(req)
}

// envelope {code, msg, data} 信封校验。
func envelope(resp *http.Response) (json.RawMessage, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var e struct {
		Code    int             `json:"code"`
		Message string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("upstream non-json (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == 401 || e.Code == 401 {
		return nil, &authError{msg: fmt.Sprintf("auth failed: HTTP %d code=%d %s", resp.StatusCode, e.Code, e.Message)}
	}
	if e.Code != 0 {
		return nil, fmt.Errorf("upstream rejected: code=%d %s", e.Code, e.Message)
	}
	return e.Data, nil
}

type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: "workbuddy", Version: version, Author: "cph",
		Label:           map[string]string{"zh": "WorkBuddy", "en": "WorkBuddy"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "tasks"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "客户端 UA 伪装值，留空使用内置默认",
					"default": ""
				},
				"ide_version": {
					"type": "string",
					"title": "IDE 版本号",
					"description": "X-IDE-Version 与埋点字段，留空则从 User-Agent 解析",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "phone_otp", Label: map[string]string{"zh": "手机验证码登录", "en": "Phone OTP Login"},
				Capabilities: []string{"refreshable", "auto_relogin"},
				Fields: []*pb.AuthField{{
					Name: "phone", Label: map[string]string{"zh": "手机号", "en": "Phone Number"}, Type: "phone", Required: true,
				}},
			},
			{
				Id: "oauth", Label: map[string]string{"zh": "浏览器授权登录", "en": "Browser Authorization"},
				Capabilities: []string{"refreshable"},
				Callback:     "auto", // 插件侧轮询上游，前端只轮询不显示输入框
			},
			{
				Id: "auth_file", Label: map[string]string{"zh": "凭据文件", "en": "Credential File"},
				Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": ".info 凭据文件内容", "en": ".info credential file content"},
					Type: "textarea", Required: true,
					Placeholder: `{"auth": {"accessToken": "..."}, "account": {"uid": "..."}}`,
				}},
			},
		},
	}}, nil
}

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
	resp, err := shared.UpstreamClient("").Do(req)
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
	resp, err := shared.UpstreamClient("").Do(req)
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

// ---------- 刷新 / 资料 / 模型 ----------

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

// profileSections 动态块组装（Refresh 与 GetProfile 共用）：签到 / 旅行 / 盲盒 / 成长计划 / 积分包。
func (p *plugin) profileSections(ctx context.Context, cred *credential, creditsJSON string) []*pb.ProfileSection {
	var secs []*pb.ProfileSection
	if sec := p.checkinSection(ctx, cred); sec != nil {
		secs = append(secs, sec)
	}
	if sec := p.travelSection(ctx, cred); sec != nil {
		secs = append(secs, sec)
	}
	if sec := p.blindboxSection(ctx, cred); sec != nil {
		secs = append(secs, sec)
	}
	if sec := p.growthSection(ctx, cred); sec != nil {
		secs = append(secs, sec)
	}
	if creditsJSON != "" {
		secs = append(secs, packagesSection(creditsJSON))
	}
	return secs
}

// GetProfile 积分明细（get-user-resource）：
// 总积分 = TotalDosage；已用/剩余 = 各积分包 Cycle* 累加（以上游给的数为准，不自己算差值）。
// 失败降级为基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{
		DisplayName: shared.OrDefault(cred.Account.Nickname, cred.Account.UID), Healthy: true, Quota: map[string]string{},
	}
	credits := p.fetchCredits(ctx, cred)
	if credits != "" {
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
	profile.Sections = append(profile.Sections, p.profileSections(ctx, cred, credits)...)
	return profile, nil
}

// growthSection 成长计划明细动态块：每条任务一行（状态徽章 + 进度 + 奖励），失败静默跳过。
func (p *plugin) growthSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	tasks, err := p.growthTasks(ctx, cred)
	if err != nil || len(tasks) == 0 {
		return nil
	}
	sec := &pb.ProfileSection{
		Id:    "growth_tasks",
		Title: map[string]string{"zh": "成长计划", "en": "Growth Plan"},
		Columns: []*pb.SectionColumn{
			{Key: "title", Title: map[string]string{"zh": "任务", "en": "Task"}},
			{Key: "status", Title: map[string]string{"zh": "状态", "en": "Status"}, Kind: "status"},
			{Key: "progress", Title: map[string]string{"zh": "进度", "en": "Progress"}},
			{Key: "reward", Title: map[string]string{"zh": "奖励", "en": "Reward"}},
		},
	}
	for _, t := range tasks {
		progress := "-"
		if t.Progress.Target > 0 {
			progress = fmt.Sprintf("%d / %d", t.Progress.Current, t.Progress.Target)
		}
		sec.Items = append(sec.Items, &pb.SectionRow{Cells: map[string]string{
			"title":    shared.OrDefault(t.Title, t.Code),
			"status":   "status:" + growthAcceptStatus(t.AcceptStatus),
			"progress": progress,
			"reward":   rewardText(t),
		}})
	}
	return sec
}

// growthAcceptStatus accept_status 五态 → 中文。
func growthAcceptStatus(s string) string {
	switch s {
	case "not_accepted":
		return "未接取"
	case "accepted":
		return "已接取"
	case "in_progress":
		return "进行中"
	case "completed":
		return "待领奖"
	case "claimed":
		return "已领奖"
	}
	return s
}

// rewardText 任务奖励文案（积分 / 能量 / 伙伴）。
func rewardText(t growthTask) string {
	parts := ""
	if t.RewardCredit > 0 {
		parts += fmt.Sprintf("积分+%d", t.RewardCredit)
	}
	if t.RewardEnergy > 0 {
		if parts != "" {
			parts += " "
		}
		parts += fmt.Sprintf("能量+%d", t.RewardEnergy)
	}
	if parts == "" {
		return "-"
	}
	return parts
}

// blindboxSection 盲盒情况动态块（能量余额 + 可开次数），失败静默跳过。
func (p *plugin) blindboxSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	data, err := p.actGet(ctx, cred, actEnergy, "盲盒能量")
	if err != nil {
		return nil
	}
	var energy struct {
		Balance int `json:"balance"`
	}
	_ = json.Unmarshal(data, &energy)
	openable := 0
	if q, err := p.actGet(ctx, cred, actQuota, "盲盒配额"); err == nil {
		var quota struct {
			Affordable int `json:"affordable"`
		}
		if json.Unmarshal(q, &quota) == nil {
			openable = quota.Affordable
		}
	}
	return &pb.ProfileSection{
		Id:    "blindbox",
		Title: map[string]string{"zh": "盲盒情况", "en": "Blind Box"},
		Entries: []*pb.SectionEntry{
			{Label: map[string]string{"zh": "能量余额", "en": "Energy"}, Value: fmt.Sprintf("%d", energy.Balance)},
			{Label: map[string]string{"zh": "可开次数", "en": "Openable"}, Value: fmt.Sprintf("%d", openable)},
		},
	}
}

// travelSection 猫猫旅行情况动态块（Buddy + 状态 + 奖励），失败静默跳过。
func (p *plugin) travelSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	data, err := p.actGet(ctx, cred, actTravelStat, "猫猫旅行状态")
	if err != nil {
		return nil
	}
	var st struct {
		State        string `json:"state"`
		BuddyID      int64  `json:"buddy_id"`
		ArriveAt     int64  `json:"arrive_at"`
		ServerNow    int64  `json:"server_now"`
		RewardCredit int    `json:"reward_credit"`
		Location     struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"location"`
	}
	if json.Unmarshal(data, &st) != nil {
		return nil
	}
	// 无猫（buddy_id=0 且 state 空）不渲染
	if st.BuddyID == 0 && (st.State == "" || st.State == "idle") {
		return nil
	}
	status := "空闲"
	if st.State == "traveling" {
		status = "在途"
		if st.ArriveAt > st.ServerNow {
			minutes := (st.ArriveAt - st.ServerNow + 59) / 60
			status = fmt.Sprintf("在途（约 %d 分钟后到达）", minutes)
		} else {
			status = "已到达，待领奖"
		}
	}
	entries := []*pb.SectionEntry{
		{Label: map[string]string{"zh": "状态", "en": "State"}, Value: "status:" + status, Kind: "status"},
	}
	if st.Location.Name != "" {
		entries = append(entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "目的地", "en": "Destination"}, Value: st.Location.Name,
		})
	}
	if st.RewardCredit > 0 {
		entries = append(entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "预计奖励", "en": "Reward"}, Value: fmt.Sprintf("积分+%d", st.RewardCredit),
		})
	}
	return &pb.ProfileSection{
		Id:      "travel",
		Title:   map[string]string{"zh": "旅行情况", "en": "Buddy Travel"},
		Entries: entries,
	}
}

// checkinSection 签到状态动态块（活动开启时才渲染；失败静默跳过）。
func (p *plugin) checkinSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+"/v2/billing/meter/checkin-activity-status", p.headers(cred, true), map[string]interface{}{})
	if err != nil {
		return nil
	}
	data, err := envelope(resp)
	if err != nil {
		return nil
	}
	var st struct {
		Active          bool   `json:"active"`
		TodayCheckedIn  bool   `json:"today_checked_in"`
		StreakDays      int    `json:"streak_days"`
		DailyCredit     int    `json:"daily_credit"`
		WeekCheckinDays int    `json:"week_checkin_days"`
		EndAt           string `json:"end_at"`
	}
	if json.Unmarshal(data, &st) != nil || !st.Active {
		return nil
	}
	status := "已签到"
	if !st.TodayCheckedIn {
		status = "未签到"
	}
	sec := &pb.ProfileSection{
		Id:    "checkin",
		Title: map[string]string{"zh": "签到状态", "en": "Check-in"},
		Entries: []*pb.SectionEntry{
			{Label: map[string]string{"zh": "今日签到", "en": "Today"}, Value: "status:" + status, Kind: "status"},
			{Label: map[string]string{"zh": "连签天数", "en": "Streak"}, Value: fmt.Sprintf("%d 天", st.StreakDays)},
		},
	}
	if st.DailyCredit > 0 {
		sec.Entries = append(sec.Entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "每日积分", "en": "Daily Credit"}, Value: fmt.Sprintf("%d", st.DailyCredit),
		})
	}
	if st.WeekCheckinDays > 0 {
		sec.Entries = append(sec.Entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "本周签到", "en": "Week"}, Value: fmt.Sprintf("%d 天", st.WeekCheckinDays),
		})
	}
	if st.EndAt != "" {
		sec.Entries = append(sec.Entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "活动截止", "en": "Ends"}, Value: st.EndAt,
		})
	}
	return sec
}

// packagesSection 积分包明细动态块：每个积分包一行（剩余/已用/到期 + 临期徽章）。
func packagesSection(creditsJSON string) *pb.ProfileSection {
	var c struct {
		Packages []struct {
			Total     string `json:"total"`
			Used      string `json:"used"`
			Remaining string `json:"remaining"`
			ExpiresAt string `json:"expiresAt"`
		} `json:"packages"`
	}
	if json.Unmarshal([]byte(creditsJSON), &c) != nil || len(c.Packages) == 0 {
		return nil
	}
	sec := &pb.ProfileSection{
		Id:    "packages",
		Title: map[string]string{"zh": "积分包", "en": "Credit Packages"},
		Columns: []*pb.SectionColumn{
			{Key: "remaining", Title: map[string]string{"zh": "剩余", "en": "Remaining"}},
			{Key: "used", Title: map[string]string{"zh": "已用", "en": "Used"}},
			{Key: "total", Title: map[string]string{"zh": "总积分", "en": "Total"}},
			{Key: "expiresAt", Title: map[string]string{"zh": "到期", "en": "Expires"}},
			{Key: "status", Title: map[string]string{"zh": "状态", "en": "Status"}, Kind: "status"},
		},
	}
	for _, pk := range c.Packages {
		sec.Items = append(sec.Items, &pb.SectionRow{Cells: map[string]string{
			"remaining": pk.Remaining,
			"used":      pk.Used,
			"total":     pk.Total,
			"expiresAt": pk.ExpiresAt,
			"status":    "status:" + packageExpiryStatus(pk.ExpiresAt),
		}})
	}
	return sec
}

// packageExpiryStatus 按到期时间判定：active / expiringSoon（7 天内）/ expired / unknown。
func packageExpiryStatus(expiresAt string) string {
	if expiresAt == "" {
		return "unknown"
	}
	t, err := time.Parse("2006-01-02 15:04:05", expiresAt)
	if err != nil {
		return "unknown"
	}
	// 上游时间按 UTC+8（与同条记录毫秒字段交叉验算确认）
	loc := time.FixedZone("CST", 8*3600)
	expires := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
	until := time.Until(expires)
	switch {
	case until <= 0:
		return "expired"
	case until <= 7*24*time.Hour:
		return "expiringSoon"
	default:
		return "active"
	}
}

// fetchCredits 查询积分明细并解析为 credits_json；失败返回空串（安全降级）。
func (p *plugin) fetchCredits(ctx context.Context, cred *credential) string {
	body := map[string]interface{}{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca",
		"Status":                     []int{0, 3},
		"PackageStartTimeRangeBegin": "2024-12-01 21:25:00",
		"PackageStartTimeRangeEnd":   time.Now().Format("2006-01-02 15:04:05"),
	}
	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+"/v2/billing/meter/get-user-resource", p.headers(cred, true), body)
	if err != nil {
		return ""
	}
	data, err := envelope(resp)
	if err != nil {
		return ""
	}
	var resource struct {
		Response struct {
			Data struct {
				TotalDosage json.Number `json:"TotalDosage"`
				Accounts    []struct {
					Used      string `json:"CycleCapacityUsedPrecise"`
					Total     string `json:"CycleCapacitySizePrecise"`
					Remaining string `json:"CycleCapacityRemainPrecise"`
					StartsAt  string `json:"CycleStartTime"`
					ExpiresAt string `json:"CycleEndTime"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if json.Unmarshal(data, &resource) != nil {
		return ""
	}
	d := resource.Response.Data
	if d.TotalDosage == "" && len(d.Accounts) == 0 {
		return ""
	}
	type pkg struct {
		Total     string `json:"total,omitempty"`
		Used      string `json:"used"`
		Remaining string `json:"remaining,omitempty"`
		ExpiresAt string `json:"expiresAt,omitempty"`
	}
	var packages []pkg
	usedSum, remainSum := 0.0, 0.0
	for _, a := range d.Accounts {
		pk := pkg{Used: a.Used}
		if a.Total != "" {
			pk.Total = a.Total
		}
		if a.Remaining != "" {
			pk.Remaining = a.Remaining
		}
		if a.ExpiresAt != "" {
			pk.ExpiresAt = a.ExpiresAt
		}
		if v, err := strconv.ParseFloat(a.Used, 64); err == nil {
			usedSum += v
		}
		if v, err := strconv.ParseFloat(a.Remaining, 64); err == nil {
			remainSum += v
		}
		packages = append(packages, pk)
	}
	out := map[string]interface{}{
		"total":     d.TotalDosage.String(),
		"used":      trimFloat(usedSum),
		"remaining": trimFloat(remainSum),
	}
	if len(packages) > 0 {
		out["packages"] = packages
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// ListModels 上游按 model=auto 路由，模型目录由客户端自行发现。
// codebuddyModels 上游模型端点不可达时的兜底清单（2026-09 实测 /v3/config cli agent 集合）。
// auto 为上游路由虚拟模型。
var codebuddyModels = []string{
	"auto",
	"hy4-preview-f", "hy3", "hy3-x",
	"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo",
	"kimi-k3-1", "kimi-k2.8-preview", "kimi-k2.7", "kimi-k2.6",
	"deepseek-v4.1-flash", "deepseek-v4-pro",
	"minimax-m3",
}

// ListModels 拉上游产品配置模型目录（cli agent 可用集），失败回退静态清单。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return &pb.ModelList{Models: staticModels()}, nil
	}
	if models := p.fetchModels(ctx, cred); len(models) > 0 {
		return &pb.ModelList{Models: models}, nil
	}
	return &pb.ModelList{Models: staticModels()}, nil
}

// fetchModels GET /console/enterprises/{enterpriseId|personal}/models，
// 取 productConfig.models，若声明了 cli agent 则按其 model 集过滤，跳过 disabled。
func (p *plugin) fetchModels(ctx context.Context, cred *credential) []*pb.ModelInfo {
	ent := cred.Account.EnterpriseID
	if ent == "" {
		ent = "personal"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", upstreamBase+"/console/enterprises/"+ent+"/models", nil)
	if err != nil {
		return nil
	}
	for k, v := range p.headers(cred, true) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var cfg struct {
		Models []struct {
			ID string `json:"id"`
			// contextWindow 上游为新版对象形态（{defaultLength, supportedLengths}），
			// 部分条目缺失该字段，用 RawMessage 兼容两种形态
			ContextWindow  json.RawMessage `json:"contextWindow"`
			MaxInputTokens int32           `json:"maxInputTokens"`
			SupportsImages bool            `json:"supportsImages"`
			Disabled       bool            `json:"disabled"`
		} `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	}
	if json.Unmarshal(body, &cfg) != nil {
		return nil
	}
	var cliSet map[string]bool
	for _, a := range cfg.Agents {
		if a.Name == "cli" {
			cliSet = map[string]bool{}
			for _, id := range a.Models {
				cliSet[id] = true
			}
		}
	}
	out := make([]*pb.ModelInfo, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if m.Disabled || m.ID == "" || (cliSet != nil && !cliSet[m.ID]) {
			continue
		}
		info := &pb.ModelInfo{Id: m.ID, ContextWindow: contextWindow(m), SupportsTools: true, SupportsStream: true}
		info.Label = map[string]string{"zh": m.ID, "en": m.ID}
		out = append(out, info)
	}
	return out
}

// contextWindow 模型上下文窗口：优先 contextWindow.defaultLength（对象形态），
// 缺失/旧形态时回退 maxInputTokens；两者都无效返回 0。
func contextWindow(m struct {
	ID             string          `json:"id"`
	ContextWindow  json.RawMessage `json:"contextWindow"`
	MaxInputTokens int32           `json:"maxInputTokens"`
	SupportsImages bool            `json:"supportsImages"`
	Disabled       bool            `json:"disabled"`
}) int32 {
	var cw struct {
		DefaultLength int32 `json:"defaultLength"`
	}
	if len(m.ContextWindow) > 0 && json.Unmarshal(m.ContextWindow, &cw) == nil && cw.DefaultLength > 0 {
		return cw.DefaultLength
	}
	return m.MaxInputTokens
}

// staticModels 上游不可达时的兜底清单。
func staticModels() []*pb.ModelInfo {
	models := make([]*pb.ModelInfo, 0, len(codebuddyModels))
	for _, id := range codebuddyModels {
		m := &pb.ModelInfo{Id: id, SupportsTools: true, SupportsStream: true}
		if id == "auto" {
			m.Label = map[string]string{"zh": "自动（上游路由）", "en": "Auto (upstream routing)"}
		}
		models = append(models, m)
	}
	return models
}

// ---------- Chat ----------

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, orHint(err)))
	}

	body := openaiup.ChatBody(req)
	body["model"] = shared.OrDefault(req.Model, "auto")
	desensitizeMessageBody(body) // system 净化：客户端特征改写 + 合规声明脱敏

	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+pathChat, p.headers(cred, true), body)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		if resp.StatusCode == 401 {
			code = 401
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}

	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sawEvent := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") && !strings.Contains(line, "[DONE]") {
			sawEvent = true
		}
		parser.Feed(line)
	}
	if err := scanner.Err(); err != nil {
		parser.FinishWithError(502, "upstream stream broken: "+err.Error())
		return nil
	}
	if !sawEvent {
		parser.FinishWithError(502, "upstream returned an empty stream")
		return nil
	}
	parser.Finish()
	return nil
}

// ---------- 任务能力：每日签到 ----------

func (p *plugin) ListTaskCapabilities(ctx context.Context, _ *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{
			{
				Id: "checkin", Label: map[string]string{"zh": "每日签到", "en": "Daily Check-in"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:10",
			},
			{
				Id: "blindbox", Label: map[string]string{"zh": "开盲盒", "en": "Blind Box"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:20",
			},
			{
				Id: "travel", Label: map[string]string{"zh": "猫猫旅行", "en": "Buddy Travel"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:30",
			},
			{
				Id: "growth_tasks", Label: map[string]string{"zh": "成长任务", "en": "Growth Tasks"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:40",
			},
		},
	}, nil
}

// RunTask 按能力分发：签到 / 盲盒 / 旅行 / 成长任务。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	switch req.CapabilityId {
	case "checkin":
		return p.runCheckin(ctx, cred)
	case "blindbox":
		return p.runBlindbox(ctx, cred)
	case "travel":
		return p.runTravel(ctx, cred)
	case "growth_tasks":
		return p.runGrowthTasks(ctx, cred)
	}
	return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
}

// runCheckin 每日签到：先查活动状态，未签则 POST daily-checkin。
func (p *plugin) runCheckin(ctx context.Context, cred *credential) (*pb.RunTaskResponse, error) {
	// 1. 活动状态
	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+"/v2/billing/meter/checkin-activity-status",
		p.headers(cred, true), map[string]interface{}{})
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	data, err := envelope(resp)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var st struct {
		Active         bool `json:"active"`
		TodayCheckedIn bool `json:"today_checked_in"`
		StreakDays     int  `json:"streak_days"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, status.Error(codes.Internal, "status parse: "+err.Error())
	}
	if !st.Active {
		return &pb.RunTaskResponse{Summary: "当前无签到活动"}, nil
	}
	if st.TodayCheckedIn {
		return &pb.RunTaskResponse{Summary: fmt.Sprintf("今日已签到（连签 %d 天）", st.StreakDays)}, nil
	}

	// 2. 执行签到
	resp2, err := postJSON(ctx, p.hc(cred), upstreamBase+"/v2/billing/meter/daily-checkin",
		p.headers(cred, true), map[string]interface{}{})
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	data2, err := envelope(resp2)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var result struct {
		Credit      int  `json:"credit"`
		StreakDays  int  `json:"streak_days"`
		IsStreakDay bool `json:"is_streak_day"`
	}
	_ = json.Unmarshal(data2, &result)
	return &pb.RunTaskResponse{
		Summary: fmt.Sprintf("签到成功，积分 +%d（连签 %d 天）", result.Credit, result.StreakDays),
	}, nil
}

// ---------- 工具 ----------

func nowMillis() int64 { return time.Now().UnixMilli() }

// trimFloat 浮点转不丢精度的十进制字符串（整数不带小数点）。
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// orHint 凭据解析失败时附上排查方向。
func orHint(err error) string {
	if strings.Contains(err.Error(), "unexpected end of JSON input") {
		return "凭据为空：账号可能未分组或凭据未正确保存，请重新授权或检查分组"
	}
	return err.Error()
}
