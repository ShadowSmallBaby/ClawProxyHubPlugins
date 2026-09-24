// newapi 插件 — New API（QuantumNous/new-api）站点反代。
// 上游：OpenAI 兼容 /v1 网关 + 管理面 /api/user/*（余额 / 签到，需系统访问令牌）。
// 站点地址来自实例（instance.base_url），同一插件可挂多个 New API 部署。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	pluginName = "newapi"
	// defaultQuotaPerUnit New API 默认额度换算：500000 quota = 1 美元
	defaultQuotaPerUnit = 500000.0
	// defaultBrowserUA 管理面 / 会话请求的内置浏览器 UA（部分站点对 Go-http-client 直接 401「unauthorized client」）；
	// 核心全局浏览器 UA（sdk.SettingBrowserUserAgent）非空时覆盖
	defaultBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// modelsUA /v1/models（模型目录 / 密钥校验，无客户端上下文）固定用 CLI 形态 UA：
	// 部分站点 /v1 只放行 CLI 客户端（claude-cli / codex），浏览器 UA 会被拒
	modelsUA    = "claude-cli/2.1.198 (external, cli)"
	settingsTTL = 30 * time.Second
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu       sync.Mutex
	settings map[int64]cachedSettings // instance_id → 实例视图设置缓存
	// loadSettings 实例视图设置读取（默认走宿主 RPC；测试注入假数据）
	loadSettings func(instanceID int64) []byte
}

type cachedSettings struct {
	cfg siteConfig
	at  time.Time
}

func (p *plugin) SetHost(host *sdk.Host) {
	p.host = host
	p.loadSettings = func(id int64) []byte { return host.InstanceSettings(pluginName, id) }
}

// ---------- 站点配置（实例视图：插件设置 ← 实例设置 ← base_url） ----------

// 签到类型（实例级 checkin_mode）：
//
//	login   登录即签到 — 任务时用账号密码重新登录，登录动作触发签到（agentrouter 类站点）
//	refresh 刷新即签到 — 请求 /api/user/self 即触发签到（anyrouter 类站点）
//	api     通用签到   — POST /api/user/checkin（New API 原生签到接口）
//	none    无签到     — 不声明 checkin 能力，不建任务
//	site    站点签到   — 需人工前往签到站，任务只做提醒（摘要 + 站内通知）
const (
	checkinLogin   = "login"
	checkinRefresh = "refresh"
	checkinAPI     = "api"
	checkinNone    = "none"
	checkinSite    = "site"
)

type siteConfig struct {
	BaseURL      string  `json:"base_url"`
	InstanceName string  `json:"instance_name"`
	QuotaPerUnit float64 `json:"quota_per_unit,string"`
	BrowserUA    string  `json:"-"` // 管理面 / 会话 UA：核心全局浏览器 UA，空回退内置
	CheckinMode  string  `json:"checkin_mode"`
	CheckinURL   string  `json:"checkin_url"`
}

