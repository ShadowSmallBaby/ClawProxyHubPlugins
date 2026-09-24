// lobsterai 插件 — 网易有道 LobsterAI 客户端反代。
// 上游：lobsterai-server.youdao.com，OpenAI 兼容（/api/proxy 前缀）+ 业务信封。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
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
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hc(cred *credential) *http.Client {
	if cred == nil || cred.proxyURL == "" {
		return sdk.UpstreamClient("")
	}
	if c, ok := proxyClients.Load(cred.proxyURL); ok {
		return c.(*http.Client)
	}
	u, err := url.Parse(cred.proxyURL)
	if err != nil {
		return sdk.UpstreamClient("")
	}
	c := sdk.UpstreamClient(u.String())
	proxyClients.Store(cred.proxyURL, c)
	return c
}

func parseCred(blob []byte) (*credential, error) {
	var c credential
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("invalid credential: %w", err)
	}
	// .auth 文件完整结构：token/user 在嵌套字段里
	if c.AccessToken == "" && c.AuthTokens != nil {
		c.AccessToken = c.AuthTokens.AccessToken
		c.RefreshToken = shared.OrDefault(c.RefreshToken, c.AuthTokens.RefreshToken)
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
		client = sdk.UpstreamClient("")
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

func withKeyfrom(body map[string]interface{}, cred *credential, version string) map[string]interface{} {
	for k, v := range keyfrom(cred, version) {
		body[k] = v
	}
	return body
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
