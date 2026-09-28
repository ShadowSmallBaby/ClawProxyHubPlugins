// main.go — 入口 + Handshake/Login + 设置缓存。
//
// raccoon 插件：Raccoon Work（商汤小浣熊）协议反代。Bearer 认证 + 业务信封，
// POST /api/web/llm/v2/chat/completions 标准 OpenAI 兼容 SSE。
// 登录采用扫码轮询（本地生成 qrcode_code，轮询 login_with_qrcode_code）；
// 官方桌面端 office-raccoon:// 自定义协议回调不可复用（宿主侧收不到）。
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

const pluginName = "raccoon"

const settingsTTL = 30 * time.Second

// version 打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	qrStates     map[string]*qrSession // code → 进行中的扫码会话（单条，覆盖旧的）
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

// clientPlatform 桌面端平台标识（desktop/v1/login/points/grant 必需）。
func (p *plugin) clientPlatform() string {
	if v := p.settingStr("client_platform"); v != "" {
		return v
	}
	return "desktop-windows"
}

// clientVersion 客户端版本伪装值（带 v 前缀）。
func (p *plugin) clientVersion() string {
	if v := p.settingStr("client_version"); v != "" {
		return v
	}
	return "v1.0.35"
}

// ---------- Manifest / 登录 ----------

const settingsSchema = `{
	"type": "object",
	"properties": {
		"client_platform": {
			"type": "string",
			"title": "X-Client-Platform",
			"description": "桌面端平台标识（desktop-windows / desktop-macos / desktop-linux），留空使用内置默认",
			"default": ""
		},
		"client_version": {
			"type": "string",
			"title": "客户端版本号",
			"description": "请求头 X-Client-Version 的伪装值，留空使用内置默认",
			"default": ""
		}
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
		Label:           map[string]string{"zh": "Raccoon", "en": "Raccoon"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "account", "tasks"},
		Endpoints:       []string{"chat_completions", "messages"},
		SettingsSchema:  settingsSchema,
		AuthMethods: []*pb.AuthMethod{
			{
				Id:           "qrcode",
				Label:        map[string]string{"zh": "扫码登录", "en": "QR Login"},
				Capabilities: []string{"refreshable"},
				Callback:     "auto_wait",
			},
			{
				Id:           "token_import",
				Label:        map[string]string{"zh": "Token 导入", "en": "Token Import"},
				Capabilities: []string{"profile"},
				Fields: []*pb.AuthField{
					{
						Name:        "access_token",
						Label:       map[string]string{"zh": "access_token", "en": "access_token"},
						Type:        "textarea",
						Required:    true,
						Placeholder: "登录 xiaohuanxiong.com 后从请求头复制 Bearer token",
					},
					{
						Name:     "refresh_token",
						Label:    map[string]string{"zh": "refresh_token（可选）", "en": "refresh_token (optional)"},
						Type:     "textarea",
						Required: false,
					},
				},
			},
		},
	}}, nil
}
