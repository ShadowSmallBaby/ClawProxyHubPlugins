// 统一信封 → GetChatMessageRequest：品牌词净化 + 工具描述注入 +
// 消息转换（当前轮图片才进 Images，历史图文本占位）。
package main

import (
	"fmt"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	devinproto "github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins/devin/devinproto"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"google.golang.org/protobuf/proto"
)

// buildChatRequest 信封 → Devin 请求；设备指纹稳定，请求标识随机。
func buildChatRequest(req *pb.ChatRequest, token string) (*devinproto.GetChatMessageRequest, error) {
	metadata := metadataFor(token)
	metadata.F = proto.String(fingerprintFromToken(token))
	result := &devinproto.GetChatMessageRequest{
		Metadata:     metadata,
		Prompt:       proto.String(sanitizeSystemPrompt(withToolDescriptions(systemPromptOf(req), req.GetTools()))),
		ChatModelUid: proto.String(strings.TrimSpace(req.GetModel())),
		RequestType:  devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
		Configuration: &devinproto.ExaCodeiumCommonPb_CompletionConfiguration{
			NumCompletions: proto.Uint64(1),
			MaxTokens:      proto.Uint64(uint64(orInt(req.GetMaxTokens(), 128000))),
			MaxNewlines:    proto.Uint64(400),
			Temperature:    proto.Float64(orFloat(req.GetTemperature(), 1)),
			TopK:           proto.Uint64(40),
			TopP:           proto.Float64(0.95),
		},
		TrajectoryReference: &devinproto.ExaCortexPb_CortexTrajectoryReference{
			TrajectoryId:   proto.String(shared.RandUUID()),
			TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
			StepType:       devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT.Enum(),
		},
		CascadeId:   proto.String(shared.RandUUID()),
		ExecutionId: proto.String(shared.RandUUID()),
		PlannerMode: devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum(),
	}

	// 当前轮 = 最后一条 assistant 消息之后的所有 user/tool 消息；
	// 仅当前轮图片可靠，历史图文本占位。
	lastAssistant := -1
	for i, m := range req.GetMessages() {
		if m.GetRole() == "assistant" {
			lastAssistant = i
		}
	}
	for i, m := range req.GetMessages() {
		if m.GetRole() == "system" {
			continue
		}
		prompts, err := convertMessage(m, i > lastAssistant)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		result.ChatMessagePrompts = append(result.ChatMessagePrompts, prompts...)
	}
	for _, tool := range req.GetTools() {
		schema, err := sanitizeToolSchema([]byte(shared.OrDefault(tool.GetParametersSchema(), "{}")))
		if err != nil {
			return nil, fmt.Errorf("sanitize tool %q schema: %w", tool.GetName(), err)
		}
		result.Tools = append(result.Tools, &devinproto.ExaChatPb_ChatToolDefinition{
			Name:             proto.String(tool.GetName()),
			Description:      proto.String(tool.GetName()),
			JsonSchemaString: proto.String(string(schema)),
		})
	}
	return result, nil
}

// convertMessage 信封消息 → ChatMessagePrompt（user/assistant/tool 三种 source）。
func convertMessage(m *pb.EnvelopeMessage, attachImages bool) ([]*devinproto.ExaChatPb_ChatMessagePrompt, error) {
	switch m.GetRole() {
	case "user":
		return []*devinproto.ExaChatPb_ChatMessagePrompt{promptForContent(
			devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER,
			m, attachImages)}, nil
	case "assistant":
		// 助手历史不回传图片；tool_calls 挂回 prompt。
		prompt := promptForContent(
			devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM,
			m, false)
		for _, call := range m.GetToolCalls() {
			prompt.ToolCalls = append(prompt.ToolCalls, &devinproto.ExaCodeiumCommonPb_ChatToolCall{
				Id:            proto.String(call.GetId()),
				Name:          proto.String(call.GetName()),
				ArgumentsJson: proto.String(call.GetArguments()),
			})
		}
		return []*devinproto.ExaChatPb_ChatMessagePrompt{prompt}, nil
	case "tool":
		prompt := promptForContent(
			devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL,
			m, attachImages)
		prompt.ToolCallId = proto.String(m.GetToolCallId())
		prompt.ToolResultIsError = proto.Bool(m.GetToolError())
		return []*devinproto.ExaChatPb_ChatMessagePrompt{prompt}, nil
	default:
		return nil, fmt.Errorf("unsupported role %q", m.GetRole())
	}
}

// promptForContent 消息文本/图片 → prompt；历史图占位，当前轮图走 base64 Images。
func promptForContent(source devinproto.ExaCodeiumCommonPb_ChatMessageSource, m *pb.EnvelopeMessage, attachImages bool) *devinproto.ExaChatPb_ChatMessagePrompt {
	prompt := &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(shared.RandUUID()),
		Source:    source.Enum(),
	}
	var text strings.Builder
	if len(m.GetParts()) == 0 {
		text.WriteString(m.GetText())
	}
	for _, part := range m.GetParts() {
		switch part.GetType() {
		case "image", "image_url":
			data, mime := imageData(part)
			if !attachImages {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString("[Image omitted from history]")
				continue
			}
			prompt.Images = append(prompt.Images, &devinproto.ExaCodeiumCommonPb_ImageData{
				Base64Data: proto.String(data),
				MimeType:   proto.String(shared.OrDefault(mime, "image/png")),
			})
		case "text":
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(part.GetText())
		}
	}
	prompt.Prompt = proto.String(text.String())
	return prompt
}

// imageData part → 纯 base64（去 data: 前缀）+ mime。
func imageData(part *pb.ContentPart) (string, string) {
	data := part.GetData()
	if data == "" {
		data = part.GetUrl()
	}
	mime := part.GetMediaType()
	if strings.HasPrefix(data, "data:") {
		if pre, encoded, ok := strings.Cut(data, ","); ok {
			data = encoded
			if mime == "" {
				if head, _, ok2 := strings.Cut(pre, ";"); ok2 {
					mime = strings.TrimPrefix(head, "data:")
				}
			}
		}
	}
	return data, mime
}

// withToolDescriptions 非空工具说明追加到 system prompt（XML 块），供模型理解原生工具用途。
func withToolDescriptions(systemPrompt string, tools []*pb.ToolDefinition) string {
	var section strings.Builder
	for _, tool := range tools {
		desc := strings.TrimSpace(tool.GetDescription())
		if desc == "" {
			continue
		}
		if section.Len() == 0 {
			section.WriteString("# tools descriptions")
		}
		section.WriteString("\n<tool name=\"")
		section.WriteString(tool.GetName())
		section.WriteString("\">\n")
		section.WriteString(desc)
		section.WriteString("\n</tool>")
	}
	if section.Len() == 0 {
		return systemPrompt
	}
	trimmed := strings.TrimRight(systemPrompt, "\r\n")
	if strings.TrimSpace(trimmed) == "" {
		return section.String()
	}
	return trimmed + "\n\n" + section.String()
}

// systemPromptOf 合并信封中的系统指令。
func systemPromptOf(req *pb.ChatRequest) string {
	var parts []string
	for _, m := range req.GetMessages() {
		if m.GetRole() == "system" {
			parts = append(parts, m.GetText())
		}
	}
	return strings.Join(parts, "\n\n")
}

func orInt(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}

func orFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}