// site 读实例视图设置（30s 缓存）；base_url 缺失即报错，避免打到空地址。
func (p *plugin) site(instanceID int64) (*siteConfig, error) {
	p.mu.Lock()
	if c, ok := p.settings[instanceID]; ok && time.Since(c.at) < settingsTTL {
		p.mu.Unlock()
		return &c.cfg, nil
	}
	p.mu.Unlock()

	cfg := siteConfig{}
	fetched := false
	if p.loadSettings != nil {
		if raw := p.loadSettings(instanceID); raw != nil {
			fetched = true
			// quota_per_unit 可能是数字或字符串，按原始值宽松解析
			var loose map[string]json.RawMessage
			if json.Unmarshal(raw, &loose) == nil {
				cfg.BaseURL = rawString(loose["base_url"])
				cfg.InstanceName = rawString(loose["instance_name"])
				cfg.QuotaPerUnit = rawNumber(loose["quota_per_unit"])
				cfg.CheckinMode = rawString(loose["checkin_mode"])
				cfg.CheckinURL = strings.TrimSpace(rawString(loose["checkin_url"]))
				cfg.BrowserUA = strings.TrimSpace(rawString(loose[sdk.SettingBrowserUserAgent]))
			}
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.QuotaPerUnit <= 0 {
		cfg.QuotaPerUnit = defaultQuotaPerUnit
	}
	if cfg.CheckinMode == "" {
		cfg.CheckinMode = checkinAPI
	}
	cfg.BrowserUA = shared.OrDefault(cfg.BrowserUA, defaultBrowserUA)
	if cfg.BaseURL == "" {
		if !fetched {
			return nil, fmt.Errorf("无法读取实例 #%d 设置（宿主连接失败），请重启插件后重试", instanceID)
		}
		return nil, fmt.Errorf("实例 #%d 未配置站点地址，请到「实例」页为该实例填写 New API 部署地址（账号列表可查看账号所属实例）", instanceID)
	}
	p.mu.Lock()
	if p.settings == nil {
		p.settings = map[int64]cachedSettings{}
	}
	p.settings[instanceID] = cachedSettings{cfg: cfg, at: time.Now()}
	p.mu.Unlock()
	return &cfg, nil
}

func rawString(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return strings.Trim(string(r), `"`)
}

func rawNumber(r json.RawMessage) float64 {
	var f float64
	if json.Unmarshal(r, &f) == nil {
		return f
	}
	f, _ = strconv.ParseFloat(strings.Trim(string(r), `"`), 64)
	return f
}

// ---------- 凭据 blob ----------

// credential 账号凭据：api_key 走 /v1 网关；管理面（余额/签到）二选一：
// access_token（系统访问令牌，长期）或站点会话（JWT / cookie，短期；账号密码登录会保存密码以便过期后自动重登）。
type credential struct {
	APIKey      string `json:"api_key"`
	AccessToken string `json:"access_token,omitempty"`
	UserID      int    `json:"user_id,omitempty"`

	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	SessionJWT    string `json:"session_jwt,omitempty"`
	SessionCookie string `json:"session_cookie,omitempty"`
	SessionExp    int64  `json:"session_exp,omitempty"` // JWT 到期（unix 秒），0 为未知
	TokenName     string `json:"token_name,omitempty"`  // 自举时指定使用的站点密钥名（空 = 首把可用）

	// 核心注入，不参与序列化
	instanceID int64  `json:"-"`
	accountID  string `json:"-"`
	proxyURL   string `json:"-"`
	keyErr     error  `json:"-"` // 自举取密钥失败原因（仅登录 / 刷新过程用）
}

// credFrom 解凭据；api_key 可为空（会话模式尚未取得明文），对话 / 目录调用时按 401 报出。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	var c credential
	if err := json.Unmarshal(blob.GetBlob(), &c); err != nil {
		return nil, fmt.Errorf("invalid credential: %w", err)
	}
	if c.APIKey == "" && !c.hasManagement() {
		return nil, fmt.Errorf("credential missing api_key")
	}
	c.instanceID = blob.GetInstanceId()
	c.accountID = blob.GetAccountId()
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return &c, nil
}

// hasManagement 是否具备管理面能力（余额 / 签到）：系统令牌、站点会话或可重登的密码任一即可。
func (c *credential) hasManagement() bool {
	return c.AccessToken != "" || c.Password != "" || c.SessionJWT != "" || c.SessionCookie != ""
}

// session 当前站点会话视图（用于无系统令牌时的管理面调用）。
func (c *credential) session() *sessionAuth {
	return &sessionAuth{bearer: c.SessionJWT, cookie: c.SessionCookie, userID: c.UserID}
}

// applySession 登录结果写回凭据。
func (c *credential) applySession(a *sessionAuth) {
	c.SessionJWT, c.SessionCookie, c.SessionExp = a.bearer, a.cookie, a.expiresAt
	if a.userID > 0 {
		c.UserID = a.userID
	}
}

// sessionStale 会话缺失或 JWT 将在 60s 内到期（需要重登）。
func (c *credential) sessionStale() bool {
	if c.SessionJWT == "" && c.SessionCookie == "" {
		return true
	}
	return c.SessionExp > 0 && time.Now().Unix() > c.SessionExp-60
}

// ---------- Manifest ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "New API", "en": "New API"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "tasks", sdk.CapabilityInstances},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema:  `{"type": "object", "properties": {}}`,
		InstanceSchema: `{
			"type": "object",
			"properties": {
				"quota_per_unit": {
					"type": "number",
					"title": "额度换算",
					"description": "默认 500000（余额按此折算为美元）",
					"default": 500000
				},
				"checkin_mode": {
					"type": "string",
					"title": "签到类型",
					"description": "站点的签到触发方式；无签到则不创建签到任务",
					"default": "api",
					"oneOf": [
						{"const": "api", "title": "通用签到（POST /api/user/checkin）"},
						{"const": "login", "title": "登录即签到（任务时用账号密码重新登录）"},
						{"const": "refresh", "title": "刷新即签到（请求用户信息即触发）"},
						{"const": "site", "title": "站点签到（人工前往签到站，任务只提醒）"},
						{"const": "none", "title": "无签到"}
					]
				},
				"checkin_url": {
					"type": "string",
					"title": "签到站地址",
					"description": "站点签到时提醒用户前往的地址",
					"default": "",
					"x-depends": {"checkin_mode": "site"}
				},
				"linuxdo_client_id": {
					"type": "string",
					"title": "LinuxDo",
					"description": "LinuxDo OAuth 应用 ClientID",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "api_key", Label: map[string]string{"zh": "API 密钥", "en": "API Key"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{Name: "api_key", Label: map[string]string{"zh": "API 密钥", "en": "API Key"}, Type: "password", Required: true, Placeholder: "sk-..."},
					{Name: "access_token", Label: map[string]string{"zh": "系统访问令牌", "en": "Access Token"}, Type: "password",
						Placeholder: "个人设置 → 安全设置 → 系统访问令牌（可选，用于余额 / 签到）"},
					{Name: "user_id", Label: map[string]string{"zh": "用户 ID", "en": "User ID"}, Type: "text",
						Placeholder: "可选，留空自动获取"},
				},
			},
			{
				Id: "password", Label: map[string]string{"zh": "账号密码", "en": "Password"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{Name: "username", Label: map[string]string{"zh": "用户名", "en": "Username"}, Type: "text", Required: true},
					{Name: "password", Label: map[string]string{"zh": "密码", "en": "Password"}, Type: "password", Required: true},
					{Name: "token_name", Label: map[string]string{"zh": "密钥名称", "en": "Token name"}, Type: "text",
						Placeholder: "可选：指定使用站点上的哪把 API 密钥；留空取首把可用，都不可用则新建"},
				},
			},
			{
				Id: "cred_file", Label: map[string]string{"zh": "凭据文件", "en": "Credential File"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{
						Name: "content", Label: map[string]string{"zh": "Cookie / 凭据 JSON", "en": "Cookie / credential JSON"},
						Type: "textarea", Required: true,
						Placeholder: `{"session": "<session cookie>", "user_id": 1} 或 {"access_token": "<登录 JWT>", "user_id": 1} 或浏览器 Cookie 头原文（含 session=...）`,
					},
					{Name: "token_name", Label: map[string]string{"zh": "密钥名称", "en": "Token name"}, Type: "text",
						Placeholder: "可选：指定使用站点上的哪把 API 密钥；留空取首把可用，都不可用则新建"},
				},
			},
			{
				Id: "oauth", Label: map[string]string{"zh": "OAuth（开发中）", "en": "OAuth (in development)"},
			},
		},
	}}, nil
}
