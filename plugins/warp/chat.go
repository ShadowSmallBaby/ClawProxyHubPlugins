// chat.go — 对话入口 + 上游 SSE POST + warp protobuf 帧解析 → StreamEvent。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// warpStreamParser 消费 SSE 帧流，翻成 StreamEvent 后 emit。
type warpStreamParser struct {
	state    *warpStreamState
	emit     func(*pb.StreamEvent)
	sawData  bool
	finished bool
	sendErr  error
}

func newWarpStreamParser(emit func(*pb.StreamEvent)) *warpStreamParser {
	return &warpStreamParser{state: newWarpStreamState(), emit: emit}
}

// scan 逐行喂 SSE；流结束（空行）时 flush 累积帧。
func (p *warpStreamParser) scan(body io.Reader) error {
	var data strings.Builder
	reader := bufio.NewReaderSize(body, 64*1024)
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		frame := data.String()
		data.Reset()
		p.sawData = true
		payload, err := decodeWarpPayload(frame)
		if err != nil {
			return fmt.Errorf("decode warp SSE payload: %w", err)
		}
		done, err := p.emitPayload(payload)
		if err != nil {
			return err
		}
		if p.sendErr != nil {
			return p.sendErr
		}
		if done {
			p.finished = true
		}
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		atEOF := errors.Is(err, io.EOF)
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if flushErr := flush(); flushErr != nil {
				return flushErr
			}
			if p.finished {
				return nil
			}
			if atEOF {
				break
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			if atEOF {
				if err := flush(); err != nil {
					return err
				}
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(line[5:]))
		}
		if atEOF {
			if err := flush(); err != nil {
				return err
			}
			break
		}
	}

	if !p.sawData {
		return fmt.Errorf("warp stream ended without any SSE data events")
	}
	if !p.finished {
		return fmt.Errorf("warp SSE stream ended without StreamFinished event")
	}
	return nil
}

// decodeWarpPayload 兼容 base64 url/std 两种编码。
func decodeWarpPayload(data string) ([]byte, error) {
	if data == "" {
		return nil, fmt.Errorf("empty payload")
	}
	encoding := base64.RawURLEncoding
	if strings.ContainsAny(data, "+/") {
		encoding = base64.RawStdEncoding
		if strings.HasSuffix(data, "=") {
			encoding = base64.StdEncoding
		}
	} else if strings.HasSuffix(data, "=") {
		encoding = base64.URLEncoding
	}
	return encoding.DecodeString(data)
}

// emitPayload 解析一帧 protobuf 并翻成 StreamEvent；返回是否已收到 StreamFinished。
func (p *warpStreamParser) emitPayload(frame []byte) (bool, error) {
	var event warpapi.ResponseEvent
	if err := proto.Unmarshal(frame, &event); err != nil {
		return false, fmt.Errorf("decode warp response event: %w", err)
	}
	if !event.HasType() {
		return false, nil
	}
	switch event.WhichType() {
	case warpapi.ResponseEvent_Init_case:
		// conversation/request id 由插件端直传，Init 只需确认流已建立
		return false, nil
	case warpapi.ResponseEvent_ClientActions_case:
		for _, action := range event.GetClientActions().GetActions() {
			p.applyClientAction(action)
		}
		return false, nil
	case warpapi.ResponseEvent_Finished_case:
		finished := event.GetFinished()
		usage := parseWarpUsage(finished)
		if err := terminalError(finished); err != nil {
			p.emit(shared.Failed(shared.ErrorStatus(err), err.Error()))
			return true, nil
		}
		p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
			MessageFinish: &pb.MessageFinish{
				FinishReason: warpFinishReason(finished, p.state.finishReason()),
				Usage:        usage,
			},
		}})
		return true, nil
	}
	return false, nil
}

