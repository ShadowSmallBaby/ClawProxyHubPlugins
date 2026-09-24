// mimo 插件 — Xiaomi MiMo（mimo-server-cn.xiaomimimo.com）反代。
// 认证：passToken（凭据导入）→ 5 步小米 SSO 换 serviceToken（30 分钟缓存，401 自动刷新重试）。
// 对话：OpenAI 兼容 /api/route/chat/completions（上游已带 tool_calls / reasoning_content），直通透传。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	pluginName = "mimo"

	defaultAPIBase = "https://mimo-server-cn.xiaomimimo.com"
	// defaultAPIUA 对话 / SSO 请求的桌面客户端形态 UA。
	defaultAPIUA = "miNative PC/Normal Windows_NT/10.0.19045 SDKV/1.0.0 " +
		"DEVT/PC DEVS/Windows APP/mimo/2.0.0"
	// ssoUA SSO 信道的 UA。
	ssoUA = "MiClaw/1.0"

	// cookieTTL serviceToken 缓存时长（对齐 Desktop 行为）。
	cookieTTL = 30 * time.Minute
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu            sync.Mutex
	settingsCache map[int64]cachedSettings
}

type cachedSettings struct {
	cfg apiConfig
	at  time.Time
}

// apiConfig 实例视图配置：地址 + 客户端 UA（可经插件设置覆盖）。
type apiConfig struct {
	APIBase string
	APIUA   string
}

// settings 读实例视图设置（30s 缓存）。
func (p *plugin) settings(instanceID int64) apiConfig {
	p.mu.Lock()
	if c, ok := p.settingsCache[instanceID]; ok && time.Since(c.at) < 30*time.Second {
		p.mu.Unlock()
		return c.cfg
	}
	p.mu.Unlock()
	cfg := apiConfig{APIBase: defaultAPIBase, APIUA: defaultAPIUA}
	if p.host != nil {
		if raw := p.host.InstanceSettings(pluginName, instanceID); raw != nil {
			var m map[string]string
			if json.Unmarshal(raw, &m) == nil {
				if v := strings.TrimRight(strings.TrimSpace(m["api_base"]), "/"); v != "" {
					cfg.APIBase = v
				}
				if v := strings.TrimSpace(m["api_ua"]); v != "" {
					cfg.APIUA = v
				}
			}
		}
	}
	p.mu.Lock()
	if p.settingsCache == nil {
		p.settingsCache = map[int64]cachedSettings{}
	}
	p.settingsCache[instanceID] = cachedSettings{cfg: cfg, at: time.Now()}
	p.mu.Unlock()
	return cfg
}

// ---------- 凭据 blob ----------

// credential 账号凭据：passToken 长期有效，经 SSO 换短期 serviceToken。
type credential struct {
	PassToken string `json:"pass_token"`
	UserID    string `json:"user_id,omitempty"`
	CUserID   string `json:"c_user_id,omitempty"`

	// 运行态（不序列化）：SSO 缓存的 service cookie（按 passToken 哈希为键，进程级共享）
	instanceID int64  `json:"-"`
	accountID  string `json:"-"`
	proxyURL   string `json:"-"`
}

// credFrom 解凭据并附加核心注入字段。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	var c credential
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), &c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if strings.TrimSpace(c.PassToken) == "" {
		return nil, fmt.Errorf("credential missing pass_token")
	}
	c.instanceID = blob.GetInstanceId()
	c.accountID = blob.GetAccountId()
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return &c, nil
}

// ---------- HTTP ----------

// upstreamClient 连接 15s / TLS 15s / 首字节 60s，流式对话整体不设超时。
// ---------- Manifest ----------

const settingsSchema = `{
	"type": "object",
	"properties": {
		"api_base": {"type": "string", "title": "上游地址", "description": "留空用 https://mimo-server-cn.xiaomimimo.com", "default": ""},
		"api_ua": {"type": "string", "title": "客户端 UA", "description": "对话 / SSO 请求 UA，留空用内置默认", "default": ""}
	}
}`

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "MiMo", "en": "MiMo"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions"},
		SettingsSchema:  settingsSchema,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "credential_file", Label: map[string]string{"zh": "凭据导入", "en": "Credential File"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": "passToken JSON", "en": "passToken JSON"},
					Type: "textarea", Required: true,
					Placeholder: `{"pass_token": "...", "user_id": "..."} 或 Desktop cookie 库导出的 {"passToken": "...", "userId": "..."}`,
				}},
			},
		},
	}}, nil
}

// ---------- 工具 ----------
