// ima Chat：信封 → question 拼装（prompt 注入式 tool calling）+ SSE 泵。
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// ---------- Chat ----------

// Chat 上游只认单条 question 文本：信封多轮 → 会话绑定（账号+对话）+ 历史/工具 prompt 拼装；
// 工具走 prompt 注入，模型输出 <function_call> 块解析为 ToolCallDelta。
func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	models := p.currentModels(ctx)
	modelType, modelUpID := resolveModel(models, req.Model)
	modelLabel := shared.OrDefault(req.Model, "ima")

	question, hasTools := buildQuestion(req, models)
	if strings.TrimSpace(question) == "" {
		return stream.Send(shared.Failed(400, "messages 不能为空"))
	}
	// 原始请求排查（debug 级）：信封消息 + 拼装后的 question
	p.host.LogFields("debug", "ima 请求: model="+shared.OrDefault(req.Model, "ima")+" messages="+fmt.Sprint(len(req.Messages)), map[string]string{"action": "chat"})
	for _, m := range req.Messages {
		p.host.LogFields("debug", "ima 请求消息: role="+m.Role+" text="+m.Text, map[string]string{"action": "chat"})
	}
	p.host.LogFields("debug", "ima question: "+question, map[string]string{"action": "chat"})

	// 会话按 账号+对话 锚点绑定；多轮复用 IMA 会话，超 20 轮由上游报错自动重建
	convKey := "ima::" + accountAnchor(c) + "::" + convAnchor(req)
	sessionID := cachedSession(convKey)
	if sessionID == "" {
		sessionID, err = p.initSession(ctx, c, firstUserText(req))
		if err != nil {
			return stream.Send(shared.Failed(mapErr(err), "session: "+err.Error()))
		}
		storeSession(convKey, sessionID)
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: modelLabel},
	}}); err != nil {
		return err
	}

	// SSE 泵 + function_call 过滤器
	filter := newFuncFilter()
	var once sync.Once
	var filterErr error
	// 调试计数：事件形状 / 文本增量（空回诊断）
	var evCount, emptyCount, textCount int
	pumpErr := p.qaStream(ctx, c, sessionID, question, modelType, modelUpID, func(event, data string) error {
		evCount++
		switch event {
		case "COMPLETED":
			// Code!=0（1401 内容审核拒答 / 1402 会话失效等）→ 上游错误，不再静默空回
			if d := completedCode(data); d != 0 {
				if filterErr == nil {
					once.Do(func() { filterErr = fmt.Errorf("上游错误(%d): %s", d, completedMsg(data)) })
				}
			}
			return nil
		case "CLOSE":
			return nil
		case "INNER_EXCEPTION", "ERROR", "FAILED":
			// 会话满（msgs_limit）等业务错误：重建会话重试一次
			if newID, e := p.initSession(ctx, c, firstUserText(req)); e == nil {
				storeSession(convKey, newID)
			}
			if filterErr == nil {
				once.Do(func() { filterErr = fmt.Errorf("上游错误: %s", eventText(data)) })
			}
			return nil
		}
		txt := eventText(data)
		if txt == "" {
			emptyCount++
			p.host.LogFields("debug", "ima 未知事件: "+event+" data="+data, map[string]string{"action": "chat"})
			return nil
		}
		textCount++
		if clean, _ := filter.feed(txt); clean != "" {
			if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
				ContentDelta: &pb.ContentDelta{Text: clean},
			}}); err != nil {
				return err
			}
		}
		return nil
	})
	if pumpErr != nil {
		p.host.LogFields("debug", fmt.Sprintf("ima stream broken: %v (events=%d text=%d empty=%d)", pumpErr, evCount, textCount, emptyCount), map[string]string{"action": "chat"})
		return stream.Send(shared.Failed(mapErr(pumpErr), pumpErr.Error()))
	}
	// 空回诊断：有事件但零文本 → 事件形状未识别（debug 级落 run_logs）
	if textCount == 0 && evCount > 0 && filterErr == nil {
		p.host.LogFields("debug", fmt.Sprintf("ima 空回: events=%d 全部未识别为文本（model=%s question_len=%d）", evCount, req.Model, len(question)), map[string]string{"action": "chat"})
	}
	if filterErr != nil {
		return stream.Send(shared.Failed(502, filterErr.Error()))
	}

	// 冲刷过滤器：残留文本 + 解析出的 function_call
	if clean, _ := filter.flush(); clean != "" {
		_ = stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
			ContentDelta: &pb.ContentDelta{Text: clean},
		}})
	}
	if hasTools {
		for _, call := range filter.allCalls() {
			if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{Id: call.ID, Name: call.Name, ArgumentsDelta: call.Arguments},
			}}); err != nil {
				return err
			}
		}
	}
	finishReason := "stop"
	if hasTools && len(filter.allCalls()) > 0 {
		finishReason = "tool_calls"
	}
	return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: finishReason},
	}})
}

