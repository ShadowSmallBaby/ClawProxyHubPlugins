// 信封 ↔ CC 私有协议双向转换：信封 → 请求体、CC NDJSON → 信封事件。
package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const maxTokensDefault = 64000
const maxTokensLimit = 200000

var (
	uuidRE         = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	statusPrefixRE = regexp.MustCompile(`^<(\d{3})>`)
	networkFailRE  = regexp.MustCompile(`^(?:network|connection|upstream)[-_\s]?error$`)
)

var toolNameAliases = map[string]string{
	"bash_output":         "shell_output",
	"task_output":         "shell_output",
	"tool_search":         "search_tools",
	"read_multiple_files": "read_file",
}

// ---------- 请求体构建 ----------

// buildCcBody 信封请求 → CC /alpha/generate 请求体（9 键，threadId 由调用方补入）。
func buildCcBody(req *pb.ChatRequest, cfg ccConfig) map[string]interface{} {
	// system 块数组：非最后一块补 \n，保留 cache_control 断点
	var systemBlocks []map[string]interface{}
	var chat []*pb.EnvelopeMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemBlocks = append(systemBlocks, systemTextBlocks(m)...)
		} else {
			chat = append(chat, m)
		}
	}
	for i := 0; i < len(systemBlocks)-1; i++ {
		systemBlocks[i]["text"] = systemBlocks[i]["text"].(string) + "\n"
	}

	// toolCallId → 工具名反查表（tool-result 需带回原工具名）
	toolNameByID := map[string]string{}
	for _, m := range chat {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Id != "" {
					toolNameByID[tc.Id] = tc.Name
				}
			}
		}
	}

	ccMessages := make([]map[string]interface{}, 0, len(chat))
	for _, m := range chat {
		switch m.Role {
		case "user":
			ccMessages = append(ccMessages, map[string]interface{}{"role": "user", "content": ccUserContent(m)})
		case "assistant":
			parts := []map[string]interface{}{}
			// reasoning 必须回传且排最前（CC 校验随历史带回，次序 [reasoning,text,tool-call]）
			for _, p := range m.Parts {
				if p.Type == "thinking" && p.Text != "" {
					parts = append(parts, map[string]interface{}{"type": "reasoning", "text": p.Text})
				}
			}
			if m.Text != "" {
				parts = append(parts, map[string]interface{}{"type": "text", "text": m.Text})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, map[string]interface{}{
					"type": "tool-call", "toolCallId": tc.Id, "toolName": tc.Name,
					"input": rawJSONOr(tc.Arguments, map[string]interface{}{}),
				})
			}
			ccMessages = append(ccMessages, map[string]interface{}{"role": "assistant", "content": parts})
		case "tool":
			ccMessages = append(ccMessages, map[string]interface{}{
				"role": "tool",
				"content": []map[string]interface{}{{
					"type": "tool-result", "toolCallId": m.ToolCallId, "toolName": toolNameByID[m.ToolCallId],
					"output": map[string]interface{}{"type": "text", "value": m.Text},
				}},
			})
		default:
			ccMessages = append(ccMessages, map[string]interface{}{"role": "user", "content": []map[string]interface{}{{"type": "text", "text": m.Text}}})
		}
	}

	// 缓存断点：已有则保留；否则 prompt_cache_key 存在时落在 system 末块
	hasCacheMarker := false
	for _, b := range systemBlocks {
		if b["cache_control"] != nil {
			hasCacheMarker = true
		}
	}
	if pck := req.Extra["prompt_cache_key"]; pck != "" && !hasCacheMarker && len(systemBlocks) > 0 {
		systemBlocks[len(systemBlocks)-1]["cache_control"] = map[string]interface{}{"type": "ephemeral"}
	}

	params := map[string]interface{}{
		"model":      shared.OrDefault(req.Model, "deepseek/deepseek-v4-flash"),
		"messages":   ccMessages,
		"max_tokens": clampMaxTokens(req.MaxTokens),
		"stream":     true,
	}
	if len(systemBlocks) > 0 {
		params["system"] = systemBlocks
	} else if cfg.emptySystemPlaceholder {
		// 空格占位阻止上游注入 ~7.5K token 默认提示词
		params["system"] = []map[string]interface{}{{"type": "text", "text": " "}}
	}
	if v, ok := req.Extra["temperature"]; ok && v != "" {
		params["temperature"] = rawJSONOr(v, nil)
	} else if req.Temperature > 0 {
		params["temperature"] = req.Temperature
	}
	if effort := reasoningEffort(req); effort != "" {
		params["reasoning_effort"] = effort
	}
	// tools 仅 name/description/input_schema，无 type
	tools := make([]map[string]interface{}, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, map[string]interface{}{
			"name": toWireToolName(t.Name), "description": t.Description,
			"input_schema": rawJSONOr(t.ParametersSchema, map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}),
		})
	}
	params["tools"] = tools
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto", "none":
			params["tool_choice"] = map[string]interface{}{"type": tc.Type}
		case "tool":
			if tc.ToolName == "" {
				params["tool_choice"] = map[string]interface{}{"type": "any"}
			} else {
				params["tool_choice"] = map[string]interface{}{"type": "tool", "name": tc.ToolName}
			}
		}
	}
	if v := req.Extra["parallel_tool_calls"]; v != "" {
		params["parallel_tool_calls"] = rawJSONOr(v, nil)
	}

	// config/environment 用 deviceProfile，不泄漏宿主真实信息
	return map[string]interface{}{
		"config": map[string]interface{}{
			"workingDir": cfg.profile.projectDir, "date": time.Now().UTC().Format("2006-01-02"),
			"environment": cfg.profile.platform, "structure": []interface{}{}, "isGitRepo": false,
			"currentBranch": "", "mainBranch": "", "gitStatus": "", "recentCommits": []interface{}{},
		},
		"memory":         nil,
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"mode":           shared.OrDefault(cfg.cliMode, "agent"),
		"params":         params,
	}
}

