// codebuff 插件 — Codebuff（Freebuff 免费层）反代。
// 上游：www.codebuff.com/api/v1，OpenAI 兼容 /chat/completions + session/run 编排。
// 登录：粘贴 codebuff Bearer token（裸 token / curl / HAR 自动嗅探）。
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
	pluginName = "codebuff"
	apiBase    = "https://www.codebuff.com/api/v1"
	// codebuffUA 伪装的 codebuff 客户端 UA（上游信道的固定头）
	codebuffUA = "ai-sdk/openai-compatible/1.0.25/codebuff"
	// rootAgentID 根 agent（run 层级起点）
	rootAgentID = "base2-free"
	// defaultModel 默认免费模型
	defaultModel = "z-ai/glm-5.3-flash"

	hdrModel         = "x-freebuff-model"
	hdrInstance      = "x-freebuff-instance-id"
	hdrMultiSession  = "x-freebuff-multi-session"
	hdrIncludeUnused = "x-freebuff-include-unused-rate-limits"
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
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

// settingStr 读插件设置（核心管理界面在线编辑），30s 内存缓存。
func (p *plugin) settingStr(key string) string {
	p.mu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < 30*time.Second
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
		Label:           map[string]string{"zh": "Codebuff", "en": "Codebuff"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "上游请求 User-Agent 伪装值，留空使用内置默认",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "token", Label: map[string]string{"zh": "粘贴 Token", "en": "Paste Token"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": "Bearer Token", "en": "Bearer Token"},
					Type: "textarea", Required: true,
					Placeholder: "粘贴 codebuff Bearer token，或含 authorization: Bearer 的 curl / HAR 文本",
				}},
			},
		},
	}}, nil
}
