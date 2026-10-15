// prompt:统一信封 → Logicc 提示词,以及客户端工具调用的文本协议。
//
// 上游 /api/chats 只接受一段纯文本 prompt,不能注册任意客户端函数,因此工具调用靠
// 「结构化提示词 + JSON 信封 + 严格校验」实现:把历史与工具声明写进提示词,要求模型
// 只回一个 {"content","tool_calls"} JSON,再解析校验成信封 ToolCall。
package logicc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// toolSpec 声明给模型的一个客户端工具。
type toolSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Kind        string                 `json:"kind"` // function / custom
	Parameters  map[string]interface{} `json:"parameters"`
}

// parsedCall 一次解析出的工具调用。
type parsedCall struct {
	ID        string
	Name      string
	Kind      string // function / custom
	Arguments string // function:JSON 对象字符串
	Input     string // custom:原样字符串
}

// nativeCall 上游原生工具调用(provider-executed 的副作用不回吐给客户端)。
type nativeCall struct {
	ID        string
	Name      string
	Arguments string
	Executed  bool
}

// promptPlan 一次请求的提示词计划(工具选择已解析)。
type promptPlan struct {
	active    []toolSpec // 允许本轮调用的工具;空 = 不启用工具文本协议
	choice    string
	required  bool
	parallel  bool
	maxTokens int32
	convoJSON string // 会话 JSON(工具协议内嵌用)
}

// resolvePlan 解析信封的 tools / tool_choice。
// 核心只投递 function 类型工具(内置工具在网关已被拒绝),故无需处理上游内置工具。
func resolvePlan(req *pb.ChatRequest) *promptPlan {
	plan := &promptPlan{choice: "auto", parallel: true, maxTokens: req.MaxTokens}
	var declared []toolSpec
	for _, t := range req.Tools {
		schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		if raw := strings.TrimSpace(t.ParametersSchema); raw != "" {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(raw), &parsed) == nil && parsed != nil {
				schema = parsed
			}
		}
		declared = append(declared, toolSpec{Name: t.Name, Description: t.Description, Kind: "function", Parameters: schema})
	}
	plan.active = declared

	if raw := req.Extra["parallel_tool_calls"]; raw != "" {
		var parallel bool
		if json.Unmarshal([]byte(raw), &parallel) == nil {
			plan.parallel = parallel
		}
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto":
			plan.choice = "auto"
		case "none":
			plan.choice = "none"
			plan.active = nil
		case "tool":
			plan.required = true
			if tc.ToolName != "" {
				plan.choice = tc.ToolName
				plan.active = nil
				for _, t := range declared {
					if t.Name == tc.ToolName {
						plan.active = []toolSpec{t}
						break
					}
				}
			} else {
				plan.choice = "required"
			}
		}
	}
	plan.convoJSON = string(mustJSON(messagesJSON(req.Messages)))
	return plan
}

// promptToolIntro 工具协议说明(仅启用工具时附加)。
const promptToolIntro = `Serialize the next assistant turn for a client application. The client application executes functions requested in your output; you only select a tool and produce its input. Do not execute the client functions yourself or invent their results.
Return only one JSON object, without markdown fences or explanations, using this envelope:
{"content": "text for the user or null", "tool_calls": [{"name": "a declared tool name", "arguments": {}}]}
For a function tool, arguments must be a JSON object matching its parameters. For a custom tool, use "input" with a string instead of arguments and follow its format.
If a tool is needed to fulfill the user's request, request it and stop. Use actual values from the conversation, never placeholder values. If a tool result has already been supplied, use it to continue the task and give the final answer when sufficient. For an ordinary answer, put the answer in content and set tool_calls to []. Do not say tools are unavailable: the external client executes the declared functions after receiving your JSON.`

