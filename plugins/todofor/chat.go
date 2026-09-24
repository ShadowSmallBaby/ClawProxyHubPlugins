// todofor 对话：信封历史 → todo content；开 WebSocket → 建 todo → 订阅 block:message 流；
// 终止时 REST 拉权威消息取内容与用量，解析 <TOOL_CALL> 客户端工具调用。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	// pollTimeout 单回合上游运行的整体上限（超时按失败处理）。
	pollTimeout = 5 * time.Minute
	// firstResponseTimeout 首个 block:message 前的等待上限。
	firstResponseTimeout = 30 * time.Second
	// restPollInterval WebSocket 漏事件时的 REST 兜底轮询间隔。
	restPollInterval = 2 * time.Second
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	cli := newClient(p.baseURL(), c.APIKey, p.hc(c))
	if c.ProjectID == "" || c.AgentID == "" {
		if err := p.resolveAccount(ctx, cli, c); err != nil {
			return stream.Send(shared.Failed(401, err.Error()))
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	// 组 AgentSettings：模型 + system + 工具协议。
	agent, err := cli.agent(runCtx, c.AgentID)
	if err != nil {
		agent = agentSettings{ID: c.AgentID} // 拉模板失败仍尝试用最小配置
	}
	agent.Model = p.resolveRunnerModel(runCtx, cli, req.Model)
	applyAgentPrompt(&agent, systemPrompt(req.Messages), req.Tools)

	content := flattenTurn(req.Messages)
	if strings.TrimSpace(content) == "" {
		return stream.Send(shared.Failed(400, "request has no content"))
	}

	// 刻意先开 WebSocket 再建 todo，避免漏掉早期运行事件。
	sub, err := cli.prepareSubscription(runCtx)
	if err != nil {
		return stream.Send(shared.Failed(upstreamCode(err), "open subscription: "+err.Error()))
	}
	defer sub.close()

	created, err := cli.createTodo(runCtx, c.ProjectID, content, agent)
	if err != nil {
		return stream.Send(shared.Failed(upstreamCode(err), "create todo: "+err.Error()))
	}
	if created.ID == "" {
		return stream.Send(shared.Failed(502, "upstream returned an empty todo id"))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}

	var filter toolCallStreamFilter
	// 流式增量经工具过滤器后下发（合法 TOOL_CALL 块扣留）。
	emitText := func(delta string) error {
		if out := filter.Push(delta); out != "" {
			return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
				ContentDelta: &pb.ContentDelta{Text: out},
			}})
		}
		return nil
	}

	result, err := p.waitAssistant(runCtx, sub, cli, created.ID, emitText)
	if err != nil {
		return stream.Send(shared.Failed(upstreamCode(err), err.Error()))
	}

	// 权威内容以 REST 为准：解析工具调用，或补发流未覆盖的尾部文本。
	// 工具块前的文本流式阶段已发过，这里只需分派工具调用或补尾。
	_, calls := parseToolCalls(result.Content)
	if len(calls) > 0 {
		for _, call := range calls {
			if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{Id: call.ID, Name: call.Name, ArgumentsDelta: call.Arguments},
			}}); err != nil {
				return err
			}
		}
	} else if strings.HasPrefix(result.Content, result.Streamed) {
		// 流已发出的部分不重发，仅补 REST 权威版本的增量尾部（清洗过工具协议）。
		if tail := filter.Push(result.Content[len(result.Streamed):]); tail != "" {
			if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
				ContentDelta: &pb.ContentDelta{Text: tail},
			}}); err != nil {
				return err
			}
		}
		if tail := filter.Flush(); tail != "" {
			if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
				ContentDelta: &pb.ContentDelta{Text: tail},
			}}); err != nil {
				return err
			}
		}
	}

	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	fin := &pb.MessageFinish{FinishReason: finish}
	if result.Usage != nil {
		fin.Usage = result.Usage
	}
	return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{MessageFinish: fin}})
}

// assistantResult 一回合的权威结果：完整内容 + 已流出的部分 + 用量。
type assistantResult struct {
	Content  string
	Streamed string
	Usage    *pb.Usage
}

// frontendPayload 前端 WebSocket 信封 payload 的公共字段。
type frontendPayload struct {
	TodoID  string `json:"todoId"`
	TodoID2 string `json:"todo_id"`
	Status  string `json:"status"`
	Content string `json:"content"`
	BlockID string `json:"blockId"`
	Updates struct {
		Status string `json:"status"`
	} `json:"updates"`
}

