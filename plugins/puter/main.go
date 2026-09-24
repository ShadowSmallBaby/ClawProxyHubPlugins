// puter 插件 — Puter 驱动调用反代（api.puter.com/drivers/call）。
// 认证：粘贴浏览器 popup auth_token，whoami 校验 + metering 月用量。
// 对话：NDJSON 流（text/reasoning/tool_use/usage/error 事件），Content-Type: text/plain;actually=json。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	pluginName     = "puter"
	puterAPIURL    = "https://api.puter.com/drivers/call"
	puterWhoamiURL = "https://api.puter.com/whoami"
	puterUsageURL  = "https://api.puter.com/metering/usage"
	puterModelsURL = "https://api.puter.com/puterai/chat/models/details"
	defaultIface   = "puter-chat-completion"
	defaultMethod  = "complete"
	requestTimeout = 10 * time.Minute
	modelsFetchTTL = 10 * time.Minute
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host
	hc   *http.Client

	modelsMu      sync.Mutex
	modelsCache   []*pb.ModelInfo
	modelsCacheAt time.Time
}

func (p *plugin) SetHost(host *sdk.Host) {
	p.host = host
	p.hc = &http.Client{Timeout: requestTimeout}
}

// ---------- 凭据 ----------

// credential 凭据 blob：auth_token 是唯一凭据（浏览器 popup grant）。
type credential struct {
	AuthToken string `json:"auth_token"`

	instanceID int64  `json:"-"`
	accountID  string `json:"-"`
	proxyURL   string `json:"-"`
}

func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	var c credential
	if err := json.Unmarshal(blob.GetBlob(), &c); err != nil {
		return nil, fmt.Errorf("invalid credential: %w", err)
	}
	if strings.TrimSpace(c.AuthToken) == "" {
		return nil, fmt.Errorf("credential missing auth_token")
	}
	c.instanceID = blob.GetInstanceId()
	c.accountID = blob.GetAccountId()
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return &c, nil
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
		Label:           map[string]string{"zh": "Puter", "en": "Puter"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema:  `{"type": "object", "properties": {}}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "auth_token", Label: map[string]string{"zh": "Popup 令牌", "en": "Popup Token"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "auth_token", Label: map[string]string{"zh": "auth_token", "en": "auth_token"},
					Type: "password", Required: true, Placeholder: "浏览器 Puter 弹窗授权后的 auth_token",
				}},
			},
		},
	}}, nil
}

// ---------- HTTP ----------

// hcFor 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hcFor(cred *credential) *http.Client {
	if p.hc == nil {
		p.hc = &http.Client{Timeout: requestTimeout}
	}
	if cred == nil || cred.proxyURL == "" {
		return p.hc
	}
	u, err := url.Parse(cred.proxyURL)
	if err != nil {
		return p.hc
	}
	return &http.Client{Timeout: requestTimeout, Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}
