// chat.go — 对话：统一信封 → puter 驱动请求 JSON，消费 NDJSON 流。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// streamChunk puter 流事件（NDJSON 每行一个）。
type streamChunk struct {
	Type      string                 `json:"type"`
	Text      string                 `json:"text,omitempty"`
	Reasoning string                 `json:"reasoning,omitempty"`
	ID        string                 `json:"id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Input     json.RawMessage        `json:"input,omitempty"`
	Usage     map[string]interface{} `json:"usage,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Error     errorField             `json:"error,omitempty"`
}

// errorField puter 错误字段（字符串或对象双形态）。
type errorField struct {
	Payload *errorPayload
	Message string
}

type errorPayload struct {
	Iface   string `json:"iface"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func (e *errorField) UnmarshalJSON(data []byte) error {
	e.Payload, e.Message = nil, ""
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var msg string
	if json.Unmarshal(data, &msg) == nil {
		e.Message = strings.TrimSpace(msg)
		return nil
	}
	var payload errorPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	e.Payload = &payload
	return nil
}

func (e *errorField) present() bool {
	return e.Payload != nil || strings.TrimSpace(e.Message) != ""
}

var puterToolCallSequence atomic.Uint64

func newToolCallID() string {
	return fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), puterToolCallSequence.Add(1))
}

// consumePuterStream 消费 NDJSON 流；结束时发 model.finish（tool_use 优先）。
func consumePuterStream(body io.Reader, emit func(*pb.StreamEvent)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawMeaningful := false
	toolCallCount := 0
	var usage *pb.Usage

	for scanner.Scan() {
		line := normalizePuterStreamLine(scanner.Text())
		if line == "" {
			continue
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			return fmt.Errorf("puter stream protocol error: invalid JSON event preview=%q", boundedPreview(line))
		}
		if chunk.Error.present() {
			return puterStreamError(chunk.Error, line)
		}
		switch strings.ToLower(strings.TrimSpace(chunk.Type)) {
		case "text":
			sawMeaningful = true
			if chunk.Text != "" {
				emit(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
					ContentDelta: &pb.ContentDelta{Text: chunk.Text},
				}})
			}
		case "reasoning":
			sawMeaningful = true
			if chunk.Reasoning != "" {
				emit(&pb.StreamEvent{Event: &pb.StreamEvent_ReasoningDelta{
					ReasoningDelta: &pb.ReasoningDelta{Text: chunk.Reasoning},
				}})
			}
		case "tool_use":
			name := strings.TrimSpace(chunk.Name)
			if name == "" {
				return fmt.Errorf("puter stream returned tool_use without a name")
			}
			id := strings.TrimSpace(chunk.ID)
			if id == "" {
				id = newToolCallID()
			}
			sawMeaningful = true
			toolCallCount++
			emit(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{Id: id, Name: name, ArgumentsDelta: normalizeStreamToolInput(chunk.Input)},
			}})
		case "usage":
			sawMeaningful = true
			usage = normalizePuterUsage(chunk.Usage)
		case "error":
			return puterStreamError(chunk.Error, line)
		default:
			return fmt.Errorf("puter stream protocol error: unknown event type %q preview=%q", chunk.Type, boundedPreview(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read puter stream: %w", err)
	}
	if !sawMeaningful {
		return fmt.Errorf("puter API returned no usable stream events")
	}

	finishReason := "end_turn"
	if toolCallCount > 0 {
		finishReason = "tool_use"
	}
	emit(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: finishReason, Usage: usage},
	}})
	return nil
}

func normalizePuterStreamLine(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(strings.ToLower(line), "data:") {
		line = strings.TrimSpace(line[5:])
	}
	if line == "[DONE]" {
		return ""
	}
	return line
}

func boundedPreview(line string) string {
	line = strings.ToValidUTF8(strings.TrimSpace(line), "�")
	if len(line) > 256 {
		return line[:256] + "…"
	}
	return line
}

// puterStreamError puter 常把 HTTP 状态码拼在 message 开头（如 "400 {json}"），
// 改写为 status=xxx 让核心分类器正确归类（400→client 不可重试、429→rate_limit、401→auth）。
func puterStreamError(field errorField, line string) error {
	message := strings.TrimSpace(field.Message)
	if field.Payload != nil {
		parts := make([]string, 0, 4)
		if code := strings.TrimSpace(field.Payload.Code); code != "" {
			parts = append(parts, "code="+code)
		}
		if field.Payload.Status > 0 {
			parts = append(parts, fmt.Sprintf("status=%d", field.Payload.Status))
		}
		if msg := strings.TrimSpace(field.Payload.Message); msg != "" {
			parts = append(parts, "message="+msg)
		}
		if len(parts) > 0 {
			message = strings.Join(parts, ", ")
		}
	}
	if message == "" {
		message = line
	}
	if code, rest := splitLeadingHTTPStatus(message); code != "" {
		if rest == "" {
			return fmt.Errorf("puter stream error: status=%s", code)
		}
		return fmt.Errorf("puter stream error: status=%s, message=%s", code, rest)
	}
	return fmt.Errorf("puter stream error: %s", message)
}

// splitLeadingHTTPStatus 识别消息开头的三位 HTTP 状态码（仅当前三位数字且后跟空格/冒号）。
func splitLeadingHTTPStatus(message string) (code, rest string) {
	if len(message) < 3 {
		return "", message
	}
	for i := 0; i < 3; i++ {
		if message[i] < '0' || message[i] > '9' {
			return "", message
		}
	}
	if len(message) == 3 {
		return message, ""
	}
	if message[3] == ' ' || message[3] == ':' {
		return message[:3], strings.TrimSpace(message[4:])
	}
	return "", message
}

// normalizeStreamToolInput 递归解包 tool_use input（最多 4 层）：
// 已展开对象原样返回；整体是 JSON 字符串去掉一层引号；OpenAI 风格 {"arguments":"..."} 解包。
func normalizeStreamToolInput(raw json.RawMessage) string {
	return normalizeStreamToolInputDepth(string(raw), 4)
}

func normalizeStreamToolInputDepth(input string, depth int) string {
	if depth <= 0 {
		return strings.TrimSpace(input)
	}
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	var text string
	if json.Unmarshal([]byte(trimmed), &text) == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return "{}"
		}
		return normalizeStreamToolInputDepth(text, depth-1)
	}
	if inner, ok := unwrapOpenAIToolArguments(trimmed); ok {
		return normalizeStreamToolInputDepth(inner, depth-1)
	}
	return trimmed
}

// unwrapOpenAIToolArguments 识别 {"arguments":"<json-string>"} 包装；仅当值是字符串且合法 JSON 时解包。
func unwrapOpenAIToolArguments(input string) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &obj) != nil {
		return "", false
	}
	rawArgs, ok := obj["arguments"]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(rawArgs, &s) != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return "{}", true
	}
	if !json.Valid([]byte(s)) {
		return "", false
	}
	return s, true
}

// normalizePuterUsage 上游 usage → 统一 Usage（缓存单列，Anthropic 语义）。
func normalizePuterUsage(raw map[string]interface{}) *pb.Usage {
	if len(raw) == 0 {
		return nil
	}
	input, hasInput := firstUsageInt(raw, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens")
	output, hasOutput := firstUsageInt(raw, "outputTokens", "output_tokens", "completionTokens", "completion_tokens")
	if !hasInput && !hasOutput {
		return nil
	}
	usage := &pb.Usage{}
	if hasInput {
		usage.InputTokens = int64(input)
	}
	if hasOutput {
		usage.OutputTokens = int64(output)
	}
	if cached, ok := firstUsageInt(raw, "cachedTokens", "cached_tokens", "prompt_cache_hit_tokens"); ok {
		usage.CachedTokens = int64(cached)
	}
	return usage
}

func firstUsageInt(values map[string]interface{}, keys ...string) (int, bool) {
	for _, key := range keys {
		switch typed := values[key].(type) {
		case float64:
			return int(typed), true
		case int:
			return typed, true
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// puterRequest 驱动调用请求体。
type puterRequest struct {
	Interface string       `json:"interface"`
	Service   string       `json:"service"`
	TestMode  bool         `json:"test_mode"`
	Method    string       `json:"method"`
	Args      puterReqArgs `json:"args"`
	AuthToken string       `json:"auth_token"`
}

type puterReqArgs struct {
	Messages          []puterMessage `json:"messages"`
	Model             string         `json:"model"`
	Stream            bool           `json:"stream"`
	Tools             []interface{}  `json:"tools,omitempty"`
	ToolChoice        interface{}    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string         `json:"reasoning_effort,omitempty"`
	EnableThinking    *bool          `json:"enable_thinking,omitempty"`
}

type puterMessage struct {
	Role             string      `json:"role"`
	Content          string      `json:"content"`
	ReasoningContent string      `json:"reasoning_content,omitempty"`
	ToolCalls        []puterTool `json:"tool_calls,omitempty"`
	ToolCallID       string      `json:"tool_call_id,omitempty"`
}

type puterTool struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function puterToolFunc `json:"function"`
}

type puterToolFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	puterReq, err := p.buildPuterRequest(req, c)
	if err != nil {
		return stream.Send(shared.Failed(400, err.Error()))
	}
	body, err := json.Marshal(puterReq)
	if err != nil {
		return stream.Send(shared.Failed(500, "marshal puter request: "+err.Error()))
	}

	resp, err := p.doChatRequest(ctx, c, body)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer (*resp).Close()

	return consumePuterStream(*resp, func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
}

// doChatRequest 发送驱动调用（限速 + Content-Type: text/plain;actually=json）。
func (p *plugin) doChatRequest(ctx context.Context, c *credential, body []byte) (*io.ReadCloser, error) {
	if err := waitForPuterRequestSlot(ctx, c.AuthToken); err != nil {
		return nil, fmt.Errorf("puter request pacing interrupted: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", puterAPIURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/x-ndjson, application/json")
	httpReq.Header.Set("Content-Type", "text/plain;actually=json")

	resp, err := p.hcFor(c).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send puter request: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return &resp.Body, nil
	}
	defer resp.Body.Close()
	raw := shared.ReadLimitedResp(resp, 8192)
	code := int32(502)
	switch resp.StatusCode {
	case 401, 403:
		code = 401
	case 429:
		code = 429
	}
	return nil, fmt.Errorf("puter API error: status=%d, body=%s (code=%d)", resp.StatusCode, shared.Truncate(string(raw), 300), code)
}

// buildPuterRequest 信封 → 驱动调用请求体。
func (p *plugin) buildPuterRequest(req *pb.ChatRequest, c *credential) (*puterRequest, error) {
	modelID := strings.TrimSpace(req.GetModel())
	if modelID == "" {
		return nil, fmt.Errorf("model is required")
	}
	service, err := serviceForModel(modelID)
	if err != nil {
		return nil, err
	}
	tools := normalizeToolDefinitions(req.GetTools())
	var toolChoice interface{}
	var parallel *bool
	if len(tools) > 0 {
		if req.GetToolChoice() != nil {
			toolChoice = normalizePuterToolChoice(req.GetToolChoice())
		}
		parallel = parseParallelToolCalls(req)
	}
	echoReasoning := service == "deepseek"
	msgs := convertMessages(req.GetMessages(), echoReasoning)
	effort, thinking := normalizePuterReasoning(req, service)
	if service == "deepseek" {
		// DeepSeek 严格角色序列：合并相邻 assistant + 拆多 tool_call
		msgs = mergeAdjacentAssistantMessages(msgs)
		msgs = splitMultiToolCalls(msgs)
	}
	return &puterRequest{
		Interface: defaultIface,
		Service:   service,
		TestMode:  false,
		Method:    defaultMethod,
		Args: puterReqArgs{
			Messages:          msgs,
			Model:             modelID,
			Stream:            true,
			Tools:             tools,
			ToolChoice:        toolChoice,
			ParallelToolCalls: parallel,
			ReasoningEffort:   effort,
			EnableThinking:    thinking,
		},
		AuthToken: c.AuthToken,
	}, nil
}

// convertMessages 信封 → puter 消息（system 独立消息；tool_use/tool_result 配对）。
func convertMessages(messages []*pb.EnvelopeMessage, echoReasoning bool) []puterMessage {
	out := make([]puterMessage, 0, len(messages))
	pendingToolCalls := make(map[string]bool)
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.GetRole()))
		if role == "" {
			role = "user"
		}
		if role == "assistant" {
			converted := convertAssistantMessage(msg, echoReasoning)
			for _, call := range converted.ToolCalls {
				pendingToolCalls[call.ID] = true
			}
			if converted.Content != "" || len(converted.ToolCalls) > 0 {
				out = append(out, converted)
			}
			continue
		}
		if role == "tool" {
			id := strings.TrimSpace(msg.GetToolCallId())
			if id == "" || !pendingToolCalls[id] {
				continue
			}
			delete(pendingToolCalls, id)
			out = append(out, puterMessage{
				Role:       "tool",
				ToolCallID: id,
				Content:    strings.TrimSpace(msg.GetText()),
			})
			continue
		}
		if text := strings.TrimSpace(msg.GetText()); text != "" {
			out = append(out, puterMessage{Role: role, Content: text})
		}
	}
	return out
}

// convertAssistantMessage assistant 消息（文本 + tool_calls 汇总）。
func convertAssistantMessage(msg *pb.EnvelopeMessage, echoReasoning bool) puterMessage {
	message := puterMessage{Role: "assistant", Content: strings.TrimSpace(msg.GetText())}
	if echoReasoning {
		message.ReasoningContent = assistantReasoning(msg)
		if message.ReasoningContent == "" {
			message.ReasoningContent = missingDeepSeekReasoningFallback
		}
	}
	for _, call := range msg.GetToolCalls() {
		id := strings.TrimSpace(call.GetId())
		if id == "" {
			id = newToolCallID()
		}
		message.ToolCalls = append(message.ToolCalls, puterTool{
			ID:   id,
			Type: "function",
			Function: puterToolFunc{
				Name:      strings.TrimSpace(call.GetName()),
				Arguments: compactToolInput(call.GetArguments()),
			},
		})
	}
	return message
}

// compactToolInput 工具参数压缩为单行 JSON（失败原样透传）。
func compactToolInput(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "{}"
	}
	var value interface{}
	if json.Unmarshal([]byte(trimmed), &value) != nil {
		return trimmed
	}
	compact, err := json.Marshal(value)
	if err != nil {
		return trimmed
	}
	return string(compact)
}

// normalizeToolDefinitions 信封工具 → OpenAI function 形态（puter 驱动通用）。
func normalizeToolDefinitions(tools []*pb.ToolDefinition) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.GetName())
		if name == "" {
			continue
		}
		parameters := json.RawMessage(tool.GetParametersSchema())
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": tool.GetDescription(),
				"parameters":  parameters,
			},
		})
	}
	return out
}

// normalizePuterToolChoice 信封 tool_choice → puter 形态。
func normalizePuterToolChoice(choice *pb.ToolChoice) interface{} {
	switch strings.ToLower(strings.TrimSpace(choice.GetType())) {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "tool":
		name := strings.TrimSpace(choice.GetToolName())
		if name == "" {
			return nil
		}
		return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": name}}
	}
	return nil
}

// serviceForModel 模型 id → 上游 service（目录宣传的标识前缀决定路由）。
func serviceForModel(modelID string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if namespace, _, ok := strings.Cut(id, ":"); ok {
		switch strings.TrimSpace(namespace) {
		case "openrouter", "infron", "alibaba", "togetherai", "deepinfra", "replicate", "deepseek", "mistral":
			return strings.TrimSpace(namespace), nil
		case "google", "gemini":
			return "google", nil
		case "openai":
			return "openai", nil
		case "anthropic", "claude":
			return "claude", nil
		case "xai", "x-ai":
			return "x-ai", nil
		}
	}
	switch {
	case strings.HasPrefix(id, "claude-"):
		return "claude", nil
	case strings.HasPrefix(id, "gpt-"):
		return "openai", nil
	case strings.HasPrefix(id, "gemini-"), strings.HasPrefix(id, "gemma-"):
		return "google", nil
	case strings.HasPrefix(id, "grok-"):
		return "x-ai", nil
	case strings.HasPrefix(id, "deepseek-"):
		return "deepseek", nil
	case strings.HasPrefix(id, "mistral-"):
		return "mistral", nil
	default:
		return "", fmt.Errorf("puter model %q has no configured service", modelID)
	}
}

var _ = bufio.NewScanner // 保留：parser scan 使用 bufio
