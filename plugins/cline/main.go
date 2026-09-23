// cline 插件 — Cline（api.cline.bot）反代。
// 上游：api.cline.bot/api/v1，OpenAI 兼容 /chat/completions + 业务信封 {data}。
// 登录：WorkOS 设备码授权 → /auth/register 换 cline refreshToken。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	pluginName   = "cline"
	apiBase      = "https://api.cline.bot/api/v1"
	deviceAuth   = "https://api.workos.com/user_management/authorize/device"
	authenticate = "https://api.workos.com/user_management/authenticate"
	workosClient = "client_01K3A541FN8TA3EPPHTD2325AR"
	settingsTTL  = 30 * time.Second
	// defaultClientVer 伪装的客户端版本默认值（X-CLIENT-VERSION / User-Agent）
	defaultClientVer = "2.3.6"
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	settingsJSON []byte // 插件设置缓存（30s）
	settingsAt   time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// settingStr 读插件设置（核心管理界面在线编辑），30s 内存缓存。
func (p *plugin) settingStr(key string) string {
	p.mu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < settingsTTL
	raw := p.settingsJSON
	p.mu.Unlock()
	if !fresh {
		if r := p.host.Settings(pluginName); r != nil {
			raw = r
		} else {
			raw = []byte("{}")
		}
		p.mu.Lock()
		p.settingsJSON, p.settingsAt = raw, time.Now()
		p.mu.Unlock()
	}
	var cfg map[string]string
	if json.Unmarshal(raw, &cfg) == nil {
		return cfg[key]
	}
	return ""
}

// userAgentStr 请求头 User-Agent（用户可配置；空 = 用 Cline/版本伪装）。
func (p *plugin) userAgentStr() string { return p.settingStr("user_agent") } // ---------- 凭据 blob ----------

type credential struct {
	RefreshToken string          `json:"refreshToken"`
	AccessToken  string          `json:"accessToken,omitempty"`
	ExpiresAt    int64           `json:"expiresAt,omitempty"` // unix 毫秒，0 为未知
	User         json.RawMessage `json:"user,omitempty"`
	UserID       string          `json:"userId,omitempty"` // clineUserId（余额接口路径用）

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.RefreshToken == "" && c.AccessToken == "" {
		return nil, fmt.Errorf("credential missing refreshToken")
	}
	c.proxyURL = shared.ProxyURL(blob.GetProxy())
	return c, nil
}

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hc(cred *credential) *http.Client {
	key := ""
	if cred != nil {
		key = cred.proxyURL
	}
	if c, ok := proxyClients.Load(key); ok {
		return c.(*http.Client)
	}
	c := shared.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
}

// headers /chat/completions 请求头（照官方客户端 resolveProviderRequestHeaders 的头集）。
func (p *plugin) headers(cred *credential, sessionID string) map[string]string {
	// accessToken 出请求头时带 workos: 前缀（照官方 auth-service.getAuthToken：
	// 后端靠前缀路由到 WorkOS 校验器）；存 blob 保持裸 token
	h := map[string]string{
		"Authorization":      "Bearer workos:" + cred.AccessToken,
		"Content-Type":       "application/json",
		"HTTP-Referer":       "https://cline.bot",
		"X-Title":            "Cline",
		"X-CLIENT-TYPE":      "cline-sdk",
		"X-CLIENT-VERSION":   p.clientVersion(),
		"X-PLATFORM":         "cline-sdk",
		"X-PLATFORM-VERSION": p.clientVersion(),
		"X-IS-MULTIROOT":     "false",
		"X-Task-ID":          sessionID,
		"User-Agent":         "Cline/" + p.clientVersion(),
	}
	return h
}

// clientVersion 伪装的客户端版本（settings client_version；空用内置默认）。
func (p *plugin) clientVersion() string {
	if v := p.settingStr("client_version"); v != "" {
		return v
	}
	return defaultClientVer
}

// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "Cline", "en": "Cline"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"client_version": {
					"type": "string",
					"title": "客户端版本号",
					"description": "X-CLIENT-VERSION / User-Agent 伪装值，留空使用内置默认",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "device", Label: map[string]string{"zh": "设备码登录", "en": "Device Login"}, Capabilities: []string{"refreshable"},
				Callback: "manual_poll", // 用户打开授权链接完成登录后手动确认
			},
			{
				Id: "refresh_file", Label: map[string]string{"zh": "凭据文件", "en": "Credential File"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": "refreshToken", "en": "refreshToken"},
					Type: "textarea", Required: true, Placeholder: `{"refreshToken": "..."}`,
				}},
			},
		},
	}}, nil
}

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

// ---------- 刷新 / 模型 ----------

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

// GetProfile 真实余额（/users/<id>/balance），失败降级为基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{DisplayName: credentialName(c), Healthy: true, Quota: map[string]string{}}
	p.fetchBalance(ctx, c, profile)
	return profile, nil
}

