// chat.go — 对话：messages 入口直通 /v1/messages（New API 原生支持），其余走 /v1/chat/completions。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/anthropicup"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(failed(401, err.Error()))
	}
	if cred.APIKey == "" {
		// 会话模式尚未取得密钥明文：按 401 报出，核心会触发 Refresh 重取后重试
		return stream.Send(failed(401, "该账号尚未取得 API 密钥（站点未返回明文）"))
	}
	site, err := p.site(cred.instanceID)
	if err != nil {
		return stream.Send(failed(500, err.Error()))
	}

	var (
		path   string
		body   map[string]interface{}
		parser sseParser
	)
	if req.Source == "messages" {
		path = "/v1/messages"
		body = anthropicup.ChatBody(req)
		parser = anthropicup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	} else {
		path = "/v1/chat/completions"
		body = openaiup.ChatBody(req)
		parser = openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	}
	body["model"] = req.Model
	body["stream"] = true
	raw, _ := json.Marshal(body)

	resp, err := p.do(ctx, cred, "POST", site.BaseURL+path, gatewayHeaders(cred, req.Extra[sdk.ExtraClientUserAgent]), bytes.NewReader(raw))
	if err != nil {
		return stream.Send(failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		switch resp.StatusCode {
		case 401, 403:
			code = 401
		case 402, 429:
			code = int32(resp.StatusCode)
		}
		return stream.Send(failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(errBody), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	return scanSSE(resp.Body, parser)
}

type sseParser interface {
	Feed(string)
	Finish()
	FinishWithError(int32, string)
}

// scanSSE 通用 SSE 扫描：空流兜底 + 结束收尾。
func scanSSE(body io.Reader, parser sseParser) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sawEvent := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") && !strings.Contains(line, "[DONE]") {
			sawEvent = true
		}
		parser.Feed(line)
	}
	if err := scanner.Err(); err != nil {
		parser.FinishWithError(502, "upstream stream broken: "+err.Error())
		return nil
	}
	if !sawEvent {
		parser.FinishWithError(502, "upstream returned an empty stream")
		return nil
	}
	parser.Finish()
	return nil
}