// waitAssistant 订阅 todo 运行事件：block:message 增量即时下发，todo:status 终止时 REST 拉权威结果。
func (p *plugin) waitAssistant(ctx context.Context, sub *subscription, cli *client, todoID string, emit func(string) error) (assistantResult, error) {
	events := make(chan wsEvent, 32)
	errc := make(chan error, 1)
	go func() { errc <- sub.subscribe(ctx, todoID, events) }()

	var buf strings.Builder
	pendingBlocks := make(map[string]struct{})
	firstTimer := time.NewTimer(firstResponseTimeout)
	restTicker := time.NewTicker(restPollInterval)
	defer firstTimer.Stop()
	defer restTicker.Stop()

	for {
		select {
		case ev := <-events:
			var payload frontendPayload
			if json.Unmarshal(ev.Payload, &payload) != nil {
				continue
			}
			evTodoID := shared.OrDefault(payload.TodoID, payload.TodoID2)
			if evTodoID != "" && evTodoID != todoID {
				continue
			}
			switch ev.Type {
			case "block:message":
				buf.WriteString(payload.Content)
				if payload.Content != "" {
					firstTimer.Stop()
					if emit != nil {
						if err := emit(payload.Content); err != nil {
							return assistantResult{}, fmt.Errorf("emit stream text: %w", err)
						}
					}
				}
			case "BLOCK_UPDATE":
				if payload.BlockID == "" {
					continue
				}
				switch payload.Updates.Status {
				case "AWAITING_APPROVAL":
					pendingBlocks[payload.BlockID] = struct{}{}
				case "COMPLETED", "DENIED", "FAILED", "ERROR", "CANCELLED":
					delete(pendingBlocks, payload.BlockID)
				}
			case "todo:status":
				switch payload.Status {
				case "READY", "READY_CHECKED", "DONE":
					if len(pendingBlocks) > 0 && payload.Status != "DONE" {
						continue
					}
					return finishAssistant(ctx, cli, todoID, buf.String())
				case "CANCELLED", "CANCELLED_CHECKED", "ERROR", "ERROR_CHECKED":
					return assistantResult{}, fmt.Errorf("todo %s ended with status %s", todoID, payload.Status)
				}
			}
		case err := <-errc:
			if ctx.Err() != nil {
				return assistantResult{}, assistantWaitError(ctx)
			}
			result, restErr := finishAssistant(ctx, cli, todoID, buf.String())
			if restErr != nil {
				return assistantResult{}, restErr
			}
			if result.Content == "" && err != nil {
				return assistantResult{}, err
			}
			return result, nil
		case <-restTicker.C:
			t, restErr := cli.getTodo(ctx, todoID)
			if restErr != nil {
				continue
			}
			switch strings.ToUpper(t.Status) {
			case "READY", "READY_CHECKED", "DONE":
				if len(pendingBlocks) > 0 && strings.ToUpper(t.Status) != "DONE" {
					continue
				}
				return finishAssistant(ctx, cli, todoID, buf.String())
			case "CANCELLED", "CANCELLED_CHECKED", "ERROR", "ERROR_CHECKED":
				return assistantResult{}, fmt.Errorf("todo %s ended with status %s", todoID, t.Status)
			}
		case <-firstTimer.C:
			if buf.Len() == 0 {
				return assistantResult{}, fmt.Errorf("upstream produced no response after %s", firstResponseTimeout)
			}
		case <-ctx.Done():
			return assistantResult{}, assistantWaitError(ctx)
		}
	}
}

// finishAssistant 从 REST 消息列表取最新 assistant 的权威内容与用量，缺失时退回流式累积。
func finishAssistant(ctx context.Context, cli *client, todoID, streamed string) (assistantResult, error) {
	msgs, err := cli.messages(ctx, todoID)
	if err != nil {
		if streamed != "" {
			return assistantResult{Content: streamed, Streamed: streamed}, nil
		}
		return assistantResult{}, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		result := assistantResult{Streamed: streamed, Usage: tokenUsage(msgs[i].RunMeta)}
		if msgs[i].Content != "" {
			result.Content = msgs[i].Content
			return result, nil
		}
		var b strings.Builder
		for _, blk := range msgs[i].Blocks {
			if blk.Type == "text" || blk.Type == "markdown" {
				b.WriteString(blk.Content)
			}
		}
		if b.Len() > 0 {
			result.Content = b.String()
		} else {
			result.Content = streamed
		}
		return result, nil
	}
	return assistantResult{Content: streamed, Streamed: streamed}, nil
}

