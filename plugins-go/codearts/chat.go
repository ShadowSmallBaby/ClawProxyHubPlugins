// chat.go — 对话：POST /api/v2/chat/completions（OpenAI 方言 SSE，HMAC 签名）。
package codearts

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// newRequest 构造基础请求（body 原样携带）。
func newRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return req, nil
}

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	body := openaiup.ChatBody(req)
	body["model"] = shared.OrDefault(req.Model, "GLM-5.2")
	// prompt_cache_key 让服务端启用前缀缓存并在 usage 中返回 cached_tokens，
	// 缺少该字段时缓存命中恒为 0
	body["prompt_cache_key"] = shared.RandUUID()
	// 输出上限：大工具参数（如 file_write）需要数万 token 生成空间，
	// 沿用后端默认会中途截断成非法 JSON
	if _, ok := body["max_tokens"]; !ok {
		body["max_tokens"] = 65536
	}
	raw := mustJSON(body)

	// benefit（免费额度）模型必须带 maas_type: benefit 签名头（参与签名），
	// 缺该头后端报 InferHub.002002009.404 "model is not registered"
	var extraSigned map[string]string
	if p.codeartsBenefit(req.Model) {
		extraSigned = map[string]string{"maas_type": "benefit"}
	}

	httpReq, err := newSignedRequest(ctx, "POST", chatAPIBase+pathChat, raw, cred, extraSigned)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	resp, err := p.hc(cred).Do(httpReq)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw2 := shared.ReadLimitedResp(resp, 8192)
		code := int32(502)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			code = 401
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw2), 300))))
	}

	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return sdk.ScanSSE(resp.Body, parser)
}

// newSignedRequest 构造带签名的对话请求（Chat-Id/Session-Id 运行头在签名后追加）。
func newSignedRequest(ctx context.Context, method, url string, body []byte, cred *credential, extraSigned map[string]string) (*http.Request, error) {
	httpReq, err := newRequest(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	signHuawei(httpReq, cred.AccessKey, cred.SecretKey, cred.SecurityToken, body, extraSigned)
	httpReq.Header.Set("Chat-Id", shared.RandUUID())
	httpReq.Header.Set("Session-Id", shared.RandUUID())
	httpReq.Header.Set("lang", "en")
	return httpReq, nil
}
