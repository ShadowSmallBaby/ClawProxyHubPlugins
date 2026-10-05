// doubao 上游协议：/chat/completion 请求构建 + SSE 流解析（照 doubao2api client.py）。
//
// 【401 登录态修复（NexPort 移动端反哺，PR 前的本地验证见 NexPort qa19）】
// chatCompletion 新增外发凭据 Cookie 头（cookieHeader()）：修复前插件仅 csrfToken()
// 消费 Cookies 一键、从不外发 Cookie 头，登录态鉴权完全缺失 → 建档即 401
// 「会话已过期」（上游返回 {"code":710012001,"msg":"登录已过期"}）。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

const (
	upstreamURL = "https://www.doubao.com"
	// defaultDeviceID/defaultWebID/defaultFp 内置设备指纹兜底（凭据未带时使用）。
	defaultDeviceID = "714003710229497"
	defaultWebID    = "7604137868021548590"
	defaultFp       = "verify_mlcfw5f7_TPq0YmFD_NrsC_4RuQ_BJPg_M5W7i58I7wV0"
	defaultBotID    = "7234781073513644036"
	// riskCodes 上游风控/限流错误码。
	riskCode1 int64 = 710022002
	riskCode2 int64 = 710022004
)

// defaultUserAgent 桌面浏览器形态 UA（对齐 qr_login.py CHROME_VERSION）。
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（SSE 流式：不限制总时长，空闲读 120s 重置）。
func (p *plugin) hc(cred *credential) *http.Client {
	key := ""
	if cred != nil {
		key = cred.proxyURL
	}
	if c, ok := proxyClients.Load(key); ok {
		return c.(*http.Client)
	}
	c := sdk.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
}

// upstreamClient 连接 15s / TLS 15s / 空闲读 120s（专家模式多轮搜索可超 3 分钟，不限总时长）。
// ---------- 请求构建 ----------

// modelDeepThink 模型 id → 上游 need_deep_think。
func modelDeepThink(model string) int {
	switch model {
	case "doubao-think":
		return 1
	case "doubao-expert":
		return 3
	default:
		return 0
	}
}

// securityParams 上游安全参数（对齐 client.py _security_params；msToken 空值触发风控，故不带空值）。
func securityParams(c *credential) url.Values {
	msToken := c.MsToken
	q := url.Values{
		"aid":                 {"582478"},
		"real_aid":            {"582478"},
		"device_id":           {c.DeviceID},
		"tea_uuid":            {c.DeviceID},
		"web_id":              {c.WebID},
		"device_platform":     {"web"},
		"language":            {"zh"},
		"region":              {"CN"},
		"sys_region":          {"CN"},
		"pkg_type":            {"release_version"},
		"version_code":        {"20800"},
		"pc_version":          {"2.1.7"},
		"chromium_version":    {"148.0.7816.0"},
		"client_platform":     {"pc_client"},
		"runtime":             {"web"},
		"runtime_version":     {"3.5.4"},
		"samantha_web":        {"1"},
		"use-olympus-account": {"1"},
		"fp":                  {c.Fp},
		"web_tab_id":          {shared.RandUUID()},
	}
	if msToken != "" {
		q.Set("msToken", msToken)
	}
	return q
}

// buildCompletionPayload /chat/completion 请求体（对齐 client.py _build_completion_payload）。
func buildCompletionPayload(text string, deepThink int, botID string) map[string]interface{} {
	return map[string]interface{}{
		"client_meta": map[string]interface{}{
			"local_conversation_id": "local_" + shared.RandHex(16),
			"conversation_id":       "",
			"bot_id":                botID,
			"last_section_id":       "",
			"last_message_index":    0,
		},
		"messages": []interface{}{map[string]interface{}{
			"local_message_id": shared.RandUUID(),
			"content_block": []interface{}{
				map[string]interface{}{
					"block_type": 10000,
					"content": map[string]interface{}{
						"text_block":     map[string]interface{}{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""},
						"pc_event_block": "",
					},
					"block_id": shared.RandUUID(), "parent_id": "", "meta_info": []interface{}{},
					"append_fields": []interface{}{}, "is_finish": true, "patch_type": 2,
				},
			},
			"message_status": 0,
		}},
		"option": map[string]interface{}{
			"send_message_scene": "", "create_time_ms": 0, "collect_id": "", "is_audio": false,
			"answer_with_suggest": true, "tts_switch": false, "need_deep_think": deepThink,
			"click_clear_context": false, "from_suggest": false, "is_regen": false, "is_replace": false,
			"disable_sse_cache": false, "select_text_action": "", "resend_for_regen": false,
			"scene_type": 0, "unique_key": shared.RandUUID(), "start_seq": 0,
			"need_create_conversation": true,
			"conversation_init_option": map[string]interface{}{"need_ack_conversation": true},
			"regen_query_id":           []interface{}{}, "edit_query_id": []interface{}{},
			"regen_instruction": "", "no_replace_for_regen": false, "message_from": 0,
			"shared_app_name": "", "action_bar_skill_id": 0,
			"sse_recv_event_options": map[string]interface{}{"support_chunk_delta": true},
			"is_ai_playground":       false,
		},
		"chat_ability": map[string]interface{}{},
		"ext": map[string]interface{}{
			"use_deep_think":                fmt.Sprintf("%d", deepThink),
			"fp":                            defaultFp,
			"use_submit_pipeline":           "1",
			"commerce_credit_config_enable": "0",
			"sub_conv_firstmet_type":        "1",
		},
	}
}

