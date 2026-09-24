// chat.go — 对话入口 + 标准 OpenAI SSE 流解析（delta/tool_calls/usage → StreamEvent）。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	if err := p.ensureFresh(ctx, c); err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	if c.SecurityOauthToken == "" {
		return stream.Send(shared.Failed(401, "缺少 securityOauthToken，请重新登录"))
	}

	reqID, sessID := shared.RandUUID(), shared.RandUUID()
	payload, _ := json.Marshal(buildChatBody(req, reqID, sessID))

	chatURL := chatBase + "/model/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", chatURL, strings.NewReader(string(payload)))
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	httpReq.Header.Set("authorization", "Bearer "+c.SecurityOauthToken)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	httpReq.Header.Set("user-agent", "qoder/1.1.16")
	httpReq.Header.Set("x-request-id", reqID)
	httpReq.Header.Set("x-session-id", sessID)

	resp, err := p.hc(c).Do(httpReq)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw := shared.ReadLimited(resp.Body, 8192)
		code := int32(502)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			code = 401
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	return p.scanOpenAISSE(resp.Body, stream)
}

// scanOpenAISSE 标准 OpenAI SSE 流：data 行直接是 chat.completion.chunk（无信封）。
func (p *plugin) scanOpenAISSE(body io.Reader, stream pb.ClawPlugin_ChatServer) error {
	sawEvent := false
	tmp := make([]byte, 64*1024)
	var pending string
	toolSeen := map[int]bool{}
	finishReason := ""
	var usage *pb.Usage

	emit := func(ev *pb.StreamEvent) { _ = stream.Send(ev) }
	finish := func() {
		if finishReason == "" {
			finishReason = "stop"
		}
		emit(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
			MessageFinish: &pb.MessageFinish{FinishReason: finishReason, Usage: usage},
		}})
	}

	for {
		n, err := body.Read(tmp)
		if n > 0 {
			pending += string(tmp[:n])
			for {
				i := strings.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := strings.TrimSuffix(pending[:i], "\r")
				pending = pending[i+1:]
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if payload == "" || payload == "[DONE]" {
					continue
				}
				content, reasoning, toolCalls, stop, u := parseOpenAIDelta(payload)
				if u != nil {
					usage = mergeUsage(usage, u)
				}
				if reasoning != "" {
					emit(&pb.StreamEvent{Event: &pb.StreamEvent_ReasoningDelta{ReasoningDelta: &pb.ReasoningDelta{Text: reasoning}}})
					sawEvent = true
				}
				if content != "" {
					emit(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{ContentDelta: &pb.ContentDelta{Text: content}}})
					sawEvent = true
				}
				for _, tc := range toolCalls {
					ev := &pb.ToolCallDelta{Id: tc.id, Name: tc.name, ArgumentsDelta: tc.args}
					if tc.id == "" && toolSeen[tc.index] {
						ev.Id = ""
					}
					toolSeen[tc.index] = true
					emit(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{ToolCallDelta: ev}})
					sawEvent = true
				}
				if stop != "" {
					finishReason = mapStop(stop)
					sawEvent = true
				}
			}
		}
		if err != nil {
			break
		}
	}
	if !sawEvent {
		parserFail(stream, 502, "upstream returned an empty stream")
		return nil
	}
	finish()
	return nil
}

// qToolCall 解析出的 tool_calls 增量。
type qToolCall struct {
	index int
	id    string
	name  string
	args  string
}

// parseOpenAIDelta 解标准 chat.completion.chunk：choices[].delta + finish_reason + usage。
func parseOpenAIDelta(payload string) (content, reasoning string, toolCalls []qToolCall, stop string, usage *pb.Usage) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int64 `json:"prompt_tokens"`
			CompletionTokens        int64 `json:"completion_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	for _, ch := range chunk.Choices {
		reasoning = shared.OrDefault(ch.Delta.ReasoningContent, reasoning)
		content = shared.OrDefault(ch.Delta.Content, content)
		for _, tc := range ch.Delta.ToolCalls {
			toolCalls = append(toolCalls, qToolCall{index: tc.Index, id: tc.ID, name: tc.Function.Name, args: tc.Function.Arguments})
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			stop = *ch.FinishReason
		}
	}
	if chunk.Usage != nil {
		usage = &pb.Usage{
			InputTokens:     chunk.Usage.PromptTokens,
			OutputTokens:    chunk.Usage.CompletionTokens,
			ReasoningTokens: chunk.Usage.CompletionTokensDetails.ReasoningTokens,
		}
	}
	return
}

// mapStop OpenAI finish_reason 已是信封语义（stop/tool_calls/length），直传。
func mapStop(reason string) string { return reason }

// mergeUsage 后到的非零字段覆盖。
func mergeUsage(cur, in *pb.Usage) *pb.Usage {
	if cur == nil {
		cur = &pb.Usage{}
	}
	if in == nil {
		return cur
	}
	if in.InputTokens > 0 {
		cur.InputTokens = in.InputTokens
	}
	if in.OutputTokens > 0 {
		cur.OutputTokens = in.OutputTokens
	}
	if in.ReasoningTokens > 0 {
		cur.ReasoningTokens = in.ReasoningTokens
	}
	return cur
}

func parserFail(stream pb.ClawPlugin_ChatServer, code int32, msg string) {
	_ = stream.Send(shared.Failed(code, msg))
}
