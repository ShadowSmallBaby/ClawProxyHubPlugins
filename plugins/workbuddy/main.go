// workbuddy 插件 — 腾讯 WorkBuddy / CodeBuddy 客户端反代。
// 上游：copilot.tencent.com，OpenAI 兼容（/v2/chat/completions，仅流式）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	upstreamBase = "https://copilot.tencent.com"
	loginBase    = "https://www.workbuddy.cn"
	pathChat     = "/v2/chat/completions"
	pathRefresh  = "/v2/plugin/auth/token/refresh"
	pathSendSMS  = "/v2/plugin/login/send-sms"
	pathLoginTok = "/v2/plugin/login/token"
	// 浏览器形态 UA 内置默认：插件登录接口在 www.workbuddy.cn，不是客户端头（核心全局浏览器 UA 非空时覆盖）
	defaultBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/138.0.7204.251 Safari/537.36"
	// 客户端标识默认值：可被插件设置覆盖（user_agent / ide_version）
	defaultUserAgent  = "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"
	defaultIDEVersion = "5.5.4"
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	authState    map[string]string // 我们签发的 state → 上游 state（上游 state 不回传前端）
	settingsJSON []byte            // 插件设置缓存（30s，客户端标识懒刷新用）
	settingsAt   time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// 当前生效的客户端标识（ensureIdentity 刷新；telemetry 等包级代码读取）。
var (
	identMu        sync.RWMutex
	identUA        = defaultUserAgent
	identIDEVer    = defaultIDEVersion
	identBrowserUA = defaultBrowserUA
)

func clientUA() string { identMu.RLock(); defer identMu.RUnlock(); return identUA }

func clientIDEVersion() string { identMu.RLock(); defer identMu.RUnlock(); return identIDEVer }

// browserUA 浏览器形态 UA：核心全局浏览器 UA（sdk.SettingBrowserUserAgent）非空则用它，否则内置。
func browserUA() string { identMu.RLock(); defer identMu.RUnlock(); return identBrowserUA }

// versionFromUA 从 UA 提取版本号（首个 "/" 后到空格前的段）。
func versionFromUA(ua string) string {
	i := strings.Index(ua, "/")
	if i < 0 {
		return ""
	}
	rest := ua[i+1:]
	if j := strings.IndexAny(rest, " /"); j > 0 {
		return rest[:j]
	}
	return rest
}

// ensureIdentity 懒刷新客户端标识：读插件设置（30s 缓存），
// user_agent 覆盖 UA；ide_version 留空则从 UA 解析；核心注入的全局浏览器 UA 覆盖内置浏览器 UA。
func (p *plugin) ensureIdentity() {
	p.mu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < 30*time.Second
	p.mu.Unlock()
	if fresh {
		return
	}
	ua, ver, bua := defaultUserAgent, "", defaultBrowserUA
	if p.host != nil {
		if raw := p.host.Settings("workbuddy"); len(raw) > 0 {
			var cfg map[string]string
			if json.Unmarshal(raw, &cfg) == nil {
				if cfg["user_agent"] != "" {
					ua = cfg["user_agent"]
				}
				ver = cfg["ide_version"]
				if v := strings.TrimSpace(cfg[sdk.SettingBrowserUserAgent]); v != "" {
					bua = v
				}
			}
		}
	}
	if ver == "" {
		ver = versionFromUA(ua)
	}
	if ver == "" {
		ver = defaultIDEVersion
	}
	identMu.Lock()
	identUA, identIDEVer, identBrowserUA = ua, ver, bua
	identMu.Unlock()
	p.mu.Lock()
	p.settingsJSON, p.settingsAt = []byte("cached"), time.Now()
	p.mu.Unlock()
}

// ---------- 凭据 blob（桌面端 .info 文件形态） ----------

