// Warp 插件：设备登录、凭据管理和对话转发。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	pluginName     = "warp"
	warpAPIBaseURL = "https://app.warp.dev"
	warpGraphQLV2  = warpAPIBaseURL + "/graphql/v2"
	warpAIURL      = warpAPIBaseURL + "/ai/multi-agent"
	warpLoginURL   = warpAPIBaseURL + "/client/login"

	warpDeviceAuthURL  = warpAPIBaseURL + "/api/v1/oauth/device/auth"
	warpDeviceTokenURL = warpAPIBaseURL + "/api/v1/oauth/token"

	firebaseTokenURL       = "https://securetoken.googleapis.com/v1/token"
	firebaseCustomTokenURL = "https://identitytoolkit.googleapis.com/v1/accounts:signInWithCustomToken"

	warpAgentCLIClientID = "warp-agent-cli"
	warpAppClientID      = "warp-app"
	clientVersion        = "v0.2026.08.19.08.15.stable_01"

	defaultModel      = "auto-open"
	cliAgentModel     = "cli-agent-auto"
	computerUseModel  = "computer-use-agent-auto"
	settingsTTL       = 30 * time.Second
	loginValidity     = 5 * time.Minute
	tokenRefreshAhead = 5 * time.Minute
	requestTimeout    = 10 * time.Minute
	modelsTTL         = 10 * time.Minute
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host
	hc   *http.Client

	settingsMu   sync.Mutex
	settingsJSON []byte
	settingsAt   time.Time

	modelsMu    sync.Mutex
	modelsCache map[string]cachedModels // refresh_token + proxy → 模型目录缓存
}

type cachedModels struct {
	models []*pb.ModelInfo
	at     time.Time
}

func (p *plugin) SetHost(host *sdk.Host) {
	p.host = host
	p.hc = &http.Client{Timeout: requestTimeout}
	p.modelsCache = make(map[string]cachedModels)
}

// settingStr 读插件设置（30s 缓存）。
func (p *plugin) settingStr(key string) string {
	p.settingsMu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < settingsTTL
	raw := p.settingsJSON
	p.settingsMu.Unlock()
	if !fresh {
		raw = []byte("{}")
		if p.host != nil {
			if r := p.host.Settings(pluginName); r != nil {
				raw = r
			}
		}
		p.settingsMu.Lock()
		p.settingsJSON, p.settingsAt = raw, time.Now()
		p.settingsMu.Unlock()
	}
	var cfg map[string]string
	if json.Unmarshal(raw, &cfg) == nil {
		return strings.TrimSpace(cfg[key])
	}
	return ""
}

// firebaseKey 从插件设置读 Firebase API key。
func (p *plugin) firebaseKey() (string, error) {
	key := p.settingStr("firebase_api_key")
	if key == "" {
		return "", fmt.Errorf("插件设置缺少 firebase_api_key（Google AIza key）")
	}
	return key, nil
}

func firebaseURL(endpoint, apiKey string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	q := u.Query()
	q.Set("key", apiKey)
	u.RawQuery = q.Encode()
	return u.String()
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
		Label:           map[string]string{"zh": "Warp", "en": "Warp"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "account"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"firebase_api_key": {
					"type": "string",
					"title": "Firebase API Key",
					"description": "Warp 登录 / 刷新所需的 Google AIza key",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "device", Label: map[string]string{"zh": "设备授权登录", "en": "Device Login"},
				Callback: "auto_wait", // 本机自动轮询完成，远端由前端继续轮询
			},
		},
	}}, nil
}

// ---------- HTTP ----------

// hcFor 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hcFor(cred *credential) *http.Client {
	if cred != nil && cred.proxyURL != "" {
		return sdk.UpstreamClient(cred.proxyURL)
	}
	if p.hc != nil {
		return p.hc
	}
	return sdk.UpstreamClient("")
}

// warpHeaders 设置客户端标识与平台请求头。
func warpHeaders(req *http.Request) {
	req.Header.Set("X-Warp-Client-ID", warpAppClientID)
	req.Header.Del("X-Warp-Client-Version")
	if cat := osCategory(); cat != "" {
		req.Header.Set("X-Warp-OS-Category", cat)
		req.Header.Set("X-Warp-OS-Name", cat)
	}
	req.Header.Set("User-Agent", "")
}

func osCategory() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}
