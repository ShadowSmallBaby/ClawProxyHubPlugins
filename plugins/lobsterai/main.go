// lobsterai 插件 — 网易有道 LobsterAI 客户端反代。
// 上游：lobsterai-server.youdao.com，OpenAI 兼容（/api/proxy 前缀）+ 业务信封。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/anthropicup"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	serverBase       = "https://lobsterai-server.youdao.com"
	pathExchange     = "/api/auth/exchange"
	pathRefresh      = "/api/auth/refresh"
	pathModels       = "/api/models/available"
	pathQuota        = "/api/user/quota"
	pathProfileSum   = "/api/user/profile-summary"
	proxyPrefix      = "/api/proxy"
	defaultClientVer = "2026.8.21"
	capabilitiesHdr  = "kimi-k3-agentic-v1,thinking-level-control-v1"
	manualCallback   = "http://127.0.0.1:53682/auth/callback"
	portalLoginURL   = "https://lobsterai.youdao.com/portal#/login"
	overmindLogin    = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/login-url"
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	oauth        map[string]*oauthSession // state → 进行中的授权会话（单条，覆盖旧的）
	oauthCB      *callbackServer          // 进行中的本地回调 server（懒起，完成/超时即关）
	settingsJSON []byte                   // 插件设置缓存（30s）
	settingsAt   time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// ---------- 凭据 blob（.auth/*.json 形态） ----------

