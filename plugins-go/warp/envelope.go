// request.go — 统一信封 → warpapi.Request protobuf（stateless transcript 渲染 + 工具上下文）。
package warp

import (
	"fmt"
	"runtime"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// 无服务器会话 id 时，渲染的 transcript 就是会话本身；上限只约束单个 protobuf 请求体。
const statelessHistoryMaxChars = 8 << 20

// buildWarpRequest 将统一信封转换为 Warp 请求。
func buildWarpRequest(req *pb.ChatRequest) (string, []byte, error) {
	query := buildWarpUserQuery(req)
	tools := req.GetTools()
	if req.GetToolChoice().GetType() == "none" {
		tools = nil
	}
	mcpContext, err := buildMCPContext(tools)
	if err != nil {
		return "", nil, err
	}
	input, inputCount := buildRequestInput(query, req.GetMessages())
	if strings.TrimSpace(query) == "" && inputCount == 0 {
		return "", nil, fmt.Errorf("empty warp prompt")
	}

	disableWarpTools := mcpContext == nil
	apiReq := warpapi.Request_builder{
		TaskContext: warpapi.Request_TaskContext_builder{}.Build(),
		Input:       input,
		Settings:    buildRequestSettings(req, disableWarpTools),
		Metadata:    buildRequestMetadata(req),
	}.Build()
	if mcpContext != nil {
		apiReq.SetMcpContext(mcpContext)
	}

	payload, err := proto.Marshal(apiReq)
	if err != nil {
		return "", nil, err
	}
	return query, payload, nil
}

// buildWarpUserQuery 渲染用户查询：无会话 id 时渲染完整 stateless transcript。
func buildWarpUserQuery(req *pb.ChatRequest) string {
	messages := req.GetMessages()
	if isServerConversationID(conversationID(req)) {
		return latestWarpUserInput(messages)
	}
	return renderWarpStatelessTranscript(messages)
}

func conversationID(req *pb.ChatRequest) string {
	if raw, ok := req.GetExtra()["conversation_id"]; ok {
		return strings.TrimSpace(raw)
	}
	return ""
}

func isServerConversationID(id string) bool {
	return id != "" && !strings.HasPrefix(id, "chat_")
}

func renderWarpStatelessTranscript(messages []*pb.EnvelopeMessage) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if rendered := renderWarpTranscriptMessage(message); rendered != "" {
			parts = append(parts, rendered)
		}
	}
	if len(parts) == 0 {
		return ""
	}

	// 长会话优先保留最近轮次。
	start := 0
	systemPart := ""
	if strings.HasPrefix(parts[0], "Instructions:") {
		systemPart, start = parts[0], 1
	}
	selected := make([]string, 0, len(parts)-start)
	used := len(systemPart)
	for i := len(parts) - 1; i >= start; i-- {
		part := parts[i]
		if used+len(part)+2 > statelessHistoryMaxChars && len(selected) > 0 {
			break
		}
		selected = append(selected, part)
		used += len(part) + 2
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	if len(selected) < len(parts)-start {
		selected = append([]string{"[Earlier conversation omitted for length]"}, selected...)
	}
	if systemPart != "" {
		selected = append([]string{systemPart}, selected...)
	}
	return strings.Join(selected, "\n\n")
}

func renderWarpTranscriptMessage(message *pb.EnvelopeMessage) string {
	role := strings.ToLower(strings.TrimSpace(message.GetRole()))
	if role == "" {
		role = "user"
	}
	if role == "system" {
		return "Instructions:\n" + sanitizeUTF8(strings.TrimSpace(message.GetText()))
	}
	parts := make([]string, 0, 2)
	if text := sanitizeUTF8(strings.TrimSpace(message.GetText())); text != "" {
		parts = append(parts, text)
	}
	for _, call := range message.GetToolCalls() {
		if name := strings.TrimSpace(call.GetName()); name != "" {
			parts = append(parts, fmt.Sprintf("tool call %s (%s)", name, sanitizeUTF8(strings.TrimSpace(call.GetArguments()))))
		}
	}
	if message.GetRole() == "tool" || message.GetToolCallId() != "" {
		role = "tool result " + message.GetToolCallId()
	}
	if len(parts) == 0 {
		return ""
	}
	return role + ":\n" + strings.Join(parts, "\n")
}

func latestWarpUserInput(messages []*pb.EnvelopeMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(messages[i].GetRole()))
		if role != "user" && role != "tool" {
			continue
		}
		return sanitizeUTF8(strings.TrimSpace(messages[i].GetText()))
	}
	return ""
}

