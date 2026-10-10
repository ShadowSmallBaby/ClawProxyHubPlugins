// chat.go — 对话：信封 → GetChatMessageRequest → Connect 服务端流 → StreamEvent（含响应帧解析）。
package devin

import (
	"errors"
	"io"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	devinproto "github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/devin/devinproto"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"google.golang.org/protobuf/proto"
)

// Chat 核心链路：组请求、开流、逐帧喂 parser。
func (p *plugin) Chat(req *pb.ChatRequest, ev pb.ClawPlugin_ChatServer) error {
	ctx := ev.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return ev.Send(shared.Failed(401, err.Error()))
	}
	if req.GetModel() == "" {
		return ev.Send(shared.Failed(400, "request has no model"))
	}
	chatReq, err := buildChatRequest(req, c.Token)
	if err != nil {
		return ev.Send(shared.Failed(400, err.Error()))
	}
	sr, err := nextStream(ctx, p.hc(c), c.Token, p.baseURL(), connectProtoPath+"GetChatMessage", mustMarshal(chatReq))
	if err != nil {
		return ev.Send(shared.Failed(shared.ErrorStatus(err), err.Error()))
	}
	defer sr.close()

	s := &stream{ev: ev, d: newDecoder(req.GetModel())}
	for {
		frame, err := sr.recv()
		if err != nil {
			return s.finish(err)
		}
		done, err := s.feed(frame)
		if err != nil {
			return ev.Send(shared.Failed(shared.ErrorStatus(err), err.Error()))
		}
		if done {
			return nil
		}
	}
}

// decoder 一次请求内的响应累计状态。
type decoder struct {
	model                          string
	hasStart, hasContent, finished bool
	stopReason                     string
	usage                          *devinproto.ExaCodeiumCommonPb_ModelUsageStats
	lastTool                       string
	toolName                       map[string]string
}

func newDecoder(model string) *decoder {
	return &decoder{model: model, toolName: map[string]string{}}
}

type stream struct {
	ev pb.ClawPlugin_ChatServer
	d  *decoder
}

// feed 解析一帧并回吐事件；返回是否结束。
func (s *stream) feed(frame []byte) (bool, error) {
	var resp devinproto.GetChatMessageResponse
	if err := proto.Unmarshal(frame, &resp); err != nil {
		return true, err
	}
	d := s.d
	if d.finished {
		return true, nil
	}
	if resp.GetActualModelUid() != "" {
		d.model = resp.GetActualModelUid()
	}
	if u := resp.GetUsage(); u != nil {
		if d.usage == nil {
			d.usage = &devinproto.ExaCodeiumCommonPb_ModelUsageStats{}
		}
		proto.Merge(d.usage, u)
	}
	if !d.hasStart {
		d.hasStart = true
		if err := s.ev.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
			MessageStart: &pb.MessageStart{Model: d.model},
		}}); err != nil {
			return true, err
		}
	}
	if resp.GetDeltaThinking() != "" || resp.GetDeltaSignature() != "" {
		d.hasContent = true
		if err := s.ev.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ReasoningDelta{
			ReasoningDelta: &pb.ReasoningDelta{Text: resp.GetDeltaThinking(), Signature: resp.GetDeltaSignature()},
		}}); err != nil {
			return true, err
		}
	}
	// text 增量
	if resp.GetDeltaText() != "" {
		d.hasContent = true
		if err := s.ev.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
			ContentDelta: &pb.ContentDelta{Text: resp.GetDeltaText()},
		}}); err != nil {
			return true, err
		}
	}
	// tool 增量
	for _, delta := range resp.GetDeltaToolCalls() {
		d.hasContent = true
		id := delta.GetId()
		if id == "" {
			id = d.lastTool
		}
		if id == "" {
			continue
		}
		d.lastTool = id
		if delta.GetName() != "" {
			d.toolName[id] = delta.GetName()
		}
		if err := s.ev.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{
			ToolCallDelta: &pb.ToolCallDelta{
				Id: id, Name: d.toolName[id], ArgumentsDelta: delta.GetArgumentsJson(),
			},
		}}); err != nil {
			return true, err
		}
	}
	// stop reason
	if sr := resp.GetStopReason(); sr != devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_UNSPECIFIED {
		d.stopReason = mapStopReason(sr)
	}
	return false, nil
}

// finish 等待流结束，保留尾部用量与 Connect 错误。
func (s *stream) finish(err error) error {
	d := s.d
	if d.finished {
		return nil
	}
	d.finished = true
	if err != nil && !errors.Is(err, io.EOF) {
		return s.ev.Send(shared.Failed(shared.ErrorStatus(err), err.Error()))
	}
	if !d.hasContent && d.stopReason == "" {
		return s.ev.Send(shared.Failed(502, "devin stream ended without content"))
	}
	if d.stopReason == "error" {
		return s.ev.Send(shared.Failed(502, "Devin stopped with an error"))
	}
	return s.ev.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: shared.OrDefault(d.stopReason, "stop"), Usage: usageOf(&devinproto.GetChatMessageResponse{Usage: d.usage})},
	}})
}

// usageOf 响应 usage → Anthropic 语义四字段（input 不含缓存）。
func usageOf(resp *devinproto.GetChatMessageResponse) *pb.Usage {
	u := resp.GetUsage()
	if u == nil {
		return nil
	}
	return &pb.Usage{
		InputTokens:         int64(u.GetInputTokens()),
		OutputTokens:        int64(u.GetOutputTokens()),
		CachedTokens:        int64(u.GetCacheReadTokens()),
		CacheCreationTokens: int64(u.GetCacheWriteTokens()),
	}
}

// mapStopReason Devin stop_reason → 信封 finish_reason。
func mapStopReason(reason devinproto.ExaCodeiumCommonPb_StopReason) string {
	switch reason {
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_MAX_TOKENS,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_INCOMPLETE,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_PARTIAL:
		return "max_tokens"
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL:
		return "tool_use"
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_ERROR:
		return "error"
	default:
		return "stop"
	}
}