// applyClientAction 将内容更新转换为去重后的增量事件。
func (p *warpStreamParser) applyClientAction(action *warpapi.ClientAction) {
	if action == nil {
		return
	}
	switch action.WhichAction() {
	case warpapi.ClientAction_CreateTask_case:
		p.appendWarpMessages(action.GetCreateTask().GetTask().GetMessages(), true)
	case warpapi.ClientAction_AddMessagesToTask_case:
		p.appendWarpMessages(action.GetAddMessagesToTask().GetMessages(), true)
	case warpapi.ClientAction_UpdateTaskMessage_case:
		p.appendWarpMessage(action.GetUpdateTaskMessage().GetMessage(), true)
	case warpapi.ClientAction_AppendToMessageContent_case:
		p.appendWarpMessage(action.GetAppendToMessageContent().GetMessage(), false)
	}
}

// appendWarpMessages snapshot=true 时内容为完整值（Update）；false 为增量（Append）。
func (p *warpStreamParser) appendWarpMessages(messages []*warpapi.Message, snapshot bool) {
	for _, message := range messages {
		p.appendWarpMessage(message, snapshot)
	}
}

func (p *warpStreamParser) appendWarpMessage(message *warpapi.Message, snapshot bool) {
	if message == nil {
		return
	}
	switch message.WhichMessage() {
	case warpapi.Message_AgentOutput_case:
		p.emitContent(message.GetId(), message.GetAgentOutput().GetText(), false, snapshot)
	case warpapi.Message_AgentReasoning_case:
		p.emitContent(message.GetId(), message.GetAgentReasoning().GetReasoning(), true, snapshot)
	case warpapi.Message_ToolCall_case:
		if call, ok := parseWarpToolCall(message.GetToolCall()); ok {
			if p.state.acceptToolCall(call.id) {
				p.state.sawToolCall = true
				p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{
					ToolCallDelta: &pb.ToolCallDelta{Id: call.id, Name: call.name, ArgumentsDelta: call.input},
				}})
			}
		}
	}
}

// emitContent 对完整快照去重，仅发送新增内容。
func (p *warpStreamParser) emitContent(messageID, text string, reasoning, snapshot bool) {
	if text == "" {
		return
	}
	delta := p.state.applyContentUpdate(messageID, text, reasoning, snapshot)
	if delta == "" {
		return
	}
	if reasoning {
		p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ReasoningDelta{
			ReasoningDelta: &pb.ReasoningDelta{Text: delta},
		}})
	} else {
		p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
			ContentDelta: &pb.ContentDelta{Text: delta},
		}})
	}
}

// ---------- 流内状态 ----------

type warpStreamState struct {
	sawToolCall   bool
	textByMessage map[string]*strings.Builder
	reasoning     map[string]*strings.Builder
	seenToolCalls map[string]struct{}
}

func newWarpStreamState() *warpStreamState {
	return &warpStreamState{
		textByMessage: make(map[string]*strings.Builder),
		reasoning:     make(map[string]*strings.Builder),
		seenToolCalls: make(map[string]struct{}),
	}
}

func (s *warpStreamState) applyContentUpdate(messageID, text string, reasoning, snapshot bool) string {
	key := strings.TrimSpace(messageID)
	if key == "" {
		key = "primary"
	}
	values := s.textByMessage
	if reasoning {
		values = s.reasoning
	}
	buffer := values[key]
	if buffer == nil {
		buffer = &strings.Builder{}
		values[key] = buffer
	}
	current := buffer.String()
	if !snapshot {
		buffer.WriteString(text)
		return text
	}
	switch {
	case current == "":
		buffer.WriteString(text)
		return text
	case text == current, strings.HasPrefix(current, text):
		return ""
	case strings.HasPrefix(text, current):
		buffer.WriteString(text[len(current):])
		return text[len(current):]
	default:
		// 流式不能收回已发文本：保留最新快照，不把冲突替换当重复输出
		buffer.Reset()
		buffer.WriteString(text)
		return ""
	}
}

func (s *warpStreamState) acceptToolCall(id string) bool {
	if strings.TrimSpace(id) == "" {
		return true
	}
	if _, exists := s.seenToolCalls[id]; exists {
		return false
	}
	s.seenToolCalls[id] = struct{}{}
	return true
}

func (s *warpStreamState) finishReason() string {
	if s.sawToolCall {
		return "tool_use"
	}
	return "end_turn"
}

