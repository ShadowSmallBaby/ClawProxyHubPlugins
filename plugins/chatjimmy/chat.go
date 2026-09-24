// Chat 编排：信封 → chatjimmy 私有请求体，解析一次性纯文本 + <|stats|> 用量尾块。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// upstreamPayload chatjimmy.ai/api/chat 请求体。
type upstreamPayload struct {
	Messages    []chatMessage `json:"messages"`
	ChatOptions chatOptions   `json:"chatOptions"`
	Attachment  interface{}   `json:"attachment"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatOptions struct {
	SelectedModel string `json:"selectedModel"`
	SystemPrompt  string `json:"systemPrompt"`
	TopK          int    `json:"topK"`
}

// upstreamStats <|stats|> 尾块的用量统计。
type upstreamStats struct {
	PrefillTokens int64 `json:"prefill_tokens"`
	DecodeTokens  int64 `json:"decode_tokens"`
	TotalTokens   int64 `json:"total_tokens"`
}

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred := credFrom(req.GetCredential())

	model := shared.OrDefault(req.Model, defaultModel)
	payload := upstreamPayload{
		Messages:    p.buildMessages(req),
		ChatOptions: chatOptions{SelectedModel: model, SystemPrompt: p.systemPrompt(req), TopK: p.topK()},
		Attachment:  nil,
	}
	if len(payload.Messages) == 0 {
		return stream.Send(shared.Failed(400, "messages 不能为空"))
	}
	raw, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(raw))
	if err != nil {
		return stream.Send(shared.Failed(500, err.Error()))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", clientUserAgent(req, p.userAgentStr()))

	resp, err := p.hc(cred).Do(httpReq)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return stream.Send(shared.Failed(mapStatus(resp.StatusCode),
			fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	// 上游整段生成后一次性返回纯文本（非 SSE），末尾附 <|stats|>JSON<|/stats|> 用量块。
	full, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return stream.Send(shared.Failed(502, "read upstream failed: "+err.Error()))
	}
	content, stats := parseUpstream(string(full))
	if strings.TrimSpace(content) == "" && stats == nil {
		return stream.Send(shared.Failed(502, "upstream returned empty response"))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: model},
	}}); err != nil {
		return err
	}
	// 纯文本无原生增量：一次性作为单个内容块发出。
	if content != "" {
		if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_ContentDelta{
			ContentDelta: &pb.ContentDelta{Text: content},
		}}); err != nil {
			return err
		}
	}
	return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: "stop", Usage: buildUsage(stats)},
	}})
}

// systemPrompt 抽取全部 system 消息拼成上游 systemPrompt。
func (p *plugin) systemPrompt(req *pb.ChatRequest) string {
	var parts []string
	for _, m := range req.Messages {
		if m.Role == "system" {
			if t := messageText(m); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// buildMessages 信封消息 → 上游 messages（剔除 system；tool 结果并入 user；仅取文本）。
func (p *plugin) buildMessages(req *pb.ChatRequest) []chatMessage {
	var out []chatMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue
		}
		text := messageText(m)
		role := m.Role
		if role == "tool" {
			// 上游不支持工具协议：把工具结果作为 user 上下文回灌。
			role = "user"
		}
		if role != "user" && role != "assistant" {
			role = "user"
		}
		if text == "" {
			continue
		}
		out = append(out, chatMessage{Role: role, Content: text})
	}
	return out
}

// messageText 取信封消息的文本内容（优先 Text，回退拼接 text parts）。
func messageText(m *pb.EnvelopeMessage) string {
	if m.Text != "" {
		return m.Text
	}
	var b strings.Builder
	for _, part := range m.Parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// parseUpstream 拆分正文与 <|stats|> 用量块。
func parseUpstream(raw string) (content string, stats *upstreamStats) {
	start := strings.LastIndex(raw, statsOpen)
	if start < 0 {
		return raw, nil
	}
	content = raw[:start]
	end := strings.LastIndex(raw, statsClose)
	if end < 0 || end < start+len(statsOpen) {
		return content, nil
	}
	var s upstreamStats
	if json.Unmarshal([]byte(raw[start+len(statsOpen):end]), &s) != nil {
		return content, nil
	}
	return content, &s
}

// buildUsage 上游 stats → 信封用量（无缓存语义，input=prefill、output=decode）。
func buildUsage(stats *upstreamStats) *pb.Usage {
	if stats == nil {
		return nil
	}
	return &pb.Usage{InputTokens: stats.PrefillTokens, OutputTokens: stats.DecodeTokens}
}

// mapStatus 上游 HTTP 状态 → 信封错误码。
func mapStatus(code int) int32 {
	switch {
	case code == 401 || code == 403:
		return 401
	case code == 429:
		return 429
	default:
		return 502
	}
}

// clientUserAgent 核心注入的客户端 UA 优先（路由 UA > 全局 UA > 客户端 UA），回退插件默认。
func clientUserAgent(req *pb.ChatRequest, fallback string) string {
	if req.Extra != nil {
		if ua := req.Extra[sdk.ExtraClientUserAgent]; ua != "" {
			return ua
		}
	}
	return fallback
}
