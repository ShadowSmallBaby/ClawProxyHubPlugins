// 对接 CNB OpenAI SSE。
package cnb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

func chatBody(r *pb.ChatRequest) []byte {
	body := openaiup.ChatBody(r)
	for _, k := range []string{"reasoning_effort", "reasoning", "enable_thinking", "thinking"} {
		delete(body, k)
	}
	raw, _ := json.Marshal(body)
	return raw
}
func (p *plugin) Chat(r *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	return p.chat(r, stream, p.site(r.GetCredential().GetInstanceId()))
}

func (p *plugin) chat(r *pb.ChatRequest, stream pb.ClawPlugin_ChatServer, s site) error {
	c, err := credFrom(r.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	endpoint, err := s.endpoint()
	if err != nil {
		return stream.Send(shared.Failed(400, err.Error()))
	}
	ctx, cancel := context.WithTimeout(stream.Context(), 5*time.Minute)
	defer cancel()
	client := sdk.UpstreamClient(sdk.ProxyURL(r.GetCredential().GetProxy()))
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if err = stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{MessageStart: &pb.MessageStart{Model: r.Model}}}); err != nil {
		return err
	}
	var sendErr error
	parser := openaiup.NewParser(func(ev *pb.StreamEvent) {
		if sendErr == nil {
			sendErr = stream.Send(ev)
			if sendErr != nil {
				cancel()
			}
		}
	})
	response, err := p.host.StreamSSE(ctx, sdk.HTTPRequest{URL: endpoint, Headers: map[string]string{"Authorization": "Bearer " + c.Token, "Content-Type": "application/json"}, Body: chatBody(r)}, client, parser)
	if sendErr != nil {
		return sendErr
	}
	if err != nil {
		parser.FinishWithError(502, "CNB upstream request interrupted")
	} else if response.Status != http.StatusOK {
		parser.FinishWithError(int32(response.Status), fmt.Sprintf("CNB upstream HTTP %d", response.Status))
	}
	return sendErr
}