// buildPrompt 信封 → 上游提示词。启用工具时附加输出协议说明。
func buildPrompt(req *pb.ChatRequest, plan *promptPlan) string {
	var parts []string
	for _, m := range req.Messages {
		switch {
		case len(m.ToolCalls) > 0:
			parts = append(parts, "[Assistant]\n"+m.Text+"\n[Tool calls]\n"+string(mustJSON(toolCallsJSON(m.ToolCalls))))
		case m.Role == "tool":
			suffix := ""
			if m.ToolError {
				suffix = "; error"
			}
			parts = append(parts, "[Tool result: "+m.ToolCallId+suffix+"]\n"+m.Text)
		case m.Role == "user":
			parts = append(parts, m.Text)
		default:
			parts = append(parts, "["+m.Role+"]\n"+m.Text)
		}
	}
	history := strings.Join(parts, "\n\n")
	limit := ""
	if plan.maxTokens > 0 {
		limit = fmt.Sprintf("\nKeep the response within approximately %d output tokens.", plan.maxTokens)
	}
	if len(plan.active) == 0 {
		return history + limit
	}
	return strings.Join([]string{
		promptToolIntro,
		"Tool choice: " + toolChoiceClause(plan),
		"Parallel calls: " + parallelClause(plan) + ".",
		"Declared client tools:",
		string(mustJSON(plan.active)),
		"Conversation (the actual task to continue):",
		plan.convoJSON,
		"Return the next assistant turn JSON now." + limit,
	}, "\n")
}

// toolChoiceClause 工具选择约束文案。
func toolChoiceClause(plan *promptPlan) string {
	if !plan.required {
		return "Choose a tool when needed, otherwise answer the user."
	}
	if plan.choice == "required" {
		return "You MUST request at least one declared client tool in this turn."
	}
	return "You MUST request " + strconvQuote(plan.choice) + " in this turn."
}

func parallelClause(plan *promptPlan) string {
	if plan.parallel {
		return "allowed"
	}
	return "disabled; return at most one tool call"
}

// parseToolOutput 上游输出 → (对客户端的文本, 工具调用)。
// 上游原生工具调用优先(仅未执行的那些);否则解析文本 JSON 信封。
func parseToolOutput(text string, plan *promptPlan, nativeCalls []nativeCall) (string, []parsedCall, error) {
	if pending := pendingNative(nativeCalls); len(pending) > 0 {
		calls, err := validateNativeCalls(pending, plan)
		if err != nil {
			return "", nil, err
		}
		return text, calls, nil
	}
	envelope := parsedEnvelope(text)
	if envelope != nil {
		if raw, ok := envelope["tool_calls"]; ok {
			return finishToolEnvelope(raw, envelope, plan)
		}
		if raw, ok := envelope["tool_call"]; ok {
			return finishToolEnvelope([]interface{}{raw}, envelope, plan)
		}
	}
	if plan.required {
		return "", nil, toolError("模型未生成有效的工具调用 JSON;没有把普通文本伪装成调用")
	}
	// 普通文本是合法的 auto 模式回答;不要解析散文里被引用的 JSON 当真调用。
	return text, nil, nil
}

func finishToolEnvelope(rawCalls interface{}, envelope map[string]interface{}, plan *promptPlan) (string, []parsedCall, error) {
	list, _ := rawCalls.([]interface{})
	calls, err := validateCalls(list, plan)
	if err != nil {
		return "", nil, err
	}
	if plan.required && len(calls) == 0 {
		return "", nil, toolError("模型未按 tool_choice 返回工具调用,请更换模型或调整请求")
	}
	content := ""
	if v, ok := envelope["content"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr {
			return "", nil, toolError("工具响应中的 content 必须为字符串或 null")
		}
		content = s
	}
	return content, calls, nil
}

// parsedEnvelope 去掉 ```json 围栏后解析;失败或非对象回 nil。
func parsedEnvelope(raw string) map[string]interface{} {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```") {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		if j := strings.LastIndex(text, "```"); j >= 0 {
			text = text[:j]
		}
		text = strings.TrimSpace(text)
	}
	var value map[string]interface{}
	// 工具参数内的大整数不能经 float64 解码后失真。
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil
	}
	return value
}

// validateCalls 上游模型给出的原始调用列表 → 校验后的调用。
func validateCalls(rawCalls []interface{}, plan *promptPlan) ([]parsedCall, error) {
	if len(rawCalls) > 32 {
		return nil, toolError("模型返回了无效的工具调用列表")
	}
	if !plan.parallel && len(rawCalls) > 1 {
		return nil, toolError("模型返回了多个调用,但 parallel_tool_calls 已禁用")
	}
	out := make([]parsedCall, 0, len(rawCalls))
	for _, raw := range rawCalls {
		item, ok := raw.(map[string]interface{})
		if !ok {
			return nil, toolError("模型返回了无效的工具调用列表")
		}
		name, _ := item["name"].(string)
		if fn, ok := item["function"].(map[string]interface{}); ok {
			if n, _ := fn["name"].(string); n != "" {
				name = n
			}
		}
		spec := findTool(plan.active, name)
		if spec == nil {
			return nil, toolError("模型请求了未声明或 tool_choice 不允许的工具")
		}
		call := parsedCall{ID: "call_" + shared.RandHex(16), Name: name, Kind: spec.Kind}
		if spec.Kind == "custom" {
			input, _ := item["input"].(string)
			if input == "" {
				return nil, toolError("自定义工具输入必须为字符串")
			}
			call.Input = input
			out = append(out, call)
			continue
		}
		args, err := callArguments(item)
		if err != nil {
			return nil, err
		}
		if err := checkArguments(args, spec.Parameters, 0); err != nil {
			return nil, err
		}
		call.Arguments = string(mustJSON(args))
		out = append(out, call)
	}
	return out, nil
}