func sanitizeUTF8(text string) string {
	return strings.ToValidUTF8(text, "")
}

// ---------- 请求输入 ----------

func buildRequestInput(query string, messages []*pb.EnvelopeMessage) (*warpapi.Request_Input, int) {
	resultBlocks := latestWarpToolResults(messages)
	inputs := make([]*warpapi.Request_Input_UserInputs_UserInput, 0, len(resultBlocks)+1)
	for _, block := range resultBlocks {
		if result := buildWarpToolResult(block); result != nil {
			inputs = append(inputs, warpapi.Request_Input_UserInputs_UserInput_builder{ToolCallResult: result}.Build())
		}
	}
	if strings.TrimSpace(query) != "" {
		inputs = append(inputs, buildWarpUserQueryInput(query))
	}
	return warpapi.Request_Input_builder{
		Context:    buildInputContext(),
		UserInputs: warpapi.Request_Input_UserInputs_builder{Inputs: inputs}.Build(),
	}.Build(), len(inputs)
}

func buildWarpUserQueryInput(query string) *warpapi.Request_Input_UserInputs_UserInput {
	agent := warpapi.AgentType_AGENT_TYPE_PRIMARY
	userQuery := warpapi.Request_Input_UserQuery_builder{
		Query:         stringPtr(query),
		Mode:          warpapi.UserQueryMode_builder{}.Build(),
		IntendedAgent: &agent,
	}.Build()
	return warpapi.Request_Input_UserInputs_UserInput_builder{UserQuery: userQuery}.Build()
}

// latestWarpToolResults 取最近一轮未回应的 tool_result（id 去重，保序）。
func latestWarpToolResults(messages []*pb.EnvelopeMessage) []*pb.EnvelopeMessage {
	var out []*pb.EnvelopeMessage
	seen := make(map[string]bool)
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.GetRole() != "tool" {
			break
		}
		id := strings.TrimSpace(m.GetToolCallId())
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---------- 工具结果转换 ----------

func buildWarpToolResult(msg *pb.EnvelopeMessage) *warpapi.Request_Input_ToolCallResult {
	id := strings.TrimSpace(msg.GetToolCallId())
	if id == "" {
		return nil
	}
	payload := sanitizeUTF8(strings.TrimSpace(msg.GetText()))
	if payload == "" {
		var raw string
		for _, part := range msg.GetParts() {
			if part.GetType() == "text" {
				raw += part.GetText()
			}
		}
		payload = sanitizeUTF8(strings.TrimSpace(raw))
	}
	builder := warpapi.Request_Input_ToolCallResult_builder{ToolCallId: stringPtr(id)}
	if msg.GetToolError() {
		// 将工具执行失败编码为 MCP 错误。
		builder.CallMcpTool = warpapi.CallMCPToolResult_builder{
			Error: warpapi.CallMCPToolResult_Error_builder{Message: stringPtr(payload)}.Build(),
		}.Build()
		return builder.Build()
	}
	builder.CallMcpTool = warpapi.CallMCPToolResult_builder{
		Success: warpapi.CallMCPToolResult_Success_builder{
			Results: []*warpapi.CallMCPToolResult_Success_Result{
				warpapi.CallMCPToolResult_Success_Result_builder{
					Text: warpapi.CallMCPToolResult_Success_Result_Text_builder{Text: stringPtr(payload)}.Build(),
				}.Build(),
			},
		}.Build(),
	}.Build()
	return builder.Build()
}

// ---------- 输入上下文 / 设置 ----------

func buildInputContext() *warpapi.InputContext {
	return warpapi.InputContext_builder{
		Directory: warpapi.InputContext_Directory_builder{Pwd: stringPtr(""), Home: stringPtr("")}.Build(),
		OperatingSystem: warpapi.InputContext_OperatingSystem_builder{
			Platform: stringPtr(osCategory()), Distribution: stringPtr(""),
		}.Build(),
		Shell:       warpapi.InputContext_Shell_builder{Name: stringPtr(defaultShellName()), Version: stringPtr("")}.Build(),
		CurrentTime: timestamppb.Now(),
	}.Build()
}

func defaultShellName() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "zsh"
}

