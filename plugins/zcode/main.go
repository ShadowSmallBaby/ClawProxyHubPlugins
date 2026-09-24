// zcode 插件 — ZCode Proxy 反代（GLM 编码套餐）。
// 上游：coding-plan 直连 api.z.ai / open.bigmodel.cn Anthropic 端点（双密钥）；
// start-plan 经 zcode.z.ai JWT 网关。登录：OAuth 设备码（cli init+poll，无本地回调）。
// 指纹：ZCode 桌面客户端 identity 头 + V4 请求签名 + 网关 system 块注入。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	pluginName = "zcode"
	// ZCode 桌面客户端版本与来源头（identity 伪装基线）
	defaultAppVersion  = "3.14.0"
	defaultReferer     = "https://zcode.z.ai"
	anthropicSDKSuffix = "ai-sdk/anthropic/3.0.81"
	anthropicVersion   = "2023-06-01"
	settingsTTL        = 30 * time.Second
	// systemPromptFile 内嵌的网关 system 块数据（与上游 system_prompt.json 同源）
	systemPromptFile = "system_prompt.json"
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
	systemData   map[string]json.RawMessage
	systemErr    error
	sg           *signer
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

// appVersion 请求头 X-ZCode-App-Version（用户可配置；空 = 内置默认）。
func (p *plugin) appVersion() string {
	if v := p.settingStr("app_version"); v != "" {
		return v
	}
	return defaultAppVersion
}

// ---------- 凭据 blob ----------

// credential 上游凭据：coding-plan 双密钥 / start-plan JWT。
type credential struct {
	Plan      string `json:"plan"`          // coding-plan / start-plan
	Provider  string `json:"provider"`      // zai / bigmodel
	APIKey    string `json:"api_key"`       // coding-plan：{id}.{secret} 双密钥串
	JWT       string `json:"jwt,omitempty"` // start-plan 计划令牌
	UserID    string `json:"user_id,omitempty"`
	DeviceMid string `json:"device_mid,omitempty"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{Plan: "coding-plan", Provider: "zai"}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.Plan != "start-plan" {
		c.Plan = "coding-plan"
	}
	if c.Provider != "bigmodel" {
		c.Provider = "zai"
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// proxyURL 代理配置 → URL 字符串。
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
// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "ZCode", "en": "ZCode"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "account"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"app_version": {
					"type": "string",
					"title": "客户端版本",
					"description": "X-ZCode-App-Version 伪装值，须与套餐要求的最低客户端版本一致",
					"default": "3.14.0"
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "oauth_zai", Label: map[string]string{"zh": "Z.AI 浏览器授权", "en": "Z.AI OAuth"},
				Capabilities: []string{"refreshable", "profile"},
				Callback:     "manual_poll", // 服务端完成授权，用户打开链接后手动确认
			},
			{
				Id: "oauth_bigmodel", Label: map[string]string{"zh": "智谱浏览器授权", "en": "BigModel OAuth"},
				Capabilities: []string{"refreshable", "profile"},
				Callback:     "manual_poll",
			},
		},
	}}, nil
}

// Login OAuth 设备码两步流程：State 空 = 发起（init → open_url），非空 = 轮询（poll）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	provider := "zai"
	if req.MethodId == "oauth_bigmodel" {
		provider = "bigmodel"
	} else if req.MethodId != "oauth_zai" {
		return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
	}
	if len(req.State) == 0 {
		return p.loginInit(ctx, provider)
	}
	return p.loginPoll(ctx, provider, req.State)
}

func (p *plugin) loginInit(ctx context.Context, provider string) (*pb.LoginResult, error) {
	pollToken := shared.RandHex(32)
	body, _ := json.Marshal(map[string]string{"provider": provider})
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", zcodeAPIBase+"/oauth/cli/init", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+pollToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.hc(nil).Do(httpReq)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "login init: " + err.Error()}}, nil
	}
	defer resp.Body.Close()
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data *struct {
			FlowID          string `json:"flow_id"`
			AuthorizeURL    string `json:"authorize_url"`
			ExpiresAt       int64  `json:"expires_at"`
			PollIntervalSec int    `json:"poll_interval_sec"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil || env.Code != 0 || env.Data == nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: fmt.Sprintf("login init failed: HTTP %d code=%d msg=%s", resp.StatusCode, env.Code, env.Msg)}}, nil
	}
	state, _ := json.Marshal(map[string]string{
		"provider": provider, "flow_id": env.Data.FlowID, "poll_token": pollToken,
	})
	return &pb.LoginResult{Next: &pb.LoginNextStep{
		Action: "open_url", Url: env.Data.AuthorizeURL,
		Prompt: map[string]string{
			"zh": "已打开授权页，请在浏览器完成登录授权，然后点击「我已完成授权」",
			"en": "Auth page opened; complete sign-in in the browser, then confirm below",
		},
		State: state,
		Wait:  false,
		Fields: []*pb.AuthField{{
			Name: "confirm", Label: map[string]string{"zh": "确认授权", "en": "Confirm"},
			Type: "confirm", Placeholder: "",
		}},
	}}, nil
}