// ---------- question 拼装 ----------

// buildQuestion 信封消息 → 单条 question 文本（照 ima2api buildToolsPrompt / 通知格式）。
// 返回 (question, 是否带工具)。
func buildQuestion(req *pb.ChatRequest, models []imaModel) (string, bool) {
	hasToolResults := false
	hasToolCalls := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			hasToolResults = true
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			hasToolCalls = true
		}
	}

	// 工具集：客户端带的优先，最多 8 个
	tools := req.Tools
	if len(tools) == 0 {
		tools = defaultTools()
	}
	tools = limitTools(tools, 8)
	toolsPrompt := buildToolsPrompt(tools)

	// 工具结果回传轮：单段系统通知格式
	if hasToolResults && hasToolCalls {
		var called, results []string
		origQ := ""
		for _, m := range req.Messages {
			switch m.Role {
			case "assistant":
				for _, tc := range m.ToolCalls {
					called = append(called, tc.Name+"("+tc.Arguments+")")
				}
			case "tool":
				results = append(results, truncateString(m.Text, 3000))
			case "user":
				if t := stripMetadata(m.Text); t != "" {
					origQ = t
				}
			}
		}
		lang := ""
		var b strings.Builder
		if cjkRe.MatchString(origQ) {
			b.WriteString("⚠️ 系统通知：你刚才调用了以下函数，返回结果如下：\n\n")
			lang = "\n请用简体中文直接回答用户的问题。说出答案即可。不要说\"你分享了\"或\"看起来像是\"。上面用 \"\"\" 包裹的内容是你自己调用函数得到的返回结果。"
		} else {
			b.WriteString("⚠️ SYSTEM: You just called these functions and received these outputs:\n\n")
			lang = "\nAnswer DIRECTLY based on the outputs. Do NOT say \"you shared\" or \"it looks like\". The \"\"\" content is YOUR function output, NOT user input."
		}
		for i := range called {
			r := ""
			if i < len(results) {
				r = results[i]
			}
			b.WriteString("函数调用: " + called[i] + "\n返回结果:\n\"\"\"\n" + r + "\n\"\"\"\n\n")
		}
		b.WriteString("用户原始提问: \"" + origQ + "\"" + lang)
		return b.String(), len(tools) > 0
	}

	// 普通问答 / 首轮 tool calling：历史 + sysPrompt + 工具注入
	var sysParts, history []string
	lastUser := ""
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if t := stripMetadata(m.Text); t != "" {
				sysParts = append(sysParts, t)
			}
		case "user":
			if t := stripMetadata(m.Text); t != "" {
				history = append(history, "User: "+t)
				lastUser = t
			}
		case "assistant":
			if t := stripMetadata(m.Text); t != "" {
				history = append(history, "Assistant: "+t)
			}
		}
	}
	history = limitHistory(history, 20, 6000)

	langHint := ""
	if cjkRe.MatchString(lastUser) {
		langHint = "\n## Language\nRespond in the same language as the user's message. The user is writing in Chinese — respond in Chinese (简体中文).\n"
	}

	var b strings.Builder
	if len(sysParts) > 0 {
		b.WriteString("(Background)\n" + strings.Join(sysParts, "\n") + "\n\n")
	}
	if toolsPrompt != "" {
		b.WriteString(toolsPrompt + "\n\n---\n")
	}
	if len(history) > 1 {
		b.WriteString(strings.Join(history, "\n"))
	} else {
		b.WriteString("User message (respond to this):\n" + shared.OrDefault(lastUser, "你好"))
	}
	b.WriteString(langHint)

	q := b.String()
	if len(q) > MAX_QUESTION {
		// 最终截断：始终保留 toolsPrompt，否则模型看不到 function 定义
		q = toolsPrompt + "\n\n---\nUser message (respond to this):\n" + truncateString(lastUser, 2000) + langHint
	}
	return q, len(tools) > 0
}

