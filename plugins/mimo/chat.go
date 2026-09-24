// chat.go — 对话：统一信封 → OpenAI 兼容 /api/route/chat/completions（流式 + 非流式均恒流式上行，
// 与 newapi 不同的是 401 需强制刷新 SSO 后重试一次）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// previewModels 旧 Preview 档（上游兼容期，需 thinking/temperature/top_p 默认值）。
var previewModels = map[string]bool{
	"mimo-x-pro-preview":   true,
	"mimo-x-flash-preview": true,
}

// upstreamModel 对外短名 → 上游真实 id（preview 档带 xiaomi/ 命名空间）。
func upstreamModel(model string) string {
	bare := model
	if i := strings.LastIndexByte(bare, '/'); i >= 0 {
		bare = bare[i+1:]
	}
	if previewModels[bare] {
		return "xiaomi/" + bare
	}
	return bare
}

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	cfg := p.settings(cred.instanceID)

	body := openaiup.ChatBody(req)
	body["model"] = upstreamModel(req.Model)
	body["stream"] = true
	if previewModels[strings.TrimPrefix(body["model"].(string), "xiaomi/")] {
		// preview 档必填默认值（v2.6 裸请求可用，不覆盖）
		body["thinking"] = map[string]interface{}{"type": "enabled"}
		if _, ok := body["temperature"]; !ok {
			body["temperature"] = 1.0
		}
		if _, ok := body["top_p"]; !ok {
			body["top_p"] = 0.95
		}
	}
	raw, _ := json.Marshal(body)

	url := cfg.APIBase + "/api/route/chat/completions"
	force := false
	for attempt := 0; attempt < 2; attempt++ {
		cookie, err := p.getServiceCookie(ctx, cfg, cred, force)
		if err != nil {
			return stream.Send(shared.Failed(502, "session unavailable: "+err.Error()))
		}
		headers := map[string]string{
			"Content-Type": "application/json",
			"Accept":       "text/event-stream",
			"User-Agent":   cfg.APIUA,
			"Cookie":       cookie,
		}
		if ua := req.Extra[sdk.ExtraClientUserAgent]; ua != "" {
			headers["User-Agent"] = ua
		}

		resp, err := postStream(ctx, cred, url, headers, raw)
		if err != nil {
			return stream.Send(shared.Failed(502, err.Error()))
		}
		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			force = true // serviceToken 过期：强制重跑 SSO 再试一次
			continue
		}
		if resp.StatusCode != 200 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			resp.Body.Close()
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
			MessageStart: &pb.MessageStart{Model: req.Model},
		}}); err != nil {
			resp.Body.Close()
			return err
		}
		parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
		err = sdk.ScanSSE(resp.Body, parser)
		resp.Body.Close()
		return err
	}
	return stream.Send(shared.Failed(502, "unreachable"))
}

// postStream 发上游 POST（SSE 上行）。
func postStream(ctx context.Context, cred *credential, url string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return sdk.UpstreamClient(cred.proxyURL).Do(req)
}
