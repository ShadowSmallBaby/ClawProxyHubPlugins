// gorkcli 插件 — Grok CLI 反代。
// 上游：cli-chat-proxy.grok.com（xAI Responses 协议，仅 /responses 端点）。
// 认证：xAI OIDC，refresh_token 换 access_token；登录粘贴 refresh_token 或 Grok CLI 凭据 JSON。
package main

import (
	"context"
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
	pluginName = "gorkcli"

	defaultBase             = "https://cli-chat-proxy.grok.com/v1"
	tokenURL                = "https://auth.x.ai/oauth2/token"
	defaultClientID         = "grok-cli"
	defaultCLIVersion       = "0.2.93"
	defaultClientIdentifier = "grok-shell"

	settingsTTL = 30 * time.Second
	// refreshSkew access_token 距过期不足此值即先刷新（当次请求 best-effort）。
	refreshSkew = 90 * time.Second
)

// version 打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	settingsJSON []byte
	settingsAt   time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// ---------- 设置 ----------

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

// baseURL 上游地址（用户可配，留空用内置 CPA 地址）。
func (p *plugin) baseURL() string {
	return strings.TrimRight(shared.OrDefault(p.settingStr("base_url"), defaultBase), "/")
}

func (p *plugin) cliVersion() string {
	return shared.OrDefault(p.settingStr("client_version"), defaultCLIVersion)
}

func (p *plugin) clientIdentifier() string {
	return shared.OrDefault(p.settingStr("client_identifier"), defaultClientIdentifier)
}

// userAgent 请求头 User-Agent（用户可配，留空用真实 CLI 形态）。
func (p *plugin) userAgent() string {
	if v := p.settingStr("user_agent"); v != "" {
		return v
	}
	return "xai-grok-workspace/" + p.cliVersion()
}

// grokHeaders 上游对话 / 模型请求的公共头（对齐 grok-cli workspace shell）。
func (p *plugin) grokHeaders(token string) map[string]string {
	return map[string]string{
		"Content-Type":             "application/json",
		"Authorization":            "Bearer " + token,
		"X-XAI-Token-Auth":         "xai-grok-cli",
		"x-grok-client-version":    p.cliVersion(),
		"x-grok-client-identifier": p.clientIdentifier(),
		"User-Agent":               p.userAgent(),
		"Accept":                   "text/event-stream",
	}
}

// ---------- 凭据 blob ----------

// credential OIDC 登录态：refresh_token 长期有效，access_token 短期 Bearer。
type credential struct {
	RefreshToken string  `json:"refresh_token"`
	AccessToken  string  `json:"access_token"`
	ExpiresAt    float64 `json:"expires_at"` // Unix 秒；0 = 未知
	Email        string  `json:"email"`
	UserID       string  `json:"user_id"`
	ClientID     string  `json:"oidc_client_id"`

	proxyURL string `json:"-"` // 出站代理（核心注入，不序列化）
}

func (c *credential) clientID() string { return shared.OrDefault(c.ClientID, defaultClientID) }

// expiringSoon access_token 缺失或临近过期。
func (c *credential) expiringSoon() bool {
	if c.AccessToken == "" {
		return true
	}
	if c.ExpiresAt <= 0 {
		return false
	}
	return time.Until(time.Unix(int64(c.ExpiresAt), 0)) < refreshSkew
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
		return nil, fmt.Errorf("credential missing token")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// proxyURL 代理配置 → URL 字符串。
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
	c := sdk.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
}

// upstreamClient 连接 15s / TLS 15s / 首字节 60s，流式对话整体不设超时（长回复合法）。
// ---------- Manifest ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "Grok CLI", "en": "Grok CLI"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"base_url": {
					"type": "string",
					"title": "上游地址",
					"description": "Grok CLI 上游 Base URL，留空使用内置 cli-chat-proxy.grok.com",
					"default": ""
				},
				"client_version": {
					"type": "string",
					"title": "客户端版本",
					"description": "x-grok-client-version 与 User-Agent 版本号，留空使用内置值",
					"default": ""
				},
				"client_identifier": {
					"type": "string",
					"title": "客户端标识",
					"description": "x-grok-client-identifier，留空使用 grok-shell",
					"default": ""
				},
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "请求头 User-Agent，留空使用内置 CLI 形态",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "refresh_token", Label: map[string]string{"zh": "Refresh Token", "en": "Refresh Token"},
				Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "refresh_token", Label: map[string]string{"zh": "Refresh Token", "en": "Refresh Token"},
					Type: "password", Required: true, Placeholder: "xai OIDC refresh_token",
				}},
			},
			{
				Id: "auth_file", Label: map[string]string{"zh": "凭据文件", "en": "Credential File"},
				Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "auth_json", Label: map[string]string{"zh": "凭据 JSON", "en": "Credential JSON"},
					Type: "textarea", Required: true, Placeholder: `{"refresh_token":"...","email":"..."}`,
				}},
			},
		},
	}}, nil
}
