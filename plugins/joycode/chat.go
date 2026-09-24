// Chat 编排：OpenAI 兼容请求体 → color gateway 流式，SSE 交 openaiup 解析。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	body := openaiup.ChatBody(req)
	body["model"] = req.Model
	body["stream"] = true
	c.prepareBody(body)
	raw, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpointURL("chat"), bytes.NewReader(raw))
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	for k, v := range c.headers() {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.hc(c).Do(httpReq)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody := readBody(resp)
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

	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return sdk.ScanSSE(resp.Body, parser)
}