// ---------- 工具调用映射 ----------

type warpToolCall struct {
	id    string
	name  string
	input string
}

func parseWarpToolCall(call *warpapi.Message_ToolCall) (warpToolCall, bool) {
	if call == nil || !call.HasTool() {
		return warpToolCall{}, false
	}
	toolName, toolInput := "", "{}"
	if mcpCall := call.GetCallMcpTool(); mcpCall != nil {
		toolName = mcpCall.GetName()
		if mcpCall.GetArgs() != nil {
			toolInput = marshalToolInput(mcpCall.GetArgs().AsMap())
		}
	} else {
		toolName, toolInput = parseWarpNativeTool(call)
		toolName = normalizeWarpToolName(toolName)
		if isIncompleteToolCall(toolName, toolInput) {
			return warpToolCall{}, false
		}
	}
	if strings.TrimSpace(toolName) == "" {
		return warpToolCall{}, false
	}
	toolID := call.GetToolCallId()
	if toolID == "" {
		toolID = derivedWarpToolCallID(toolName, toolInput)
	}
	return warpToolCall{id: toolID, name: toolName, input: toolInput}, true
}

func normalizeWarpToolName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "write_to_long_running_shell_command":
		return "Bash"
	default:
		return normalizeToolNameFallback(name)
	}
}

func marshalToolInput(input map[string]interface{}) string {
	if len(input) == 0 {
		return "{}"
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func derivedWarpToolCallID(toolName, toolInput string) string {
	input := strings.TrimSpace(toolInput)
	if input == "" {
		input = "{}"
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(toolName))))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(input))
	return fmt.Sprintf("warp_anon_%x", h.Sum64())
}

// isIncompleteToolCall 无关键参数的调用不发出（上游分片未完成的占位）。
func isIncompleteToolCall(toolName, toolInput string) bool {
	key := ""
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "bash":
		key = "command"
	case "read", "write", "edit":
		key = "file_path"
	case "grep", "glob":
		key = "pattern"
	default:
		return false
	}
	var payload map[string]interface{}
	if json.Unmarshal([]byte(toolInput), &payload) != nil {
		return false
	}
	value, _ := payload[key].(string)
	if strings.TrimSpace(value) == "" {
		return true
	}
	if strings.EqualFold(toolName, "edit") {
		_, old := payload["old_string"]
		_, new := payload["new_string"]
		return !old || !new
	}
	return false
}

// parseWarpNativeTool 将原生工具调用转换为名称和参数。
func parseWarpNativeTool(call *warpapi.Message_ToolCall) (string, string) {
	switch {
	case call.GetRunShellCommand() != nil:
		return "Bash", marshalToolInput(map[string]interface{}{"command": call.GetRunShellCommand().GetCommand()})
	case call.GetWriteToLongRunningShellCommand() != nil:
		return "Bash", marshalToolInput(map[string]interface{}{"command": string(call.GetWriteToLongRunningShellCommand().GetInput())})
	case call.GetReadFiles() != nil:
		var files []string
		for _, f := range call.GetReadFiles().GetFiles() {
			if name := strings.TrimSpace(f.GetName()); name != "" {
				files = append(files, name)
			}
		}
		if len(files) == 1 {
			return "Read", marshalToolInput(map[string]interface{}{"file_path": files[0]})
		}
	case call.GetGrep() != nil:
		c := call.GetGrep()
		if len(c.GetQueries()) == 1 {
			return "Grep", marshalToolInput(map[string]interface{}{"pattern": c.GetQueries()[0], "path": c.GetPath()})
		}
	case call.GetFileGlob() != nil:
		c := call.GetFileGlob()
		if len(c.GetPatterns()) == 1 {
			return "Glob", marshalToolInput(map[string]interface{}{"pattern": c.GetPatterns()[0], "path": c.GetPath()})
		}
	case call.GetFileGlobV2() != nil:
		c := call.GetFileGlobV2()
		if len(c.GetPatterns()) == 1 {
			return "Glob", marshalToolInput(map[string]interface{}{"pattern": c.GetPatterns()[0], "path": c.GetSearchDir()})
		}
	case call.GetApplyFileDiffs() != nil:
		c := call.GetApplyFileDiffs()
		if len(c.GetNewFiles())+len(c.GetDiffs()) != 1 {
			break
		}
		if len(c.GetNewFiles()) == 1 {
			f := c.GetNewFiles()[0]
			return "Write", marshalToolInput(map[string]interface{}{"file_path": f.GetFilePath(), "content": f.GetContent()})
		}
		f := c.GetDiffs()[0]
		return "Edit", marshalToolInput(map[string]interface{}{"file_path": f.GetFilePath(), "old_string": f.GetSearch(), "new_string": f.GetReplace()})
	}
	return "", "{}"
}

