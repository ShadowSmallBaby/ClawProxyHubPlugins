// improvado 插件 — Improvado Agent（report.improvado.io）反代。
// 上游：/experimental/agent/api/claude-code/execute，Cookie 认证（浏览器导出），
// SSE 流只产 text-delta 事件（纯文本，无工具调用）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	pluginName  = "improvado"
	upstream    = "https://report.improvado.io/experimental/agent/api/claude-code/execute"
	origin      = "https://report.improvado.io"
	settingsTTL = 30 * time.Second
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

// settingStr 读插件设置（核心管理界面 / 实例视图），30s 内存缓存。
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

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "Improvado", "en": "Improvado"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "login"},
		Endpoints:       []string{"chat_completions"},
		SettingsSchema:  `{"type": "object", "properties": {}}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "cookie", Label: map[string]string{"zh": "浏览器 Cookie", "en": "Browser Cookie"},
				Fields: []*pb.AuthField{
					{
						Name: "cookie", Label: map[string]string{"zh": "Cookie 头", "en": "Cookie header"},
						Type: "textarea", Required: true,
						Placeholder: "浏览器 DevTools 复制 report.improvado.io 请求的 Cookie 头原文",
					},
					{
						Name: "workspace_id", Label: map[string]string{"zh": "Workspace ID", "en": "Workspace ID"},
						Type: "text", Required: true, Placeholder: "数字 ID（地址栏 workspace= 后的值）",
					},
					{
						Name: "provider", Label: map[string]string{"zh": "Provider", "en": "Provider"},
						Type: "text", Placeholder: "默认 codex-cli",
					},
					{
						Name: "effort", Label: map[string]string{"zh": "Effort", "en": "Effort"},
						Type: "text", Placeholder: "默认 xhigh（low/medium/high/xhigh）",
					},
				},
			},
		},
	}}, nil
}
