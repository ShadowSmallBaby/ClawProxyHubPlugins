// chat.go — 对话：apiFormat=anthropic 直通 /v1/messages，其余转 openai 方言。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/anthropicup"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// anthropicModels apiFormat=anthropic 的模型缓存（Chat 时惰性填充）。
var anthropicModels sync.Map

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, orHint(err)))
	}

	// apiFormat=anthropic 的模型直通 /v1/messages，其余转 openai 方言
	if p.isAnthropicModel(ctx, cred, req.Model) {
		return p.chatAnthropic(req, stream, cred)
	}

	body := openaiup.ChatBody(req)
	body["model"] = shared.OrDefault(req.Model, "auto")

	resp, err := postJSON(ctx, p.hc(cred), serverBase+proxyPrefix+"/v1/chat/completions", p.authHeaders(cred), body)
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
	return sdk.ScanSSE(resp.Body, parser)
}

// chatAnthropic anthropic 方言直通：POST /api/proxy/v1/messages。
func (p *plugin) chatAnthropic(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer, cred *credential) error {
	ctx := stream.Context()
	body := anthropicup.ChatBody(req)

	resp, err := postJSON(ctx, p.hc(cred), serverBase+proxyPrefix+"/v1/messages", p.authHeaders(cred), body)
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
	parser := anthropicup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return sdk.ScanSSE(resp.Body, parser)
}

// isAnthropicModel 查模型方言（缓存 miss 时拉一次目录）。
func (p *plugin) isAnthropicModel(ctx context.Context, cred *credential, model string) bool {
	if v, ok := anthropicModels.Load(model); ok {
		return v.(bool)
	}
	list, err := p.ListModels(ctx, &pb.CredentialBlob{Blob: mustJSON(cred), Proxy: proxyPB(cred)})
	if err != nil {
		return false
	}
	for _, m := range list.Models {
		// 声明 SupportsTools=false 的即 anthropic 方言（ListModels 的映射约定）
		anthropicModels.Store(m.Id, !m.SupportsTools)
	}
	v, _ := anthropicModels.Load(model)
	is, _ := v.(bool)
	return is
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