// tokenUsage 汇总上游 AI 消息元数据为信封用量；无权威元数据返回 nil（不用估算填充）。
func tokenUsage(meta []runMeta) *pb.Usage {
	var u pb.Usage
	available := false
	for _, m := range meta {
		if m.Type != "todo:msg_meta_ai" {
			continue
		}
		available = true
		u.InputTokens += int64(m.Extras.InputTokens)
		u.OutputTokens += int64(m.Extras.OutputTokens)
		u.CachedTokens += int64(m.Extras.CacheReadTokens)
		u.CacheCreationTokens += int64(m.Extras.CacheWriteTokens)
	}
	if !available {
		return nil
	}
	return &u
}

func assistantWaitError(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out waiting for assistant reply")
	}
	return fmt.Errorf("waiting for assistant reply: %v", ctx.Err())
}

// upstreamCode 上游错误 → 信封错误码（鉴权 401 / 限流 429 / 其余 502）。
func upstreamCode(err error) int32 {
	if he, ok := err.(*httpError); ok {
		switch he.StatusCode {
		case 401, 403:
			return 401
		case 429:
			return 429
		}
	}
	return 502
}

// ---------- 信封 → todo content ----------

// systemPrompt 汇总所有 system 消息文本（作 AgentSettings.SystemMessage）。
func systemPrompt(msgs []*pb.EnvelopeMessage) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" {
			if t := messageText(m); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// flattenTurn 把非 system 历史拼成上游可读的 todo content（工具请求 / 结果格式化）。
func flattenTurn(msgs []*pb.EnvelopeMessage) string {
	toolNames := toolNamesByID(msgs)
	var b strings.Builder
	first := true
	sep := func() {
		if !first {
			b.WriteString("\n\n")
		}
		first = false
	}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			continue // 已并入 AgentSettings.SystemMessage
		case "assistant":
			sep()
			if len(m.ToolCalls) > 0 {
				b.WriteString("[assistant tool request] ")
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "%s(%s) ", tc.GetName(), tc.GetArguments())
				}
			} else {
				b.WriteString("[assistant] " + messageText(m))
			}
		case "tool":
			sep()
			fmt.Fprintf(&b, "[tool result for %s]\n%s", toolResultName(m, toolNames), messageText(m))
		default: // user
			sep()
			b.WriteString(messageText(m))
		}
	}
	return b.String()
}

// messageText 信封消息 → 纯文本（Text 优先，退回 Parts 的 text）。
func messageText(m *pb.EnvelopeMessage) string {
	if strings.TrimSpace(m.Text) != "" {
		return m.Text
	}
	var b strings.Builder
	for _, part := range m.Parts {
		if (part.Type == "text" || part.Type == "thinking") && part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// toolNamesByID assistant tool_calls 的 id → name 映射（tool 结果消息缺 name 时回填）。
func toolNamesByID(msgs []*pb.EnvelopeMessage) map[string]string {
	names := make(map[string]string)
	for _, m := range msgs {
		for _, call := range m.ToolCalls {
			if call.GetId() != "" {
				names[call.GetId()] = call.GetName()
			}
		}
	}
	return names
}

// toolResultName tool 结果消息的工具名（name > 由 tool_call_id 回填 > id > unknown）。
func toolResultName(m *pb.EnvelopeMessage, names map[string]string) string {
	if name := names[m.ToolCallId]; name != "" {
		return name
	}
	if m.ToolCallId != "" {
		return m.ToolCallId
	}
	return "unknown"
}

// applyAgentPrompt 组合用户 system 与工具协议 system，注入 AgentSettings（raw 模式），并按需拒绝上游自执行工具。
func applyAgentPrompt(agent *agentSettings, system string, tools []*pb.ToolDefinition) {
	toolPrompt := buildToolSystemPrompt(tools)
	switch {
	case system != "" && toolPrompt != "":
		agent.SystemMessage = system + "\n\n" + toolPrompt
		agent.SystemMessageMode = "raw"
	case system != "":
		agent.SystemMessage = system
		agent.SystemMessageMode = "raw"
	case toolPrompt != "":
		agent.SystemMessage = toolPrompt
		agent.SystemMessageMode = "raw"
	}
	if len(tools) > 0 {
		agent.Permissions = &toolPermissions{
			Allow: []string{},
			Deny:  []string{"device:*", "cloud:*"},
		}
	}
}