// ---------- 流式对话 ----------

// chatCompletion 发起 /chat/completion SSE 流并逐事件回调。
// 返回 HTTP/解析层错误；业务错误经 onEvent(SSE 事件 JSON) 内处理。
func (p *plugin) chatCompletion(ctx context.Context, cred *credential, req *pb.ChatRequest, model string,
	onSSE func(name string, data []byte) error) error {

	text := buildPrompt(req)
	deepThink := modelDeepThink(model)
	payload, _ := json.Marshal(buildCompletionPayload(text, deepThink, cred.BotID))

	u := upstreamURL + "/chat/completion?" + securityParams(cred).Encode()
	httpReq, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Origin", upstreamURL)
	httpReq.Header.Set("Referer", upstreamURL+"/chat")
	httpReq.Header.Set("User-Agent", p.userAgentStr())
	httpReq.Header.Set("x-tt-passport-csrf-token", csrfToken(cred))
	// 【401 登录态修复】凭据 Cookies 全量外发为 Cookie 头——登录态鉴权依据
	//（修复前出站请求无 Cookie 头，实测抓包证实 → 服务端视为未登录返回 401）。
	httpReq.Header.Set("Cookie", cookieHeader(cred))

	resp, err := p.hc(cred).Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))
	}

	// 非流式响应 = 上游 JSON 错误（如 {"code":710012001,"msg":"登录已过期"}）。
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return parseUpstreamError(string(body))
	}

	var eventName string
	return sdk.ReadSSE(resp.Body, 4<<20, func(line string) error {
		switch {
		case line == "":
			eventName = ""
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			name := eventName
			if name == "" {
				name = "message"
			}
			return onSSE(name, []byte(strings.TrimPrefix(line[5:], " ")))
		}
		return nil
	})
}

// parseUpstreamError 上游 JSON 错误体 / SSE 文本中的 gateway-error → 业务错误。
func parseUpstreamError(body string) error {
	s := strings.TrimSpace(body)
	if s == "" {
		return fmt.Errorf("upstream returned empty response")
	}
	// SSE 文本块中找 gateway-error
	for _, block := range strings.Split(s, "\n\n") {
		var name, data string
		for _, l := range strings.Split(block, "\n") {
			if strings.HasPrefix(l, "event:") {
				name = strings.TrimSpace(l[6:])
			} else if strings.HasPrefix(l, "data:") {
				data = strings.TrimSpace(l[5:])
			}
		}
		if name == "gateway-error" && data != "" {
			var obj struct {
				Code    interface{} `json:"code"`
				Message string      `json:"message"`
			}
			if json.Unmarshal([]byte(data), &obj) == nil && obj.Message != "" {
				return fmt.Errorf("gateway-error: %v %s", obj.Code, obj.Message)
			}
			return fmt.Errorf("gateway-error: %s", data)
		}
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			Code    int64       `json:"code"`
			Msg     string      `json:"msg"`
			Message string      `json:"message"`
			CodeAny interface{} `json:"-"`
		}
		if err := json.Unmarshal([]byte(s), &obj); err == nil && (obj.Code != 0 || obj.Msg != "") {
			if obj.Code == 710012001 || strings.Contains(obj.Msg, "登录已过期") {
				return errAuth(obj.Msg)
			}
			return fmt.Errorf("code=%d msg=%s", obj.Code, obj.Msg)
		}
	}
	return fmt.Errorf("unexpected response: %s", shared.Truncate(s, 300))
}

// ---------- 工具函数 ----------

// csrfToken 取凭据 Cookie 中的 passport_csrf_token。
func csrfToken(c *credential) string {
	if v := c.Cookies["passport_csrf_token"]; v != "" {
		return v
	}
	return c.Cookies["passport_csrf_token_default"]
}

// cookieHeader 组装凭据 Cookies 全量 "k=v; k2=v2" Cookie 头（键序固定保证可复现）。
// 【401 登录态修复】登录态鉴权完全依赖此头。
func cookieHeader(c *credential) string {
	keys := make([]string, 0, len(c.Cookies))
	for k := range c.Cookies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+c.Cookies[k])
	}
	return strings.Join(parts, "; ")
}

// errAuth 会话失效类错误（core 侧提示重新登录）。
func errAuth(msg string) error {
	return fmt.Errorf("会话已过期: %s", msg)
}

// sha256Hex SHA-256 hex 摘要。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
