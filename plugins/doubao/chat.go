// chat.go — 对话入口 + SSE 事件解析状态机（思考/正文/工具增量；HTTP 见 upstream.go）。
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// Chat 上游只取信封多轮拼成的单 prompt（客户端协议为单轮请求），逐事件转发 SSE 增量。
func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	model := shared.OrDefault(req.Model, "doubao")
	text := buildPrompt(req)
	if strings.TrimSpace(text) == "" {
		return stream.Send(shared.Failed(400, "messages 不能为空"))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: model},
	}}); err != nil {
		return err
	}

	s := &chunkState{bufferTools: len(req.GetTools()) > 0}
	httpErr := p.chatCompletion(cred, req, model, func(name string, data []byte) error {
		done, err := p.onSSEChunk(s, model, name, data, stream.Send)
		if err != nil {
			return err
		}
		if !done {
			return errDone
		}
		return nil
	})
	if httpErr != nil && httpErr != errDone {
		return stream.Send(shared.Failed(mapErr(httpErr), httpErr.Error()))
	}

	finishReason := "stop"
	usage := &pb.Usage{}
	if s.riskCode != 0 {
		finishReason = "error"
		usage = nil
	} else if s.bufferTools {
		// 工具场景：流末解析缓冲正文，剩余文本先发，再逐个发工具调用
		calls, rest := extractToolCalls(s.contentBuf.String())
		if rest != "" {
			if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
				ContentDelta: &pb.ContentDelta{Text: rest},
			}}); err != nil {
				return err
			}
		}
		for _, c := range calls {
			if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{Id: c.Id, Name: c.Name, ArgumentsDelta: c.Arguments},
			}}); err != nil {
				return err
			}
		}
		if len(calls) > 0 {
			finishReason = "tool_calls"
		}
	}
	return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: finishReason, Usage: usage},
	}})
}

// errDone SSE 状态机结束信号（is_done / FIN）。
var errDone = fmt.Errorf("done")

// mapErr 错误 → 信封错误码（会话失效 401，风控 429，其余 502）。
func mapErr(err error) int32 {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "会话已过期") || strings.Contains(msg, "gateway-error"):
		return 401
	case strings.Contains(msg, "风控/限流"):
		return 429
	default:
		return 502
	}
}

// chunkState /chat/completion SSE 聚合状态：思考两段式（首块 10040 进入、次块退出）。
type chunkState struct {
	thinkingCount int
	inThinking    bool
	inUserID      bool
	riskCode      int64
	errMsg        string
	bufferTools   bool            // 工具场景缓冲正文，流末统一解析 <tool_call>
	contentBuf    strings.Builder // bufferTools 时的正文累积
}

// onSSEChunk 单个 SSE 事件 → 信封流事件（返回 false 表示结束流）。
// data 为事件 JSON；思考增量走 ReasoningDelta，正文增量走 ContentDelta。
func (p *plugin) onSSEChunk(s *chunkState, model string, name string, data []byte,
	send func(*pb.StreamEvent) error) (bool, error) {

	var obj map[string]interface{}
	if json.Unmarshal(data, &obj) != nil {
		return true, nil
	}

	// gateway-error（会话失效）
	if name == "gateway-error" {
		return false, fmt.Errorf("gateway-error: %v %s", obj["code"], str(obj, "message"))
	}

	// SSE_ACK：会话 id（暂不使用，跳过）
	if name == "SSE_ACK" {
		return true, nil
	}

	// STREAM_ERROR / 内联 error_code：风控码直接报错，其余记为业务错误（结束流）
	if name == "STREAM_ERROR" || hasKey(obj, "error_code") {
		code, _ := obj["error_code"].(float64)
		msg := str(obj, "error_msg")
		if int64(code) == riskCode1 || int64(code) == riskCode2 {
			s.riskCode = int64(code)
		} else if msg != "" {
			s.errMsg = msg
		}
		if msg != "" || s.riskCode != 0 {
			return false, errRisk(s)
		}
		return true, nil
	}

	// chunk_delta 紧凑格式 {"text": "..."}
	if t, ok := obj["text"].(string); ok && t != "" && !hasKey(obj, "error_code") {
		return p.emitText(s, t, send)
	}

	// content_block 数组（patch_op / content 两种容器）
	for _, cb := range iterBlocks(obj) {
		bt, _ := cb["block_type"].(float64)
		content, _ := cb["content"].(map[string]interface{})
		switch int(bt) {
		case 10040: // 思考两段式状态机
			s.thinkingCount++
			s.inThinking = s.thinkingCount == 1
		case 10000:
			tb, _ := content["text_block"].(map[string]interface{})
			if t, _ := tb["text"].(string); t != "" {
				if _, err := p.emitText(s, t, send); err != nil {
					return false, err
				}
			}
		}
	}

	// patch_op content 字符串（新 STREAM_CHUNK 格式）
	for _, patch := range sliceOf(obj["patch_op"]) {
		pv, _ := patch.(map[string]interface{})["patch_value"].(map[string]interface{})
		if pv == nil {
			continue
		}
		if cs, _ := pv["content"].(string); cs != "" {
			var co struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(cs), &co) == nil && co.Text != "" {
				if _, err := p.emitText(s, co.Text, send); err != nil {
					return false, err
				}
			}
		}
	}

	return true, nil
}

// emitText 发出文本增量（思考态 → ReasoningDelta，正文 → ContentDelta）。
func (p *plugin) emitText(s *chunkState, t string, send func(*pb.StreamEvent) error) (bool, error) {
	if s.inThinking {
		return true, send(&pb.StreamEvent{Event: &pb.StreamEvent_ReasoningDelta{
			ReasoningDelta: &pb.ReasoningDelta{Text: t},
		}})
	}
	if s.bufferTools {
		s.contentBuf.WriteString(t) // 缓冲，流末解析工具调用
		return true, nil
	}
	return true, send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
		ContentDelta: &pb.ContentDelta{Text: t},
	}})
}

// iterBlocks 展开事件中的 content_block 数组（patch_op.patch_value / content 两种容器）。
func iterBlocks(obj map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, patch := range sliceOf(obj["patch_op"]) {
		pm, _ := patch.(map[string]interface{})
		if pm == nil {
			continue
		}
		pv, _ := pm["patch_value"].(map[string]interface{})
		if pv == nil {
			continue
		}
		for _, cb := range sliceOf(pv["content_block"]) {
			if m, ok := cb.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
	}
	if dc, ok := obj["content"].(map[string]interface{}); ok {
		for _, cb := range sliceOf(dc["content_block"]) {
			if m, ok := cb.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// errRisk 风控/限流错误。
func errRisk(s *chunkState) error {
	if s.riskCode != 0 {
		return fmt.Errorf("风控/限流 (code=%d)，请稍后重试或更换会话", s.riskCode)
	}
	return fmt.Errorf("%s", s.errMsg)
}

func hasKey(obj map[string]interface{}, key string) bool {
	_, ok := obj[key]
	return ok
}

func str(obj map[string]interface{}, key string) string {
	v, _ := obj[key].(string)
	return v
}

func sliceOf(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}
