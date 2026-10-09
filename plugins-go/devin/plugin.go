// Devin 插件：会话认证、模型目录和对话转发。
package devin

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
	pluginName  = "devin"
	settingsTTL = 30 * time.Second
	// chisel 客户端伪装：metadata 身份字段与 Devin 客户端对齐。
	clientName    = "chisel"
	clientVersion = "3000.2.17"
)

// New 由平台入口注入版本，桌面与 Android 复用业务实现。
func New(version string) sdk.Plugin {
	if version == "" {
		version = "dev"
	}
	return &plugin{version: version}
}

type plugin struct {
	version string
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	settingsJSON []byte // 插件设置缓存（30s）
	settingsAt   time.Time

	modelsMu  sync.Mutex
	modelsKey string
	models    []*pb.ModelInfo
	modelsAt  time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// settingStr 读插件设置（核心管理界面在线编辑），30s 内存缓存。
func (p *plugin) settingStr(key string) string {
	p.mu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < settingsTTL
	raw := p.settingsJSON
	p.mu.Unlock()
	if !fresh {
		if p.host != nil {
			if r := p.host.Settings(pluginName); r != nil {
				raw = r
			}
		}
		if raw == nil {
			raw = []byte("{}")
		}
		p.mu.Lock()
		p.settingsJSON, p.settingsAt = raw, time.Now()
		p.mu.Unlock()
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal(raw, &cfg) == nil {
		if v, ok := cfg[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil {
				return s
			}
		}
	}
	return ""
}

// baseURL 读取配置的服务地址，未配置时使用默认值。
func (p *plugin) baseURL() string {
	return shared.OrDefault(strings.TrimRight(strings.TrimSpace(p.settingStr("base_url")), "/"), defaultBaseURL)
}

// ---------- 凭据 blob ----------

// credential devin 静态凭据：会话 token。
type credential struct {
	Token string `json:"token"`

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
	c.Token = strings.TrimSpace(c.Token)
	if c.Token == "" {
		return nil, fmt.Errorf("credential missing token")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// displayName 账号展示名（token 缩写）。
func (c *credential) displayName() string {
	return "devin-" + shortID(c.Token)
}

// hc 复用 SDK 按代理缓存的上游连接。
func (p *plugin) hc(c *credential) *http.Client {
	proxy := ""
	if c != nil {
		proxy = c.proxyURL
	}
	return sdk.UpstreamClient(proxy)
}

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: p.version, Author: "cph",
		Label:           map[string]string{"zh": "Devin", "en": "Devin"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "account"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"base_url": {
					"type": "string",
					"title": "上游地址",
					"description": "Devin Connect 服务基地址，留空用官方默认 https://server.codeium.com",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "api_key", Label: map[string]string{"zh": "会话 Token", "en": "Session Token"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{
						Name: "token", Label: map[string]string{"zh": "会话 Token", "en": "Session Token"},
						Type: "password", Required: true, Placeholder: "devin-session-token$...（Devin 客户端 state.vscdb 的 windsurfAuthStatus.apiKey）",
					},
				},
			},
		},
	}}, nil
}

// Login token 校验：拉一次模型列表验活，顺带回填资料。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	c := &credential{Token: strings.TrimSpace(req.Form["token"])}
	if c.Token == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写会话 Token"}}, nil
	}
	profile := &pb.AccountProfile{DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{}}
	if err := p.fetchProfile(ctx, c, profile); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{Blob: blob, Profile: profile}, nil
}

// ---------- 刷新 / 资料 ----------

// Refresh 静态 token 无刷新态：原样保留凭据，重新拉资料验活。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	profile := &pb.AccountProfile{DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{}}
	if err := p.fetchProfile(ctx, c, profile); err != nil {
		code := int32(503)
		if strings.Contains(err.Error(), "unauthenticated") || strings.Contains(err.Error(), "permission") {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: profile}, nil
}

// GetProfile 用户套餐与用量（SeatManagement/GetUserStatus）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{}}
	if err := p.fetchProfile(ctx, c, profile); err != nil {
		return nil, err
	}
	return profile, nil
}

// ---------- 工具 ----------

// shortID 取 id 前 8 位作展示缩写。
func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// defaultBaseURL 默认 Connect 服务地址。
const defaultBaseURL = "https://server.codeium.com"
