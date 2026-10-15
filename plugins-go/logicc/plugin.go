// logicc 插件 — Logicc(app.logicc.com)反代。
// 上游:POST /api/chats 建会话 → POST /api/chats/{id} 发提示词,返回 AI SDK v6 事件流(data: JSON 帧)。
// 认证:Clerk 会话 Cookie(自动解析 Session ID,按需刷新 Session JWT)或仅 Session JWT(用至过期)。
// 核心已接管账号池调度、三协议归一化与控制台,本插件只负责上游协议收敛:
// 信封 ↔ 提示词/工具文本协议、事件流 ↔ StreamEvent、凭据刷新。
// 分层:plugin(骨架/设置/凭据) / model(模型目录) / auth(登录与刷新) / prompt(信封→提示词与工具协议) /
// chat(编排 + 事件流解析)。
package logicc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	pluginName       = "logicc"
	defaultAPIBase   = "https://app.logicc.com/api"
	defaultClerkBase = "https://clerk.logicc.com"
	settingsTTL      = 30 * time.Second
)

// New 由平台入口注入版本,桌面与 Android 复用业务实现。
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
	settingsJSON []byte
	settingsAt   time.Time

	modelsMu sync.Mutex
	models   []*pb.ModelInfo
	modelsAt time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// ---------- 设置 ----------

// settingStr 读插件设置(核心管理界面在线编辑),30s 内存缓存。
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
	var cfg map[string]json.RawMessage
	if json.Unmarshal(raw, &cfg) == nil {
		if v, ok := cfg[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil {
				return s
			}
		}
	}
	return ""
}

// apiBase 上游 API 根地址(settings api_base 可覆盖,空用官方默认)。
func (p *plugin) apiBase() string {
	return shared.OrDefault(strings.TrimRight(p.settingStr("api_base"), "/"), defaultAPIBase)
}

// clerkBase Clerk 认证根地址(settings clerk_base 可覆盖,空用官方默认)。
func (p *plugin) clerkBase() string {
	return shared.OrDefault(strings.TrimRight(p.settingStr("clerk_base"), "/"), defaultClerkBase)
}

// defaultAssistantID 建会话时指定的助手 id(空 = 上游默认助手)。
func (p *plugin) defaultAssistantID() string { return strings.TrimSpace(p.settingStr("assistant_id")) }

// defaultModel model 缺省或为 "default" 时使用的模型 id(空 = 取上游目录的首个默认模型)。
func (p *plugin) defaultModel() string { return strings.TrimSpace(p.settingStr("default_model")) }

// ---------- Manifest ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: p.version, Author: "cph",
		Label:           map[string]string{"zh": "Logicc", "en": "Logicc"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"api_base": {"type": "string", "title": "API 根地址", "description": "上游 API 根地址,留空用 https://app.logicc.com/api", "default": ""},
				"clerk_base": {"type": "string", "title": "Clerk 地址", "description": "Clerk 认证根地址,留空用 https://clerk.logicc.com", "default": ""},
				"assistant_id": {"type": "string", "title": "助手 ID", "description": "建会话时使用的上游助手(决定其 systemPrompt);留空用上游默认助手", "default": ""},
				"default_model": {"type": "string", "title": "默认模型", "description": "客户端模型留空或为 default 时使用的模型 id;留空取上游目录默认项", "default": ""},
				"stream_idle_seconds": {"type": "string", "title": "流空闲超时(秒)", "description": "上游无新数据的中断阈值,留空用 120s", "default": ""}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "cookie", Label: map[string]string{"zh": "Cookie", "en": "Cookie"},
				Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{Name: "cookie", Label: map[string]string{"zh": "Cookie", "en": "Cookie"}, Type: "textarea", Required: true,
						Placeholder: "浏览器 DevTools 复制 clerk.logicc.com 的 Cookie 头原文(含 __client)"},
				},
			},
			{
				Id: "token", Label: map[string]string{"zh": "Session JWT", "en": "Session JWT"},
				Fields: []*pb.AuthField{
					{Name: "token", Label: map[string]string{"zh": "Session JWT", "en": "Session JWT"}, Type: "textarea", Required: true,
						Placeholder: "浏览器控制台 await window.Clerk.session.getToken({ skipCache: true })"},
				},
			},
		},
	}}, nil
}

