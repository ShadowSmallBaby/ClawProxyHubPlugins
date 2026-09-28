// main.go — 入口 + Handshake + 设置缓存。
//
// trae 插件：字节跳动 TRAE IDE（SOLO CN）协议反代。Cloud-IDE-JWT 认证，
// POST /api/agent/v3/llm_utils_chat 自定义 SSE；登录走授权页回调（两步式），
// 续期用 ExchangeToken（refreshToken 轮换，旧值即刻失效）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const pluginName = "trae"

const settingsTTL = 30 * time.Second

// version 打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu        sync.Mutex
	oauth     map[string]*oauthSession // state → 进行中的授权会话（单条，覆盖旧的）
	oauthCB   *callbackServer          // 进行中的本地回调 server（懒起，完成/超时即关）
	settingsJSON []byte                // 插件设置缓存（30s）
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

// ---------- Manifest ----------

const settingsSchema = `{
	"type": "object",
	"properties": {}
}`

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "TRAE (字节)", "en": "TRAE (ByteDance)"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "account", "tasks"},
		Endpoints:       []string{"chat_completions"},
		SettingsSchema:  settingsSchema,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "oauth", Label: map[string]string{"zh": "浏览器登录", "en": "Browser Login"}, Capabilities: []string{"refreshable"},
				Callback: "auto_wait", // 本机访问自动回调，服务器部署转手动粘贴
			},
			{
				Id: "token_import", Label: map[string]string{"zh": "Token 导入", "en": "Token Import"}, Capabilities: []string{"profile"},
				Fields: []*pb.AuthField{
					{
						Name:     "access_token",
						Label:    map[string]string{"zh": "access_token", "en": "access_token"},
						Type:     "textarea",
						Required: true,
						Placeholder: "Cloud-IDE-JWT token（抓包 X-Cloudide-Token 或登录页获取）",
					},
					{
						Name:        "refresh_token",
						Label:       map[string]string{"zh": "refresh_token（可选）", "en": "refresh_token (optional)"},
						Type:        "textarea",
						Required:    false,
					},
					{
						Name:        "uid",
						Label:       map[string]string{"zh": "uid（可选）", "en": "uid (optional)"},
						Type:        "text",
						Required:    false,
						Placeholder: "用户 ID（签到设备身份派生用，缺失时从 GetUserInfo 补）",
					},
				},
			},
		},
	}}, nil
}
