// doubao 插件 — 豆包（www.doubao.com）客户端协议反代。
// 上游：POST /chat/completion 桌面客户端 SSE 端点（JSON 明文 + chunk_delta），免 KEY。
// 登录：Cookie 导入（Cookie 头或 .doubao_session.json），无原生 token 刷新。
// 多模态对话 + 深度思考（ReasoningDelta）+ 联网搜索（tool_info 以文本透出）；
// 工具调用经提示词注入 + <tool_call> 标签解析模拟（见 envelope.go）。
// 【401 登录态修复（NexPort 移动端反哺）】两处：① loginCookieHeader 建档前预检
// 会话 Cookie（sessionid/sessionid_ss any-of，缺失 400 拒建档——登录前匿名 Cookie
// 如 ttwid/msToken/passport_csrf_token 等十余条解析通过却必然 401，坏档静默入库，
// 面板手动粘贴路径同被此预检堵住）；② finalizeLogin 的 msToken 兜底先取凭据
// Cookies 内 msToken（浏览器捕获面自然含该键，此前被丢弃致出站缺省——风控
// 710022002/710022004 已知诱因）再落全局设置。Cookie 头外发见 upstream.go。
package doubao

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

const pluginName = "doubao"

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
		"user_agent": {"type": "string", "title": "User-Agent", "description": "请求头 User-Agent 伪装值，留空使用内置浏览器形态", "default": ""},
		"ms_token": {"type": "string", "title": "msToken", "description": "全局 msToken 兜底（凭据未带时使用；空值会触发风控）", "default": ""}
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
		Label:           map[string]string{"zh": "豆包", "en": "Doubao"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login"},
		Endpoints:       []string{"chat_completions", "messages"},
		SettingsSchema:  settingsSchema,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "cookie_header", Label: map[string]string{"zh": "Cookie 导入", "en": "Cookie Header"},
				Fields: []*pb.AuthField{{
					Name: "cookie", Label: map[string]string{"zh": "Cookie 头", "en": "Cookie Header"},
					Type: "textarea", Required: true,
					Placeholder: "sessionid=...; ttwid=...; passport_csrf_token=...",
				}},
			},
			{
				Id: "session_json", Label: map[string]string{"zh": "Session JSON 导入", "en": "Session JSON"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": "Session JSON", "en": "Session JSON"},
					Type: "textarea", Required: true,
					Placeholder: `{"cookies":{"sessionid":"..."},"params":{"ms_token":"..."}}`,
				}},
			},
		},
	}}, nil
}

// Login 两方式：cookie_header（贴浏览器 Cookie 头）与 session_json（贴 .doubao_session.json），均一步建档。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "cookie_header":
		return p.loginCookieHeader(req)
	case "session_json":
		return p.loginSessionJSON(req)
	}
	return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "unknown auth method: " + req.MethodId}}, nil
}

// loginCookieHeader 解析浏览器复制的 Cookie 头字符串。
// 【401 登录态修复】建档前预检会话 Cookie：sessionid/sessionid_ss 任一存在即放行
// （any-of，保守不误杀）。
func (p *plugin) loginCookieHeader(req *pb.LoginRequest) (*pb.LoginResult, error) {
	cookies := parseCookieHeader(req.Form["cookie"])
	if len(cookies) == 0 {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "Cookie 头解析失败，需含 sessionid 等"}}, nil
	}
	if cookies["sessionid"] == "" && cookies["sessionid_ss"] == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "Cookie 缺少 sessionid/sessionid_ss（未登录）：请先在浏览器完成登录再复制，" +
			"应形如 sessionid=...; ttwid=...; passport_csrf_token=..."}}, nil
	}
	return p.finalizeLogin(&credential{Label: "doubao-cookie", Cookies: cookies})
}

// loginSessionJSON 解析 .doubao_session.json（cookies + params 设备参数）。
func (p *plugin) loginSessionJSON(req *pb.LoginRequest) (*pb.LoginResult, error) {
	var raw struct {
		Cookies map[string]string `json:"cookies"`
		Params  struct {
			MsToken  string `json:"ms_token"`
			DeviceID string `json:"device_id"`
			WebID    string `json:"web_id"`
			Fp       string `json:"fp"`
			BotID    string `json:"bot_id"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(req.Form["content"]), &raw); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "Session 必须是 JSON: " + err.Error()}}, nil
	}
	if len(raw.Cookies) == 0 {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "Session JSON 缺少 cookies"}}, nil
	}
	return p.finalizeLogin(&credential{
		Label: "doubao-session", Cookies: raw.Cookies,
		MsToken: raw.Params.MsToken, DeviceID: raw.Params.DeviceID,
		WebID: raw.Params.WebID, Fp: raw.Params.Fp, BotID: raw.Params.BotID,
	})
}

// finalizeLogin 补 msToken 兜底 → 建档。
// 【401 登录态修复】三级取值：凭据参数自带 > 凭据 Cookies 内 msToken > 全局设置。
func (p *plugin) finalizeLogin(c *credential) (*pb.LoginResult, error) {
	if c.MsToken == "" {
		if v := c.Cookies["msToken"]; v != "" {
			c.MsToken = v
		} else {
			c.MsToken = strings.TrimSpace(p.settingStr("ms_token"))
		}
	}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: c.Label, Healthy: true, Quota: map[string]string{}},
	}, nil
}

// parseCookieHeader 解析 "k=v; k2=v2" Cookie 头。
func parseCookieHeader(header string) map[string]string {
	cookies := map[string]string{}
	for _, item := range strings.Split(header, ";") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		cookies[kv[0]] = strings.TrimSpace(kv[1])
	}
	return cookies
}
