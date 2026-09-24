// qoder 插件 — Qoder 私有协议反代（新版协议）。
// 认证：PAT → jobToken（gateway.qoder.com.cn，cosy 签名 + 自定义 base64），到期前 refreshToken 轮换。
// 对话：securityOauthToken 直接做 Bearer，POST api2-v2.qoder.sh 的 OpenAI 兼容端点 → 标准 OpenAI SSE。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	pluginName  = "qoder"
	gateway     = "https://gateway.qoder.com.cn" // 认证类（PAT→jobToken）
	chatBase    = "https://api2-v2.qoder.sh"     // 新版对话（纯 Bearer + OpenAI SSE）
	cosyVersion = "0.1.43"
	appCode     = "cosy"
	// signatureSecret cosy 请求签名密钥（base64("war, war never changes")）
	signatureSecret = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw=="
	settingsTTL     = 30 * time.Second
	// refreshMargin jobToken 到期前提前续期（2 小时）
	refreshMargin = 2 * time.Hour
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	settingsMu   sync.Mutex
	settingsJSON []byte // 插件设置缓存（30s）
	settingsAt   time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// settingStr 读插件设置，30s 内存缓存。
func (p *plugin) settingStr(key string) string {
	p.settingsMu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < settingsTTL
	raw := p.settingsJSON
	p.settingsMu.Unlock()
	if !fresh {
		if r := p.host.Settings(pluginName); r != nil {
			raw = r
		} else {
			raw = []byte("{}")
		}
		p.settingsMu.Lock()
		p.settingsJSON, p.settingsAt = raw, time.Now()
		p.settingsMu.Unlock()
	}
	var cfg map[string]string
	if json.Unmarshal(raw, &cfg) == nil {
		return cfg[key]
	}
	return ""
}

// ---------- HTTP client ----------

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hc(cred *credential) *http.Client {
	key := ""
	if cred != nil {
		key = cred.proxyURL
	}
	if key == "" {
		return sdk.UpstreamClient("")
	}
	if u, err := url.Parse(key); err == nil {
		c := sdk.UpstreamClient(u.String())
		return c
	}
	return sdk.UpstreamClient("")
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
		Label:           map[string]string{"zh": "Qoder CN", "en": "Qoder CN"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema:  `{"type": "object", "properties": {}}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "pat", Label: map[string]string{"zh": "PAT 令牌", "en": "PAT"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "pat", Label: map[string]string{"zh": "个人访问令牌", "en": "Personal Access Token"},
					Type: "password", Required: true, Placeholder: "pt-xxxx_019exxxx-...",
				}},
			},
		},
	}}, nil
}