type credential struct {
	AccessToken      string          `json:"accessToken"`
	RefreshToken     string          `json:"refreshToken"`
	ExpiresAt        float64         `json:"expiresAt"`
	User             json.RawMessage `json:"user,omitempty"`
	InstallationUUID string          `json:"installation_uuid,omitempty"`
	EnterpriseID     string          `json:"enterpriseId,omitempty"`
	// .auth 文件整体结构兼容（桌面端 / 状态文件两种形态）
	AuthTokens *struct {
		AccessToken  string  `json:"accessToken"`
		RefreshToken string  `json:"refreshToken"`
		ExpiresAt    float64 `json:"expiresAt"`
	} `json:"auth_tokens,omitempty"`
	AuthUser json.RawMessage `json:"auth_user,omitempty"`
	// .auth 文件的企业上下文（enterpriseId 在嵌套里）
	AuthEnterprise struct {
		EnterpriseID string `json:"enterpriseId"`
		ID           string `json:"id"`
	} `json:"auth_enterprise,omitempty"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c, err := parseCred(blob.GetBlob())
	if err != nil {
		return nil, err
	}
	c.proxyURL = proxyURL(blob.GetProxy())
	return c, nil
}

// proxyURL 代理配置 → URL 字符串。
func proxyURL(p *pb.ProxyConfig) string {
	if p == nil || p.GetHost() == "" {
		return ""
	}
	u := &url.URL{Scheme: orDefault(p.GetScheme(), "http"), Host: fmt.Sprintf("%s:%d", p.GetHost(), p.GetPort())}
	if p.GetUsername() != "" {
		u.User = url.UserPassword(p.GetUsername(), p.GetPassword())
	}
	return u.String()
}

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hc(cred *credential) *http.Client {
	if cred == nil || cred.proxyURL == "" {
		return upstreamClient("")
	}
	if c, ok := proxyClients.Load(cred.proxyURL); ok {
		return c.(*http.Client)
	}
	u, err := url.Parse(cred.proxyURL)
	if err != nil {
		return upstreamClient("")
	}
	c := upstreamClient(u.String())
	proxyClients.Store(cred.proxyURL, c)
	return c
}

// upstreamClient 上游 HTTP client：连接 15s / TLS 15s / 首字节 60s，
// 流式对话整体不设超时（长回复合法）。
func upstreamClient(proxyURL string) *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: transport}
}

func parseCred(blob []byte) (*credential, error) {
	var c credential
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("invalid credential: %w", err)
	}
	// .auth 文件完整结构：token/user 在嵌套字段里
	if c.AccessToken == "" && c.AuthTokens != nil {
		c.AccessToken = c.AuthTokens.AccessToken
		c.RefreshToken = orDefault(c.RefreshToken, c.AuthTokens.RefreshToken)
		if c.ExpiresAt == 0 {
			c.ExpiresAt = c.AuthTokens.ExpiresAt
		}
	}
	if len(c.User) == 0 && len(c.AuthUser) > 0 {
		c.User = c.AuthUser
	}
	if c.AccessToken == "" {
		return nil, fmt.Errorf("credential missing accessToken（支持 .auth 文件原文或 {\"accessToken\": ...} 精简格式）")
	}
	// 企业标识兼容两种形态：顶层 enterpriseId（本插件签发）与 auth_enterprise 嵌套（.auth 文件）
	if c.EnterpriseID == "" {
		if c.AuthEnterprise.EnterpriseID != "" {
			c.EnterpriseID = c.AuthEnterprise.EnterpriseID
		} else {
			c.EnterpriseID = c.AuthEnterprise.ID
		}
	}
	return &c, nil
}

func (p *plugin) clientVersion() string {
	if v := p.settingStr("client_version"); v != "" {
		return v
	}
	return defaultClientVer
}

// userAgentStr 请求头 User-Agent（用户可配置；空 = 不设置，上游不校验）。
func (p *plugin) userAgentStr() string {
	return p.settingStr("user_agent")
}

// settingStr 读插件设置（核心管理界面在线编辑），30s 内存缓存。
func (p *plugin) settingStr(key string) string {
	p.mu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < 30*time.Second
	raw := p.settingsJSON
	p.mu.Unlock()
	if !fresh {
		if r := p.host.Settings("lobsterai"); r != nil {
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

// capabilityHeaders 上游能力头（企业账号带 enterprise 头）。
func (p *plugin) capabilityHeaders(cred *credential) map[string]string {
	h := map[string]string{
		"X-LobsterAI-Client-Capabilities": capabilitiesHdr,
		"X-LobsterAI-Client-Version":      p.clientVersion(),
		"User-Agent":                      p.userAgentStr(),
	}
	if cred.EnterpriseID != "" {
		h["X-LobsterAI-Account-Mode"] = "enterprise"
		h["X-LobsterAI-Enterprise-Id"] = cred.EnterpriseID
	} else {
		h["X-LobsterAI-Account-Mode"] = "personal"
	}
	return h
}

func (p *plugin) authHeaders(cred *credential) map[string]string {
	h := p.capabilityHeaders(cred)
	h["Authorization"] = "Bearer " + cred.AccessToken
	h["Content-Type"] = "application/json"
	return h
}

// keyfrom 归因负载。
func keyfrom(cred *credential, version string) map[string]string {
	payload := map[string]string{
		"firstKeyfrom": "official", "latestKeyfrom": "official",
		"uuid": cred.InstallationUUID, "version": version,
	}
	var user struct {
		UserID string `json:"userId"`
		ID     string `json:"id"`
	}
	_ = json.Unmarshal(cred.User, &user)
	if user.UserID != "" {
		payload["userId"] = user.UserID
	} else if user.ID != "" {
		payload["userId"] = user.ID
	}
	return payload
}

// envelope 校验 {code,message,data} 业务信封。
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
	if resp.StatusCode == 401 || e.Code == 40100 || e.Code == 40101 || e.Code == 41602 {
		return nil, &upstreamAuthError{msg: fmt.Sprintf("auth failed: HTTP %d code=%d %s", resp.StatusCode, e.Code, e.Message)}
	}
	if e.Code != 0 {
		return nil, &upstreamError{code: e.Code, message: e.Message}
	}
	return e.Data, nil
}

type upstreamAuthError struct{ msg string }

func (e *upstreamAuthError) Error() string { return e.msg }

// upstreamError 上游业务拒绝（envelope code != 0），保留原始业务码供按码分支。
type upstreamError struct {
	code    int
	message string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream rejected: code=%d %s", e.code, e.message)
}

// activityErrorCodes 上游活动接口业务码（照桌面端语义）。
var activityErrorCodes = map[int]string{
	51100: "NotFound",
	51101: "NotActive",
	51102: "LoginRequired",
	51103: "ActionInvalid",
	51104: "AlreadyClaimed",
	51105: "ConfigInvalid",
	51106: "RevisionMismatch",
}

// benignActivityCodes 无需重试的良性码：目标已达成或无活动，按成功结果汇报。
var benignActivityCodes = map[int]string{
	51101: "当前无签到活动",
	51104: "今日已签到（上游确认）",
}

// activityResult 活动接口错误的统一翻译：良性业务码转成功结果，其余带码名透传。
func activityResult(err error) (*pb.RunTaskResponse, error) {
	if ue, ok := err.(*upstreamError); ok {
		if msg, benign := benignActivityCodes[ue.code]; benign {
			return &pb.RunTaskResponse{Summary: msg}, nil
		}
		if name := activityErrorCodes[ue.code]; name != "" {
			return nil, status.Error(codes.Internal, fmt.Sprintf("%s（%s）", err.Error(), name))
		}
	}
	return nil, status.Error(codes.Internal, err.Error())
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
		client = upstreamClient("")
	}
	return client.Do(req)
}

// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: "lobsterai", Version: version, Author: "cph",
		Label:           map[string]string{"zh": "LobsterAI", "en": "LobsterAI"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "tasks"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"client_version": {
					"type": "string",
					"title": "客户端版本号",
					"description": "请求头 X-LobsterAI-Client-Version 的伪装值，留空使用内置默认",
					"default": ""
				},
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "请求头 User-Agent 伪装值，留空则不设置（上游不校验）",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "oauth", Label: map[string]string{"zh": "浏览器登录", "en": "Browser Login"}, Capabilities: []string{"refreshable"},
				Callback: "auto_wait", // 本机访问自动回调，服务器部署转手动粘贴
			},
			{
				Id: "auth_file", Label: map[string]string{"zh": "凭据文件", "en": "Credential File"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": ".auth/account.json 内容", "en": ".auth/account.json content"},
					Type: "textarea", Required: true, Placeholder: `{"accessToken": "...", "refreshToken": "..."}`,
				}},
			},
		},
	}}, nil
}

func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "auth_file":
		cred, err := parseCred([]byte(req.Form["content"]))
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
		}
		return loginDone(cred)

	case "oauth":
		return p.loginOAuth(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "unknown auth method: "+req.MethodId)
}

// loginOAuth 浏览器授权：发起时才监听本地回调端口（懒加载），
// 回调到达即在插件侧完成兑换并立即关停监听；超时自动作废。
// 服务器部署时回调地址不可达，用户把授权后地址栏的完整 URL 粘贴到 callback_url 提交。
func (p *plugin) loginOAuth(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		// 第一步：起本地回调 server + 生成登录链接
		state := randHex(16)
		cb := startCallbackServer(state, func(code string) {
			go p.completeOAuth(state, code) // 回调到达：异步兑换，成功即关停监听
		})
		p.mu.Lock()
		p.oauth = map[string]*oauthSession{state: {status: "pending"}} // 单条即可，覆盖旧的
		p.oauthCB = cb
		p.mu.Unlock()
		// 超时兜底：到点未完成则关停监听并作废会话
		time.AfterFunc(oauthTTL, func() { p.expireOAuth(state) })

		loginBase, err := resolveLoginURL(ctx)
		if err != nil {
			p.finishOAuth(state)
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
		}
		loginURL := composeLoginURL(loginBase, state, cb.RedirectURI())
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url", Url: loginURL,
			Prompt: map[string]string{"zh": "已打开浏览器授权页，完成登录后此处自动完成", "en": "Browser auth page opened; this step completes automatically after sign-in"},
			State:  []byte(state),
			Wait:   true,
			Fields: callbackURLField(),
		}}, nil
	}

	// 后续步：手动粘贴的回调 URL 优先，其次轮询回调会话状态
	state := string(req.State)
	p.mu.Lock()
	sess := p.oauth[state]
	p.mu.Unlock()
	if sess == nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "state 已失效，请重新发起"}}, nil
	}

	if raw := strings.TrimSpace(req.Form["callback_url"]); raw != "" {
		cbState, code := parseCallbackURL(raw)
		if code == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "回调地址中缺少 code 参数，请复制浏览器地址栏的完整 URL"}}, nil
		}
		if cbState != "" && cbState != state {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "回调地址 state 不匹配，请重新发起授权"}}, nil
		}
		p.finishOAuth(state)
		cred, fail := p.exchangeCred(ctx, code)
		if fail != nil {
			return fail, nil
		}
		return loginDone(cred)
	}

	// 轮询：pending 继续等 / failed 报错 / done 即刻建档
	p.mu.Lock()
	status, cred, failErr := sess.status, sess.cred, sess.err
	p.mu.Unlock()
	switch status {
	case "done":
		p.finishOAuth(state)
		return loginDone(cred)
	case "failed":
		p.finishOAuth(state)
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: failErr}}, nil
	}
	return &pb.LoginResult{Next: &pb.LoginNextStep{
		Action: "open_url",
		Prompt: map[string]string{"zh": "等待浏览器完成授权...（服务器部署时请把回调地址粘贴到下方提交）", "en": "Waiting for browser authorization... (server deployment: paste the callback URL below and submit)"},
		State:  []byte(state),
		Wait:   true,
		Fields: callbackURLField(),
	}}, nil
}

// oauthSession 一次授权会话的状态：pending → done（凭据就绪）/ failed。
type oauthSession struct {
	status string
	cred   *credential
	err    string
}

// oauthTTL 授权会话有效期：超时自动关停监听并作废。
const oauthTTL = 5 * time.Minute

// completeOAuth 回调到达：兑换凭据写入会话，成功即关停监听（页面已自动关闭）。
func (p *plugin) completeOAuth(state, code string) {
	cred, fail := p.exchangeCred(context.Background(), code)
	p.mu.Lock()
	sess, ok := p.oauth[state]
	if ok {
		if fail != nil {
			sess.status, sess.err = "failed", fail.Error.GetMessage()
		} else {
			sess.status, sess.cred = "done", cred
		}
	}
	cb := p.oauthCB
	p.oauthCB = nil
	p.mu.Unlock()
	if cb != nil {
		cb.Close()
	}
}

// expireOAuth 超时兜底：关停监听并作废未完成的会话（已被新一轮覆盖的不动）。
func (p *plugin) expireOAuth(state string) {
	p.mu.Lock()
	sess, ok := p.oauth[state]
	if !ok || sess.status != "pending" {
		p.mu.Unlock()
		return
	}
	sess.status, sess.err = "failed", "授权超时（5 分钟未完成），请重新发起"
	cb := p.oauthCB
	p.oauthCB = nil
	p.mu.Unlock()
	if cb != nil {
		cb.Close()
	}
}

// callbackURLField 手动粘贴回调地址的输入框（服务器部署时本地回调不可达）。
func callbackURLField() []*pb.AuthField {
	return []*pb.AuthField{{
		Name:        "callback_url",
		Label:       map[string]string{"zh": "回调地址", "en": "Callback URL"},
		Type:        "textarea",
		Placeholder: "授权后浏览器地址栏的完整 URL（http://127.0.0.1:…/auth/callback?code=…&state=…）",
	}}
}

// finishOAuth 清理进行中的授权会话与回调 server。
func (p *plugin) finishOAuth(state string) {
	p.mu.Lock()
	delete(p.oauth, state)
	cb := p.oauthCB
	p.oauthCB = nil
	p.mu.Unlock()
	if cb != nil {
		cb.Close()
	}
}

// exchangeCred 用授权码换凭据；fail 非 nil 表示兑换失败（已含错误信息）。
func (p *plugin) exchangeCred(ctx context.Context, code string) (*credential, *pb.LoginResult) {
	cred := &credential{InstallationUUID: newUUID()}
	exchangeBody := withKeyfrom(map[string]interface{}{"authCode": code}, cred, p.clientVersion())
	resp, err := postJSON(ctx, nil, serverBase+pathExchange, map[string]string{"Content-Type": "application/json"}, exchangeBody)
	if err != nil {
		return nil, &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}
	}
	data, err := envelope(resp)
	if err != nil {
		return nil, &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}
	}
	var exchanged struct {
		AccessToken  string          `json:"accessToken"`
		RefreshToken string          `json:"refreshToken"`
		ExpiresAt    float64         `json:"expiresAt"`
		User         json.RawMessage `json:"user"`
	}
	if err := json.Unmarshal(data, &exchanged); err != nil || exchanged.AccessToken == "" {
		return nil, &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "exchange 响应缺少 accessToken"}}
	}
	cred.AccessToken, cred.RefreshToken, cred.ExpiresAt, cred.User =
		exchanged.AccessToken, exchanged.RefreshToken, exchanged.ExpiresAt, exchanged.User
	return cred, nil
}

// parseCallbackURL 从粘贴的回调 URL 提取 state 与 code（解析失败时裸扫 code 兜底）。
func parseCallbackURL(raw string) (state, code string) {
	if u, err := url.Parse(raw); err == nil {
		q := u.Query()
		return q.Get("state"), q.Get("code")
	}
	return "", extractCode(raw)
}

// callbackServer 本地回环回调：浏览器授权后上游跳转到这里，自动取走 code。
// 页面在通知 onCode 后自动关闭（脚本 window.close，手动打开的标签兜底提示）。
type callbackServer struct {
	listener net.Listener
	server   *http.Server
	state    string
	onCode   func(code string)
}

func startCallbackServer(state string, onCode func(code string)) *callbackServer {
	cb := &callbackServer{state: state, onCode: onCode}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != cb.state || q.Get("code") == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(autoClosePage("登录状态校验失败，请回到授权窗口重试。")))
			return
		}
		if cb.onCode != nil {
			cb.onCode(q.Get("code"))
		}
		w.Write([]byte(autoClosePage("登录成功，正在完成授权…")))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return &callbackServer{state: state, onCode: onCode} // 无回调能力时退化为粘回调路径
	}
	cb.listener = ln
	cb.server = &http.Server{Handler: mux}
	go cb.server.Serve(ln)
	return cb
}

// autoClosePage 授权结果页：短暂展示后自动关闭（脚本开的窗口可关，手动开的兜底文案）。
func autoClosePage(msg string) string {
	return "<html><body style=\"font-family:sans-serif;text-align:center;padding-top:80px\">" +
		"<h2>" + msg + "</h2>" +
		"<p style=\"color:#888\">本页面将自动关闭，若未关闭可手动关闭。</p>" +
		"<script>setTimeout(function(){window.close();},800)</script>" +
		"</body></html>"
}

// RedirectURI 回调地址（监听失败返回手工粘贴用的固定端口形态）。
func (c *callbackServer) RedirectURI() string {
	if c.listener == nil {
		return manualCallback + "?return_to=none"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/auth/callback", c.listener.Addr().(*net.TCPAddr).Port)
}

func (c *callbackServer) Close() {
	if c.server != nil {
		c.server.Close()
	}
}

func loginDone(cred *credential) (*pb.LoginResult, error) {
	blob, _ := json.Marshal(cred)
	name := credentialName(cred)
	return &pb.LoginResult{
		Blob: blob,
		Profile: &pb.AccountProfile{
			DisplayName: name, Healthy: true, Quota: map[string]string{},
		},
	}, nil
}

func credentialName(cred *credential) string {
	var user struct {
		Nickname string `json:"nickname"`
		UserName string `json:"userName"`
		Email    string `json:"email"`
		UserID   string `json:"userId"`
	}
	_ = json.Unmarshal(cred.User, &user)
	for _, v := range []string{user.Nickname, user.UserName, user.Email, user.UserID} {
		if v != "" {
			return v
		}
	}
	return "lobsterai-account"
}

// ---------- 刷新 / 资料 / 模型 ----------

func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if cred.RefreshToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "没有 refreshToken，请重新登录"}}, nil
	}
	body := withKeyfrom(map[string]interface{}{"refreshToken": cred.RefreshToken}, cred, p.clientVersion())
	resp, err := postJSON(ctx, p.hc(cred), serverBase+pathRefresh, map[string]string{"Content-Type": "application/json"}, body)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: err.Error()}}, nil
	}
	data, err := envelope(resp)
	if err != nil {
		code := int32(503)
		if _, isAuth := err.(*upstreamAuthError); isAuth {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	var refreshed struct {
		AccessToken  string          `json:"accessToken"`
		RefreshToken string          `json:"refreshToken"`
		ExpiresAt    float64         `json:"expiresAt"`
		User         json.RawMessage `json:"user"`
	}
	if err := json.Unmarshal(data, &refreshed); err != nil || refreshed.AccessToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: "refresh 响应缺少 accessToken"}}, nil
	}
	if refreshed.RefreshToken != "" {
		cred.RefreshToken = refreshed.RefreshToken
	}
	if len(refreshed.User) > 0 {
		cred.User = refreshed.User
	}
	cred.AccessToken, cred.ExpiresAt = refreshed.AccessToken, refreshed.ExpiresAt
	blob, _ := json.Marshal(cred)
	profile := &pb.AccountProfile{
		DisplayName: credentialName(cred), Healthy: true, Quota: map[string]string{},
	}
	// 刷新成功后顺带拉积分与签到状态，避免快照缺块
	p.fetchQuota(ctx, cred, profile)
	if sec := p.checkinSection(ctx, cred); sec != nil {
		profile.Sections = append(profile.Sections, sec)
	}
	return &pb.RefreshResult{Blob: blob, Profile: profile}, nil
}

// GetProfile 真实积分余额（profile-summary），失败降级为基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{
		DisplayName: credentialName(cred), Healthy: true, Quota: map[string]string{},
	}
	p.fetchQuota(ctx, cred, profile)
	if sec := p.checkinSection(ctx, cred); sec != nil {
		profile.Sections = append(profile.Sections, sec)
	}
	return profile, nil
}

// checkinSection 签到状态动态块（活动开启时才渲染；失败静默跳过）。
// 复用 RunTask checkin 的前两步：slot 找活动 → context 查状态，不执行签到动作。
func (p *plugin) checkinSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	q := url.Values{
		"placement":           {"desktop_sidebar"},
		"clientVersion":       {p.clientVersion()},
		"containerApiVersion": {"2"},
		"platform":            {"win32"},
	}
	slotReq, _ := http.NewRequestWithContext(ctx, "GET", serverBase+"/api/client-activities/slot?"+q.Encode(), nil)
	for k, v := range p.authHeaders(cred) {
		slotReq.Header.Set(k, v)
	}
	slotReq.Header.Set("Cache-Control", "no-store")
	resp, err := p.hc(cred).Do(slotReq)
	if err != nil {
		return nil
	}
	slotData, err := envelope(resp)
	if err != nil {
		return nil
	}
	var slot struct {
		Activity struct {
			ActivityCode   string `json:"activityCode"`
			ConfigRevision int    `json:"configRevision"`
		} `json:"activity"`
		ActivityCode   string `json:"activityCode"`
		ConfigRevision int    `json:"configRevision"`
	}
	_ = json.Unmarshal(slotData, &slot)
	code, revision := slot.Activity.ActivityCode, slot.Activity.ConfigRevision
	if code == "" {
		code, revision = slot.ActivityCode, slot.ConfigRevision
	}
	if code == "" {
		return nil // 无活动
	}
	ctxReq, _ := http.NewRequestWithContext(ctx, "GET",
		serverBase+"/api/client-activities/"+code+"/context?configRevision="+fmt.Sprint(revision), nil)
	for k, v := range p.authHeaders(cred) {
		ctxReq.Header.Set(k, v)
	}
	ctxReq.Header.Set("Cache-Control", "no-store")
	resp2, err := p.hc(cred).Do(ctxReq)
	if err != nil {
		return nil
	}
	ctxData, err := envelope(resp2)
	if err != nil {
		return nil
	}
	var state struct {
		State struct {
			ClaimedToday bool `json:"claimedToday"`
			ClaimedDays  int  `json:"claimedDays"`
			// 兼容旧字段形状
			TodayCheckedIn bool `json:"todayCheckedIn"`
			StreakDays     int  `json:"streakDays"`
		} `json:"state"`
		ClaimedToday   bool `json:"claimedToday"`
		ClaimedDays    int  `json:"claimedDays"`
		TodayCheckedIn bool `json:"todayCheckedIn"`
		StreakDays     int  `json:"streakDays"`
	}
	if json.Unmarshal(ctxData, &state) != nil {
		return nil
	}
	checked := state.State.ClaimedToday || state.ClaimedToday || state.State.TodayCheckedIn || state.TodayCheckedIn
	streak := state.State.ClaimedDays
	if streak == 0 {
		streak = state.ClaimedDays
	}
	if streak == 0 {
		streak = state.State.StreakDays
	}
	if streak == 0 {
		streak = state.StreakDays
	}
	status := "已签到"
	if !checked {
		status = "未签到"
	}
	entries := []*pb.SectionEntry{
		{Label: map[string]string{"zh": "今日签到", "en": "Today"}, Value: "status:" + status, Kind: "status"},
	}
	if streak > 0 {
		entries = append(entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "连签天数", "en": "Streak"}, Value: fmt.Sprintf("%d 天", streak),
		})
	}
	return &pb.ProfileSection{
		Id:      "checkin",
		Title:   map[string]string{"zh": "签到情况", "en": "Check-in"},
		Entries: entries,
	}
}

// fetchQuota 拉取积分并写入标准键（数字字符串），失败静默降级。
// 两接口并发取数：/api/user/quota 给免费池上限/已用（多形状归一化），
// /api/user/profile-summary 补真实剩余与分池明细（creditItems）。
func (p *plugin) fetchQuota(ctx context.Context, cred *credential, profile *pb.AccountProfile) {
	quota := p.fetchQuotaPool(ctx, cred)
	summary := p.fetchQuotaSummary(ctx, cred)

	if summary == nil {
		summary = &quotaSummary{}
	}
	// 真实可用 = profile-summary 的 totalCreditsRemaining（主数字）。
	// quota 接口的数字只是免费池（limit/used），池子用尽或过期后恒为 0，
	// 不作为"总积分/已用"透出（会与真实可用自相矛盾），独立放 free_* 键。
	if summary.Remaining != 0 {
		profile.Quota["credits"] = trimFloat(summary.Remaining)
	}
	if len(summary.Packages) > 0 {
		profile.Quota["packages"] = fmt.Sprintf("%d", len(summary.Packages))
	}
	// credits_json：真实可用 + 积分包 + 免费池附注
	if len(summary.Packages) > 0 || summary.Remaining != 0 {
		out := map[string]interface{}{}
		if summary.Remaining != 0 {
			out["remaining"] = trimFloat(summary.Remaining)
		}
		if len(summary.Packages) > 0 {
			out["packages"] = summary.Packages
		}
		// 免费池附注（quota 接口数字）：上限同时作为列表列的"总积分"
		if quota.Total != 0 || quota.Used != 0 {
			out["free_limit"] = trimFloat(quota.Total)
			out["free_used"] = trimFloat(quota.Used)
			if quota.Total != 0 {
				out["total"] = trimFloat(quota.Total)
			}
		}
		if b, err := json.Marshal(out); err == nil {
			profile.CreditsJson = string(b)
		}
	}
	// 积分包明细动态块（分池 creditItems：剩余 + 到期），有包才定义
	if len(summary.Packages) > 0 {
		sec := &pb.ProfileSection{
			Id:    "packages",
			Title: map[string]string{"zh": "积分包", "en": "Credit Packages"},
			Columns: []*pb.SectionColumn{
				{Key: "remaining", Title: map[string]string{"zh": "剩余", "en": "Remaining"}},
				{Key: "label", Title: map[string]string{"zh": "积分包", "en": "Package"}},
				{Key: "expiresAt", Title: map[string]string{"zh": "到期", "en": "Expires"}},
			},
		}
		for _, pk := range summary.Packages {
			sec.Items = append(sec.Items, &pb.SectionRow{Cells: pk})
		}
		profile.Sections = append(profile.Sections, sec)
	}
}

// quotaSummary profile-summary 的解析结果（真实剩余 + 分池明细）。
type quotaSummary struct {
	Remaining float64
	Packages  []map[string]string
}

// fetchQuotaSummary GET /api/user/profile-summary → 真实剩余 + 分池明细；失败返回 nil。
func (p *plugin) fetchQuotaSummary(ctx context.Context, cred *credential) *quotaSummary {
	req, _ := http.NewRequestWithContext(ctx, "GET", serverBase+pathProfileSum, nil)
	for k, v := range p.authHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil
	}
	data, err := envelope(resp)
	if err != nil {
		return nil
	}
	var raw struct {
		// envelope 已解包 data 层，creditItems 就在根上；
		// 兼容上游又包一层 data 的形状
		Data struct {
			TotalCreditsRemaining float64 `json:"totalCreditsRemaining"`
			CreditItems           []struct {
				Type             string  `json:"type"`
				Label            string  `json:"label"`
				CreditsRemaining float64 `json:"creditsRemaining"`
				ExpiresAt        string  `json:"expiresAt"`
			} `json:"creditItems"`
		} `json:"data"`
		TotalCreditsRemaining float64 `json:"totalCreditsRemaining"`
		CreditItems           []struct {
			Type             string  `json:"type"`
			Label            string  `json:"label"`
			CreditsRemaining float64 `json:"creditsRemaining"`
			ExpiresAt        string  `json:"expiresAt"`
		} `json:"creditItems"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	s := &quotaSummary{}
	// 兼容平铺（envelope 已解包）与再包一层 data 两种形状（照 normalize_credit_summary）
	items := raw.CreditItems
	s.Remaining = raw.TotalCreditsRemaining
	if len(items) == 0 && s.Remaining == 0 {
		items = raw.Data.CreditItems
		s.Remaining = raw.Data.TotalCreditsRemaining
	}
	for _, it := range items {
		expiry := it.ExpiresAt
		if t, err := time.Parse(time.RFC3339, it.ExpiresAt); err == nil {
			expiry = t.Format("2006-01-02")
		}
		s.Packages = append(s.Packages, map[string]string{
			"remaining": trimFloat(it.CreditsRemaining),
			"label":     orDefault(it.Label, orDefault(it.Type, "积分包")),
			"expiresAt": expiry,
		})
	}
	return s
}

