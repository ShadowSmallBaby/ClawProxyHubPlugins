// Package main — zcode 对话：统一信封 → Anthropic 上游（双计划）→ StreamEvent。
// coding-plan：直连 Anthropic 端点 + V4 签名；start-plan：zcode.z.ai JWT 网关 + system 块注入。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/anthropicup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// signer 插件级 V4 签名管理器（懒建，origin 固定 zcode.z.ai）。
func (p *plugin) signerFor() *signer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sg == nil {
		p.sg = newSigner(p, "https://zcode.z.ai")
	}
	return p.sg
}

// upstreamBase 上游 Anthropic 端点。
func upstreamBase(provider string) string {
	if provider == "bigmodel" {
		return "https://api.z.ai/api/anthropic"
	}
	return "https://api.z.ai/api/anthropic"
}

// Chat 统一信封 → Anthropic SSE。
func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}

	body := anthropicup.ChatBody(req)
	body["model"] = req.Model
	body["stream"] = true

	// UA：核心注入优先，否则 ZCode 桌面客户端形态（SDK 后缀只加 LLM 平面）
	ua := "ZCode/" + p.appVersion() + " " + anthropicSDKSuffix
	if v := req.Extra[sdk.ExtraClientUserAgent]; v != "" {
		ua = v
	}

	endpoint, headers := p.buildUpstream(ctx, cred, req, body, ua)

	// start-plan 网关注入 system 块 + context_prefix（3012 拒载防线）
	if cred.Plan == "start-plan" {
		if err := p.applyStartPlanBody(body, cred, req); err != nil {
			return stream.Send(shared.Failed(502, err.Error()))
		}
	}
	raw, _ := json.Marshal(body)

	// V4 签名（coding-plan 双密钥 + 门开才签；fail-open）
	sc := &signCtx{ctx: ctx, url: endpoint, headers: headers, cred: cred.APIKey, appVersion: p.appVersion()}
	if cred.Plan == "coding-plan" {
		sc.headers["x-session-id"] = sessionIDFor(req)
		if signed, _ := p.signerFor().signHeaders(sc); signed != nil {
			headers = signed
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.hc(cred).Do(httpReq)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			code = 401
		case resp.StatusCode == 429:
			code = 429
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}

	// 上游固定 Anthropic 协议，按入口方言选解析器回吐
	parser := anthropicup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return sdk.ScanSSE(resp.Body, parser)
}

// buildUpstream 端点 + identity/trace 头（coding-plan 双 auth 头；start-plan Bearer JWT）。
func (p *plugin) buildUpstream(ctx context.Context, cred *credential, req *pb.ChatRequest, body map[string]interface{}, ua string) (string, map[string]string) {
	headers := p.llmIdentityHeaders(cred)
	headers["User-Agent"] = ua

	if cred.Plan == "start-plan" && cred.JWT != "" {
		headers["Authorization"] = "Bearer " + cred.JWT
		headers["anthropic-version"] = anthropicVersion
		return "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages", headers
	}
	// coding-plan：bundle ebo 双 auth 头（x-api-key + Authorization 同值）
	headers["x-api-key"] = cred.APIKey
	headers["Authorization"] = "Bearer " + cred.APIKey
	headers["anthropic-version"] = anthropicVersion
	// trace/attribution 头（bundle Bdt 形态）
	headers["x-request-id"] = shared.RandUUID()
	headers["x-zcode-session-type"] = "main"
	headers["x-zcode-trace-id"] = shared.RandUUID()
	headers["x-query-id"] = shared.RandUUID()
	return upstreamBase(cred.Provider) + "/v1/messages", headers
}

// sessionIDFor 对话会话 id（信封无 session 概念，按请求生成；签名与 trace 共用）。
func sessionIDFor(req *pb.ChatRequest) string {
	return shared.RandUUID()
}

