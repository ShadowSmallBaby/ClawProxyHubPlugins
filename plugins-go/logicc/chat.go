// chat:Chat 编排 — 建会话 → 发提示词 → 解析 AI SDK v6 事件流 → 回吐 StreamEvent。
// 无客户端工具时文本实时下发;有工具时整轮缓冲,结束后按 JSON 信封校验再转工具调用。
package logicc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/requestutil"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	defaultStreamIdle = 120 * time.Second
	maxPromptBytes    = 2 << 20 // 合并后提示词上限
	maxBufferedBytes  = 1 << 20 // 工具模式缓冲上限
)

// streamIdle 从设置读空闲超时(stream_idle_seconds,非法回退 120s)。
func (p *plugin) streamIdle() time.Duration {
	if n := p.settingStr("stream_idle_seconds"); n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			return time.Duration(v) * time.Second
		}
	}
	return defaultStreamIdle
}

// Chat 一次对话:会话 id 由上游创建,随后按 ai-sdk-v6 流式返回事件帧。
func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(failed(401, err.Error()))
	}
	if err := validateParts(req.Messages); err != nil {
		return stream.Send(failed(400, err.Error()))
	}
	plan := resolvePlan(req)

	model, err := p.resolveModel(ctx, cred, req.Model)
	if err != nil {
		return stream.Send(failed(shared.ErrorStatus(err), err.Error()))
	}
	prompt := buildPrompt(req, plan)
	if len(prompt) > maxPromptBytes {
		return stream.Send(failed(413, "合并后的上下文过大,请缩短历史或开始新会话"))
	}

	chatID, err := p.createChat(ctx, cred)
	if err != nil {
		return stream.Send(failed(shared.ErrorStatus(err), err.Error()))
	}
	body := map[string]interface{}{
		"modelId":       model,
		"prompt":        prompt,
		"isVoicePrompt": false,
	}
	if effort := requestutil.ReasoningEffort(req.Extra); effort != "" && effort != "none" {
		body["reasoningEffort"] = effort
	}

	resp, err := p.upstream(ctx, cred, "chats/"+chatID, reqOpts{Method: "POST", Body: body, Stream: true})
	if err != nil {
		return stream.Send(failed(shared.ErrorStatus(err), err.Error()))
	}
	defer resp.Body.Close()

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: model},
	}}); err != nil {
		return err
	}
	return p.pump(ctx, resp.Body, plan, stream)
}

// createChat 建会话。设置了 assistant_id 时按其 systemPrompt 建,否则用上游默认助手。
// 上游返回字符串 id 或 {id},两者都兼容。
func (p *plugin) createChat(ctx context.Context, cred *credential) (string, error) {
	body := map[string]interface{}{}
	if id := p.defaultAssistantID(); id != "" {
		body["assistantId"] = id
	}
	resp, err := p.upstream(ctx, cred, "chats", reqOpts{Method: "POST", Body: body})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", shared.HTTPError{Code: 502, Message: "读取上游会话响应失败"}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", shared.HTTPError{Code: 502, Message: "上游创建会话未返回 ID"}
	}
	if strings.HasPrefix(text, "\"") {
		var id string
		if json.Unmarshal(raw, &id) == nil && id != "" {
			return id, nil
		}
	}
	var payload struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.ID != "" {
		return payload.ID, nil
	}
	return "", shared.HTTPError{Code: 502, Message: "上游创建会话未返回 ID"}
}

// resolveModel 解析实际模型:显式模型 > 设置默认模型 > 上游目录首选(默认标记优先)。
func (p *plugin) resolveModel(ctx context.Context, cred *credential, requested string) (string, error) {
	if m := strings.TrimSpace(requested); m != "" && m != "default" {
		return m, nil
	}
	if m := p.defaultModel(); m != "" {
		return m, nil
	}
	return p.pickDefaultModel(ctx, cred)
}

