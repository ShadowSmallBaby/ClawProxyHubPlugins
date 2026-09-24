// 凭据解析、HTTP client、鉴权头与登录（token 嗅探）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

type credential struct {
	Token string `json:"token"`

	// 账号面缓存（随 Refresh 回传 Blob 持久化）
	Name       string  `json:"name,omitempty"`
	AccessTier string  `json:"accessTier,omitempty"`
	Freebucks  float64 `json:"freebucks,omitempty"` // daily remaining
	FreebucksD float64 `json:"freebucksLimit,omitempty"`
	InstanceID string  `json:"instanceId,omitempty"` // 最近会话实例（复用）

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
	if c.Token == "" {
		return nil, fmt.Errorf("credential missing token")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
// 连接 15s / TLS 15s / 首字节 60s，流式对话整体不设超时（长回复合法）。
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

// authHeaders 上游请求头（Bearer 鉴权 + codebuff 客户端伪装 UA）。
func (p *plugin) authHeaders(c *credential) map[string]string {
	ua := p.settingStr("user_agent")
	return map[string]string{
		"Authorization": "Bearer " + c.Token,
		"Content-Type":  "application/json",
		"User-Agent":    shared.OrDefault(ua, codebuffUA),
	}
}

// Login 粘贴导入：裸 token / curl / HAR 自动嗅探出 Bearer token，session 探测校验。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if req.MethodId != "token" {
		return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
	}
	tok := sniffToken(req.Form["content"])
	if tok == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "未能从输入中提取到 Bearer token，请粘贴 token 本身或含 authorization: Bearer 的 curl/HAR"}}, nil
	}
	c := &credential{Token: tok}
	if err := p.fetchProfile(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "token 校验失败：" + err.Error()}}, nil
	}
	return loginDone(c), nil
}

// bearerRe 从任意文本抓 Bearer token（curl/HAR 通用）。
var bearerRe = regexp.MustCompile(`(?i)bearer\s+([A-Za-z0-9._\-]{16,})`)

// sniffToken 提取 Bearer token：优先 Bearer 模式，否则整段裸串当 token。
func sniffToken(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return ""
	}
	// JSON 包裹 {"token":"..."} / {"content":"..."}
	if strings.HasPrefix(s, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(s), &m) == nil {
			for _, k := range []string{"token", "content", "authorization", "Authorization"} {
				if v := strings.TrimSpace(m[k]); v != "" {
					s = v
					break
				}
			}
		}
	}
	if g := bearerRe.FindStringSubmatch(s); len(g) == 2 {
		return g[1]
	}
	if !strings.ContainsAny(s, " \t\r\n") && len(s) >= 16 {
		return s
	}
	return ""
}

func loginDone(c *credential) *pb.LoginResult {
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{
		Blob: blob,
		Profile: &pb.AccountProfile{
			DisplayName: credentialName(c), Healthy: true, Quota: map[string]string{},
		},
	}
}

func credentialName(c *credential) string {
	if c.Name != "" {
		return c.Name
	}
	return "codebuff-" + version
}
