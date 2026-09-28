// chat.go — 对话入口：单 content 建流 + OpenAI 兼容 SSE 解析 + 空回重试。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

	// emit 消费一个 SSE data 载荷，返回是否应结束流。
	emit := func(data string) (bool, error) {
		data = strings.TrimSpace(data)
		if data == "" {
			return false, nil
		}
		if data == "[DONE]" {
			return true, nil
		}
		var obj map[string]interface{}
		if json.Unmarshal([]byte(data), &obj) != nil {
			return false, nil
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

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20) // 长行上限 8MB
	var buf []string
	var sendErr error

	flush := func() (bool, error) {
		if len(buf) == 0 {
			return false, nil
		}
		data := strings.Join(buf, "\n")
		buf = buf[:0]
		return emit(data)
	}

loop:
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			stop, e := flush()
			if e != nil {
				sendErr = e
				break loop
			}
			if stop {
				break loop
			}
		case strings.HasPrefix(line, "data:"):
			buf = append(buf, strings.TrimSpace(line[5:]))
		default:
			// 裸 JSON 行（非 data: 帧）= Loomy 在健康连接上报错（Athena 900000 等）。
			s := strings.TrimSpace(line)
			if strings.HasPrefix(s, "{") && strings.Contains(s, `"code"`) {
				if !emitted {
					return false, loomyErrf(502, "Loomy 返回错误: %s", shared.Truncate(s, 300))
				}
				break loop // 已在转发正文：收尾即可
			}
		}
	}
	if sendErr == nil {
		if _, e := flush(); e != nil {
			sendErr = e
		}
	}

	if !emitted {
		if err := scanner.Err(); err != nil {
			return false, loomyErrf(502, "读取流失败: %v", err)
		}
		return false, nil // 空流：可重试
	}
	if sendErr != nil {
		return true, nil // 下游已断，无需再发
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