// systemTextBlocks system 消息 → text 块数组（保留 cache_control）。
func systemTextBlocks(m *pb.EnvelopeMessage) []map[string]interface{} {
	var blocks []map[string]interface{}
	if len(m.Parts) == 0 {
		if m.Text != "" {
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": m.Text})
		}
		return blocks
	}
	for _, p := range m.Parts {
		if p.Type != "text" || (p.Text == "" && p.CacheControl == "") {
			continue
		}
		blk := map[string]interface{}{"type": "text", "text": p.Text}
		if p.CacheControl != "" {
			blk["cache_control"] = rawJSONOr(p.CacheControl, nil)
		}
		blocks = append(blocks, blk)
	}
	return blocks
}

// ccUserContent user 内容块：text + image（CC 格式）。
func ccUserContent(m *pb.EnvelopeMessage) []map[string]interface{} {
	if len(m.Parts) == 0 {
		return []map[string]interface{}{{"type": "text", "text": m.Text}}
	}
	var items []map[string]interface{}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			items = append(items, map[string]interface{}{"type": "text", "text": p.Text})
		case "image":
			url := p.Url
			if url == "" {
				url = "data:" + p.MediaType + ";base64," + p.Data
			}
			item := map[string]interface{}{"type": "image", "image": url}
			if p.MediaType != "" {
				item["mimeType"] = p.MediaType
			}
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		items = append(items, map[string]interface{}{"type": "text", "text": m.Text})
	}
	return items
}

// reasoningEffort 显式 reasoning_effort 优先，否则 Anthropic thinking budget → effort。
func reasoningEffort(req *pb.ChatRequest) string {
	if v := req.Extra["reasoning_effort"]; v != "" {
		return v
	}
	raw := req.Extra["thinking"]
	if raw == "" {
		return ""
	}
	var t struct {
		Type   string `json:"type"`
		Budget int    `json:"budget_tokens"`
	}
	if json.Unmarshal([]byte(raw), &t) != nil || t.Type == "disabled" || t.Type == "none" {
		return ""
	}
	switch {
	case t.Budget >= 10000:
		return "high"
	case t.Budget >= 5000:
		return "medium"
	}
	return "low"
}

