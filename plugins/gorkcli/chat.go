// Chat 编排：统一信封 → xAI Responses 请求，SSE 交 responsesup 解析。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/responsesup"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	if c.expiringSoon() && c.RefreshToken != "" {
		_ = p.doRefresh(ctx, c) // best-effort：核心随后经 Refresh 持久化新 token
	}

	body := responsesup.ChatBody(req)
	body["model"] = req.Model
	body["stream"] = true
	if _, ok := body["instructions"]; !ok {
		body["instructions"] = "" // CPA 缓存路径要求该字段存在
	}
	convID := ""
	for _, k := range []string{"prompt_cache_key", "user"} {
		if v := strings.TrimSpace(req.Extra[k]); v != "" {
			convID = v
			break
		}
	}
	if convID != "" {
		if _, ok := body["prompt_cache_key"]; !ok {
			body["prompt_cache_key"] = convID
		}
	}
	raw, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL()+"/responses", bytes.NewReader(raw))
	if err != nil {
		return stream.Send(shared.Failed(500, err.Error()))
	}
	for k, v := range p.grokHeaders(c.AccessToken) {
		httpReq.Header.Set(k, v)
	}
	if convID != "" {
		httpReq.Header.Set("x-grok-conv-id", convID)
	}
	resp, err := p.hc(c).Do(httpReq)
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
		case 429:
			code = 429
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}
	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	parser := responsesup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return sdk.ScanSSE(resp.Body, parser)
}
