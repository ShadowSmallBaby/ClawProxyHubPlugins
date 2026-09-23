// opencode 插件 — OpenCode Zen 反代。
// 上游：opencode.ai/zen（Zen）与 /zen/go（Zen Go）双池，OpenAI/Anthropic 原生端点。
// 登录：Zen / Go API Key 直填（凭据 blob 区分 tier）；免费模型可走匿名 public key。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/anthropicup"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/responsesup"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	pluginName  = "opencode"
	zenBase     = "https://opencode.ai/zen"
	goBase      = "https://opencode.ai/zen/go"
	anonZenKey  = "public"
	settingsTTL = 30 * time.Second
	// defaultUserAgent 真实 CLI 的 UA 形态（ai-sdk 运行时拼接），免费池按此校验来源。
	defaultUserAgent = "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"
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
	modelsMu     sync.Mutex
	zenModels    map[string]bool
	goModels     map[string]bool
	modelsAt     time.Time
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

// preferGo 模型同时存在于 Zen 与 Go 时的认证顺序偏好（默认 go）。
func (p *plugin) preferGo() bool { return p.settingStr("prefer") != "zen" }

// userAgentStr 请求头 User-Agent（用户可配置；空 = 真实 CLI 形态，免费池按 UA 校验来源）。
func (p *plugin) userAgentStr() string {
	if v := p.settingStr("user_agent"); v != "" {
		return v
	}
	return defaultUserAgent
}

// ---------- 凭据 blob ----------