// ---------- 凭据 ----------

// credential Logicc 账号:Clerk 会话 Cookie(解析出 __client 与 Session ID)或仅 Session JWT。
// blob 由核心 opaque 持久化,插件回写轮换后的 Cookie / 新 JWT。
type credential struct {
	ClientCookie string `json:"clientCookie,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	Token        string `json:"token,omitempty"`

	// accountID 由核心注入,只用于进程内缓存身份,不落 blob(核心已单独持久化账号 id)。
	accountID string `json:"-"`
	proxyURL  string `json:"-"`
}

func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.Token == "" && c.ClientCookie == "" {
		return nil, fmt.Errorf("credential missing session cookie or token")
	}
	c.accountID = blob.GetAccountId()
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

func (c *credential) toBlob() []byte {
	raw, _ := json.Marshal(c)
	return raw
}

// cacheKey 进程内缓存身份:按账号 id 优先,回退 Session ID,JWT 类账号用 token 摘要(不落全量敏感值)。
func (c *credential) cacheKey() string {
	if c.accountID != "" {
		return "acc:" + c.accountID
	}
	if c.SessionID != "" {
		return "sid:" + c.SessionID
	}
	sum := sha256.Sum256([]byte(c.Token))
	return "tok:" + hex.EncodeToString(sum[:8])
}

// ---------- 进程内凭据缓存 ----------
//
// 对话中途换到的 JWT(以及 Clerk 轮换后的 Cookie)无法经 RPC 回写核心的持久化凭据,
// 因此在插件进程内按账号复用:否则存量 JWT 一旦过期,之后每条请求都要打一次 Clerk。
// 核心主动调 Refresh 时仍会回写 blob,重启后由持久化值兜底。
var (
	tokenCache     sync.Map // cacheKey → *tokenEntry
	refreshFlights sync.Map // cacheKey → *refreshCall
)

type tokenEntry struct {
	token  string
	cookie string
}

type refreshCall struct {
	done  chan struct{}
	value string
	err   error
}

// cachedToken 取缓存中仍有效的 JWT,并把轮换后的 Cookie 回灌到本次凭据。
func cachedToken(c *credential) (string, bool) {
	raw, ok := tokenCache.Load(c.cacheKey())
	if !ok {
		return "", false
	}
	entry := raw.(*tokenEntry)
	if !jwtFresh(entry.token) {
		tokenCache.Delete(c.cacheKey())
		return "", false
	}
	if entry.cookie != "" {
		c.ClientCookie = entry.cookie
	}
	c.Token = entry.token
	return entry.token, true
}

// storeToken 写入缓存(含 Clerk 轮换后的 Cookie)。
func storeToken(c *credential) {
	tokenCache.Store(c.cacheKey(), &tokenEntry{token: c.Token, cookie: c.ClientCookie})
}

// ---------- HTTP ----------

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client(流式对话整体不设超时,由 stream_idle 看门狗兜底)。
func (p *plugin) hc(cred *credential) *http.Client {
	key := ""
	if cred != nil {
		key = cred.proxyURL
	}
	if c, ok := proxyClients.Load(key); ok {
		return c.(*http.Client)
	}
	c := sdk.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
}

// ---------- 通用工具 ----------

// decodeJWT 解析 JWT payload(不验签,只读 sid / exp)。
func decodeJWT(token string) (map[string]interface{}, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT")
	}
	// 兼容带 padding 与标准字母表:Clerk 用 RawURL,个别来源可能带 padding。
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil, fmt.Errorf("invalid JWT payload: %w", err)
		}
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("invalid JWT payload: %w", err)
	}
	return claims, nil
}

// jwtFresh token 在 10s 容差后仍有效。
func jwtFresh(token string) bool {
	claims, err := decodeJWT(token)
	if err != nil {
		return false
	}
	exp, ok := claims["exp"].(float64)
	return ok && int64(exp) > time.Now().Unix()+10
}