func buildRequestSettings(req *pb.ChatRequest, disableTools bool) *warpapi.Request_Settings {
	toolsEnabled := !disableTools
	parallelTools := toolsEnabled && req.GetExtra()["parallel_tool_calls"] != "false"
	autonomy := warpapi.AutonomyLevel_SUPERVISED
	isolation := warpapi.IsolationLevel_NONE
	supportedTools := []warpapi.ToolType{warpapi.ToolType_CALL_MCP_TOOL}
	supportedCliTools := warpTextOnlyToolFence
	if !toolsEnabled {
		// 空工具列表表示不限制；禁用时仅声明非执行能力。
		supportedTools = warpTextOnlyToolFence
		supportedCliTools = warpTextOnlyToolFence
	}
	modelConfig := warpapi.Request_Settings_ModelConfig_builder{
		Base:             stringPtr(normalizeWarpModel(req.GetModel())),
		CliAgent:         stringPtr(cliAgentModel),
		ComputerUseAgent: stringPtr(computerUseModel),
	}
	return warpapi.Request_Settings_builder{
		ModelConfig:                                modelConfig.Build(),
		WebContextRetrievalEnabled:                 boolPtr(false),
		SupportsParallelToolCalls:                  boolPtr(parallelTools),
		UseAnthropicTextEditorTools:                boolPtr(false),
		PlanningEnabled:                            boolPtr(false),
		WarpDriveContextEnabled:                    boolPtr(false),
		SupportsCreateFiles:                        boolPtr(false),
		SupportedTools:                             supportedTools,
		SupportsLongRunningCommands:                boolPtr(false),
		ShouldPreserveFileContentInHistory:         boolPtr(true),
		SupportsTodosUi:                            boolPtr(false),
		SupportsLinkedCodeBlocks:                   boolPtr(false),
		SupportsStartedChildTaskMessage:            boolPtr(false),
		SupportsSuggestPrompt:                      boolPtr(false),
		SupportsReadImageFiles:                     boolPtr(false),
		SupportsReasoningMessage:                   boolPtr(true),
		AutonomyLevel:                              &autonomy,
		IsolationLevel:                             &isolation,
		WebSearchEnabled:                           boolPtr(false),
		SupportedCliAgentTools:                     supportedCliTools,
		SupportsV4AFileDiffs:                       boolPtr(false),
		SupportsSummarizationViaMessageReplacement: boolPtr(false),
		SupportsBundledSkills:                      boolPtr(false),
		SupportsResearchAgent:                      boolPtr(false),
		SupportsOrchestrationV2:                    boolPtr(false),
	}.Build()
}

func buildRequestMetadata(req *pb.ChatRequest) *warpapi.Request_Metadata {
	builder := warpapi.Request_Metadata_builder{}
	if id := conversationID(req); isServerConversationID(id) {
		builder.ConversationId = stringPtr(id)
	}
	return builder.Build()
}

// 原生终端工具需要专有结果和会话上下文；插件将客户端工具统一声明为 MCP。
var warpTextOnlyToolFence = []warpapi.ToolType{warpapi.ToolType_SUGGEST_PROMPT}

func buildMCPContext(tools []*pb.ToolDefinition) (*warpapi.Request_MCPContext, error) {
	var mcpTools []*warpapi.Request_MCPContext_MCPTool
	seen := make(map[string]bool)
	for _, tool := range tools {
		name := strings.TrimSpace(tool.GetName())
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		var schema *structpb.Struct
		if raw := strings.TrimSpace(tool.GetParametersSchema()); raw != "" {
			schema = &structpb.Struct{}
			if err := protojson.Unmarshal([]byte(raw), schema); err != nil {
				return nil, fmt.Errorf("tool %q schema: %w", name, err)
			}
		}
		mcpTools = append(mcpTools, warpapi.Request_MCPContext_MCPTool_builder{
			Name: stringPtr(name), Description: stringPtr(strings.TrimSpace(tool.GetDescription())), InputSchema: schema,
		}.Build())
	}
	if len(mcpTools) == 0 {
		return nil, nil
	}
	server := warpapi.Request_MCPContext_MCPServer_builder{
		Name: stringPtr("client"), Description: stringPtr("Tools declared by the client request"),
		Id: stringPtr("client-request-tools"), Tools: mcpTools,
	}.Build()
	return warpapi.Request_MCPContext_builder{Servers: []*warpapi.Request_MCPContext_MCPServer{server}}.Build(), nil
}

// ---------- 模型归一化 ----------

func normalizeWarpModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch model {
	case "", "auto", "auto-efficient", "auto-genius":
		return defaultModel
	default:
		return model
	}
}

func stringPtr(value string) *string { return &value }
func boolPtr(value bool) *bool       { return &value }
