// chat.go — 对话：模型选择器解析（[1m]/effort）+ Claude(messages)/Codex(responses) 分派 → relay SSE。
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/anthropicup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/responsesup"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// effortLadder relay 接受的努力档（ultra 折为 max）。
var effortLadder = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	s := p.settings(req.GetCredential().GetInstanceId())
	rc := p.relay(c, s)

	// 解析模型选择器：剥离 [1m] / (effort) 后缀，得真实 id + 控制参数
	realModel, longContext, effort, effortErr := parseModelSelector(req.Model)
	if effortErr != "" {
		return stream.Send(shared.Failed(400, effortErr))
	}

	claude := isClaudeModel(realModel)
	var path string
	var body []byte
	// 用真实模型名重建请求
	req.Model = realModel
	if claude {
		path = "/v1/messages"
		payload := anthropicup.ChatBody(req)
		payload["model"] = realModel
		applyClaudeThinking(payload, effort)
		body, _ = json.Marshal(payload)
	} else {
		path = "/v1/responses"
		payload := responsesup.ChatBody(req)
		payload["model"] = realModel
		payload["stream"] = true // Codex 协议恒流式上行
		applyCodexThinking(payload, effort)
		body, _ = json.Marshal(payload)
	}

	extra := map[string]string{}
	if claude {
		extra["Anthropic-Version"] = "2023-06-01"
		if longContext {
			extra["Anthropic-Beta"] = "context-1m-2025-08-07"
		}
	}

	resp, err := rc.relayDo(ctx, path, body, extra)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errBody := shared.ReadLimited(resp.Body, 8192)
		code := int32(502)
		switch resp.StatusCode {
		case 401, 403:
			code = 401
		case 429:
			code = 429
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: realModel},
	}}); err != nil {
		return err
	}
	var parser interface {
		Feed(string)
		Finish()
		FinishWithError(int32, string)
	}
	if claude {
		parser = anthropicup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	} else {
		parser = responsesup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	}
	return sdk.ScanSSE(resp.Body, parser)
}

// parseModelSelector 剥离 mirasim/ 前缀、[1m] 长上下文别名、(effort) 后缀。
// 返回真实模型 id、是否长上下文、归一化 effort（ultra→max）、错误文案（effort 越界）。
func parseModelSelector(model string) (real string, longContext bool, effort string, errMsg string) {
	real = strings.TrimSpace(model)
	for strings.HasPrefix(real, "mirasim/") {
		real = real[len("mirasim/"):]
	}
	// (effort) 后缀
	if i := strings.LastIndexByte(real, '('); i >= 0 && strings.HasSuffix(real, ")") {
		effort = strings.ToLower(strings.TrimSpace(real[i+1 : len(real)-1]))
		real = strings.TrimSpace(real[:i])
		if effort == "ultra" {
			effort = "max" // ultra 即 max 的单请求形态
		}
		if effort != "" && !effortLadder[effort] && effort != "none" && effort != "auto" {
			return real, false, "", fmt.Sprintf("effort 须为 low/medium/high/xhigh/max/ultra，收到 %q", effort)
		}
	}
	// [1m] 长上下文别名
	if strings.HasSuffix(real, "[1m]") {
		longContext = true
		real = real[:len(real)-len("[1m]")]
	}
	return real, longContext, effort, ""
}

// applyClaudeThinking effort → Claude thinking（effort 形态：adaptive + output_config.effort）。
func applyClaudeThinking(payload map[string]interface{}, effort string) {
	switch effort {
	case "", "auto":
		return
	case "none":
		payload["thinking"] = map[string]interface{}{"type": "disabled"}
		delete(payload, "output_config")
	default:
		payload["thinking"] = map[string]interface{}{"type": "adaptive"}
		payload["output_config"] = map[string]interface{}{"effort": effort}
	}
}

// applyCodexThinking effort → Codex reasoning.effort。
func applyCodexThinking(payload map[string]interface{}, effort string) {
	switch effort {
	case "", "auto":
		return
	case "none":
		return
	default:
		payload["reasoning"] = map[string]interface{}{"effort": effort}
	}
}