// pendingNative 过滤掉上游已执行的调用(不要再让客户端重复执行副作用)。
func pendingNative(calls []nativeCall) []nativeCall {
	out := make([]nativeCall, 0, len(calls))
	for _, c := range calls {
		if !c.Executed && c.Name != "" {
			out = append(out, c)
		}
	}
	return out
}

// validateNativeCalls 上游原生工具调用 → 校验后的调用(保留上游 call id)。
func validateNativeCalls(nativeCalls []nativeCall, plan *promptPlan) ([]parsedCall, error) {
	raw := make([]interface{}, 0, len(nativeCalls))
	for _, c := range nativeCalls {
		raw = append(raw, map[string]interface{}{"name": c.Name, "arguments": rawJSONInput(c.Arguments)})
	}
	calls, err := validateCalls(raw, plan)
	if err != nil {
		return nil, err
	}
	// 保留上游 call id(客户端要按原 id 回传工具结果)。
	for i := range calls {
		if i < len(nativeCalls) && nativeCalls[i].ID != "" {
			calls[i].ID = nativeCalls[i].ID
		}
	}
	return calls, nil
}

// rawJSONInput 原生参数原样交给校验器(空参视为 {}),非法 JSON 由校验器报错。
func rawJSONInput(raw string) interface{} {
	if strings.TrimSpace(raw) == "" {
		return map[string]interface{}{}
	}
	return json.RawMessage(raw)
}

// callArguments 取调用参数:function.arguments / arguments / input,字符串则解析为 JSON 对象。
func callArguments(item map[string]interface{}) (map[string]interface{}, error) {
	var value interface{}
	if fn, ok := item["function"].(map[string]interface{}); ok {
		if v, ok := fn["arguments"]; ok {
			value = v
		}
	}
	if value == nil {
		if v, ok := item["arguments"]; ok {
			value = v
		}
	}
	if value == nil {
		if v, ok := item["input"]; ok {
			value = v
		}
	}
	switch typed := value.(type) {
	case nil:
		return map[string]interface{}{}, nil
	case string:
		return decodeArgs(typed)
	case map[string]interface{}:
		return typed, nil
	case json.RawMessage:
		return decodeArgs(string(typed))
	case []byte:
		return decodeArgs(string(typed))
	}
	return nil, toolError("工具参数必须为 JSON 对象")
}

// decodeArgs 参数文本 → JSON 对象;空文本视为 {}。
func decodeArgs(text string) (map[string]interface{}, error) {
	if strings.TrimSpace(text) == "" {
		return map[string]interface{}{}, nil
	}
	var value interface{}
	// 工具参数内的大整数不能经 float64 解码后失真。
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil, toolError("工具参数不是有效 JSON")
	}
	obj, ok := value.(map[string]interface{})
	if !ok {
		return nil, toolError("工具参数必须为 JSON 对象")
	}
	return obj, nil
}