type credential struct {
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	} `json:"account"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c, err := parseCred(blob.GetBlob())
	if err != nil {
		return nil, err
	}
	if pr := blob.GetProxy(); pr != nil && pr.GetHost() != "" {
		u := &url.URL{Scheme: shared.OrDefault(pr.GetScheme(), "http"), Host: fmt.Sprintf("%s:%d", pr.GetHost(), pr.GetPort())}
		if pr.GetUsername() != "" {
			u.User = url.UserPassword(pr.GetUsername(), pr.GetPassword())
		}
		c.proxyURL = u.String()
	}
	return c, nil
}

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hc(cred *credential) *http.Client {
	if cred == nil || cred.proxyURL == "" {
		return sdk.UpstreamClient("")
	}
	if c, ok := proxyClients.Load(cred.proxyURL); ok {
		return c.(*http.Client)
	}
	u, err := url.Parse(cred.proxyURL)
	if err != nil {
		return sdk.UpstreamClient("")
	}
	c := sdk.UpstreamClient(u.String())
	proxyClients.Store(cred.proxyURL, c)
	return c
}

func parseCred(blob []byte) (*credential, error) {
	var c credential
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("invalid credential: %w", err)
	}
	if c.Auth.AccessToken == "" {
		return nil, fmt.Errorf("credential missing auth.accessToken")
	}
	return &c, nil
}

// headers WorkBuddy 客户端伪装头。
func (p *plugin) headers(cred *credential, auth bool) map[string]string {
	p.ensureIdentity()
	domain := cred.Auth.Domain
	if domain == "" {
		domain = "www.codebuddy.cn"
	}
	h := map[string]string{
		"Content-Type":                "application/json",
		"Accept":                      "application/json",
		"X-User-Id":                   cred.Account.UID,
		"X-Enterprise-Id":             cred.Account.EnterpriseID,
		"X-Tenant-Id":                 cred.Account.EnterpriseID,
		"X-Domain":                    domain,
		"User-Agent":                  clientUA(),
		"X-IDE-Type":                  "WorkBuddy",
		"X-IDE-Name":                  "WorkBuddy",
		"X-IDE-Version":               clientIDEVersion(),
		"X-Private-Data":              "false",
		"X-Product":                   "SaaS",
		"x-stainless-arch":            "x64",
		"x-stainless-lang":            "js",
		"x-stainless-os":              "Windows",
		"x-stainless-package-version": "6.25.0",
		"x-stainless-retry-count":     "0",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v22.21.1",
		"X-Agent-Intent":              "craft",
		"X-Agent-Purpose":             "conversation_topic",
	}
	if auth {
		h["Authorization"] = "Bearer " + cred.Auth.AccessToken
	}
	return h
}

func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body interface{}) (*http.Response, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = sdk.UpstreamClient("")
	}
	return client.Do(req)
}

// envelope {code, msg, data} 信封校验。
func envelope(resp *http.Response) (json.RawMessage, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var e struct {
		Code    int             `json:"code"`
		Message string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("upstream non-json (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == 401 || e.Code == 401 {
		return nil, &authError{msg: fmt.Sprintf("auth failed: HTTP %d code=%d %s", resp.StatusCode, e.Code, e.Message)}
	}
	if e.Code != 0 {
		return nil, fmt.Errorf("upstream rejected: code=%d %s", e.Code, e.Message)
	}
	return e.Data, nil
}

type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: "workbuddy", Version: version, Author: "cph",
		Label:           map[string]string{"zh": "WorkBuddy", "en": "WorkBuddy"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "tasks"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "客户端 UA 伪装值，留空使用内置默认",
					"default": ""
				},
				"ide_version": {
					"type": "string",
					"title": "IDE 版本号",
					"description": "X-IDE-Version 与埋点字段，留空则从 User-Agent 解析",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "phone_otp", Label: map[string]string{"zh": "手机验证码登录", "en": "Phone OTP Login"},
				Capabilities: []string{"refreshable", "auto_relogin"},
				Fields: []*pb.AuthField{{
					Name: "phone", Label: map[string]string{"zh": "手机号", "en": "Phone Number"}, Type: "phone", Required: true,
				}},
			},
			{
				Id: "oauth", Label: map[string]string{"zh": "浏览器授权登录", "en": "Browser Authorization"},
				Capabilities: []string{"refreshable"},
				Callback:     "auto", // 插件侧轮询上游，前端只轮询不显示输入框
			},
			{
				Id: "auth_file", Label: map[string]string{"zh": "凭据文件", "en": "Credential File"},
				Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": ".info 凭据文件内容", "en": ".info credential file content"},
					Type: "textarea", Required: true,
					Placeholder: `{"auth": {"accessToken": "..."}, "account": {"uid": "..."}}`,
				}},
			},
		},
	}}, nil
}

// ---------- Chat ----------

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, orHint(err)))
	}

	body := openaiup.ChatBody(req)
	body["model"] = shared.OrDefault(req.Model, "auto")
	desensitizeMessageBody(body) // system 净化：客户端特征改写 + 合规声明脱敏

	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+pathChat, p.headers(cred, true), body)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		if resp.StatusCode == 401 {
			code = 401
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}

	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sawEvent := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") && !strings.Contains(line, "[DONE]") {
			sawEvent = true
		}
		parser.Feed(line)
	}
	if err := scanner.Err(); err != nil {
		parser.FinishWithError(502, "upstream stream broken: "+err.Error())
		return nil
	}
	if !sawEvent {
		parser.FinishWithError(502, "upstream returned an empty stream")
		return nil
	}
	parser.Finish()
	return nil
}

// ---------- 工具 ----------

func nowMillis() int64 { return time.Now().UnixMilli() }

// trimFloat 浮点转不丢精度的十进制字符串（整数不带小数点）。
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// orHint 凭据解析失败时附上排查方向。
func orHint(err error) string {
	if strings.Contains(err.Error(), "unexpected end of JSON input") {
		return "凭据为空：账号可能未分组或凭据未正确保存，请重新授权或检查分组"
	}
	return err.Error()
}