// quotaPool /api/user/quota 的总积分/已用（多形状字段归一化，照 _QUOTA_FIELD_SHAPES）。
type quotaPool struct {
	Total float64
	Used  float64
}

// fetchQuotaPool GET /api/user/quota → 归一化的总积分/已用；失败返回零值。
func (p *plugin) fetchQuotaPool(ctx context.Context, cred *credential) quotaPool {
	req, _ := http.NewRequestWithContext(ctx, "GET", serverBase+pathQuota, nil)
	for k, v := range p.authHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return quotaPool{}
	}
	data, err := envelope(resp)
	if err != nil {
		return quotaPool{}
	}
	// 兼容 data/quota 包一层与平铺（照 normalize_auth_quota）
	var outer map[string]json.RawMessage
	if json.Unmarshal(data, &outer) != nil {
		return quotaPool{}
	}
	body := data
	for _, key := range []string{"quota", "data"} {
		var wrapped map[string]json.RawMessage
		if json.Unmarshal(body, &wrapped) == nil {
			if inner, ok := wrapped[key]; ok {
				var probe map[string]json.RawMessage
				if json.Unmarshal(inner, &probe) == nil {
					body = inner
					break
				}
			}
		}
	}
	// 多形状字段对：服务端会下发多套字段，按顺序取第一组非零
	shapes := [][2]string{
		{"limit", "used"},
		{"freeCreditsTotal", "freeCreditsUsed"},
		{"monthlyCreditsLimit", "monthlyCreditsUsed"},
		{"dailyCreditsLimit", "dailyCreditsUsed"},
		{"creditsLimit", "creditsUsed"},
	}
	var out quotaPool
	for _, shape := range shapes {
		// data 里混有 bool/string 字段，逐字段用 json.Number 按名取，避免整体 unmarshal 失败
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			continue
		}
		total, used := numField(fields, shape[0]), numField(fields, shape[1])
		if total != 0 || used != 0 {
			out.Total, out.Used = total, used
			break
		}
	}
	return out
}

