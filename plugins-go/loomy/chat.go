// chat.go — 对话入口：单 content 建流 + OpenAI 兼容 SSE 解析 + 空回重试。
package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"io"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// chatAttempts 单请求最多尝试次数：prime 到首个正文前不下发事件，空回可安全换会话重试。
const chatAttempts = 2

// Chat 无状态：每轮开新 Loomy 对话并回放完整历史（信封已带多轮），不维持会话 id。
func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	model := resolveModel(req.GetModel())
	if model == "" {
		return stream.Send(shared.Failed(400, "不支持的模型: "+req.GetModel()))
	}
	if !hasUserText(req) {
		return stream.Send(shared.Failed(400, "messages 不能为空"))
	}

	ctx := stream.Context()
	var last error = fmt.Errorf("Loomy 未产生文本输出")
	for attempt := 1; attempt <= chatAttempts; attempt++ {
		content := buildContent(req, attempt > 1)
		emitted, sErr := p.streamOnce(ctx, cred, model, content, stream)
		if emitted {
			return nil
		}
		if sErr != nil {
			last = sErr
			if isAuthErr(sErr) {
				break // 鉴权失效不重试
			}
		}
		// 空回或瞬时错误：尚未下发任何事件，换新会话重试。
	}
	return stream.Send(shared.Failed(mapErr(last), last.Error()))
}

// streamOnce 发一次请求并转发 SSE。prime 语义：读到首个正文增量前不向下游发任何
// 事件，故空流可安全重试。返回 (是否产出正文, 未产出时的错误)。
func (p *plugin) streamOnce(ctx context.Context, cred *credential, model, content string,
	stream pb.ClawPlugin_ChatServer) (bool, error) {

	payload := map[string]interface{}{
		"conversationId": "",
		"messageId":      "web-" + shared.RandUUID(),
		"model":          model,
		"content":        content,
	}
	resp, err := p.openStream(ctx, cred, payload)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	emitted := false
	finish := "stop"
	terminal := false

	// emit 消费一个 SSE data 载荷，返回是否应结束流。
	emit := func(data string) (bool, error) {
		data = strings.TrimSpace(data)
		if data == "" {
			return false, nil
		}
		if data == "[DONE]" {
			terminal = true
			return true, nil
		}
		var obj map[string]interface{}
		if json.Unmarshal([]byte(data), &obj) != nil {
			return false, fmt.Errorf("invalid upstream JSON")
		}
		if reason := finishReasonOf(obj); reason != "" {
			terminal = true
			finish = reason
		}
		if finishReasonOf(obj) == "length" {
			finish = "length"
		}
		text := contentOf(obj)
		if text == "" {
			return false, nil
		}
		if !emitted {
			if e := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
				MessageStart: &pb.MessageStart{Model: model},
			}}); e != nil {
				return true, e
			}
			emitted = true
		}
		return false, stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
			ContentDelta: &pb.ContentDelta{Text: text},
		}})
	}

	sendErr := sdk.ReadSSE(resp.Body, 1<<20, func(line string) error {
		if strings.HasPrefix(line, "data:") {
			stop, err := emit(strings.TrimPrefix(line[5:], " "))
			if err != nil {
				return err
			}
			if stop {
				return io.EOF
			}
		} else {
			raw := strings.TrimSpace(line)
			if strings.HasPrefix(raw, "{") && strings.Contains(raw, `"code"`) {
				return loomyErrf(502, "Loomy 返回错误: %s", shared.Truncate(raw, 300))
			}
		}
		return nil
	})

	if !emitted {
		if err := sendErr; err != nil {
			return false, loomyErrf(502, "读取流失败: %v", err)
		}
		return false, nil // 空流：可重试
	}
	if sendErr != nil {
		_ = stream.Send(shared.Failed(502, sendErr.Error()))
		return true, nil // 下游已断，无需再发
	}
	if !terminal {
		_ = stream.Send(shared.Failed(502, "upstream ended before a terminal event"))
		return true, nil
	}
	_ = stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: finish, Usage: &pb.Usage{}},
	}})
	return true, nil
}

// contentOf 取 OpenAI chunk 的 choices[0].delta.content。
func contentOf(obj map[string]interface{}) string {
	choices, _ := obj["choices"].([]interface{})
	if len(choices) == 0 {
		return ""
	}
	c0, _ := choices[0].(map[string]interface{})
	delta, _ := c0["delta"].(map[string]interface{})
	s, _ := delta["content"].(string)
	return s
}

// finishReasonOf 取 choices[0].finish_reason。
func finishReasonOf(obj map[string]interface{}) string {
	choices, _ := obj["choices"].([]interface{})
	if len(choices) == 0 {
		return ""
	}
	c0, _ := choices[0].(map[string]interface{})
	fr, _ := c0["finish_reason"].(string)
	return fr
}

// mapErr 错误 → 信封错误码（鉴权失效 401，其余取携带状态码，默认 502）。
func mapErr(err error) int32 {
	if isAuthErr(err) {
		return 401
	}
	return int32(statusOf(err))
}