// loginPoll 轮询授权结果：ready → 解析上游 API Key（coding-plan 双密钥）+ JWT 建档。
func (p *plugin) loginPoll(ctx context.Context, provider string, state []byte) (*pb.LoginResult, error) {
	var s struct {
		FlowID    string `json:"flow_id"`
		PollToken string `json:"poll_token"`
	}
	if json.Unmarshal(state, &s) != nil || s.FlowID == "" || s.PollToken == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "state 已失效，请重新发起登录"}}, nil
	}
	httpReq, _ := http.NewRequestWithContext(ctx, "GET", zcodeAPIBase+"/oauth/cli/poll/"+url.PathEscape(s.FlowID), nil)
	httpReq.Header.Set("Authorization", "Bearer "+s.PollToken)
	resp, err := p.hc(nil).Do(httpReq)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "login poll: " + err.Error()}}, nil
	}
	defer resp.Body.Close()
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data *struct {
			Status string `json:"status"`
			Token  string `json:"token"`
			User   struct {
				UserID string `json:"user_id"`
			} `json:"user"`
			Zai *struct {
				AccessToken string `json:"access_token"`
			} `json:"zai"`
			Bigmodel *struct {
				AccessToken string `json:"access_token"`
			} `json:"bigmodel"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "login poll decode: " + err.Error()}}, nil
	}
	if env.Code != 0 {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "授权失败: " + env.Msg}}, nil
	}
	switch env.Data.Status {
	case "pending":
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "wait",
			Prompt: map[string]string{
				"zh": "尚未完成授权，请先在浏览器完成登录后重试",
				"en": "Authorization pending; complete sign-in in the browser and retry",
			},
			State: state,
			Wait:  false,
			Fields: []*pb.AuthField{{
				Name: "confirm", Label: map[string]string{"zh": "确认授权", "en": "Confirm"},
				Type: "confirm", Placeholder: "",
			}},
		}}, nil
	case "failed":
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "授权失败，请重试"}}, nil
	}

	var accessToken string
	if provider == "zai" && env.Data.Zai != nil {
		accessToken = strings.TrimSpace(env.Data.Zai.AccessToken)
	} else if provider == "bigmodel" && env.Data.Bigmodel != nil {
		accessToken = strings.TrimSpace(env.Data.Bigmodel.AccessToken)
	}
	if accessToken == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "授权响应缺少 access_token"}}, nil
	}

	c := &credential{Plan: "coding-plan", Provider: provider, JWT: env.Data.Token, UserID: env.Data.User.UserID}
	if err := p.resolveCodingPlanKey(ctx, c, accessToken); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	if c.DeviceMid == "" {
		c.DeviceMid = shared.RandUUID()
	}
	if err := p.verifyCredential(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "凭据校验失败: " + err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	name := "zcode-" + provider
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}},
	}, nil
}

// verifyCredential 能列模型即有效。
func (p *plugin) verifyCredential(ctx context.Context, c *credential) error {
	_, err := p.listModels(ctx, c)
	return err
}

// ---------- system 数据 ----------

// systemBlocks 读内嵌 system_prompt.json（随包分发，缓存）。
func (p *plugin) systemBlocks() (map[string]json.RawMessage, error) {
	p.mu.Lock()
	data, err := p.systemData, p.systemErr
	p.mu.Unlock()
	if data != nil || err != nil {
		return data, err
	}
	raw, readErr := os.ReadFile(filepath.Join(pluginDir(), systemPromptFile))
	if readErr != nil {
		readErr = fmt.Errorf("read %s: %w", systemPromptFile, readErr)
		p.mu.Lock()
		p.systemErr = readErr
		p.mu.Unlock()
		return nil, readErr
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		p.mu.Lock()
		p.systemErr = err
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Lock()
	p.systemData = m
	p.mu.Unlock()
	return m, nil
}

// pluginDir 插件二进制所在目录（system_prompt.json 随包同目录分发）。
func pluginDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
