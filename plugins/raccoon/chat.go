// chat.go — 对话：POST /api/web/llm/v2/chat/completions（标准 OpenAI 兼容 SSE）。
package main

import (
	"fmt"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	body := openaiup.ChatBody(req)
	body["model"] = shared.OrDefault(req.Model, "sn-sensenova-6-8-flash")
	raw := mustJSON2(body)

	resp, err := p.postUpstream(ctx, cred, apiBase+llmPrefix+"/chat/completions", raw)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw2 := shared.ReadLimitedResp(resp, 8192)
		code := int32(resp.StatusCode)
		if resp.StatusCode == 401 {
			code = 401
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw2), 300))))
	}

	// 标准 OpenAI 兼容 + 标准 SSE
	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return sdk.ScanSSE(resp.Body, parser)
}