// buildToolsPrompt prompt 注入式工具定义。
// 语气温和（中文简短说明，无 CRITICAL/XML 标签）：激进英文指令段 + XML 会触发 ima 上游
// 意图识别拒答（COMPLETED Code:1401），函数列表形态实测通过且模型能按格式输出调用。
func buildToolsPrompt(tools []*pb.ToolDefinition) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range tools {
		b.WriteString("- " + t.Name + "(" + paramNames(t) + "): " + shared.OrDefault(t.Description, "No description") + "\n")
	}
	return "你可以调用以下函数来完成任务。如果任务适合用函数完成，请只输出如下格式的函数调用后停止：\n" +
		"{\"name\": \"<函数名>\", \"arguments\": {<参数json>}}\n\n" +
		"可用函数：\n" + b.String() + "\n" +
		"- arguments 必须是符合该函数参数的合法 JSON\n"
}

// paramNames 函数参数名列表（紧凑签名展示）。
func paramNames(t *pb.ToolDefinition) string {
	var obj map[string]interface{}
	if json.Unmarshal([]byte(t.ParametersSchema), &obj) != nil {
		return ""
	}
	props, _ := obj["properties"].(map[string]interface{})
	if len(props) == 0 {
		return ""
	}
	var names []string
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// defaultTools 客户端未带工具时的默认核心五件套。
func defaultTools() []*pb.ToolDefinition {
	mk := func(name, desc, params string) *pb.ToolDefinition {
		return &pb.ToolDefinition{Name: name, Description: desc, ParametersSchema: params}
	}
	return []*pb.ToolDefinition{
		mk("Bash", "Execute bash command", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		mk("Read", "Read a file", `{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`),
		mk("Write", "Write to a file", `{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`),
		mk("Glob", "Find files by pattern", `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`),
		mk("Grep", "Search file contents", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`),
	}
}

// limitTools 工具数量截断（防 prompt 超限）。
func limitTools(tools []*pb.ToolDefinition, max int) []*pb.ToolDefinition {
	if len(tools) <= max {
		return tools
	}
	essential := map[string]bool{"Bash": true, "Read": true, "Write": true, "Glob": true, "Grep": true}
	var pri, rest []*pb.ToolDefinition
	for _, t := range tools {
		if essential[t.Name] {
			pri = append(pri, t)
		} else {
			rest = append(rest, t)
		}
	}
	out := pri
	for _, t := range rest {
		if len(out) >= max {
			break
		}
		out = append(out, t)
	}
	return out
}

// limitHistory 历史条数 + 总字符双限制（保留最新）。
func limitHistory(lines []string, maxCount, maxChars int) []string {
	if len(lines) > maxCount {
		lines = lines[len(lines)-maxCount:]
	}
	total := 0
	start := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		total += len(lines[i]) + 1
		if total > maxChars && i < len(lines)-1 {
			start = i + 1
			break
		}
		start = i
	}
	return lines[start:]
}

// stripMetadata 清理用户消息中的系统注入元数据。
func stripMetadata(text string) string {
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, "<system-reminder") || strings.HasPrefix(text, "<session") {
		return ""
	}
	return text
}

// firstUserText 首条用户文本（作会话标题）。
func firstUserText(req *pb.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role == "user" && m.Text != "" {
			return truncateString(m.Text, 50)
		}
	}
	return "新对话"
}

// acc点（IMA-UID）。
func accountAnchor(c *credential) string {
	return shared.OrDefault(cookieField(c.Cookie, "IMA-UID"), "anon")
}

// convAnchor 对话锚点：系统消息前 200 字符 hash（客户端不带会话 id 时稳定复用会话）。
func convAnchor(req *pb.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role == "system" && m.Text != "" {
			return shortHash(m.Text)
		}
	}
	if len(req.Messages) > 0 {
		return shortHash(req.Messages[0].Text)
	}
	return "empty"
}

func shortHash(s string) string {
	var h uint64 = 5381
	for _, ch := range s {
		h = h*33 + uint64(ch)
	}
	return fmt.Sprintf("%012x", h)
}

// ---------- function_call 过滤器 ----------

// funcCall 解析出的函数调用。
type funcCall struct {
	ID        string
	Name      string
	Arguments string
}

// funcFilter 实时过滤流式输出中的 <function_call> 块（防客户端看到 XML 标记截停），
// 捕获的块解析为 ToolCall（JSON 破损时尽力修复）。
type funcFilter struct {
	buf      strings.Builder
	inFunc   bool
	blocks   []string
	parsed   []funcCall
	leftover string
}

func newFuncFilter() *funcFilter { return &funcFilter{} }

// feed 喂入文本增量，返回应转发的纯净文本。
func (f *funcFilter) feed(chunk string) (string, []funcCall) {
	f.buf.WriteString(chunk)
	s := f.buf.String()
	f.buf.Reset()
	f.buf.WriteString("") // 占位：内容在 s 中处理
	return f.consume(s, false)
}

// flush 流结束时冲刷：未闭合的 function_call 块也尝试解析。
func (f *funcFilter) flush() (string, []funcCall) {
	s := f.buf.String()
	f.buf.Reset()
	clean, _ := f.consume(s, true)
	return clean, f.parsed
}

// consume 从 s 中剥离 <function_call> 块，返回纯净文本与本次新解析的调用。
func (f *funcFilter) consume(s string, final bool) (string, []funcCall) {
	const startTag = "<function_call>"
	const endTag = "</function_call>"
	var out strings.Builder
	var newCalls []funcCall
	for {
		if !f.inFunc {
			idx := strings.Index(s, startTag)
			if idx < 0 {
				// 尾部部分匹配时保留在 buf
				if !final {
					if n := partialMatch(s, startTag); n > 0 {
						f.buf.WriteString(s[len(s)-n:])
						s = s[:len(s)-n]
					}
				}
				out.WriteString(s)
				break
			}
			out.WriteString(s[:idx])
			s = s[idx+len(startTag):]
			f.inFunc = true
		}
		idx := strings.Index(s, endTag)
		if idx < 0 {
			if final {
				// 流在闭合标签前结束：残留 JSON 尽力修复
				if blk := strings.TrimSpace(s); blk != "" {
					f.blocks = append(f.blocks, blk)
				}
				s = ""
			}
			f.buf.WriteString(s)
			s = ""
			break
		}
		if blk := strings.TrimSpace(s[:idx]); blk != "" {
			f.blocks = append(f.blocks, blk)
		}
		s = s[idx+len(endTag):]
		f.inFunc = false
	}
	if len(f.blocks) > len(f.parsed)+len(newCalls) || len(f.blocks) > 0 && len(f.parsed) < len(f.blocks) {
		for _, blk := range f.blocks[len(f.parsed)+len(newCalls):] {
			if call := tryParseCall(blk); call != nil {
				newCalls = append(newCalls, *call)
				f.parsed = append(f.parsed, *call)
			} else {
				note := "\n[Function call (malformed)]:\n" + blk + "\n"
				out.WriteString(note)
				f.leftover += note
			}
		}
	}
	return out.String(), newCalls
}

// calls 已解析的全部调用。
func (f *funcFilter) allCalls() []funcCall { return f.parsed }

// partialMatch 尾部部分匹配 tag 的长度。
func partialMatch(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

// tryParseCall 解析 function_call JSON（含破损修复：尾逗号/缺括号/裸换行）。
func tryParseCall(raw string) *funcCall {
	var m map[string]interface{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		m = repairJSON(raw)
		if m == nil {
			return nil
		}
	}
	name, _ := m["name"].(string)
	if name == "" {
		return nil
	}
	args := m["arguments"]
	if args == nil {
		if p, ok := m["parameters"]; ok {
			args = p
		}
	}
	argsJSON := "{}"
	if args != nil {
		if b, err := json.Marshal(args); err == nil {
			argsJSON = string(b)
		}
	}
	return &funcCall{ID: "toolu_" + shared.RandHex(12), Name: name, Arguments: argsJSON}
}

// repairJSON LLM 常见 JSON 错误修复（尾逗号 / 缺闭合括号 / 字符串内裸换行）。
func repairJSON(raw string) map[string]interface{} {
	s := escapeCtrl(raw)
	var m map[string]interface{}
	if json.Unmarshal([]byte(s), &m) == nil {
		return m
	}
	// 去尾逗号
	s2 := regexp.MustCompile(`,(\s*[}\]])`).ReplaceAllString(s, "$1")
	if json.Unmarshal([]byte(s2), &m) == nil {
		return m
	}
	// 补缺失闭合括号
	depth := 0
	for _, ch := range s2 {
		switch ch {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	if depth > 0 {
		if depth > 5 {
			depth = 5
		}
		closer := "}"
		if strings.HasSuffix(strings.TrimSpace(s2), "}") {
			closer = "]"
		}
		s3 := s2 + strings.Repeat(closer, depth)
		if json.Unmarshal([]byte(s3), &m) == nil {
			return m
		}
	}
	return nil
}

// escapeCtrl 转义字符串内的裸控制符。
func escapeCtrl(s string) string {
	var b strings.Builder
	inStr, esc := false, false
	for _, ch := range s {
		switch {
		case esc:
			b.WriteRune(ch)
			esc = false
		case ch == '\\':
			b.WriteByte('\\')
			esc = true
		case ch == '"':
			inStr = !inStr
			b.WriteByte('"')
		case inStr && ch == '\n':
			b.WriteString("\\n")
		case inStr && ch == '\r':
			b.WriteString("\\r")
		case inStr && ch == '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// ---------- 错误映射 ----------

// mapErr 错误 → 信封错误码（鉴权失效 401，其余 502）。
func mapErr(err error) int32 {
	if mapAuthErr(err) {
		return 401
	}
	return 502
}

// completedCode COMPLETED 事件的业务 Code（0 = 成功）。
func completedCode(data string) int64 {
	var d struct {
		Code int64 `json:"Code"`
	}
	if json.Unmarshal([]byte(data), &d) != nil {
		return 0
	}
	return d.Code
}

// completedMsg COMPLETED 事件的上游消息。
func completedMsg(data string) string {
	var d struct {
		Msg string `json:"Msg"`
	}
	if json.Unmarshal([]byte(data), &d) != nil {
		return ""
	}
	return d.Msg
}

// eventText SSE data → 文本（多形状兼容，照 ima2api extractEventText）。
func eventText(data string) string {
	if strings.TrimSpace(data) == "" {
		return ""
	}
	var d map[string]interface{}
	if json.Unmarshal([]byte(data), &d) != nil {
		// JSON 字符串形态（"文本"）：取字符串值
		var s string
		if json.Unmarshal([]byte(data), &s) == nil {
			return s
		}
		return ""
	}
	for _, k := range []string{"Text", "text", "Content", "content", "Delta", "delta", "Msg", "msg", "reply", "Reply", "answer", "Answer"} {
		if v, ok := d[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