// credential Zen / Go tier 的 API Key。
type credential struct {
	Tier string `json:"tier"` // zen / go
	Key  string `json:"key"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{Tier: "zen"}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.Key == "" {
		return nil, fmt.Errorf("credential missing key")
	}
	if c.Tier != "zen" && c.Tier != "go" {
		c.Tier = "zen"
	}
	c.proxyURL = shared.ProxyURL(blob.GetProxy())
	return c, nil
}

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
	c := shared.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
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
		Label:           map[string]string{"zh": "OpenCode", "en": "OpenCode"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"prefer": {
					"type": "string",
					"title": "首选通道",
					"description": "模型同时存在于 Zen 与 Go 时的认证顺序",
					"default": "go",
					"oneOf": [
						{"const": "go", "title": "Go 优先"},
						{"const": "zen", "title": "Zen 优先"}
					]
				},
				"user_agent": {
					"type": "string",
					"title": "User-Agent",
					"description": "请求头 User-Agent 伪装值，留空使用内置 CLI 形态",
					"default": ""
				}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "api_key", Label: map[string]string{"zh": "Zen API Key", "en": "Zen API Key"},
				Fields: []*pb.AuthField{{
					Name: "key", Label: map[string]string{"zh": "API Key", "en": "API Key"},
					Type: "password", Required: true, Placeholder: "sk-...",
				}},
			},
			{
				Id: "go_key", Label: map[string]string{"zh": "Zen Go API Key", "en": "Zen Go API Key"},
				Fields: []*pb.AuthField{{
					Name: "key", Label: map[string]string{"zh": "API Key", "en": "API Key"},
					Type: "password", Required: true, Placeholder: "sk-...",
				}},
			},
		},
	}}, nil
}

// Login 两种方式：api_key（Zen tier）/ go_key（Go tier）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	tier := "zen"
	if req.MethodId == "go_key" {
		tier = "go"
	} else if req.MethodId != "api_key" {
		return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
	}
	key := strings.TrimSpace(req.Form["key"])
	if key == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 API Key"}}, nil
	}
	c := &credential{Tier: tier, Key: key}

	// 校验：能列模型即有效（匿名 public key 对免费模型也放行）
	if _, err := p.listModels(ctx, c, tierBase(tier), key); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "API Key 校验失败: " + err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	name := "opencode-" + tier
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}},
	}, nil
}

func tierBase(tier string) string {
	if tier == "go" {
		return goBase
	}
	return zenBase
}

// ---------- 模型目录 ----------

// listModels 拉上游 /v1/models 模型 id 列表。
func (p *plugin) listModels(ctx context.Context, cred *credential, base, key string) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", p.userAgentStr())
	req.Header.Set("x-opencode-client", "cli")
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models endpoint returned an empty list")
	}
	return out, nil
}

// refreshCatalog 双池目录并发拉取（缓存 5 分钟）；失败沿用旧目录。
func (p *plugin) refreshCatalog(ctx context.Context) {
	p.modelsMu.Lock()
	if !p.modelsAt.IsZero() && time.Since(p.modelsAt) < 5*time.Minute {
		p.modelsMu.Unlock()
		return
	}
	p.modelsMu.Unlock()

	zen, goModels := p.fetchTier(ctx, zenBase), p.fetchTier(ctx, goBase)
	p.modelsMu.Lock()
	if zen != nil {
		p.zenModels = zen
	}
	if goModels != nil {
		p.goModels = goModels
	}
	p.modelsAt = time.Now()
	p.modelsMu.Unlock()
}

// fetchTier 拉 tier 目录，失败返回 nil（沿用旧值）。
func (p *plugin) fetchTier(ctx context.Context, base string) map[string]bool {
	// 先用匿名 public 探测（免费模型）；失败再用无凭据直连兜底
	ids, err := p.listModels(ctx, nil, base, anonZenKey)
	if err != nil {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// nil2Time 零值时间判断。
func nil2Time(t time.Time) bool { return !t.IsZero() }

// inferProtocol 模型名推断上游原生协议
func inferProtocol(model string) string {
	m := strings.ToLower(model)
	if m == "deepseek-v4-flash-free" {
		return "chat"
	}
	for _, prefix := range []string{"claude-", "qwen"} {
		if strings.HasPrefix(m, prefix) {
			return "anthropic"
		}
	}
	for _, prefix := range []string{"gpt-", "o1", "o3", "o4", "grok-", "muse-"} {
		if strings.HasPrefix(m, prefix) {
			return "responses"
		}
	}
	return "chat"
}

// isFreeModel 免费模型判定：名称含 free（大小写不敏感）。
func isFreeModel(model string) bool { return strings.Contains(strings.ToLower(model), "free") }

// GetProfile 基本档案（Zen 无余额接口）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return &pb.AccountProfile{DisplayName: "opencode-" + c.Tier, Healthy: true, Quota: map[string]string{}}, nil
}

// ListModels 双池并集；匿名场景只透出免费模型。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	p.refreshCatalog(ctx)
	p.modelsMu.Lock()
	seen := make(map[string]bool, len(p.zenModels)+len(p.goModels))
	for m := range p.zenModels {
		seen[m] = true
	}
	for m := range p.goModels {
		seen[m] = true
	}
	p.modelsMu.Unlock()

	cred, _ := credFrom(credBlob)
	var models []*pb.ModelInfo
	for id := range seen {
		// 无凭据（匿名模式）：只透出免费模型
		if (cred == nil || cred.Key == anonZenKey) && !isFreeModel(id) {
			continue
		}
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": id},
			SupportsTools: true, SupportsStream: true,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("模型目录为空：上游目录刷新失败或无可用模型")
	}
	return &pb.ModelList{Models: models}, nil
}

// ---------- Chat ----------

// chatRoute 单次请求的路由决策。
type chatRoute struct {
	base string
	key  string
	// fallback 非 nil 时首选失败后回退（tier 池 + key）
	fallback *chatRoute
}

// routeFor 凭据 + 模型 → 路由。匿名凭据先走 Zen public，失败回退凭据 key。
func (p *plugin) routeFor(ctx context.Context, cred *credential, model string) chatRoute {
	protocol := inferProtocol(model)
	_ = protocol
	if cred == nil || cred.Key == "" {
		return chatRoute{base: zenBase, key: anonZenKey}
	}
	base := tierBase(cred.Tier)
	if isFreeModel(model) {
		// 免费模型先走匿名 Zen，失败回退凭据 key
		return chatRoute{base: zenBase, key: anonZenKey, fallback: &chatRoute{base: base, key: cred.Key}}
	}
	return chatRoute{base: base, key: cred.Key}
}

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	p.refreshCatalog(ctx)

	route := p.routeFor(ctx, cred, req.Model)
	resp, err := p.chatOnce(ctx, cred, route, req)
	if err != nil && route.fallback != nil {
		// 首选（匿名）失败回退凭据 key 重试一次
		resp, err = p.chatOnce(ctx, cred, *route.fallback, req)
	}
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			code = 401
		case resp.StatusCode == 429:
			code = 429
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}

	// 按模型原生协议选解析器
	var parser interface {
		Feed(string)
		Finish()
		FinishWithError(int32, string)
	}
	switch inferProtocol(req.Model) {
	case "anthropic":
		parser = anthropicup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	case "responses":
		parser = responsesup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	default:
		parser = openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	}
	return shared.ScanSSE(resp.Body, parser)
}

// chatOnce 组请求体 + 发上游（按模型原生协议选请求体与端点；cred 决定出站代理）。
func (p *plugin) chatOnce(ctx context.Context, cred *credential, route chatRoute, req *pb.ChatRequest) (*http.Response, error) {
	protocol := inferProtocol(req.Model)
	var path string
	var body map[string]interface{}
	switch protocol {
	case "anthropic":
		path = "/v1/messages"
		body = anthropicup.ChatBody(req)
	case "responses":
		path = "/v1/responses"
		body = responsesup.ChatBody(req)
	default:
		path = "/v1/chat/completions"
		body = openaiup.ChatBody(req)
	}
	body["model"] = req.Model
	body["stream"] = true
	if route.key == anonZenKey {
		ensureFreeTierTools(body, protocol)
	}
	raw, _ := json.Marshal(body)

	endpoint := strings.TrimRight(route.base, "/") + path
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", p.userAgentStr())
	httpReq.Header.Set("x-opencode-client", "cli")
	httpReq.Header.Set("x-opencode-project", "global")
	httpReq.Header.Set("x-opencode-request", opencodeID("msg", false))
	httpReq.Header.Set("x-opencode-session", opencodeID("ses", true))
	if protocol == "anthropic" {
		httpReq.Header.Set("x-api-key", route.key)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+route.key)
	}
	return p.hc(cred).Do(httpReq)
}

// ensureFreeTierTools 免费池要求 tools 含 bash 且不少于 2 个，缺则补占位工具（不改动用户已有工具）。
func ensureFreeTierTools(body map[string]interface{}, protocol string) {
	// 两套 ChatBody 的 tools 切片类型不同，统一成 []interface{}
	var tools []interface{}
	switch v := body["tools"].(type) {
	case []interface{}:
		tools = v
	case []map[string]interface{}:
		for _, t := range v {
			tools = append(tools, t)
		}
	}
	hasBash := false
	for _, t := range tools {
		if m, ok := t.(map[string]interface{}); ok && toolName(m, protocol) == "bash" {
			hasBash = true
			break
		}
	}
	if hasBash && len(tools) >= 2 {
		return
	}
	if !hasBash {
		tools = append(tools, placeholderTool("bash", protocol))
	}
	if len(tools) < 2 {
		tools = append(tools, placeholderTool("read", protocol))
	}
	body["tools"] = tools
}

// toolName 按协议取工具名（chat 嵌在 function 下，anthropic / responses 在顶层）。
func toolName(t map[string]interface{}, protocol string) string {
	if protocol == "chat" {
		if fn, ok := t["function"].(map[string]interface{}); ok {
			n, _ := fn["name"].(string)
			return n
		}
		return ""
	}
	n, _ := t["name"].(string)
	return n
}

// placeholderTool 最小占位工具（空 schema，描述提示模型勿调用）。
func placeholderTool(name, protocol string) map[string]interface{} {
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	desc := "Unavailable in this session. Do not call."
	switch protocol {
	case "anthropic":
		return map[string]interface{}{"name": name, "description": desc, "input_schema": schema}
	case "responses":
		return map[string]interface{}{"type": "function", "name": name, "description": desc, "parameters": schema, "strict": false}
	}
	return map[string]interface{}{
		"type":     "function",
		"function": map[string]interface{}{"name": name, "description": desc, "parameters": schema},
	}
}

// bufioScanner 占位：Go 惯用 bufio.Scanner，这里统一走 Read 循环。
func bufioScanner(io.Reader) struct{} { return struct{}{} }

// ---------- 工具 ----------

// idCounter 同毫秒内的单调计数（timestamp*0x1000+counter 的 id 生成算法）。
var idCounter uint64

// opencodeID 生成 opencode 形态 id：prefix_ + 12位时间戳hex + 14位base62。
// desc 为按位取反的降序 id（session 用降序，request 用升序）；免费池按此格式识别客户端。
func opencodeID(prefix string, desc bool) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	v := uint64(time.Now().UnixMilli())*0x1000 + (atomic.AddUint64(&idCounter, 1) & 0xfff)
	if desc {
		v = ^v
	}
	timePart := fmt.Sprintf("%012x", v&0xffffffffffff) // 低 48bit → 12 hex
	b := make([]byte, 14)
	rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%62]
	}
	return prefix + "_" + timePart + string(b)
}
