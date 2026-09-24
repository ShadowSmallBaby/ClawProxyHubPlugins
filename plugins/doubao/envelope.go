// doubao 工具调用（模拟 Function Calling）：豆包上游无原生工具能力，改用
// 提示词注入 + 输出标签解析——多轮历史与工具定义合并为单条 prompt 下发，
// 模型以 <tool_call>{...}</tool_call> 回吐，出站再解析为 ToolCallDelta。
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// toolSystemPrompt 工具调用指令模板；%s 处填入工具清单。
const toolSystemPrompt = `你可以调用以下工具完成任务。需要调用时，严格按如下格式输出（可多次），不要在标签外解释：
<tool_call>
{"name": "工具名", "arguments": {"参数名": 参数值}}
</tool_call>
若无需工具，直接正常回答。

## 可用工具
%s`

// toolCallRe 匹配模型输出中的 <tool_call>...</tool_call> 块（含跨行）。
var toolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

// messageText 取单条信封消息的文本（text 优先，回退 parts 拼接）。
func messageText(m *pb.EnvelopeMessage) string {
	if m.GetText() != "" {
		return m.GetText()
	}
	var b strings.Builder
	for _, part := range m.GetParts() {
		if part.GetType() == "text" {
			b.WriteString(part.GetText())
		}
	}
	return b.String()
}

// onlyOneUserTurn 判断是否为「单条 user、无历史」——此时保持纯文本下发不加角色前缀。
func onlyOneUserTurn(msgs []*pb.EnvelopeMessage) bool {
	count := 0
	for _, m := range msgs {
		if m.GetRole() != "user" {
			return false
		}
		count++
	}
	return count <= 1
}

// buildPrompt 把信封多轮消息拼成单条 prompt（豆包上游仅接受单 text），
// 工具定义非空时注入工具系统提示。修复原仅发最后一条 user 导致的多轮失忆。
func buildPrompt(req *pb.ChatRequest) string {
	msgs := req.GetMessages()
	tools := formatTools(req.GetTools())
	if tools == "" && onlyOneUserTurn(msgs) {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].GetRole() == "user" {
				return messageText(msgs[i])
			}
		}
		return ""
	}

	var b strings.Builder
	if tools != "" {
		b.WriteString(fmt.Sprintf(toolSystemPrompt, tools))
		b.WriteString("\n\n")
	}
	for _, m := range msgs {
		text := messageText(m)
		switch m.GetRole() {
		case "system":
			if text != "" {
				b.WriteString("[system]: " + text + "\n\n")
			}
		case "assistant":
			if tc := m.GetToolCalls(); len(tc) > 0 {
				b.WriteString("[assistant]: " + reconstructToolCalls(tc) + "\n\n")
			} else if text != "" {
				b.WriteString("[assistant]: " + text + "\n\n")
			}
		case "tool":
			b.WriteString("[tool_result " + m.GetToolCallId() + "]: " + text + "\n\n")
		default: // user
			if text != "" {
				b.WriteString("[user]: " + text + "\n\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// formatTools 工具定义 → 提示词清单（直接用 JSON Schema，不做参数压缩）。
func formatTools(tools []*pb.ToolDefinition) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range tools {
		b.WriteString("- " + t.GetName())
		if d := t.GetDescription(); d != "" {
			b.WriteString(": " + d)
		}
		if s := t.GetParametersSchema(); s != "" {
			b.WriteString("\n  参数(JSON Schema): " + s)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// reconstructToolCalls assistant 历史工具调用 → XML（多轮回放给上游）。
func reconstructToolCalls(tcs []*pb.ToolCall) string {
	var b strings.Builder
	for _, tc := range tcs {
		args := tc.GetArguments()
		if args == "" {
			args = "{}"
		}
		b.WriteString(`<tool_call>{"name":"` + tc.GetName() + `","arguments":` + args + `}</tool_call>`)
	}
	return b.String()
}

// extractToolCalls 从完整输出提取工具调用，返回调用列表与剥离标签后的剩余正文。
func extractToolCalls(text string) ([]*pb.ToolCall, string) {
	matches := toolCallRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, text
	}
	var calls []*pb.ToolCall
	for _, m := range matches {
		var obj struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(m[1])), &obj) == nil && obj.Name != "" {
			args := strings.TrimSpace(string(obj.Arguments))
			if args == "" {
				args = "{}"
			}
			calls = append(calls, &pb.ToolCall{Id: "call_" + shared.RandHex(8), Name: obj.Name, Arguments: args})
		}
	}
	return calls, strings.TrimSpace(toolCallRe.ReplaceAllString(text, ""))
}