// clampMaxTokens 缺省 64000，上限 200000。
func clampMaxTokens(v int32) int {
	if v <= 0 {
		return maxTokensDefault
	}
	if v > maxTokensLimit {
		return maxTokensLimit
	}
	return int(v)
}

func toWireToolName(name string) string {
	if a, ok := toolNameAliases[name]; ok {
		return a
	}
	return name
}

// orderedBody 补 threadId（对话锚点优先于 per-key session；非法 UUID 整键省略）。
func orderedBody(body map[string]interface{}, sessionID, threadKey string) map[string]interface{} {
	threadID := sessionID
	if threadKey != "" {
		threadID = threadKey
	}
	if uuidRE.MatchString(strings.ToLower(threadID)) {
		body["threadId"] = threadID
	}
	return body
}

func rawJSONOr(s string, def interface{}) interface{} {
	if s == "" {
		return def
	}
	var v interface{}
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	return v
}

// ---------- 响应解析 ----------

// ccEvent CC NDJSON 单行事件的公共字段（按 type 取用）。
type ccEvent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Delta        string          `json:"delta"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	Input        json.RawMessage `json:"input"`
	FinishReason string          `json:"finishReason"`
	Usage        *ccUsage        `json:"usage"`
	TotalUsage   *ccUsage        `json:"totalUsage"`
	Error        *ccEventError   `json:"error"`
	Message      string          `json:"message"`
}

type ccEventError struct {
	Message     string `json:"message"`
	Code        string `json:"code"`
	StatusCode  int    `json:"statusCode"`
	IsRetryable bool   `json:"isRetryable"`
}

// ccUsage inputTokens 是「总输入」（含缓存），缓存明细在 inputTokenDetails。
type ccUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens"`
	InputTokenDetails struct {
		NoCacheTokens    int64 `json:"noCacheTokens"`
		CacheReadTokens  int64 `json:"cacheReadTokens"`
		CacheWriteTokens int64 `json:"cacheWriteTokens"`
	} `json:"inputTokenDetails"`
}

// toEnvelope CC 语义 → 信封（Anthropic）语义：input 只计非缓存部分。
// 优先 noCacheTokens，缺失回退减法；outputTokens=0 全清零（防误计费）。
func (u *ccUsage) toEnvelope() *pb.Usage {
	if u == nil {
		return nil
	}
	if u.OutputTokens == 0 {
		return &pb.Usage{}
	}
	cacheRead := u.CachedInputTokens
	if cacheRead == 0 {
		cacheRead = u.InputTokenDetails.CacheReadTokens
	}
	cacheWrite := u.InputTokenDetails.CacheWriteTokens
	input := u.InputTokenDetails.NoCacheTokens
	if input <= 0 {
		input = u.InputTokens - cacheRead - cacheWrite
	}
	if input < 0 {
		input = 0
	}
	return &pb.Usage{
		InputTokens:         input,
		OutputTokens:        u.OutputTokens,
		CachedTokens:        cacheRead,
		CacheCreationTokens: cacheWrite,
	}
}

// Parser CC NDJSON → 信封事件。实现 Feed/Finish/FinishWithError，与 scanNDJSON 兼容。
type Parser struct {
	emit         func(*pb.StreamEvent)
	finishReason string
	usage        *pb.Usage
	sawFinish    bool
	sentFinish   bool
}

func newParser(emit func(*pb.StreamEvent)) *Parser { return &Parser{emit: emit} }

