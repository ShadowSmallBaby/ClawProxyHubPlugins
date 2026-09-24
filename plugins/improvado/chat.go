// 信封 → 上游消息转换、Chat 编排与 SSE 流解析。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// upstreamMessage 上游消息形态（content + parts 双份）。
type upstreamMessage struct {
	ID      string     `json:"id"`
	Role    string     `json:"role"`
	Content string     `json:"content"`
	Parts   []textPart `json:"parts"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	ws := c.WorkspaceID
	if ws == "" {
		ws = p.settingStr("workspace_id")
	}
	if ws == "" {
		return stream.Send(shared.Failed(400, "缺少 Workspace ID（凭据或插件设置里配置）"))
	}

	// 上游只认纯文本消息：parts 取 text 拼接
	var messages []upstreamMessage
	for _, m := range req.Messages {
		text := m.Text
		for _, part := range m.Parts {
			if part.Type == "text" {
				text = shared.OrDefault(text, part.Text)
			}
		}
		if m.Role == "tool" {
			// 工具结果以 user 文本形式回传（上游无 tool 角色）
			text = "Tool result:\n" + m.Text
			messages = append(messages, upstreamMessage{ID: shared.RandHex(16), Role: "user", Content: text, Parts: []textPart{{Type: "text", Text: text}}})
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		messages = append(messages, upstreamMessage{ID: shared.RandHex(16), Role: m.Role, Content: text, Parts: []textPart{{Type: "text", Text: text}}})
	}
	if len(messages) == 0 {
		return stream.Send(shared.Failed(400, "messages 为空"))
	}
	// 无会话 id：转发全量历史，上游 execute 每轮从头读完整上下文（截断会导致多轮失忆）

	payload := map[string]interface{}{
		"messages":    messages,
		"executionId": nil,
		"provider":    c.Provider,
		"model":       shared.OrDefault(req.Model, "gpt-5.6-sol"),
		"effort":      c.Effort,
		"forceApiKey": false,
	}
	raw, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", upstream, bytes.NewReader(raw))
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Im-Workspace-Id", ws)
	httpReq.Header.Set("Origin", origin)
	httpReq.Header.Set("Referer", origin+"/experimental/agent/new-agent/?workspace="+ws)
	httpReq.Header.Set("Cookie", c.Cookie)

	resp, err := p.hc(c).Do(httpReq)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 不回传上游响应体（可能含提示词 / 账号元数据）
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		code := int32(502)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			code = 401
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("upstream returned %s", resp.Status)))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	return p.scanSSE(resp.Body, stream)
}

// scanSSE 上游 SSE：data: {"type":"text-delta","delta":"..."}，仅转发文本增量。
func (p *plugin) scanSSE(body io.Reader, stream pb.ClawPlugin_ChatServer) error {
	tmp := make([]byte, 64*1024)
	var pending string
	sawEvent := false
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if data == "[DONE]" {
			return nil
		}
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return nil // 非 JSON 行（心跳 / 注释）静默跳过
		}
		if ev.Type == "text-delta" && ev.Delta != "" {
			sawEvent = true
			if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
				ContentDelta: &pb.ContentDelta{Text: ev.Delta},
			}}); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			pending += string(tmp[:n])
			for {
				i := strings.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := strings.TrimSuffix(pending[:i], "\r")
				pending = pending[i+1:]
				if line == "" {
					if err := flush(); err != nil {
						return err
					}
					continue
				}
				if strings.HasPrefix(line, "data:") {
					dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				}
			}
		}
		if err != nil {
			if err := flush(); err != nil {
				return err
			}
			break
		}
	}
	if !sawEvent {
		return stream.Send(shared.Failed(502, "upstream returned an empty stream"))
	}
	return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: "stop"},
	}})
}
