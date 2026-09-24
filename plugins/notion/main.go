// notion 插件 — Notion AI（www.notion.so）私有协议反代。
// 上游：/api/v3/runInferenceTranscript（NDJSON 流）+ /api/v3/saveTransactionsFanout（建线程）。
// 认证：静态 Cookie（token_v2）+ space_id + user_id，无刷新态；登录仅做会话预热校验。
package main

import (
	"bytes"
	"context"
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
	pluginName    = "notion"
	notionBase    = "https://www.notion.so"
	epRunInfer    = notionBase + "/api/v3/runInferenceTranscript"
	epSaveTx      = notionBase + "/api/v3/saveTransactionsFanout"
	settingsTTL   = 30 * time.Second
	defaultClient = "23.13.20251011.2037" // 伪装的 notion-client-version
	// defaultUA 会话与对话请求的浏览器 UA（Notion 走 Cloudflare，Go 默认 UA 易被拦）。
	defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
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

// clientVersion notion-client-version 头（settings 可覆盖，空用内置默认）。
func (p *plugin) clientVersion() string {
	return shared.OrDefault(p.settingStr("client_version"), defaultClient)
}

// userAgent 请求 UA（settings 可覆盖，空用内置浏览器 UA）。
func (p *plugin) userAgent() string {
	return shared.OrDefault(p.settingStr("user_agent"), defaultUA)
}

// ---------- 凭据 blob ----------

// credential Notion 静态凭据：Cookie（token_v2 或完整 Cookie 头）+ 空间 / 用户标识。
// block_id / user_name / user_email 可选（部分模型的 context 需要）。
type credential struct {
	Cookie    string `json:"cookie"`
	SpaceID   string `json:"space_id"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	BlockID   string `json:"block_id,omitempty"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// validate 三要素齐备才算有效凭据。
func (c *credential) validate() error {
	c.Cookie = strings.TrimSpace(c.Cookie)
	c.SpaceID = strings.TrimSpace(c.SpaceID)
	c.UserID = strings.TrimSpace(c.UserID)
	switch {
	case c.Cookie == "":
		return fmt.Errorf("credential missing cookie（token_v2）")
	case c.SpaceID == "":
		return fmt.Errorf("credential missing space_id")
	case c.UserID == "":
		return fmt.Errorf("credential missing user_id")
	}
	return nil
}

// cookieHeader Cookie 头：整段含 "=" 视为原文，否则按 token_v2 裸值包装。
func (c *credential) cookieHeader() string {
	if strings.Contains(c.Cookie, "=") {
		return c.Cookie
	}
	return "token_v2=" + c.Cookie
}

// displayName 账号展示名（邮箱 > 用户名 > 空间 id 缩写）。
func (c *credential) displayName() string {
	if c.UserEmail != "" {
		return c.UserEmail
	}
	if c.UserName != "" {
		return c.UserName
	}
	return "notion-" + shortID(c.SpaceID)
}

// proxyURL 代理配置 → URL 字符串。
// ---------- HTTP ----------

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
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

// upstreamClient 连接 15s / TLS 15s / 首字节 60s，流式对话整体不设超时（长回复合法）。
// headers runInference / saveTransactions 公共请求头（照官方 web 客户端）。
func (p *plugin) headers(c *credential) map[string]string {
	return map[string]string{
		"Content-Type":                "application/json",
		"Accept":                      "application/x-ndjson",
		"Cookie":                      c.cookieHeader(),
		"x-notion-space-id":           c.SpaceID,
		"x-notion-active-user-header": c.UserID,
		"x-notion-client-version":     p.clientVersion(),
		"notion-audit-log-platform":   "web",
		"Origin":                      notionBase,
		"Referer":                     notionBase + "/",
		"User-Agent":                  p.userAgent(),
	}
}

// postJSON 发一个 JSON POST（返回响应交调用方处理）。
func (p *plugin) postJSON(ctx context.Context, c *credential, rawURL string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range p.headers(c) {
		req.Header.Set(k, v)
	}
	return p.hc(c).Do(req)
}

// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "Notion AI", "en": "Notion AI"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"client_version": {
					"type": "string",
					"title": "客户端版本号",
					"description": "x-notion-client-version 伪装值，留空使用内置默认",
					"default": ""
				},
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "请求头 User-Agent 伪装值，留空使用内置浏览器 UA",
					"default": ""
				},
				"model_map": {
					"type": "string",
					"title": "模型映射（JSON）",
					"description": "覆盖内置的「对外模型名 → Notion 后台模型」映射，如 {\"claude-sonnet-4.5\":\"anthropic-sonnet-alt\"}，留空用内置表",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "cookie", Label: map[string]string{"zh": "浏览器 Cookie", "en": "Browser Cookie"},
				Fields: []*pb.AuthField{
					{
						Name: "cookie", Label: map[string]string{"zh": "token_v2 / Cookie 头", "en": "token_v2 / Cookie"},
						Type: "textarea", Required: true,
						Placeholder: "DevTools → Application → Cookies → notion.so 的 token_v2 值（或完整 Cookie 头原文）",
					},
					{
						Name: "space_id", Label: map[string]string{"zh": "Space ID", "en": "Space ID"},
						Type: "text", Required: true, Placeholder: "请求头 x-notion-space-id 的值",
					},
					{
						Name: "user_id", Label: map[string]string{"zh": "User ID", "en": "User ID"},
						Type: "text", Required: true, Placeholder: "请求头 x-notion-active-user-header 的值",
					},
					{
						Name: "user_name", Label: map[string]string{"zh": "用户名", "en": "User name"},
						Type: "text", Placeholder: "可选，部分模型的 context 需要",
					},
					{
						Name: "user_email", Label: map[string]string{"zh": "邮箱", "en": "Email"},
						Type: "text", Placeholder: "可选，作为账号展示名",
					},
					{
						Name: "block_id", Label: map[string]string{"zh": "Block ID", "en": "Block ID"},
						Type: "text", Placeholder: "可选，指定对话上下文所在的 block",
					},
				},
			},
		},
	}}, nil
}

