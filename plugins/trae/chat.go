// chat.go — 对话：POST /api/agent/v3/llm_utils_chat（OpenAI 方言 + 标准 SSE 流）。
//
// ⚠️ 模型路由：SOLO 协议模型只在列出它的通道里可调用（跨通道发 4001），
// ListModels 时把 模型 → function 记进 modelChannel，Chat 时按通道发。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// modelChannel 模型 → 提供它的聊天通道（ListModels 填充，Chat 消费）。
var modelChannel sync.Map

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	body := openaiup.ChatBody(req)
	body["model"] = shared.OrDefault(req.Model, "glm-5-turbo")
	raw, _ := json.Marshal(body)

	// 模型只在列出它的通道里可调用：按 ListModels 记录的通道发，缺省走默认通道
	channel := defaultChannel
	if v, ok := modelChannel.Load(req.Model); ok {
		if s, ok := v.(string); ok && s != "" {
			channel = s
		}
	}
	_ = channel // llm_utils_chat 走统一入口，通道差异由上游 model 字段承载

	resp, err := p.postChat(ctx, cred, raw)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw2 := shared.ReadLimitedResp(resp, 8192)
		code := int32(502)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			code = 401
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw2), 300))))
	}

	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return sdk.ScanSSE(resp.Body, parser)
}

// postChat 对话请求（SOLO 信道头 + 流式 Accept）。
func (p *plugin) postChat(ctx context.Context, cred *credential, body []byte) (*http.Response, error) {
	req, err := httpNewReq(ctx, "POST", agentHost+pathChat, body)
	if err != nil {
		return nil, err
	}
	for k, v := range p.soloHeaders(cred, true) {
		req.Header.Set(k, v)
	}
	return p.hc(cred).Do(req)
}

