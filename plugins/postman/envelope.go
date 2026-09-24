// 信封消息提取 + Postman 私有 SSE 解析。
// Postman 每行 `data: {json}`，eventType 分派：textChunk / thinkingChunk / conversation /
// toolCall(Chunk) / usage / [DONE]。
package main

import (
	"encoding/json"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// messageText 信封消息 → 纯文本（Text 优先，退回 Parts 的 text / thinking）。
func messageText(m *pb.EnvelopeMessage) string {
	if strings.TrimSpace(m.GetText()) != "" {
		return m.GetText()
	}
	var b strings.Builder
	for _, part := range m.GetParts() {
		if (part.GetType() == "text" || part.GetType() == "thinking") && part.GetText() != "" {
			b.WriteString(part.GetText())
		}
	}
	return b.String()
}

// lastUserText 最后一条 user 消息文本（Postman 只吃最后一轮 query）。
func lastUserText(msgs []*pb.EnvelopeMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].GetRole() == "user" {
			if t := messageText(msgs[i]); strings.TrimSpace(t) != "" {
				return t
			}
		}
	}
	return ""
}

// systemContext 所有 system 消息文本拼接（首轮注入 query 前缀，模型据此认清 agent 身份）。
func systemContext(msgs []*pb.EnvelopeMessage) string {
	var parts []string
	for _, m := range msgs {
		if m.GetRole() == "system" {
			if t := messageText(m); strings.TrimSpace(t) != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// toolResult 末尾工具结果消息。
type toolResult struct {
	callID  string
	content string
}

// lastToolOutput 末尾若是工具结果消息则返回它（工具续轮标志）；否则 nil。
func lastToolOutput(msgs []*pb.EnvelopeMessage) *toolResult {
	if len(msgs) == 0 {
		return nil
	}
	last := msgs[len(msgs)-1]
	if last.GetRole() != "tool" {
		return nil
	}
	return &toolResult{callID: last.GetToolCallId(), content: messageText(last)}
}

// ---------- Postman SSE 解析 ----------

// postmanEvent Agent Mode SSE 事件公共字段。
type postmanEvent struct {
	EventType string `json:"eventType"`
	Data      struct {
		TextContent     string `json:"textContent"`
		ThinkingContent string `json:"thinkingContent"`
		ID              string `json:"id"` // conversation 事件的会话 id
		ToolCalls       []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"toolCalls"`
	} `json:"data"`
}

// streamState 一次 Chat 的流解析状态：捕获会话 id、累积工具调用、把事件翻成 StreamEvent。
type streamState struct {
	p         *plugin
	threadKey string
	convID    string
	emit      func(*pb.StreamEvent)
	sawTool   bool
	seenTools map[string]bool
	// unregistered toolCall 早于 conversation 到达时暂存，convID 就绪后补登记。
	unregistered []string
}

// handleLine 处理一行 SSE。
func (s *streamState) handleLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var ev postmanEvent
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	switch ev.EventType {
	case "textChunk":
		if ev.Data.TextContent != "" {
			s.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
				ContentDelta: &pb.ContentDelta{Text: ev.Data.TextContent},
			}})
		}
	case "thinkingChunk":
		if ev.Data.ThinkingContent != "" {
			s.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ReasoningDelta{
				ReasoningDelta: &pb.ReasoningDelta{Text: ev.Data.ThinkingContent},
			}})
		}
	case "conversation":
		if ev.Data.ID != "" {
			s.convID = ev.Data.ID
			s.p.rememberThread(s.threadKey, s.convID)
			for _, id := range s.unregistered {
				s.p.rememberPending(id, s.threadKey, s.convID)
			}
			s.unregistered = nil
		}
	case "toolCall", "toolCallChunk":
		s.handleToolCalls(ev)
	}
}

// handleToolCalls 累积 Postman 分块工具调用为 ToolCallDelta：首块带 id+name，后续只带 arguments 增量。
func (s *streamState) handleToolCalls(ev postmanEvent) {
	if s.seenTools == nil {
		s.seenTools = map[string]bool{}
	}
	for _, tc := range ev.Data.ToolCalls {
		if tc.ID == "" {
			continue
		}
		if !s.seenTools[tc.ID] {
			s.seenTools[tc.ID] = true
			s.sawTool = true
			s.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{Id: tc.ID, Name: tc.Function.Name},
			}})
			// 登记调用归属，下游回传结果时据此续会话（convID 未到则暂存）。
			if s.convID != "" {
				s.p.rememberPending(tc.ID, s.threadKey, s.convID)
			} else {
				s.unregistered = append(s.unregistered, tc.ID)
			}
		}
		if tc.Function.Arguments != "" {
			s.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{ArgumentsDelta: tc.Function.Arguments},
			}})
		}
	}
}