// pump 解析上游事件帧并驱动客户端流。
func (p *plugin) pump(ctx context.Context, body io.Reader, plan *promptPlan, stream pb.ClawPlugin_ChatServer) error {
	toolMode := len(plan.active) > 0
	var buffered strings.Builder
	var nativeCalls []nativeCall
	nativeIndex := map[string]int{}
	var usage *pb.Usage
	var finishReason string
	sawFinish := false
	contentSent := false

	send := func(ev *pb.StreamEvent) error { return stream.Send(ev) }
	fail := func(code int32, msg string) error { return send(failed(code, msg)) }

	emit := func(frame string) error {
		ev, err := parseFrame(frame)
		if err != nil {
			return fail(502, err.Error())
		}
		if ev == nil {
			return nil
		}
		switch ev.Type {
		case "text-delta":
			delta := shared.OrDefault(ev.Delta, ev.Text)
			if delta == "" {
				return nil
			}
			if toolMode {
				buffered.WriteString(delta)
				if buffered.Len() > maxBufferedBytes {
					return fail(502, "上游输出超过本机代理的 1 MiB 响应限制")
				}
				return nil
			}
			contentSent = true
			return send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{ContentDelta: &pb.ContentDelta{Text: delta}}})
		case "reasoning-delta":
			if ev.Text == "" {
				return nil
			}
			return send(&pb.StreamEvent{Event: &pb.StreamEvent_ReasoningDelta{ReasoningDelta: &pb.ReasoningDelta{Text: ev.Text}}})
		case "tool-input-start", "tool-input-available", "tool-input-delta":
			collectNativeCall(ev, nativeIndex, &nativeCalls, plan)
			return nil
		case "tool-output-available", "tool-output-error":
			// 上游已执行的副作用:标记后不再要求客户端重复执行。
			markNativeExecuted(ev, nativeIndex, nativeCalls)
			return nil
		case "finish", "finish-step":
			sawFinish = true
			if ev.FinishReason != "" {
				finishReason = ev.FinishReason
			}
			if u := usageOf(ev); u != nil {
				usage = u
			}
			return nil
		case "error":
			return fail(502, "上游流错误:"+shared.OrDefault(ev.ErrorText, "未知错误"))
		}
		return nil
	}

	scanErr := p.scanFrames(ctx, body, emit)
	if scanErr != nil {
		return fail(shared.ErrorStatus(scanErr), scanErr.Error())
	}
	if !sawFinish {
		return fail(502, "上游数据流提前结束,未收到 finish 事件")
	}

	reason := normalizeFinishReason(finishReason)
	produced := contentSent
	if toolMode {
		text, calls, err := parseToolOutput(buffered.String(), plan, nativeCalls)
		if err != nil {
			return fail(shared.ErrorStatus(err), err.Error())
		}
		if text != "" {
			if err := send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{ContentDelta: &pb.ContentDelta{Text: text}}}); err != nil {
				return err
			}
			produced = true
		}
		for _, call := range calls {
			if err := sendToolCall(stream, call); err != nil {
				return err
			}
			produced = true
		}
		if len(calls) > 0 {
			reason = "tool_calls"
		}
	}
	if !produced && usage != nil && usage.GetOutputTokens() == 0 {
		// 上游有时回 200 但产出为空:按可重试的 429 上报,避免客户端把空回答当成功。
		return fail(429, "上游返回了空响应(输出 0 token)")
	}
	return send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: reason, Usage: usage},
	}})
}

// collectNativeCall 累积上游原生工具调用(仅声明过的工具,未知工具忽略)。
func collectNativeCall(ev *upstreamEvent, index map[string]int, calls *[]nativeCall, plan *promptPlan) {
	id := shared.OrDefault(ev.ToolCallID, ev.ID)
	if id == "" {
		return
	}
	name := ev.ToolName
	if name != "" && findTool(plan.active, name) == nil {
		return
	}
	i, ok := index[id]
	if !ok {
		i = len(*calls)
		index[id] = i
		*calls = append(*calls, nativeCall{ID: id})
	}
	call := (*calls)[i]
	if name != "" {
		call.Name = name
	}
	switch ev.Type {
	case "tool-input-available":
		if len(ev.Input) > 0 {
			call.Arguments = inputToString(ev.Input)
		}
	case "tool-input-delta":
		call.Arguments += shared.OrDefault(ev.InputTextDelta, ev.Delta)
	}
	(*calls)[i] = call
}

// markNativeExecuted 标记上游已执行该原生调用(工具结果不会再要求客户端回传)。
func markNativeExecuted(ev *upstreamEvent, index map[string]int, calls []nativeCall) {
	id := shared.OrDefault(ev.ToolCallID, ev.ID)
	i, ok := index[id]
	if !ok {
		return
	}
	calls[i].Executed = true
}

// sendToolCall 工具调用 → 事件流。参数整段一次给出:核心据此一次性产出
// chat 的 tool_calls / messages 的 input_json_delta / responses 的 function_call。
func sendToolCall(stream pb.ClawPlugin_ChatServer, call parsedCall) error {
	if call.Kind == "custom" {
		// 自定义工具:输入是原样字符串,按 JSON 字符串编码保证下游可解析。
		raw, _ := json.Marshal(call.Input)
		return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{ToolCallDelta: &pb.ToolCallDelta{
			Id: call.ID, Name: call.Name, ArgumentsDelta: string(raw),
		}}})
	}
	return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{ToolCallDelta: &pb.ToolCallDelta{
		Id: call.ID, Name: call.Name, ArgumentsDelta: call.Arguments,
	}}})
}