// normalizeToolNameFallback 将工具别名映射为统一名称。
func normalizeToolNameFallback(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	mapped, ok := toolNameFallbacks[lower]
	if ok {
		return mapped
	}
	return name
}

var toolNameFallbacks = map[string]string{
	"str_replace_editor": "Edit", "edit": "Edit", "apply_file_diffs": "Edit",
	"view": "Read", "readfile": "Read", "read_file": "Read", "read_files": "Read", "read": "Read",
	"listdir": "Glob", "list_dir": "Glob", "list_directory": "Glob", "ls": "Glob", "globtool": "Glob", "glob": "Glob", "find_files": "Glob", "file_glob": "Glob", "file_glob_v2": "Glob",
	"ripgreptool": "Grep", "ripgrep": "Grep", "search_code": "Grep", "search_codebase": "Grep", "grep": "Grep",
	"exec": "Bash", "execute": "Bash", "execute_command": "Bash", "execute-command": "Bash", "run_command": "Bash", "runcommand": "Bash", "launch-process": "Bash", "run_shell_command": "Bash", "shell": "Bash", "bash": "Bash",
	"writefile": "Write", "write_file": "Write", "create_file": "Write", "createfile": "Write", "save-file": "Write", "write": "Write",
	"update_todo_list": "TodoWrite", "todo": "TodoWrite", "todo_write": "TodoWrite", "todowrite": "TodoWrite",
	"web_fetch": "web_fetch", "webfetch": "web_fetch", "fetch": "web_fetch", "builtin_web_fetch": "web_fetch",
	"web_search": "web_search", "websearch": "web_search", "builtin_web_search": "web_search",
	"ask_followup_question": "AskUserQuestion", "ask": "AskUserQuestion",
	"enter_plan_mode": "EnterPlanMode", "exit_plan_mode": "ExitPlanMode",
	"new_task": "Task", "agent": "Task", "subagent": "Task", "subagents": "Task", "spawn_agent": "Task", "spawn_subagent": "Task",
	"task_output": "TaskOutput", "task_stop": "TaskStop",
	"use_skill": "Skill", "skill": "Skill",
}

// ---------- StreamFinished 解析 ----------

// parseWarpUsage 转换用量，缓存读写单独计数。
func parseWarpUsage(finished *warpapi.ResponseEvent_StreamFinished) *pb.Usage {
	input, output, cacheRead, cacheWrite := int64(0), int64(0), int64(0), int64(0)
	for _, usage := range finished.GetTokenUsage() {
		input += int64(usage.GetTotalInput())
		output += int64(usage.GetOutput())
	}
	if charges := finished.GetRequestCharges(); charges != nil {
		ci, co, cr, cw := summarizeRequestCharges(charges)
		if ci+co+cr+cw > 0 {
			input, output, cacheRead, cacheWrite = ci, co, cr, cw
		}
	}
	if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 {
		return nil
	}
	return &pb.Usage{
		InputTokens:         input,
		OutputTokens:        output,
		CachedTokens:        cacheRead,
		CacheCreationTokens: cacheWrite,
	}
}

