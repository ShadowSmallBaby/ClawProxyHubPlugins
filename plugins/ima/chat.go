// Chat 编排：会话绑定 + SSE 泵 + function_call 过滤。
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// Chat 上游只认单条 question 文本：信封多轮 → 会话绑定 + question 拼装（envelope.go）；
// 工具走 prompt 注入，模型输出 <function_call> 块过滤解析为 ToolCallDelta。
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
	// 原始请求排查（debug 级）
	p.host.LogFields("debug", "ima 请求: model="+shared.OrDefault(req.Model, "ima")+" messages="+fmt.Sprint(len(req.Messages)), map[string]string{"action": "chat"})
	for _, m := range req.Messages {
		p.host.LogFields("debug", "ima 请求消息: role="+m.Role+" text="+m.Text, map[string]string{"action": "chat"})
	}
	p.host.LogFields("debug", "ima question: "+question, map[string]string{"action": "chat"})

	// 会话按 账号+对话 锚点绑定；超 20 轮由上游报错自动重建
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
	var evCount, emptyCount, textCount int
	pumpErr := p.qaStream(ctx, c, sessionID, question, modelType, modelUpID, func(event, data string) error {
		evCount++
		switch event {
		case "COMPLETED":
			// Code!=0（1401 审核拒答 / 1402 会话失效等）→ 上游错误，不再静默空回
			if d := completedCode(data); d != 0 {
				if filterErr == nil {
					once.Do(func() { filterErr = fmt.Errorf("上游错误(%d): %s", d, completedMsg(data)) })
				}
			}
			return nil
		case "CLOSE":
			return nil
		case "INNER_EXCEPTION", "ERROR", "FAILED":
			// 会话满等业务错误：重建会话重试一次
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
	// 空回诊断：有事件但零文本 → 事件形状未识别
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

// ---------- function_call 过滤器 ----------

// funcCall 解析出的函数调用。
type funcCall struct {
	ID        string
	Name      string
	Arguments string
}

// funcFilter 实时剥离流式输出中的 <function_call> 块并解析为 ToolCall。
// pending 保存跨 chunk 的半截内容；seen 计已消费的块，malformed 块同样推进（防反复输出）。
type funcFilter struct {
	pending strings.Builder
	inFunc  bool
	blocks  []string
	parsed  []funcCall
	seen    int
}

func newFuncFilter() *funcFilter { return &funcFilter{} }

// feed 喂入文本增量，返回应转发的纯净文本与本次新解析的调用。
func (f *funcFilter) feed(chunk string) (string, []funcCall) {
	return f.consume(f.pending.String()+chunk, false)
}

// flush 流结束时冲刷：未闭合的 function_call 块也尝试解析。
func (f *funcFilter) flush() (string, []funcCall) {
	return f.consume(f.pending.String(), true)
}

// consume 从 s 中剥离 <function_call> 块，返回纯净文本与本次新解析的调用。
func (f *funcFilter) consume(s string, final bool) (string, []funcCall) {
	const startTag = "<function_call>"
	const endTag = "</function_call>"
	f.pending.Reset()
	var out strings.Builder
	for {
		if !f.inFunc {
			idx := strings.Index(s, startTag)
			if idx < 0 {
				// 尾部部分匹配时保留在 pending（防 tag 被 chunk 切开）
				if !final {
					if n := partialMatch(s, startTag); n > 0 {
						f.pending.WriteString(s[len(s)-n:])
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
				// 流在闭合标签前结束：残留 JSON 尽力解析
				if blk := strings.TrimSpace(s); blk != "" {
					f.blocks = append(f.blocks, blk)
				}
				s = ""
			} else {
				f.pending.WriteString(s)
			}
			s = ""
			break
		}
		if blk := strings.TrimSpace(s[:idx]); blk != "" {
			f.blocks = append(f.blocks, blk)
		}
		s = s[idx+len(endTag):]
		f.inFunc = false
	}
	// 只解析本次新增的块；malformed 块也推进 seen（用 parsed 作起点会让坏块反复输出）
	var newCalls []funcCall
	for _, blk := range f.blocks[f.seen:] {
		if call := tryParseCall(blk); call != nil {
			newCalls = append(newCalls, *call)
			f.parsed = append(f.parsed, *call)
		} else {
			out.WriteString("\n[Function call (malformed)]:\n" + blk + "\n")
		}
	}
	f.seen = len(f.blocks)
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

// repairJSON LLM 常见 JSON 错误修复。
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

// eventText SSE data → 文本（多形状兼容）。
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
