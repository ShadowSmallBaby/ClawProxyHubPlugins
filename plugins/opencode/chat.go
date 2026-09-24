// chat.go — 对话：按模型原生协议选请求体/端点/解析器，双池路由与免费池工具补全。
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
	"sync/atomic"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/anthropicup"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/responsesup"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// chatRoute 单次请求的路由决策。
type chatRoute struct {
	base string
	key  string
	// fallback 非 nil 时首选失败后回退（tier 池 + key）
	fallback *chatRoute
}

// routeFor 凭据 + 模型 → 路由。匿名凭据先走 Zen public，失败回退凭据 key。
func (p *plugin) routeFor(ctx context.Context, cred *credential, model string) chatRoute {
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
	return sdk.ScanSSE(resp.Body, parser)
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
