// postman 插件 — Postman Agent Mode 反代。
// 上游：POST {origin}/_gw/chat（团队子域网关），把统一信封转成 Agent Mode 请求、
// 把 Postman 私有 SSE 翻回 StreamEvent。鉴权支持 Postman API Key（PMAK/PAT）或会话 Cookie。
// origin（团队子域）来自实例 base_url，同一插件可挂多个团队。
package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	pluginName = "postman"
	// browserUA Agent Mode 请求的浏览器伪装 UA；核心全局浏览器 UA 非空时覆盖。
	browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	defaultAppVersion = "12.23.0"
	// nativeToolsHash Postman workspace 工具集 hash（逆向自 postman-web 12.23.0）；
	// 空 hash 会报 "ExecutionPlan selected without the client sending tool hash"。
	nativeToolsHash = "clienttools-workspace_v12-browser-12.23.0-260810-0232-d54b0e12f268"
	// maxQueryLen Agent Mode 单条 query 上限（"accepts upto 10000 characters"），留余量。
	maxQueryLen = 9800
	settingsTTL = 30 * time.Second
)

// version 打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu       sync.Mutex
	settings map[int64]cachedSettings // instance_id → 站点设置缓存
	// loadSettings 实例视图设置读取（默认走宿主 RPC；测试注入假数据）。
	loadSettings func(instanceID int64) []byte

	// convMu 保护会话映射。插件是长驻进程，用内存维护「线程 → Postman 会话」。
	convMu sync.Mutex
	// convByThread 线程键 → conversationId。线程键取首条用户消息 hash（同一会话每轮稳定）。
	convByThread map[string]string
	// pendingCall 已下发工具调用的归属：callId → {threadKey, convId}。
	// 下游回传工具结果时只能匹配登记过的 callId，否则 Postman 静默拒绝。
	pendingCall map[string]pendingToolCall
}

type pendingToolCall struct {
	threadKey string
	convID    string
}

type cachedSettings struct {
	cfg siteConfig
	at  time.Time
}

func (p *plugin) SetHost(host *sdk.Host) {
	p.host = host
	p.loadSettings = func(id int64) []byte { return host.InstanceSettings(pluginName, id) }
}

// ---------- 站点配置（实例视图：base_url → origin） ----------

type siteConfig struct {
	Origin     string `json:"-"` // 团队子域网关（= 实例 base_url）
	Product    string `json:"product"`
	AppVersion string `json:"app_version"`
	BrowserUA  string `json:"-"` // 核心全局浏览器 UA，空回退内置
}

// site 读实例视图设置（30s 缓存）；base_url 缺失即报错。
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
			var loose map[string]json.RawMessage
			if json.Unmarshal(raw, &loose) == nil {
				cfg.Origin = rawString(loose["base_url"])
				cfg.Product = rawString(loose["product"])
				cfg.AppVersion = rawString(loose["app_version"])
				cfg.BrowserUA = strings.TrimSpace(rawString(loose[sdk.SettingBrowserUserAgent]))
			}
		}
	}
	cfg.Origin = strings.TrimRight(strings.TrimSpace(cfg.Origin), "/")
	cfg.Product = shared.OrDefault(strings.TrimSpace(cfg.Product), "postman")
	cfg.AppVersion = shared.OrDefault(strings.TrimSpace(cfg.AppVersion), defaultAppVersion)
	cfg.BrowserUA = shared.OrDefault(cfg.BrowserUA, browserUA)
	if cfg.Origin == "" {
		if !fetched {
			return nil, fmt.Errorf("无法读取实例 #%d 设置（宿主连接失败），请重启插件后重试", instanceID)
		}
		return nil, fmt.Errorf("实例 #%d 未配置团队子域，请到「实例」页填写 Postman 团队网关地址（如 https://xxx.postman.co）", instanceID)
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

// ---------- 凭据 blob ----------

// credential 账号凭据：apiKey（PMAK/PAT，纯 HTTP，不过期）与 cookie（浏览器会话，会过期）二选一。
// workspaceID 首次解析后缓存进 blob，避免每轮重复调用。
type credential struct {
	APIKey      string `json:"api_key,omitempty"`
	Cookie      string `json:"cookie,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	TeamID      string `json:"team_id,omitempty"`

	// 核心注入，不参与序列化。
	instanceID int64  `json:"-"`
	accountID  string `json:"-"`
	proxyURL   string `json:"-"`
}

// credFrom 解凭据；apiKey / cookie 至少一项。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	var c credential
	if err := json.Unmarshal(blob.GetBlob(), &c); err != nil {
		return nil, fmt.Errorf("invalid credential: %w", err)
	}
	if c.APIKey == "" && c.Cookie == "" {
		return nil, fmt.Errorf("credential missing api_key or cookie")
	}
	c.instanceID = blob.GetInstanceId()
	c.accountID = blob.GetAccountId()
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return &c, nil
}

// ---------- HTTP ----------

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

// upstreamClient 连接 15s / TLS 15s / 首字节 60s，流式对话整体不设超时。
// ---------- Manifest ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "Postman", "en": "Postman"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "account", sdk.CapabilityInstances},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema:  `{"type":"object","properties":{}}`,
		InstanceSchema: `{
			"type": "object",
			"properties": {
				"product": {
					"type": "string",
					"title": "产品线",
					"description": "Agent Mode product 字段，默认 workspace",
					"default": "workspace"
				},
				"app_version": {
					"type": "string",
					"title": "客户端版本",
					"description": "x-app-version 伪装版本，默认 12.23.0",
					"default": "12.23.0"
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "api_key", Label: map[string]string{"zh": "API 密钥", "en": "API Key"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{Name: "api_key", Label: map[string]string{"zh": "Postman API 密钥", "en": "Postman API Key"}, Type: "password", Required: true, Placeholder: "PMAK-... / PAT-..."},
					{Name: "workspace_id", Label: map[string]string{"zh": "工作区 ID", "en": "Workspace ID"}, Type: "text", Placeholder: "可选，留空自动获取首个工作区"},
				},
			},
			{
				Id: "cookie", Label: map[string]string{"zh": "会话 Cookie", "en": "Session Cookie"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{Name: "cookie", Label: map[string]string{"zh": "Cookie 头", "en": "Cookie header"}, Type: "textarea", Required: true, Placeholder: "登录 postman.co 后浏览器 devtools 的完整 Cookie 头"},
					{Name: "workspace_id", Label: map[string]string{"zh": "工作区 ID", "en": "Workspace ID"}, Type: "text", Placeholder: "可选，留空自动获取首个工作区"},
					{Name: "team_id", Label: map[string]string{"zh": "团队 ID", "en": "Team ID"}, Type: "text", Placeholder: "可选，多团队时指定"},
				},
			},
		},
	}}, nil
}

// ---------- 小工具 ----------

// threadKeyOf 取首条用户消息文本 hash 作会话线程键（同一会话每轮稳定）。
func threadKeyOf(msgs []*pb.EnvelopeMessage) string {
	for _, m := range msgs {
		if m.GetRole() == "user" {
			t := messageText(m)
			if strings.TrimSpace(t) != "" {
				sum := sha1.Sum([]byte(t))
				return hex.EncodeToString(sum[:])
			}
		}
	}
	return ""
}
