// envelope.go — 信封 → 上游 OpenAI 兼容请求体（messages/tools/tool_choice 转换）。
package main

import (
	"encoding/json"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// buildChatBody 组新版 OpenAI 兼容请求体（model/messages/stream/stream_options/metadata）。
func buildChatBody(req *pb.ChatRequest, reqID, sessID string) map[string]interface{} {
	model := req.Model
	if model == "" {
		model = "lite"
	}
	body := map[string]interface{}{
		"model":          model,
		"messages":       buildOpenAIMessages(req),
		"stream":         true,
		"stream_options": map[string]interface{}{"include_usage": true},
		"metadata": map[string]interface{}{
			"context": map[string]interface{}{
				"request_id":     reqID,
				"request_set_id": reqID,
				"session_id":     sessID,
				"task_id":        "common",
				"client_type":    "qodercli",
			},
		},
	}
	if len(req.Tools) > 0 {
		var tools []interface{}
		for _, t := range req.Tools {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  rawJSON(t.ParametersSchema),
				},
			})
		}
		body["tools"] = tools
		if tc := toolChoiceForOpenAI(req.GetToolChoice()); tc != nil {
			body["tool_choice"] = tc
		}
		if v := strings.TrimSpace(req.Extra["parallel_tool_calls"]); v != "" {
			body["parallel_tool_calls"] = rawJSON(v)
		}
	}
	return body
}

// toolChoiceForOpenAI 信封 tool_choice → OpenAI 形态：auto/none 直传字符串，
// tool 有名 → function 对象，tool 无名（对应 required）→ "required"。
func toolChoiceForOpenAI(tc *pb.ToolChoice) interface{} {
	if tc == nil || tc.GetType() == "" {
		return nil
	}
	switch tc.GetType() {
	case "tool":
		if n := tc.GetToolName(); n != "" {
			return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": n}}
		}
		return "required"
	default: // auto / none
		return tc.GetType()
	}
}

// buildOpenAIMessages 信封消息 → 标准 OpenAI messages（有图片走 content 数组，否则纯文本）。
func buildOpenAIMessages(req *pb.ChatRequest) []interface{} {
	out := make([]interface{}, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			var images []interface{}
			for _, part := range m.Parts {
				if part.Type == "image" {
					u := part.Url
					if u == "" {
						u = "data:" + part.MediaType + ";base64," + part.Data
					}
					images = append(images, map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": u}})
				}
			}
			if len(images) > 0 {
				parts := images
				if strings.TrimSpace(m.Text) != "" {
					parts = append(parts, map[string]interface{}{"type": "text", "text": m.Text})
				}
				out = append(out, map[string]interface{}{"role": "user", "content": parts})
			} else {
				out = append(out, map[string]interface{}{"role": "user", "content": m.Text})
			}
		case "assistant":
			msg := map[string]interface{}{"role": "assistant", "content": m.Text}
			if len(m.ToolCalls) > 0 {
				var tcs []interface{}
				for _, tc := range m.ToolCalls {
					tcs = append(tcs, map[string]interface{}{
						"id": tc.Id, "type": "function",
						"function": map[string]interface{}{"name": tc.Name, "arguments": tc.Arguments},
					})
				}
				msg["tool_calls"] = tcs
			}
			out = append(out, msg)
		case "tool":
			out = append(out, map[string]interface{}{"role": "tool", "content": m.Text, "tool_call_id": m.ToolCallId})
		default: // system
			out = append(out, map[string]interface{}{"role": "system", "content": m.Text})
		}
	}
	return out
}

// rawJSON 解析 JSON 字符串（失败回空对象）。
func rawJSON(s string) interface{} {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return map[string]interface{}{}
	}
	return v
}
