// chatjimmy 插件 — ChatJimmy（chatjimmy.ai）反代。
// 上游：chatjimmy.ai/api/chat，私有协议（一次性纯文本响应 + <|stats|> 尾块），免 KEY。
// 登录：匿名一键建档（上游无需凭据，账号仅供核心路由与分组）。
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
	pluginName   = "chatjimmy"
	upstreamURL  = "https://chatjimmy.ai/api/chat"
	defaultModel = "llama3.1-8B"
	defaultTopK  = 8
	settingsTTL  = 30 * time.Second
	// defaultUserAgent 桌面浏览器形态 UA（上游按来源校验）。
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// statsOpen/statsClose 上游在纯文本尾部附加的用量统计块界定符。
	statsOpen  = "<|stats|>"
	statsClose = "<|/stats|>"
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

// userAgentStr 请求头 User-Agent（用户可配置；空 = 内置浏览器形态）。
func (p *plugin) userAgentStr() string {
	if v := p.settingStr("user_agent"); v != "" {
		return v
	}
	return defaultUserAgent
}

// modelIDs 可用模型列表（设置里逗号分隔覆盖；空 = 内置默认单模型）。
func (p *plugin) modelIDs() []string {
	raw := p.settingStr("models")
	if raw == "" {
		return []string{defaultModel}
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{defaultModel}
	}
	return out
}

// topK Top-K 采样参数（设置可覆盖；非法/空 = 8）。
func (p *plugin) topK() int {
	if v := strings.TrimSpace(p.settingStr("top_k")); v != "" {
		n := 0
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return defaultTopK
}

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "ChatJimmy", "en": "ChatJimmy"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"models": {
					"type": "string",
					"title": "模型列表",
					"description": "逗号分隔的可用模型 id，留空使用内置默认 llama3.1-8B",
					"default": ""
				},
				"top_k": {
					"type": "string",
					"title": "Top-K",
					"description": "Top-K 采样参数，留空使用默认 8",
					"default": ""
				},
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "请求头 User-Agent 伪装值，留空使用内置浏览器形态",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id:    "anonymous",
				Label: map[string]string{"zh": "匿名接入", "en": "Anonymous"},
			},
		},
	}}, nil
}