// numField 从原始 JSON 字段表里按名取数字（bool/string 忽略，取不到返回 0）。
func numField(fields map[string]json.RawMessage, name string) float64 {
	raw, ok := fields[name]
	if !ok {
		return 0
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		return 0
	}
	return n
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for k, v := range keyfrom(cred, p.clientVersion()) {
		if v != "" {
			q.Set(k, v)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", serverBase+pathModels+"?"+q.Encode(), nil)
	for k, v := range p.authHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	data, err := envelope(resp)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ModelID   string `json:"modelId"`
		ModelName string `json:"modelName"`
		APIFormat string `json:"apiFormat"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var models []*pb.ModelInfo
	for _, m := range raw {
		if m.ModelID == "" {
			continue
		}
		isAnthropic := m.APIFormat == "anthropic"
		anthropicModels.Store(m.ModelID, isAnthropic) // Chat 直通判定缓存
		models = append(models, &pb.ModelInfo{
			Id: m.ModelID, Label: map[string]string{"en": orDefault(m.ModelName, m.ModelID)},
			SupportsTools: !isAnthropic, SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: models}, nil
}

// ---------- Chat ----------

// anthropicModels apiFormat=anthropic 的模型缓存（Chat 时惰性填充）。
var anthropicModels sync.Map

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(failed(401, orHint(err)))
	}

	// apiFormat=anthropic 的模型直通 /v1/messages，其余转 openai 方言
	if p.isAnthropicModel(ctx, cred, req.Model) {
		return p.chatAnthropic(req, stream, cred)
	}

	body := openaiup.ChatBody(req)
	body["model"] = orDefault(req.Model, "auto")

	resp, err := postJSON(ctx, p.hc(cred), serverBase+proxyPrefix+"/v1/chat/completions", p.authHeaders(cred), body)
	if err != nil {
		return stream.Send(failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		if resp.StatusCode == 401 {
			code = 401
		}
		return stream.Send(failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}

	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return scanSSE(resp.Body, parser, stream)
}

// chatAnthropic anthropic 方言直通：POST /api/proxy/v1/messages。
func (p *plugin) chatAnthropic(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer, cred *credential) error {
	ctx := stream.Context()
	body := anthropicup.ChatBody(req)

	resp, err := postJSON(ctx, p.hc(cred), serverBase+proxyPrefix+"/v1/messages", p.authHeaders(cred), body)
	if err != nil {
		return stream.Send(failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		if resp.StatusCode == 401 {
			code = 401
		}
		return stream.Send(failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))))
	}
	parser := anthropicup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return scanSSE(resp.Body, parser, stream)
}

// scanSSE 通用 SSE 扫描：空流兜底 + 结束收尾。
func scanSSE(body io.Reader, parser interface {
	Feed(string)
	Finish()
	FinishWithError(int32, string)
}, stream pb.ClawPlugin_ChatServer) error {
	scanner := bufio.NewScanner(body)
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

// isAnthropicModel 查模型方言（缓存 miss 时拉一次目录）。
func (p *plugin) isAnthropicModel(ctx context.Context, cred *credential, model string) bool {
	if v, ok := anthropicModels.Load(model); ok {
		return v.(bool)
	}
	list, err := p.ListModels(ctx, &pb.CredentialBlob{Blob: mustJSON(cred), Proxy: proxyPB(cred)})
	if err != nil {
		return false
	}
	for _, m := range list.Models {
		// 声明 SupportsTools=false 的即 anthropic 方言（ListModels 的映射约定）
		anthropicModels.Store(m.Id, !m.SupportsTools)
	}
	v, _ := anthropicModels.Load(model)
	is, _ := v.(bool)
	return is
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---------- 任务能力：每日签到 ----------

func (p *plugin) ListTaskCapabilities(ctx context.Context, _ *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{
			{
				Id: "checkin", Label: map[string]string{"zh": "每日签到", "en": "Daily Check-in"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:05",
			},
		},
	}, nil
}

// RunTask checkin 三步：slot 找活动 → context 查状态 → action 幂等签到。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	if req.CapabilityId != "checkin" {
		return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
	}
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// 1. 活动位
	q := url.Values{
		"placement":           {"desktop_sidebar"},
		"clientVersion":       {p.clientVersion()},
		"containerApiVersion": {"2"},
		"platform":            {"win32"},
	}
	slotReq, _ := http.NewRequestWithContext(ctx, "GET", serverBase+"/api/client-activities/slot?"+q.Encode(), nil)
	for k, v := range p.authHeaders(cred) {
		slotReq.Header.Set(k, v)
	}
	slotReq.Header.Set("Cache-Control", "no-store")
	resp, err := p.hc(cred).Do(slotReq)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	slotData, err := envelope(resp)
	if err != nil {
		return activityResult(err)
	}
	var slot struct {
		Activity struct {
			ActivityCode   string `json:"activityCode"`
			ConfigRevision int    `json:"configRevision"`
		} `json:"activity"`
		ActivityCode   string `json:"activityCode"`
		ConfigRevision int    `json:"configRevision"`
	}
	_ = json.Unmarshal(slotData, &slot)
	code, revision := slot.Activity.ActivityCode, slot.Activity.ConfigRevision
	if code == "" {
		code, revision = slot.ActivityCode, slot.ConfigRevision
	}
	if code == "" {
		return &pb.RunTaskResponse{Summary: "当前无签到活动"}, nil
	}

	// 2. 活动状态（今天签没签）
	ctxReq, _ := http.NewRequestWithContext(ctx, "GET",
		serverBase+"/api/client-activities/"+code+"/context?configRevision="+fmt.Sprint(revision), nil)
	for k, v := range p.authHeaders(cred) {
		ctxReq.Header.Set(k, v)
	}
	ctxReq.Header.Set("Cache-Control", "no-store")
	resp2, err := p.hc(cred).Do(ctxReq)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	ctxData, err := envelope(resp2)
	if err != nil {
		return activityResult(err)
	}
	var state struct {
		State struct {
			ClaimedToday bool `json:"claimedToday"`
			ClaimedDays  int  `json:"claimedDays"`
			// 兼容旧字段形状
			TodayCheckedIn bool `json:"todayCheckedIn"`
			StreakDays     int  `json:"streakDays"`
		} `json:"state"`
		ClaimedToday   bool `json:"claimedToday"`
		ClaimedDays    int  `json:"claimedDays"`
		TodayCheckedIn bool `json:"todayCheckedIn"`
		StreakDays     int  `json:"streakDays"`
	}
	_ = json.Unmarshal(ctxData, &state)
	checked := state.State.ClaimedToday || state.ClaimedToday || state.State.TodayCheckedIn || state.TodayCheckedIn
	if checked {
		streak := state.State.ClaimedDays
		if streak == 0 {
			streak = state.ClaimedDays
		}
		if streak == 0 {
			streak = state.State.StreakDays
		}
		if streak == 0 {
			streak = state.StreakDays
		}
		return &pb.RunTaskResponse{Summary: fmt.Sprintf("今日已签到（连签 %d 天）", streak)}, nil
	}

	// 3. 执行签到（幂等键防重复发积分）
	actionURL := fmt.Sprintf("%s/api/client-activities/%s/actions/check_in", serverBase, code)
	actionBody := map[string]interface{}{
		"configRevision": revision,
		"idempotencyKey": newUUID(),
		"payload":        map[string]interface{}{},
	}
	resp3, err := postJSON(ctx, p.hc(cred), actionURL, p.authHeaders(cred), actionBody)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	actionData, err := envelope(resp3)
	if err != nil {
		return activityResult(err)
	}
	var result struct {
		Result struct {
			Replayed bool `json:"replayed"`
			Rewards  []struct {
				Amount int    `json:"amount"`
				Type   string `json:"type"`
			} `json:"rewards"`
		} `json:"result"`
	}
	_ = json.Unmarshal(actionData, &result)
	summary := "签到成功"
	if len(result.Result.Rewards) > 0 {
		summary = fmt.Sprintf("签到成功，积分 +%d", result.Result.Rewards[0].Amount)
	}
	if result.Result.Replayed {
		summary += "（幂等重放，未重复发分）"
	}
	return &pb.RunTaskResponse{Summary: summary}, nil
}

// ---------- 登录 URL 构造 ----------

// resolveLoginURL 配置下发接口取真实登录页，失败回退 Portal。
// 响应形状：{data: {value: "<url>"}}。
func resolveLoginURL(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", overmindLogin, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := upstreamClient("").Do(req)
	if err == nil {
		defer resp.Body.Close()
		var body struct {
			Data struct {
				Value string `json:"value"`
			} `json:"data"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil && strings.TrimSpace(body.Data.Value) != "" {
			return strings.TrimSpace(body.Data.Value), nil
		}
	}
	return portalLoginURL, nil
}

// composeLoginURL 拼登录 URL；redirectURI 为本地回调地址（auto 模式为真实回调）。
func composeLoginURL(loginBase, state, redirectURI string) string {
	// return_to 指回登录页自身（hash 路由参数拼在 fragment 上）
	returnTo := appendHashParams(loginBase, map[string]string{"source": "electron", "electronLogin": "success"})
	redirectURI = appendQueryParams(redirectURI, map[string]string{"return_to": returnTo})
	return appendHashParams(loginBase, map[string]string{
		"source": "electron", "redirect_uri": redirectURI, "state": state,
	})
}

// appendQueryParams 普通 URL 的 query 参数追加。
// 纯字符串拼接：u.String() 会对 RawQuery 再 escape，导致双重编码。
func appendQueryParams(base string, params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + q.Encode()
}

// appendHashParams hash 路由页面的参数要落在 fragment 的 query 上。
// 纯字符串拼接，避免 u.String() 对 fragment 的二次 escape。
func appendHashParams(base string, params map[string]string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if u.Fragment == "" {
		return appendQueryParams(base, params)
	}
	fragPath, fq, _ := strings.Cut(u.Fragment, "?")
	values, _ := url.ParseQuery(fq)
	for k, v := range params {
		values.Set(k, v)
	}
	prefix := base[:strings.Index(base, "#")]
	return prefix + "#" + fragPath + "?" + values.Encode()
}

func extractCode(callbackURL string) string {
	if i := strings.Index(callbackURL, "code="); i >= 0 {
		rest := callbackURL[i+len("code="):]
		for _, sep := range []string{"&", "\"", " ", "'"} {
			if j := strings.Index(rest, sep); j >= 0 {
				rest = rest[:j]
			}
		}
		return rest
	}
	return ""
}

func withKeyfrom(body map[string]interface{}, cred *credential, version string) map[string]interface{} {
	for k, v := range keyfrom(cred, version) {
		body[k] = v
	}
	return body
}

func failed(code int32, msg string) *pb.StreamEvent {
	return &pb.StreamEvent{Event: &pb.StreamEvent_TaskFailed{
		TaskFailed: &pb.TaskFailed{Error: &pb.Error{Code: code, Message: msg}},
	}}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// proxyPB credential 里的代理回填为 PB 配置（isAnthropicModel 拉目录时透传）。
func proxyPB(cred *credential) *pb.ProxyConfig {
	if cred == nil || cred.proxyURL == "" {
		return nil
	}
	if u, err := url.Parse(cred.proxyURL); err == nil {
		host := u.Hostname()
		port := 0
		fmt.Sscanf(u.Port(), "%d", &port)
		return &pb.ProxyConfig{Scheme: u.Scheme, Host: host, Port: int32(port)}
	}
	return nil
}

// orHint 凭据解析失败时附上排查方向。
func orHint(err error) string {
	if strings.Contains(err.Error(), "unexpected end of JSON input") {
		return "凭据为空：账号可能未分组或凭据未正确保存，请重新授权或检查分组"
	}
	return err.Error()
}
