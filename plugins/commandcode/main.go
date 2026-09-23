// commandcode 插件 — Command Code（api.commandcode.ai）反代。
// 上游：/alpha/generate（CC 私有信封，NDJSON 流）。核心已把三协议归一化成信封，
// 本插件只做信封 ↔ CC 私有协议双向转换，并移植 CLI 的反检测机制：
// 设备指纹（登录时生成、随凭据持久化）、fingerprint/lifecycle 预请求、per-key session。
// 分层：main（骨架/设置）/ auth（登录+凭据）/ model（模型目录）/ upstream（协议收敛）/
// envelope（信封转换）/ chat（编排）/ fingerprint（反检测）。
package main

import (
	"context"
	"crypto/rand"
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
	pluginName     = "commandcode"
	defaultAPIBase = "https://api.commandcode.ai"
	// ccProtocolVersion 实际实现的 wire 协议版本（对齐 command-code@1.53.1 源码）。
	// 报的是协议版本，npm 更新只需维护者手动对齐，不自动跟随。
	ccProtocolVersion = "1.53.1"
	settingsTTL       = 30 * time.Second
	sessionDuration   = 12 * time.Hour
	sessionJitter     = 1 * time.Hour
	initRefresh       = 8 * time.Hour
	initJitter        = 2 * time.Hour
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

	sessions sync.Map // apiKey → *sessionEntry（per-key session）

	modelsMu sync.Mutex
	models   []*pb.ModelInfo
	modelsAt time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

type sessionEntry struct {
	id        string
	expiresAt time.Time
}

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
	var cfg map[string]interface{}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	if v, ok := cfg[key].(string); ok {
		return v
	}
	return ""
}

func (p *plugin) settingBool(key string, def bool) bool {
	v := p.settingStr(key)
	if v == "" {
		return def
	}
	return v != "false" && v != "0"
}

func (p *plugin) apiBase() string {
	if v := strings.TrimRight(p.settingStr("api_base"), "/"); v != "" {
		return v
	}
	return defaultAPIBase
}

// ccConfigFrom 从设置解析运行配置。
func (p *plugin) ccConfigFrom() ccConfig {
	return ccConfig{
		profile:                defaultDeviceProfile(p.settingStr("device_project_dir")),
		fingerprintSalt:        p.settingStr("fingerprint_salt"),
		cliMode:                shared.OrDefault(p.settingStr("cli_mode"), "agent"),
		cliSessionMode:         shared.OrDefault(p.settingStr("cli_session_mode"), "interactive"),
		zdr:                    p.settingBool("zdr", false),
		emptySystemPlaceholder: p.settingBool("empty_system_placeholder", true),
	}
}

// ---------- Manifest ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "Command Code", "en": "Command Code"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"api_base": {"type": "string", "title": "API Base", "description": "CC 上游地址，留空用官方", "default": ""},
				"fingerprint_salt": {"type": "string", "title": "指纹盐", "description": "成批更换设备身份（真实账号 key 不动，这是逃生口）", "default": ""},
				"device_project_dir": {"type": "string", "title": "伪造项目目录", "description": "留空用内置 C:\\Users\\dev\\projects\\app；换值 = 所有账号换一台设备", "default": ""},
				"cli_mode": {"type": "string", "title": "信封 mode", "description": "agent / learning / custom-agent / title-gen / compact / vision", "default": "agent"},
				"cli_session_mode": {"type": "string", "title": "会话 mode", "description": "lifecycle metadata：interactive / non-interactive", "default": "interactive"},
				"zdr": {"type": "boolean", "title": "ZDR", "description": "请求 CC 的 ZDR-only 路由", "default": false},
				"empty_system_placeholder": {"type": "boolean", "title": "空 system 占位", "description": "无 system 时发空格，阻止上游注入 ~7.5K token 默认提示词", "default": true},
				"stream_idle_seconds": {"type": "string", "title": "流空闲超时(秒)", "description": "上游无新数据的中断阈值；推理模型被误杀时可调大，留空用 90s", "default": ""}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{{
			Id: "api_key", Label: map[string]string{"zh": "API Key", "en": "API Key"},
			Fields: []*pb.AuthField{{
				Name: "key", Label: map[string]string{"zh": "API Key", "en": "API Key"},
				Type: "password", Required: true, Placeholder: "user_...",
			}},
		}},
	}}, nil
}

// ---------- 工具 ----------

// traceparent W3C Trace Context（OpenTelemetry）。
func traceparent() string {
	return "00-" + shared.RandHex(16) + "-" + shared.RandHex(8) + "-01"
}

// randDur [0, max) 的随机时长。
func randDur(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	b := make([]byte, 8)
	rand.Read(b)
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return time.Duration(v % uint64(max))
}