// Feed 处理一行裸 JSON 事件。
func (p *Parser) Feed(line string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed == "[DONE]" || strings.HasPrefix(trimmed, ":") {
		return
	}
	var ev ccEvent
	if json.Unmarshal([]byte(trimmed), &ev) != nil || ev.Type == "" {
		return
	}
	switch ev.Type {
	case "text-delta":
		if text := shared.OrDefault(ev.Text, ev.Delta); text != "" {
			p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{ContentDelta: &pb.ContentDelta{Text: text}}})
		}
	case "reasoning-delta":
		if ev.Text != "" {
			p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ReasoningDelta{ReasoningDelta: &pb.ReasoningDelta{Text: ev.Text}}})
		}
	case "tool-call":
		// CC 一次性给全量 input（非增量）
		args := "{}"
		if len(ev.Input) > 0 {
			args = ccInputString(ev.Input)
		}
		p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{ToolCallDelta: &pb.ToolCallDelta{
			Id: ev.ToolCallID, Name: ev.ToolName, ArgumentsDelta: args,
		}}})
	case "finish-step":
		p.sawFinish = true
		if ev.FinishReason != "" {
			p.finishReason = mapFinishReason(ev.FinishReason)
		}
		if u := orUsage(ev.Usage, ev.TotalUsage); u != nil {
			p.usage = u.toEnvelope()
		}
	case "finish":
		p.sawFinish = true
		if ev.FinishReason != "" {
			p.finishReason = mapFinishReason(ev.FinishReason)
		}
		if u := orUsage(ev.TotalUsage, ev.Usage); u != nil {
			p.usage = u.toEnvelope()
		}
	case "error":
		p.finishError(ev.Error, ev.Message)
	default:
		// start / start-step / text-start / reasoning-* / tool-input-* / tool-error 等无内容静默
	}
}

// Finish 无 finish 事件视为截断（可重试 502）；零输出报 429 让下游退避重试。
func (p *Parser) Finish() {
	if p.sentFinish {
		return
	}
	if !p.sawFinish {
		p.FinishWithError(502, "upstream stream ended without a completion finish — response was truncated")
		return
	}
	p.sentFinish = true
	if p.usage != nil && p.usage.GetOutputTokens() == 0 {
		p.FinishWithError(429, "empty response from upstream (zero output tokens)")
		return
	}
	reason := p.finishReason
	if reason == "" {
		reason = "stop"
	}
	p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{MessageFinish: &pb.MessageFinish{
		FinishReason: reason, Usage: p.usage,
	}}})
}

// FinishWithError 流异常结束：发失败事件。
func (p *Parser) FinishWithError(code int32, message string) {
	if p.sentFinish {
		return
	}
	p.sentFinish = true
	p.emit(&pb.StreamEvent{Event: &pb.StreamEvent_TaskFailed{TaskFailed: &pb.TaskFailed{
		Error: &pb.Error{Code: code, Message: message, Retryable: retryableCode(code)},
	}}})
}

// finishError CC error 事件 → 失败事件，读 statusCode / message 前缀 <NNN> 还原状态。
func (p *Parser) finishError(e *ccEventError, fallbackMsg string) {
	msg := fallbackMsg
	if e != nil && e.Message != "" {
		msg = e.Message
	}
	if msg == "" {
		msg = "upstream error"
	}
	status := 502
	if m := statusPrefixRE.FindStringSubmatch(msg); m != nil {
		status, _ = strconv.Atoi(m[1])
	} else if e != nil && e.StatusCode != 0 {
		status = e.StatusCode
	}
	p.FinishWithError(mapCcStatus(status), msg)
}

// mapFinishReason length 家族不止 'length'；未知值原样返回，不静默折成 stop。
func mapFinishReason(reason string) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	switch r {
	case "":
		return "stop"
	case "tool-calls", "tool_calls", "tool_use":
		return "tool_calls"
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded":
		return "length"
	}
	if networkFailRE.MatchString(r) {
		return "length" // 信封无 upstream_error，连接失败按截断处理
	}
	return r
}

// mapCcStatus 上游 HTTP 状态 → 下游状态。
func mapCcStatus(cc int) int32 {
	switch cc {
	case 400, 422:
		return 400
	case 401, 403:
		return 401
	case 402, 429:
		return 429
	case 404:
		return 404
	case 503:
		return 503
	}
	return 502
}

func retryableCode(code int32) bool { return code == 429 || code == 502 || code == 503 }

// ccInputString 工具入参 → 参数 JSON 字符串（字符串值去引号取原值）。
func ccInputString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func orUsage(a, b *ccUsage) *ccUsage {
	if a != nil {
		return a
	}
	return b
}
