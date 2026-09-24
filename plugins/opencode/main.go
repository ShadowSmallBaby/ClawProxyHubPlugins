// opencode 插件 — OpenCode Zen 反代。
// 上游：opencode.ai/zen（Zen）与 /zen/go（Zen Go）双池，OpenAI/Anthropic 原生端点。
// 登录：Zen / Go API Key 直填（凭据 blob 区分 tier）；免费模型可走匿名 public key。
package main

import (
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
	pluginName  = "opencode"
	zenBase     = "https://opencode.ai/zen"
	goBase      = "https://opencode.ai/zen/go"
	anonZenKey  = "public"
	settingsTTL = 30 * time.Second
	// defaultUserAgent 真实 CLI 的 UA 形态（ai-sdk 运行时拼接），免费池按此校验来源。
	defaultUserAgent = "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
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
	modelsMu     sync.Mutex
	zenModels    map[string]bool
	goModels     map[string]bool
	modelsAt     time.Time
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

// preferGo 模型同时存在于 Zen 与 Go 时的认证顺序偏好（默认 go）。
func (p *plugin) preferGo() bool { return p.settingStr("prefer") != "zen" }

// userAgentStr 请求头 User-Agent（用户可配置；空 = 真实 CLI 形态，免费池按 UA 校验来源）。
func (p *plugin) userAgentStr() string {
	if v := p.settingStr("user_agent"); v != "" {
		return v
	}
	return defaultUserAgent
}

// ---------- 凭据 blob ----------

// credential Zen / Go tier 的 API Key。
type credential struct {
	Tier string `json:"tier"` // zen / go
	Key  string `json:"key"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{Tier: "zen"}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.Key == "" {
		return nil, fmt.Errorf("credential missing key")
	}
	if c.Tier != "zen" && c.Tier != "go" {
		c.Tier = "zen"
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

// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "OpenCode", "en": "OpenCode"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"prefer": {
					"type": "string",
					"title": "首选通道",
					"description": "模型同时存在于 Zen 与 Go 时的认证顺序",
					"default": "go",
					"oneOf": [
						{"const": "go", "title": "Go 优先"},
						{"const": "zen", "title": "Zen 优先"}
					]
				},
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "请求头 User-Agent 伪装值，留空使用内置 CLI 形态",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "api_key", Label: map[string]string{"zh": "Zen API Key", "en": "Zen API Key"},
				Fields: []*pb.AuthField{{
					Name: "key", Label: map[string]string{"zh": "API Key", "en": "API Key"},
					Type: "password", Required: true, Placeholder: "sk-...",
				}},
			},
			{
				Id: "go_key", Label: map[string]string{"zh": "Zen Go API Key", "en": "Zen Go API Key"},
				Fields: []*pb.AuthField{{
					Name: "key", Label: map[string]string{"zh": "API Key", "en": "API Key"},
					Type: "password", Required: true, Placeholder: "sk-...",
				}},
			},
		},
	}}, nil
}