func summarizeRequestCharges(charges *warpapi.RequestCharges) (input, output, cacheRead, cacheWrite int64) {
	if charges == nil {
		return
	}
	for _, charged := range charges.GetUsageByCategory() {
		if charged == nil {
			continue
		}
		usageSets := []map[string]*warpapi.InferenceUsage{
			charged.GetDirectApiInferenceUsage(),
			charged.GetByokInferenceUsage(),
			charged.GetCustomEndpointInferenceUsage(),
		}
		for _, usages := range usageSets {
			for _, usage := range usages {
				if usage == nil {
					continue
				}
				if count := usage.GetTokenCount(); count != nil {
					input += int64(count.GetInput())
					output += int64(count.GetOutput())
					cacheRead += int64(count.GetInputCacheRead())
					cacheWrite += int64(count.GetInputCacheWrite())
				}
			}
		}
	}
	return
}

// terminalError 将完成原因转换为可分类错误。
func terminalError(finished *warpapi.ResponseEvent_StreamFinished) error {
	if finished == nil {
		return fmt.Errorf("warp stream finished with invalid event")
	}
	switch finished.WhichReason() {
	case warpapi.ResponseEvent_StreamFinished_Done_case,
		warpapi.ResponseEvent_StreamFinished_Other_case,
		warpapi.ResponseEvent_StreamFinished_MaxTokenLimit_case:
		return nil
	case warpapi.ResponseEvent_StreamFinished_QuotaLimit_case:
		return shared.HTTPError{Code: 402, Message: "quota limit"}
	case warpapi.ResponseEvent_StreamFinished_ContextWindowExceeded_case:
		return shared.HTTPError{Code: 400, Message: "context window exceeded"}
	case warpapi.ResponseEvent_StreamFinished_LlmUnavailable_case:
		return fmt.Errorf("warp stream finished with llm_unavailable: model unavailable")
	case warpapi.ResponseEvent_StreamFinished_InternalError_case:
		if msg := finished.GetInternalError().GetMessage(); msg != "" {
			return fmt.Errorf("warp stream finished with internal_error: %s", msg)
		}
		return fmt.Errorf("warp stream finished with internal_error")
	case warpapi.ResponseEvent_StreamFinished_InvalidApiKey_case:
		return shared.HTTPError{Code: 401, Message: "invalid API key"}
	default:
		return nil
	}
}

// ---------- Chat ----------

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	if err := p.ensureFresh(ctx, c); err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	_, payload, err := buildWarpRequest(req)
	if err != nil {
		return stream.Send(shared.Failed(400, err.Error()))
	}

	resp, err := p.doStream(ctx, c, payload)
	if err != nil {
		return stream.Send(shared.Failed(shared.ErrorStatus(err), err.Error()))
	}
	defer resp.Body.Close()
	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{MessageStart: &pb.MessageStart{Model: req.GetModel()}}}); err != nil {
		return err
	}

	var parser *warpStreamParser
	parser = newWarpStreamParser(func(ev *pb.StreamEvent) {
		if parser.sendErr == nil {
			parser.sendErr = stream.Send(ev)
		}
	})
	if err := parser.scan(resp.Body); err != nil {
		if parser.sendErr != nil {
			return parser.sendErr
		}
		return stream.Send(shared.Failed(shared.ErrorStatus(err), err.Error()))
	}
	return nil
}

// doStream 发送 protobuf 请求、拿 SSE。
func (p *plugin) doStream(ctx context.Context, c *credential, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", warpAIURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.AccessToken)
	warpHeaders(httpReq)
	httpReq.Header.Set("X-Warp-Experiment-Id", c.DeviceID)
	httpReq.Header.Set("X-Warp-Experiment-Bucket", experimentBucket(c.DeviceID))
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Accept-Encoding", "identity")

	resp, err := p.hcFor(c).Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw := shared.ReadLimitedResp(resp, 4096)
		resp.Body.Close()
		return nil, shared.HTTPError{Code: int32(resp.StatusCode), Message: shared.Truncate(string(raw), 300)}
	}
	return resp, nil
}

func warpFinishReason(finished *warpapi.ResponseEvent_StreamFinished, fallback string) string {
	if finished.WhichReason() == warpapi.ResponseEvent_StreamFinished_MaxTokenLimit_case {
		return "length"
	}
	return fallback
}
