// mirasim 插件 — Mirasim（relay.mirasim.ai）私有中继反代。
// 认证：邮件验证码 / 凭据导入 → access+refresh token + 本地 Ed25519 设备密钥。
// 中继：mrs-sig-v2 Ed25519 签名 + mrs-seal-v1（X25519+HKDF+ChaCha20）封密元数据 +
// /v1/device/session 铸造票据；Claude 走 /v1/messages，GPT 走 /v1/responses。
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
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	pluginName = "mirasim"

	defaultRelayURL      = "https://relay.mirasim.ai"
	defaultAdminURL      = "https://auth.mirasim.ai"
	defaultClientVersion = "0.0.336"
	// defaultSealPubKey relay 元数据封密的收件人 X25519 公钥（可经设置覆盖）。
	defaultSealPubKey = "HlyNMMeGXryasYLJuYQ/9ksCD4AYVVy1zXKAtJdpJn4="

	signatureVersion = "mrs-sig-v2"
	sealVersion      = "mrs-seal-v1"

	sessionPath = "/v1/device/session"
	modelsPath  = "/v1/models"
	limitsPath  = "/v1/limits"
	rosterPath  = "/v1/model-roster"

	settingsTTL = 30 * time.Second
	// accessStaleLead access token 进入过期前 30s 即刷新。
	accessStaleLead = 30 * time.Second
	// ticketRefreshLead 票据提前 2 分钟续期。
	ticketRefreshLead = 2 * time.Minute
	// ticketDefaultTTL 票据默认有效期（响应未给时）。
	ticketDefaultTTL = 10 * time.Minute

	maxRespBody = 8 << 20
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

// settings 读实例合并视图设置，30s 内存缓存。
func (p *plugin) settings(instanceID int64) map[string]string {
	p.settingsMu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < settingsTTL
	raw := p.settingsJSON
	p.settingsMu.Unlock()
	if !fresh {
		if r := p.host.InstanceSettings(pluginName, instanceID); r != nil {
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
		return cfg
	}
	return map[string]string{}
}

// relayURL / adminURL / clientVersion 读设置，空回默认。
func (p *plugin) relayURL(s map[string]string) string {
	return shared.OrDefault(cleanURL(s["relay_url"]), defaultRelayURL)
}
func (p *plugin) adminURL(s map[string]string) string {
	return shared.OrDefault(cleanURL(s["admin_url"]), defaultAdminURL)
}
func (p *plugin) clientVersion(s map[string]string) string {
	return shared.OrDefault(strings.TrimSpace(s["client_version"]), defaultClientVersion)
}

// collectOff 采集是否显式关闭（collect=false → 发 x-mirasim-collect:off）。
func collectOff(s map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(s["collect"])) {
	case "false", "0", "off":
		return true
	}
	return false
}

// ---------- Manifest / 登录 ----------

const settingsSchema = `{
	"type": "object",
	"properties": {
		"relay_url": {"type": "string", "title": "中继地址", "description": "留空用 https://relay.mirasim.ai", "default": ""},
		"admin_url": {"type": "string", "title": "认证服务地址", "description": "留空用 https://auth.mirasim.ai", "default": ""},
		"client_version": {"type": "string", "title": "客户端版本", "description": "x-mirasim-client 值，留空用内置默认", "default": ""},
		"collect": {"type": "string", "title": "采集信号", "description": "填 false 发送 x-mirasim-collect:off", "default": ""},
		"seal_pubkey": {"type": "string", "title": "封密公钥", "description": "relay 元数据封密收件人公钥（base64），留空用内置默认", "default": ""}
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
		Label:           map[string]string{"zh": "Mirasim", "en": "Mirasim"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema:  settingsSchema,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "email", Label: map[string]string{"zh": "邮件验证码", "en": "Email Code"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "email", Label: map[string]string{"zh": "邮箱", "en": "Email"},
					Type: "text", Required: true, Placeholder: "you@example.com",
				}},
			},
			{
				Id: "credential_file", Label: map[string]string{"zh": "凭据导入", "en": "Credential File"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": "凭据 JSON", "en": "Credential JSON"},
					Type: "textarea", Required: true, Placeholder: `{"access_token":"...","refresh_token":"..."}`,
				}},
			},
		},
	}}, nil
}

// ---------- 工具 ----------

func cleanURL(v string) string {
	return strings.TrimRight(strings.TrimSpace(v), "/")
}
