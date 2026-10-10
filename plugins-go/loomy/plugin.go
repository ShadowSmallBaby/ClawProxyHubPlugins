// main.go — 入口 + Handshake/Login + 设置缓存。
//
// loomy 插件：Loomy（讯飞）Web 版协议反代。Cookie(loomy_web_session) 认证，
// POST /web/api/chat/completions 回 OpenAI 兼容 SSE；接口无 role 数组、无工具位，
// 故信封多轮历史折叠进单 content，仅支持纯文本对话。
package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const pluginName = "loomy"

const settingsTTL = 30 * time.Second

// New 由平台入口注入版本，桌面与 Android 复用业务实现。
func New(version string) sdk.Plugin {
	if version == "" {
		version = "dev"
	}
	return &plugin{version: version}
}

type plugin struct {
	version string
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	settingsJSON []byte // 插件设置缓存（30s）
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

// userAgentStr 请求头 User-Agent（用户可配置；空 = 内置浏览器形态）。
func (p *plugin) userAgentStr() string {
	if v := p.settingStr("user_agent"); v != "" {
		return v
	}
	return defaultUserAgent
}

// ---------- Manifest / 登录 ----------

const settingsSchema = `{
	"type": "object",
	"properties": {
		"user_agent": {"type": "string", "title": "User-Agent", "description": "请求头 User-Agent 伪装值，留空使用内置浏览器形态", "default": ""}
	}
}`

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: p.version, Author: "cph",
		Label:           map[string]string{"zh": "Loomy", "en": "Loomy"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "account", "tasks"},
		Endpoints:       []string{"chat_completions", "messages"},
		SettingsSchema:  settingsSchema,
		AuthMethods: []*pb.AuthMethod{
			{
				Id:           "cookie_header",
				Label:        map[string]string{"zh": "Cookie 导入", "en": "Cookie Import"},
				Capabilities: []string{"profile"},
				Fields: []*pb.AuthField{{
					Name:        "cookie",
					Label:       map[string]string{"zh": "loomy_web_session", "en": "loomy_web_session"},
					Type:        "textarea",
					Required:    true,
					Placeholder: "loomy_web_session=...（登录 loomy.xunfei.cn 后从浏览器 Cookie 复制）",
				}},
			},
		},
	}}, nil
}

// Login 单方式 cookie_header：贴 loomy_web_session（或完整 Cookie 头），建档前校验并拉额度。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if req.MethodId != "cookie_header" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "unknown auth method: " + req.MethodId}}, nil
	}
	cookie := normalizeCookie(req.Form["cookie"])
	if cookie == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "Cookie 不能为空"}}, nil
	}
	c := &credential{Cookie: cookie, Label: "loomy"}
	blob, _ := json.Marshal(c)
	prof, _ := p.GetProfile(ctx, &pb.CredentialBlob{Blob: blob})
	if prof == nil || !prof.Healthy {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "Cookie 无效或已过期，请重新登录 Loomy 后复制 loomy_web_session"}}, nil
	}
	return &pb.LoginResult{Blob: blob, Profile: prof}, nil
}