// scanFrames 逐帧读上游 SSE(data: 行),idle 只计「无新数据的等待」,触发即中断。
func (p *plugin) scanFrames(ctx context.Context, body io.Reader, emit func(string) error) error {
	idle := p.streamIdle()
	reader, ok := body.(io.ReadCloser)
	if !ok {
		return shared.HTTPError{Code: 502, Message: "上游流不可中断读取"}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stopClose()
	// 看门狗与客户端取消都会结束 ctx,用标志区分,避免把断连误报成上游空闲超时。
	var timedOut atomic.Bool
	timer := time.AfterFunc(idle, func() {
		timedOut.Store(true)
		cancel()
	})
	defer timer.Stop()

	buf := make([]byte, 64*1024)
	var pending string
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			timer.Reset(idle)
			pending += string(buf[:n])
			for {
				frame, rest, found := splitFrame(pending)
				if !found {
					break
				}
				pending = rest
				if e := emit(frame); e != nil {
					return e
				}
			}
			if len(pending) > 1<<20 {
				return shared.HTTPError{Code: 502, Message: "上游返回了无法解析的数据流(帧过大)"}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				if timedOut.Load() {
					return shared.HTTPError{Code: 504, Message: "上游空闲超时:无新数据超过 " + idle.String()}
				}
				return ctx.Err()
			}
			if err != io.EOF {
				return shared.HTTPError{Code: 502, Message: "上游数据流中断: " + err.Error()}
			}
			break
		}
	}
	if strings.TrimSpace(pending) != "" {
		if e := emit(pending); e != nil {
			return e
		}
	}
	return nil
}

// splitFrame 按空行分帧(\r\n\r\n 或 \n\n)。
func splitFrame(s string) (string, string, bool) {
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		return s[:i], s[i+4:], true
	}
	if i := strings.Index(s, "\n\n"); i >= 0 {
		return s[:i], s[i+2:], true
	}
	return "", s, false
}

// parseFrame 一帧 → 事件:拼接所有 data: 行,JSON 解析;空帧与 [DONE] 回 nil。
func parseFrame(frame string) (*upstreamEvent, error) {
	var datas []string
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			datas = append(datas, strings.TrimSpace(v))
		}
	}
	if len(datas) == 0 {
		return nil, nil
	}
	text := strings.TrimSpace(strings.Join(datas, "\n"))
	if text == "" || text == "[DONE]" {
		return nil, nil
	}
	var ev upstreamEvent
	if err := json.Unmarshal([]byte(text), &ev); err != nil {
		return nil, fmt.Errorf("上游返回了无法解析的数据流")
	}
	return &ev, nil
}

// upstreamEvent AI SDK v6 事件帧(按 type 取用字段)。
type upstreamEvent struct {
	Type           string          `json:"type"`
	Text           string          `json:"text"`
	Delta          string          `json:"delta"`
	InputTextDelta string          `json:"inputTextDelta"`
	ToolCallID     string          `json:"toolCallId"`
	ID             string          `json:"id"`
	ToolName       string          `json:"toolName"`
	Input          json.RawMessage `json:"input"`
	FinishReason   string          `json:"finishReason"`
	ErrorText      string          `json:"errorText"`
	Usage          *upstreamUsage  `json:"usage"`
	TotalUsage     *upstreamUsage  `json:"totalUsage"`
}

// upstreamUsage 上游用量:inputTokens/outputTokens 可能是数字或 {total:N}。
type upstreamUsage struct {
	InputTokens  json.RawMessage `json:"inputTokens"`
	OutputTokens json.RawMessage `json:"outputTokens"`
	PromptTokens *int64          `json:"prompt_tokens"`
	OutputLegacy *int64          `json:"completion_tokens"`
}

// usageOf 取事件用量 → 信封 Usage(totalUsage 优先)。
func usageOf(ev *upstreamEvent) *pb.Usage {
	usage := ev.TotalUsage
	if usage == nil {
		usage = ev.Usage
	}
	if usage == nil {
		return nil
	}
	input, okIn := usageTokens(usage.InputTokens, usage.PromptTokens)
	output, okOut := usageTokens(usage.OutputTokens, usage.OutputLegacy)
	if !okIn || !okOut {
		return nil
	}
	return &pb.Usage{InputTokens: input, OutputTokens: output}
}

// usageTokens 解析 token 计数:数字或 {"total":N};缺失回退 legacy。
func usageTokens(raw json.RawMessage, legacy *int64) (int64, bool) {
	if len(raw) > 0 {
		var n int64
		if json.Unmarshal(raw, &n) == nil {
			return n, true
		}
		var obj struct {
			Total *int64 `json:"total"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Total != nil {
			return *obj.Total, true
		}
	}
	if legacy != nil {
		return *legacy, true
	}
	return 0, false
}

// inputToString 上游工具入参 → 参数 JSON 字符串(字符串值去引号取原值)。
func inputToString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// normalizeFinishReason 上游 finish 原因 → 信封原因(未知一律 stop)。
func normalizeFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded":
		return "length"
	case "content_filter":
		return "content_filter"
	case "tool-calls", "tool_calls", "tool_use":
		return "tool_calls"
	}
	return "stop"
}

// failed 失败事件(带可重试标记)。
func failed(code int32, msg string) *pb.StreamEvent {
	return &pb.StreamEvent{Event: &pb.StreamEvent_TaskFailed{TaskFailed: &pb.TaskFailed{
		Error: &pb.Error{Code: code, Message: msg, Retryable: code == 429 || code == 502 || code == 503},
	}}}
}
