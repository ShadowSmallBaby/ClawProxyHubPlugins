// cline 插件 — Cline（api.cline.bot）反代。
// 上游：api.cline.bot/api/v1，OpenAI 兼容 /chat/completions + 业务信封 {data}。
// 登录：WorkOS 设备码授权 → /auth/register 换 cline refreshToken。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
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
func (p *plugin) userAgentStr() string { return p.settingStr("user_agent") }

// ---------- 凭据 blob ----------

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
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
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
	c := sdk.UpstreamClient(key)
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

// ---------- Manifest ----------

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

// ---------- 通用工具 ----------

// postJSON POST JSON（client 为 nil 时用无代理默认 client）。
func postJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, bytes.NewReader(body))
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
