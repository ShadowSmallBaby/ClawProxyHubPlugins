// todofor 插件 — todofor.ai 反代。
// 上游：api.todofor.ai/api/v1，REST 建 todo + 前端 WebSocket 订阅运行事件。
// 认证：静态 API Key（X-API-Key），登录时自动补齐 project_id / agent_id，无 token 刷新态。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	pluginName  = "todofor"
	settingsTTL = 30 * time.Second
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

var errEmptyModels = fmt.Errorf("models endpoint returned an empty list")

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

// baseURL 上游 REST 基地址（settings base_url 可覆盖，空用官方默认）。
func (p *plugin) baseURL() string {
	return shared.OrDefault(strings.TrimSpace(p.settingStr("base_url")), defaultBaseURL)
}

// ---------- 凭据 blob ----------

// credential 定义在 account.go（credFrom 解析 + proxyURL 注入）。

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

// upstreamClient 连接 15s / TLS 15s，流式对话整体不设超时（长回复合法，Chat 侧用 ctx 控时）。
// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "TodoFor", "en": "TodoFor"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"base_url": {
					"type": "string",
					"title": "上游地址",
					"description": "todofor.ai API 基地址，留空用官方默认 https://api.todofor.ai/api/v1",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "api_key", Label: map[string]string{"zh": "API 密钥", "en": "API Key"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{
						Name: "api_key", Label: map[string]string{"zh": "API Key", "en": "API Key"},
						Type: "text", Required: true, Placeholder: "todofor.ai 账号设置里的 API Key",
					},
					{
						Name: "project_id", Label: map[string]string{"zh": "项目 ID", "en": "Project ID"},
						Type: "text", Placeholder: "可选，留空自动选账号首个项目",
					},
					{
						Name: "agent_id", Label: map[string]string{"zh": "Agent ID", "en": "Agent ID"},
						Type: "text", Placeholder: "可选，留空自动选账号首个 Agent 模板",
					},
					{
						Name: "email", Label: map[string]string{"zh": "邮箱", "en": "Email"},
						Type: "text", Placeholder: "可选，作为账号展示名",
					},
				},
			},
		},
	}}, nil
}

// Login API Key 校验：补齐 project_id / agent_id（顺带验活），失败按 401 报出。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	c := &credential{
		APIKey:    strings.TrimSpace(req.Form["api_key"]),
		ProjectID: strings.TrimSpace(req.Form["project_id"]),
		AgentID:   strings.TrimSpace(req.Form["agent_id"]),
		Email:     strings.TrimSpace(req.Form["email"]),
	}
	if c.APIKey == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 API Key"}}, nil
	}
	cli := newClient(p.baseURL(), c.APIKey, p.hc(c))
	if err := p.resolveAccount(ctx, cli, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	profile := &pb.AccountProfile{DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{}}
	p.fillBalance(ctx, cli, profile)
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{Blob: blob, Profile: profile}, nil
}

// resolveAccount 补齐缺失的 project_id / agent_id；任一查询失败视作凭据无效（一并验活）。
func (p *plugin) resolveAccount(ctx context.Context, cli *client, c *credential) error {
	if c.ProjectID == "" {
		id, err := cli.firstProject(ctx)
		if err != nil {
			return fmt.Errorf("find project: %w", err)
		}
		c.ProjectID = id
	}
	if c.AgentID == "" {
		agent, err := cli.firstAgent(ctx)
		if err != nil {
			return fmt.Errorf("load agent: %w", err)
		}
		c.AgentID = agent.ID
	}
	return nil
}

// ---------- 刷新 / 资料 ----------

// Refresh 静态凭据无 token 刷新：补齐缺失字段 + 拉余额，API Key 原样保留。
// 遵循「静态密钥类插件刷新健壮性」：不因无刷新态清空凭据或改动展示名。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	cli := newClient(p.baseURL(), c.APIKey, p.hc(c))
	if c.ProjectID == "" || c.AgentID == "" {
		if err := p.resolveAccount(ctx, cli, c); err != nil {
			code := int32(503)
			if he, ok := err.(*httpError); ok && (he.StatusCode == 401 || he.StatusCode == 403) {
				code = 401
			}
			return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
		}
	}
	profile := &pb.AccountProfile{DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{}}
	p.fillBalance(ctx, cli, profile)
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: profile}, nil
}

// ---------- 工具 ----------

// shortID 取 id 前 8 位作展示缩写。
func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// formatUSD 余额 → 美元数字字符串（保留两位小数以内，去尾零）。
func formatUSD(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}
