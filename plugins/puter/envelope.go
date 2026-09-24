// deepseek.go — puter DeepSeek 思考模式专属处理：思考开关归一化、reasoning_content
// 多轮回传、严格角色序列（合并相邻 assistant、拆多 tool_call）。仅 service==deepseek 启用。
package main

import (
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// missingDeepSeekReasoningFallback 空 reasoning 占位：DeepSeek 要求 assistant 回传
// reasoning_content，缺失会 400，用非空占位兜底。
const missingDeepSeekReasoningFallback = "(reasoning omitted)"

// normalizePuterReasoning DeepSeek 思考开关：客户端给 effort 则开并透传，"none" 关，其余不设。
func normalizePuterReasoning(req *pb.ChatRequest, service string) (string, *bool) {
	if service != "deepseek" {
		return "", nil
	}
	switch e := strings.ToLower(strings.TrimSpace(req.GetExtra()["reasoning_effort"])); e {
	case "":
		return "", nil
	case "none":
		off := false
		return "", &off
	default:
		on := true
		return e, &on
	}
}

// parseParallelToolCalls 从 Extra 读 parallel_tool_calls（"true"/"false"），未设返回 nil。
func parseParallelToolCalls(req *pb.ChatRequest) *bool {
	v := strings.ToLower(strings.TrimSpace(req.GetExtra()["parallel_tool_calls"]))
	if v == "" {
		return nil
	}
	b := v == "true"
	return &b
}

// assistantReasoning 从 assistant 消息的 thinking 块提取推理正文（无则空）。
func assistantReasoning(msg *pb.EnvelopeMessage) string {
	for _, p := range msg.GetParts() {
		if p.GetType() == "thinking" {
			if t := strings.TrimSpace(p.GetText()); t != "" {
				return t
			}
		}
	}
	return ""
}

// mergeAdjacentAssistantMessages 合并相邻 assistant（DeepSeek 严格角色序列要求）。
func mergeAdjacentAssistantMessages(messages []puterMessage) []puterMessage {
	if len(messages) < 2 {
		return messages
	}
	out := make([]puterMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role != "assistant" || len(out) == 0 || out[len(out)-1].Role != "assistant" {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		prev.Content = joinReplayText(prev.Content, m.Content)
		prev.ReasoningContent = joinReplayReasoning(prev.ReasoningContent, m.ReasoningContent)
		prev.ToolCalls = append(prev.ToolCalls, m.ToolCalls...)
	}
	return out
}

func joinReplayText(l, r string) string {
	l, r = strings.TrimSpace(l), strings.TrimSpace(r)
	switch {
	case l == "":
		return r
	case r == "":
		return l
	default:
		return l + "\n" + r
	}
}

func joinReplayReasoning(l, r string) string {
	if l == missingDeepSeekReasoningFallback {
		l = ""
	}
	if r == missingDeepSeekReasoningFallback {
		r = ""
	}
	if j := joinReplayText(l, r); j != "" {
		return j
	}
	return missingDeepSeekReasoningFallback
}

// splitMultiToolCalls 单 assistant 多 tool_calls 拆成「assistant(单调用)→tool(回应)」序列，
// 绕开 puter DeepSeekProvider 在 tool 消息后注入 system 打断配对导致的校验失败。
func splitMultiToolCalls(messages []puterMessage) []puterMessage {
	out := make([]puterMessage, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) < 2 {
			out = append(out, m)
			continue
		}
		j := i + 1
		var results []puterMessage
		for j < len(messages) && messages[j].Role == "tool" {
			results = append(results, messages[j])
			j++
		}
		byID := make(map[string]puterMessage, len(results))
		for _, r := range results {
			byID[r.ToolCallID] = r
		}
		if len(byID) != len(m.ToolCalls) {
			out = append(out, m) // 回应不齐，保持原样
			continue
		}
		for k, tc := range m.ToolCalls {
			part := puterMessage{Role: "assistant", ReasoningContent: m.ReasoningContent, ToolCalls: []puterTool{tc}}
			if k == 0 {
				part.Content = m.Content
			}
			out = append(out, part)
			if res, ok := byID[tc.ID]; ok {
				out = append(out, res)
			}
		}
		i = j - 1
	}
	return out
}
