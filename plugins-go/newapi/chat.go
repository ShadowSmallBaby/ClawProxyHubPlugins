// chat.go — 按入口协议转发，Responses 可按实例切换为 Chat 兼容模式。
package newapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/anthropicup"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/responsesup"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	if cred.APIKey == "" {
		// 会话模式尚未取得密钥明文：按 401 报出，核心会触发 Refresh 重取后重试
		return stream.Send(shared.Failed(401, "该账号尚未取得 API 密钥（站点未返回明文）"))
	}
	site, err := p.site(cred.instanceID)
	if err != nil {
		return stream.Send(shared.Failed(500, err.Error()))
	}

	path, body, parser := upstreamChat(req, site.ResponsesMode, func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	body["model"] = req.Model
	body["stream"] = true
	raw, _ := json.Marshal(body)

	resp, err := p.do(ctx, cred, "POST", site.BaseURL+path, gatewayHeaders(cred, req.Extra[sdk.ExtraClientUserAgent]), bytes.NewReader(raw))
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		switch resp.StatusCode {
		case 401, 403:
			code = 401
		case 402, 429:
			code = int32(resp.StatusCode)
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	return sdk.ScanSSE(resp.Body, parser)
}

// upstreamChat 同步选择路径、请求构造器和响应解析器，避免协议错配。
func upstreamChat(req *pb.ChatRequest, responsesMode string, emit func(*pb.StreamEvent)) (string, map[string]interface{}, shared.SSEParser) {
	switch {
	case req.Source == "messages":
		return "/v1/messages", anthropicup.ChatBody(req), anthropicup.NewParser(emit)
	case req.Source == "responses" && responsesMode != "chat":
		return "/v1/responses", responsesup.ChatBody(req), responsesup.NewParser(emit)
	default:
		return "/v1/chat/completions", openaiup.ChatBody(req), openaiup.NewParser(emit)
	}
}
