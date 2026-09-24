// notion 对话：建线程 → 组 transcript → runInference NDJSON 流 → 信封事件。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	mapped := p.resolveModel(req.Model)
	threadType := "workflow"
	if strings.HasPrefix(mapped, "vertex-") {
		threadType = "markdown-chat"
	}

	threadID, err := p.createThread(ctx, c, threadType)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}

	payload := buildPayload(req, c, threadID, mapped, threadType)
	raw, _ := json.Marshal(payload)

	resp, err := p.postJSON(ctx, c, epRunInfer, raw)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		switch resp.StatusCode {
		case 401, 403:
			code = 401
		case 429:
			code = 429
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("notion runInference HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	return p.scanNDJSON(resp.Body, stream)
}

// createThread saveTransactionsFanout 建对话线程，返回 threadID。
func (p *plugin) createThread(ctx context.Context, c *credential, threadType string) (string, error) {
	threadID := shared.RandUUID()
	payload := map[string]interface{}{
		"requestId": shared.RandUUID(),
		"transactions": []interface{}{
			map[string]interface{}{
				"id":      shared.RandUUID(),
				"spaceId": c.SpaceID,
				"operations": []interface{}{
					map[string]interface{}{
						"pointer": map[string]interface{}{"table": "thread", "id": threadID, "spaceId": c.SpaceID},
						"path":    []interface{}{},
						"command": "set",
						"args": map[string]interface{}{
							"id": threadID, "version": 1, "parent_id": c.SpaceID,
							"parent_table": "space", "space_id": c.SpaceID,
							"created_time":     time.Now().UnixMilli(),
							"created_by_id":    c.UserID,
							"created_by_table": "notion_user",
							"messages":         []interface{}{}, "data": map[string]interface{}{},
							"alive": true, "type": threadType,
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := p.postJSON(ctx, c, epSaveTx, body)
	if err != nil {
		return "", fmt.Errorf("notion 建线程失败: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", fmt.Errorf("notion 建线程鉴权失败: HTTP %d（Cookie 可能已过期）", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("notion 建线程失败: HTTP %d", resp.StatusCode)
	}
	return threadID, nil
}

// buildPayload 组 runInferenceTranscript 请求体（照官方 web 客户端）。
func buildPayload(req *pb.ChatRequest, c *credential, threadID, mapped, threadType string) map[string]interface{} {
	isGemini := strings.HasPrefix(mapped, "vertex-")

	ctxVal := map[string]interface{}{
		"timezone":        "Asia/Shanghai",
		"spaceId":         c.SpaceID,
		"userId":          c.UserID,
		"userEmail":       c.UserEmail,
		"currentDatetime": time.Now().Format(time.RFC3339),
	}
	if bid := normalizeBlockID(c.BlockID); bid != "" {
		ctxVal["blockId"] = bid
	}

	var config map[string]interface{}
	if isGemini {
		ctxVal["userName"] = " " + c.UserName
		ctxVal["spaceName"] = c.UserName + "的 Notion"
		ctxVal["surface"] = "ai_module"
		config = map[string]interface{}{
			"type": threadType, "model": mapped, "useWebSearch": true,
			"enableAgentAutomations": false, "enableAgentIntegrations": false,
			"enableBackgroundAgents": false, "enableCodegenIntegration": false,
			"enableCustomAgents": false, "enableExperimentalIntegrations": false,
			"enableLinkedDatabases": false, "enableAgentViewVersionHistoryTool": false,
			"searchScopes":         []interface{}{map[string]interface{}{"type": "everything"}},
			"enableDatabaseAgents": false, "enableAgentComments": false,
			"enableAgentForms": false, "enableAgentMakesFormulas": false,
			"enableUserSessionContext": false, "modelFromUser": true, "isCustomAgent": false,
		}
	} else {
		ctxVal["userName"] = c.UserName
		ctxVal["surface"] = "workflows"
		config = map[string]interface{}{
			"type": threadType, "model": mapped, "useWebSearch": true,
		}
	}

	transcript := []interface{}{
		map[string]interface{}{"id": shared.RandUUID(), "type": "config", "value": config},
		map[string]interface{}{"id": shared.RandUUID(), "type": "context", "value": ctxVal},
	}
	now := time.Now().Format(time.RFC3339)
	for _, m := range req.Messages {
		text := messageText(m)
		switch m.Role {
		case "assistant":
			transcript = append(transcript, map[string]interface{}{
				"id": shared.RandUUID(), "type": "agent-inference",
				"value": []interface{}{map[string]interface{}{"type": "text", "content": text}},
			})
		default: // user / system / tool 都并入 user 文本
			transcript = append(transcript, map[string]interface{}{
				"id": shared.RandUUID(), "type": "user",
				"value":     []interface{}{[]interface{}{text}},
				"userId":    c.UserID,
				"createdAt": now,
			})
		}
	}

	payload := map[string]interface{}{
		"traceId":                 shared.RandUUID(),
		"spaceId":                 c.SpaceID,
		"transcript":              transcript,
		"threadId":                threadID,
		"createThread":            false,
		"isPartialTranscript":     true,
		"asPatchResponse":         true,
		"generateTitle":           true,
		"saveAllThreadOperations": true,
		"threadType":              threadType,
	}
	if isGemini {
		payload["debugOverrides"] = map[string]interface{}{
			"emitAgentSearchExtractedResults": true,
			"cachedInferences":                map[string]interface{}{},
			"annotationInferences":            map[string]interface{}{},
			"emitInferences":                  false,
		}
	}
	return payload
}

// messageText 信封消息 → 纯文本（Notion transcript 只认文本；parts 取 text 补齐）。
func messageText(m *pb.EnvelopeMessage) string {
	if strings.TrimSpace(m.Text) != "" {
		return m.Text
	}
	var b strings.Builder
	for _, part := range m.Parts {
		if part.Type == "text" && part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

var blockIDHex = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// normalizeBlockID 32 位裸 hex → 连字符 UUID 形态；已带连字符或非法则原样返回。
func normalizeBlockID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	b := strings.ReplaceAll(id, "-", "")
	if blockIDHex.MatchString(b) {
		return fmt.Sprintf("%s-%s-%s-%s-%s", b[0:8], b[8:12], b[12:16], b[16:20], b[20:])
	}
	return id
}

// scanNDJSON runInference 的 NDJSON 流：逐行解析，增量 "x" 补丁直接流式发出，
// 完整内容（markdown-chat / record-map / 完整 patch）仅在无增量时兜底发出（清洗后）。
func (p *plugin) scanNDJSON(body io.Reader, stream pb.ClawPlugin_ChatServer) error {
	tmp := make([]byte, 64*1024)
	var pending string
	sawIncremental := false
	sawAny := false
	finalMsg := ""

	emit := func(ev *pb.StreamEvent) { _ = stream.Send(ev) }

	handleLine := func(line string) {
		for _, tc := range parseNDJSONLine(line) {
			sawAny = true
			switch tc.kind {
			case "incremental":
				if tc.text != "" {
					sawIncremental = true
					emit(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
						ContentDelta: &pb.ContentDelta{Text: tc.text},
					}})
				}
			case "final":
				finalMsg = tc.text
			}
		}
	}

	for {
		n, rerr := body.Read(tmp)
		if n > 0 {
			pending += string(tmp[:n])
			for {
				i := strings.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := strings.TrimSuffix(pending[:i], "\r")
				pending = pending[i+1:]
				if strings.TrimSpace(line) != "" {
					handleLine(line)
				}
			}
		}
		if rerr != nil {
			if strings.TrimSpace(pending) != "" {
				handleLine(pending)
			}
			if rerr != io.EOF {
				emit(shared.Failed(502, "notion 流中断: "+rerr.Error()))
				return nil
			}
			break
		}
	}

	// 无增量但有完整消息：清洗后一次性发出（Gemini / record-map 路径）
	if !sawIncremental && finalMsg != "" {
		if cleaned := cleanContent(finalMsg); cleaned != "" {
			emit(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
				ContentDelta: &pb.ContentDelta{Text: cleaned},
			}})
		}
	}

	if !sawAny {
		emit(shared.Failed(502, "notion 未返回任何有效文本（请检查 Cookie / space_id / user_id 是否有效）"))
		return nil
	}

	emit(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: "stop"},
	}})
	return nil
}

// textChunk 一行解析出的文本片段：kind=incremental（增量）/ final（完整）。
type textChunk struct {
	kind string
	text string
}

// parseNDJSONLine 解析一行 NDJSON → 文本片段（照 notion-2api 的三种上游格式）。
func parseNDJSONLine(line string) []textChunk {
	var out []textChunk
	var data map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(line)), &data) != nil {
		return out
	}
	typ := rawStr(data["type"])

	switch typ {
	case "markdown-chat": // 格式1：Gemini 直接事件
		if v := rawStr(data["value"]); v != "" {
			out = append(out, textChunk{"final", v})
		}

	case "patch": // 格式2：补丁流（Claude/GPT 增量 + 完整；Gemini patch）
		var ops []map[string]json.RawMessage
		if json.Unmarshal(data["v"], &ops) == nil {
			for _, op := range ops {
				o := rawStr(op["o"])
				path := rawStr(op["p"])
				switch {
				// Gemini 完整内容：a + /s/- + {type:markdown-chat}
				case o == "a" && strings.HasSuffix(path, "/s/-"):
					if v := objField(op["v"], "markdown-chat", "value"); v != "" {
						out = append(out, textChunk{"final", v})
					}
				// Claude/GPT 完整内容：a + /value/- + {type:text}
				case o == "a" && strings.HasSuffix(path, "/value/-"):
					if v := objField(op["v"], "text", "content"); v != "" {
						out = append(out, textChunk{"final", v})
					}
				// Gemini 增量：x + /s/ + /value 结尾 + 字符串
				case o == "x" && strings.Contains(path, "/s/") && strings.HasSuffix(path, "/value"):
					if v := rawStr(op["v"]); v != "" {
						out = append(out, textChunk{"incremental", v})
					}
				// Claude/GPT 增量：x + 含 /value/ + 字符串
				case o == "x" && strings.Contains(path, "/value/"):
					if v := rawStr(op["v"]); v != "" {
						out = append(out, textChunk{"incremental", v})
					}
				}
			}
		}

	case "record-map": // 格式3：record-map 里的 thread_message
		if v := parseRecordMap(data["recordMap"]); v != "" {
			out = append(out, textChunk{"final", v})
		}
	}
	return out
}

// parseRecordMap 从 recordMap.thread_message 提取首条有效消息文本。
func parseRecordMap(raw json.RawMessage) string {
	var rm struct {
		ThreadMessage map[string]struct {
			Value struct {
				Value struct {
					Step json.RawMessage `json:"step"`
				} `json:"value"`
			} `json:"value"`
		} `json:"thread_message"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &rm) != nil {
		return ""
	}
	for _, msg := range rm.ThreadMessage {
		step := msg.Value.Value.Step
		if len(step) == 0 {
			continue
		}
		var s struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(step, &s) != nil {
			continue
		}
		switch s.Type {
		case "markdown-chat":
			var v string
			if json.Unmarshal(s.Value, &v) == nil && v != "" {
				return v
			}
		case "agent-inference":
			var items []struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			}
			if json.Unmarshal(s.Value, &items) == nil {
				for _, it := range items {
					if it.Type == "text" && it.Content != "" {
						return it.Content
					}
				}
			}
		}
	}
	return ""
}

// rawStr RawMessage → 字符串（非字符串返回空）。
func rawStr(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// objField 解 {type: wantType, <field>: string}，type 匹配时返回字段值。
func objField(raw json.RawMessage, wantType, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	if rawStr(obj["type"]) != wantType {
		return ""
	}
	return rawStr(obj[field])
}

var (
	reLang     = regexp.MustCompile(`(?s)<lang primary="[^"]*"\s*/>\n*`)
	reThinking = regexp.MustCompile(`(?is)<thinking>.*?</thinking>\s*`)
	reThought  = regexp.MustCompile(`(?is)<thought>.*?</thought>\s*`)
)

// cleanContent 清洗完整消息：剥离 <lang/> 标记与 <thinking>/<thought> 推理块。
func cleanContent(s string) string {
	if s == "" {
		return ""
	}
	s = reLang.ReplaceAllString(s, "")
	s = reThinking.ReplaceAllString(s, "")
	s = reThought.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}