// fetchBalance 拉真实余额写标准键（美元数字字符串），失败降级为基本档案（原因进宿主日志）。
// /users/me 补 userId，/users/<id>/balance 给余额（照官方 ClineAccountBalance）。
func (p *plugin) fetchBalance(ctx context.Context, c *credential, profile *pb.AccountProfile) {
	if err := p.ensureToken(ctx, c); err != nil {
		p.host.Log("warn", "cline balance: token not ready: "+err.Error())
		return
	}
	userID := c.UserID
	if userID == "" {
		me, err := p.accountJSON(ctx, c, "GET", "/users/me", nil)
		if err != nil {
			p.host.Log("warn", "cline balance: /users/me failed: "+err.Error())
			return
		}
		userID = rawString(me["id"])
		c.UserID = userID // 缓存到凭据（随 Refresh 回传 Blob 持久化）
	}
	if userID == "" {
		p.host.Log("warn", "cline balance: /users/me returned no id")
		return
	}
	data, err := p.accountJSON(ctx, c, "GET", "/users/"+urlPathEscape(userID)+"/balance", nil)
	if err != nil {
		p.host.Log("warn", "cline balance: /users/<id>/balance failed: "+err.Error())
		return
	}
	balance, _ := data["balance"].(float64)
	if balance == 0 {
		if n, ok := data["balance"].(json.Number); ok {
			balance, _ = n.Float64()
		}
	}
	// 照官方 normalizeCreditBalance：balance 单位是百万分之一美元（microUSD），
	// 展示前 / 1_000_000 折成美元。免费账号 balance=0 也照写（积分栏显 0 而非空缺）。
	dollars := balance / 1_000_000
	// remaining/total 是核心前端积分列的标准键（Accounts.vue 读 credits.remaining/total）
	profile.Quota["remaining"] = formatUSD(dollars)
	profile.Quota["total"] = formatUSD(dollars)
	// CreditsJson：microUSD 原值 + 折算后的美元值（remaining/total 同值，前端积分列标准键）
	if b, err := json.Marshal(map[string]interface{}{
		"balance": balance, "dollars": dollars,
		"remaining": formatUSD(dollars), "total": formatUSD(dollars),
	}); err == nil {
		profile.CreditsJson = string(b)
	}
}

// accountJSON 账号面调用（{success, data} 信封），headers 与对话同源。
func (p *plugin) accountJSON(ctx context.Context, c *credential, method, path string, body []byte) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range p.headers(c, fmt.Sprintf("sess_account_%d", time.Now().UnixMilli())) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("account auth failed: HTTP 401")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var env struct {
		Success bool                   `json:"success"`
		Error   string                 `json:"error"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("non-json response")
	}
	if !env.Success {
		return nil, fmt.Errorf("%s", shared.OrDefault(env.Error, "account request failed"))
	}
	return env.Data, nil
}

// rawString 从原始 JSON 字段表按名取字符串。
func rawString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// urlPathEscape 路径段转义（userId 含特殊字符时防注入）。
func urlPathEscape(s string) string {
	var b strings.Builder
	for _, ch := range s {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// formatUSD 余额 → 美元数字字符串（保留两位小数以内）。
func formatUSD(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// ListModels 免费模型目录（官方 recommended-models 动态拉取；需 accessToken）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	if err := p.ensureToken(ctx, c); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", apiBase+"/ai/cline/recommended-models", nil)
	for k, v := range p.headers(c, fmt.Sprintf("sess_models_%d", time.Now().UnixMilli())) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Free []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"free"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil, fmt.Errorf("invalid models response")
	}
	var models []*pb.ModelInfo
	for _, m := range payload.Free {
		if m.ID == "" {
			continue
		}
		models = append(models, &pb.ModelInfo{
			Id: m.ID, Label: map[string]string{"en": shared.OrDefault(m.Name, m.ID)},
			SupportsTools: true, SupportsStream: true,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models endpoint returned an empty list")
	}
	return &pb.ModelList{Models: models}, nil
}

// ---------- Chat ----------

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	if err := p.ensureToken(ctx, c); err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	body := openaiup.ChatBody(req)
	body["model"] = shared.OrDefault(req.Model, "deepseek/deepseek-v4-flash")
	raw, _ := json.Marshal(body)

	resp, err := postJSON(ctx, p.hc(c), apiBase+"/chat/completions", p.headers(c, shared.RandHex(16)), raw)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		switch {
		case resp.StatusCode == 401:
			code = 401
		case resp.StatusCode == 429:
			code = 429
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return shared.ScanSSE(resp.Body, parser)
}

// ---------- 工具 ----------

func postJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, bytes.NewReader(body))
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

func postForm(ctx context.Context, rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return shared.UpstreamClient("").Do(req)
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