// checkArguments 按 JSON Schema 校验参数(类型 / enum / required / additionalProperties / items)。
func checkArguments(value interface{}, schema map[string]interface{}, depth int) error {
	if schema == nil || depth > 32 {
		return nil
	}
	if types, ok := schemaType(schema["type"]); ok && !typeMatches(types, value) {
		return toolError("工具参数的数据类型不符合 schema")
	}
	if enum, ok := schema["enum"].([]interface{}); ok {
		matched := false
		for _, item := range enum {
			if jsonEqual(item, value) {
				matched = true
				break
			}
		}
		if !matched {
			return toolError("工具参数不在允许的枚举值中")
		}
	}
	if obj, ok := value.(map[string]interface{}); ok {
		if required, ok := schema["required"].([]interface{}); ok {
			for _, key := range required {
				name, _ := key.(string)
				if _, ok := obj[name]; !ok {
					return toolError("工具参数缺少必填字段")
				}
			}
		}
		properties, _ := schema["properties"].(map[string]interface{})
		for key, child := range obj {
			property, _ := properties[key].(map[string]interface{})
			if schema["additionalProperties"] == false && property == nil {
				return toolError("工具参数包含 schema 未允许的字段")
			}
			sub := property
			if sub == nil {
				sub, _ = schema["additionalProperties"].(map[string]interface{})
			}
			if err := checkArguments(child, sub, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if items, ok := value.([]interface{}); ok {
		itemSchema, _ := schema["items"].(map[string]interface{})
		for _, item := range items {
			if err := checkArguments(item, itemSchema, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// schemaType 归一化 schema 的 type(可为字符串或字符串数组)。
func schemaType(raw interface{}) ([]string, bool) {
	switch v := raw.(type) {
	case string:
		return []string{v}, true
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}

func typeMatches(types []string, value interface{}) bool {
	for _, t := range types {
		switch t {
		case "null":
			if value == nil {
				return true
			}
		case "array":
			if _, ok := value.([]interface{}); ok {
				return true
			}
		case "object":
			if _, ok := value.(map[string]interface{}); ok {
				return true
			}
		case "integer":
			if isIntegerNumber(value) {
				return true
			}
		case "number":
			if isNumber(value) {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		}
	}
	return false
}

func isNumber(value interface{}) bool {
	_, ok := value.(json.Number)
	return ok
}

// isIntegerNumber 判整数:json.Number 无小数点/指数,或指数展开后为整。
func isIntegerNumber(value interface{}) bool {
	n, ok := value.(json.Number)
	if !ok {
		return false
	}
	if !strings.ContainsAny(n.String(), ".eE") {
		return true
	}
	f, err := n.Float64()
	return err == nil && f == float64(int64(f))
}

func jsonEqual(a, b interface{}) bool {
	return bytes.Equal(mustJSON(a), mustJSON(b))
}

func findTool(tools []toolSpec, name string) *toolSpec {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// toolError 工具协议失败:502,客户端可见为上游工具生成错误。
func toolError(message string) error {
	return shared.HTTPError{Code: 502, Message: message}
}

// invalidError 请求本身不合法(400)。
func invalidError(message string) error {
	return shared.HTTPError{Code: 400, Message: message}
}

// validateParts 校验消息内容块:上游只收纯文本提示词,仅文本与推理块可用。
// 图片等其余内容块明确报错,不静默丢弃。
func validateParts(messages []*pb.EnvelopeMessage) error {
	for _, m := range messages {
		for _, part := range m.Parts {
			switch part.Type {
			case "", "text", "thinking", "redacted_thinking":
			default:
				return invalidError("暂不支持内容块 " + part.Type + ",当前仅支持文本")
			}
		}
	}
	return nil
}

// messagesJSON 信封消息 → 会话 JSON(工具协议内嵌)。
func messagesJSON(messages []*pb.EnvelopeMessage) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(messages))
	for _, m := range messages {
		item := map[string]interface{}{"role": m.Role, "content": m.Text}
		if m.Role == "tool" && m.ToolCallId != "" {
			item["tool_call_id"] = m.ToolCallId
		}
		if len(m.ToolCalls) > 0 {
			item["tool_calls"] = toolCallsJSON(m.ToolCalls)
		}
		out = append(out, item)
	}
	return out
}

func toolCallsJSON(calls []*pb.ToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(calls))
	for _, c := range calls {
		args := map[string]interface{}{}
		if raw := strings.TrimSpace(c.Arguments); raw != "" {
			var parsed map[string]interface{}
			decoder := json.NewDecoder(strings.NewReader(raw))
			decoder.UseNumber()
			if decoder.Decode(&parsed) == nil && parsed != nil {
				args = parsed
			}
		}
		out = append(out, map[string]interface{}{
			"id": c.Id, "type": "function",
			"function": map[string]interface{}{"name": c.Name, "arguments": args},
		})
	}
	return out
}

func mustJSON(value interface{}) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return []byte("null")
	}
	return raw
}

// strconvQuote 输出带引号的 JSON 字符串(提示词内嵌工具名)。
func strconvQuote(s string) string { return string(mustJSON(s)) }