// applyStartPlanBody start-plan 网关身份块：system 三块（ephemeral）+ currentDate 前缀 user 消息。
func (p *plugin) applyStartPlanBody(body map[string]interface{}, cred *credential, req *pb.ChatRequest) error {
	data, err := p.systemBlocks()
	if err != nil {
		return err
	}
	var cliPrefix, dynBefore, dynAfter string
	var envMap, cpMap, srMap map[string]string
	json.Unmarshal(data["cliPrefix"], &cliPrefix)
	var stableArr []string
	json.Unmarshal(data["stableSections"], &stableArr)
	json.Unmarshal(data["dynamicSections"], &struct {
		BeforeEnvironment *string
		AfterEnvironment  *string
	}{&dynBefore, &dynAfter})
	json.Unmarshal(data["environment"], &envMap)
	json.Unmarshal(data["contextPrefix"], &cpMap)
	json.Unmarshal(data["systemReminder"], &srMap)

	// Environment 段（真实运行时值 + powered-by）
	env := strings.Join([]string{
		envMap["heading"],
		envMap["invokedLine"],
		"- " + envMap["cwdLabel"] + ": " + cwdOrUnknown(),
		"- " + envMap["gitLabel"] + ": " + envMap["gitNo"],
		"- " + envMap["platformLabel"] + ": " + Platform(),
		"- " + envMap["shellLabel"] + ": " + shellName(),
		"- " + envMap["osVersionLabel"] + ": " + osVersionLine(),
		"- " + strings.Replace(strings.Replace(envMap["poweredByLine"], "{provider}", providerID(cred.Provider), 1), "{model}", req.Model, 1),
	}, "\n")

	stableText := strings.Join(stableArr, "\n\n")
	dynamic := strings.Join([]string{dynBefore, env, dynAfter}, "\n\n")

	system := []map[string]interface{}{
		{"type": "text", "text": cliPrefix, "cache_control": map[string]string{"type": "ephemeral"}},
		{"type": "text", "text": stableText, "cache_control": map[string]string{"type": "ephemeral"}},
		{"type": "text", "text": "\n\n" + dynamic, "cache_control": map[string]string{"type": "ephemeral"}},
	}
	// 客户端 system 块保留在官方块之后（剥 cache_control）
	if existing, ok := body["system"]; ok {
		system = append(system, normalizeUserSystem(existing)...)
	}
	body["system"] = system

	// context_prefix：currentDate <system-reminder> 前缀 user 消息
	if msgs, ok := body["messages"].([]interface{}); ok && len(msgs) > 0 {
		date := time.Now().Format("2006-01-02")
		currentDate := cpMap["currentDateHeading"] + "\n" + strings.Replace(cpMap["currentDateLine"], "{date}", date, 1)
		text := srMap["open"] + strings.Join([]string{cpMap["intro"], currentDate, "", cpMap["outro"]}, "\n") + srMap["close"]
		prefix := map[string]interface{}{
			"role":    "user",
			"content": []interface{}{map[string]interface{}{"type": "text", "text": text}},
		}
		body["messages"] = append([]interface{}{prefix}, msgs...)
	}
	// tools 的 cache_control 剥离（官方 3 块 + 末消息标记已占满 4 断点预算）
	if tools, ok := body["tools"].([]interface{}); ok {
		for _, t := range tools {
			if m, ok := t.(map[string]interface{}); ok {
				delete(m, "cache_control")
			}
		}
	}
	return nil
}

// normalizeUserSystem 客户端 system 块归一化（string / 块数组；剥 cache_control）。
func normalizeUserSystem(v interface{}) []map[string]interface{} {
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []map[string]interface{}{{"type": "text", "text": s}}
		}
	case []interface{}:
		var out []map[string]interface{}
		for _, item := range t {
			if m, ok := item.(map[string]interface{}); ok && m["type"] == "text" {
				if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, map[string]interface{}{"type": "text", "text": s})
				}
			} else if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, map[string]interface{}{"type": "text", "text": s})
			}
		}
		return out
	}
	return nil
}

func cwdOrUnknown() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "unknown"
}

func shellName() string {
	if v := os.Getenv("SHELL"); v != "" {
		return filepath.Base(v)
	}
	if v := os.Getenv("ComSpec"); v != "" {
		return filepath.Base(v)
	}
	return "unknown"
}

func osVersionLine() string {
	if r := Release(); r != "" {
		return Platform() + " " + r
	}
	return Platform()
}

func providerID(provider string) string {
	if provider == "bigmodel" {
		return "bigmodel-api"
	}
	return "zai-api"
}

// ---------- SSE 扫描 ----------

// idCounter 同毫秒内的单调计数。
var idCounter uint64

var (
	_ = atomic.AddUint64
	_ = rand.Read
)