// Login Cookie 校验：三要素齐备 → 会话预热探活（失败仍建档，真实校验交首次 Chat）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	c := &credential{
		Cookie:    strings.TrimSpace(req.Form["cookie"]),
		SpaceID:   strings.TrimSpace(req.Form["space_id"]),
		UserID:    strings.TrimSpace(req.Form["user_id"]),
		UserName:  strings.TrimSpace(req.Form["user_name"]),
		UserEmail: strings.TrimSpace(req.Form["user_email"]),
		BlockID:   strings.TrimSpace(req.Form["block_id"]),
	}
	if err := c.validate(); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	p.warmup(ctx, c) // 预热失败不阻断（凭据可能仍有效，真实校验走首次对话）
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{}},
	}, nil
}

// warmup 会话预热：GET 首页领取 Cloudflare Cookie，提高后续请求成功率（失败仅记日志）。
func (p *plugin) warmup(ctx context.Context, c *credential) {
	req, err := http.NewRequestWithContext(ctx, "GET", notionBase+"/", nil)
	if err != nil {
		return
	}
	req.Header.Set("Cookie", c.cookieHeader())
	req.Header.Set("User-Agent", p.userAgent())
	resp, err := p.hc(c).Do(req)
	if err != nil {
		if p.host != nil {
			p.host.Log("warn", "notion warmup failed: "+err.Error())
		}
		return
	}
	resp.Body.Close()
}

// ---------- 刷新 / 资料 ----------

// Refresh 静态凭据无 token 刷新：原样回传 blob（保留三要素），顺带预热探活。
// 遵循「静态密钥类插件刷新健壮性」：不因无刷新态而清空凭据或改动展示名。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	p.warmup(ctx, c)
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: &pb.AccountProfile{
		DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{},
	}}, nil
}

// GetProfile 基本档案（Notion 无余额接口，固定展示名 + 健康）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return &pb.AccountProfile{DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{}}, nil
}

// ---------- 工具 ----------

// shortID 取 id 前 8 位作展示缩写。
func shortID(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
