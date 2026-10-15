// protocol_test:工具文本协议、提示词组装、事件帧解析与用量换算的纯函数单测。
package logicc

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

func tools() []*pb.ToolDefinition {
	return []*pb.ToolDefinition{{
		Name: "get_weather", Description: "weather lookup",
		ParametersSchema: `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`,
	}}
}

func TestBuildPromptWithoutTools(t *testing.T) {
	req := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{
		{Role: "system", Text: "be brief"},
		{Role: "user", Text: "hi"},
	}}
	prompt := buildPrompt(req, resolvePlan(req))
	if strings.Contains(prompt, "tool_calls") {
		t.Fatalf("plain prompt must not carry the tool protocol: %q", prompt)
	}
	if !strings.Contains(prompt, "be brief") || !strings.Contains(prompt, "hi") {
		t.Fatalf("history missing: %q", prompt)
	}
}

func TestBuildPromptWithToolsDeclaresEnvelope(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(), Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "weather in Hangzhou?"}}}
	plan := resolvePlan(req)
	prompt := buildPrompt(req, plan)
	for _, want := range []string{"tool_calls", "get_weather", "Choose a tool when needed", "Parallel calls: allowed"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %q", want, prompt)
		}
	}
}

func TestBuildPromptRequiredChoice(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(),
		ToolChoice: &pb.ToolChoice{Type: "tool"},
		Messages:   []*pb.EnvelopeMessage{{Role: "user", Text: "go"}}}
	prompt := buildPrompt(req, resolvePlan(req))
	if !strings.Contains(prompt, "You MUST request at least one declared client tool") {
		t.Fatalf("required choice not enforced: %q", prompt)
	}
}

func TestParseToolOutputEnvelope(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(), Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "weather?"}}}
	plan := resolvePlan(req)
	text, calls, err := parseToolOutput(
		"```json\n{\"content\":\"checking\",\"tool_calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hangzhou\"}}]}\n```", plan, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if text != "checking" || len(calls) != 1 {
		t.Fatalf("unexpected parse result: text=%q calls=%d", text, len(calls))
	}
	if calls[0].Name != "get_weather" || calls[0].Arguments != `{"city":"Hangzhou"}` {
		t.Fatalf("unexpected call: %+v", calls[0])
	}
	if !strings.HasPrefix(calls[0].ID, "call_") {
		t.Fatalf("generated id missing: %+v", calls[0])
	}
}

func TestParseToolOutputPlainTextIsNotACall(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(), Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "hi"}}}
	text, calls, err := parseToolOutput(`the json {"tool_calls":[]} quoted in prose`, resolvePlan(req), nil)
	if err != nil {
		t.Fatalf("auto mode must accept plain text: %v", err)
	}
	if text == "" || len(calls) != 0 {
		t.Fatalf("plain text must pass through: text=%q calls=%d", text, len(calls))
	}
}

func TestParseToolOutputRejectsUndeclaredTool(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(), Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "go"}}}
	_, _, err := parseToolOutput(`{"content":null,"tool_calls":[{"name":"nope","arguments":{}}]}`, resolvePlan(req), nil)
	if err == nil {
		t.Fatal("undeclared tool must be rejected")
	}
}

func TestParseToolOutputRejectsSchemaViolation(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(), Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "go"}}}
	// 缺少 required.city
	if _, _, err := parseToolOutput(`{"tool_calls":[{"name":"get_weather","arguments":{}}]}`, resolvePlan(req), nil); err == nil {
		t.Fatal("missing required field must be rejected")
	}
	// 类型不符
	if _, _, err := parseToolOutput(`{"tool_calls":[{"name":"get_weather","arguments":{"city":7}}]}`, resolvePlan(req), nil); err == nil {
		t.Fatal("wrong argument type must be rejected")
	}
	// additionalProperties:false 之外的字段
	if _, _, err := parseToolOutput(`{"tool_calls":[{"name":"get_weather","arguments":{"city":"x","extra":1}}]}`, resolvePlan(req), nil); err == nil {
		t.Fatal("extra field must be rejected")
	}
}

func TestParseToolOutputRequiredWithoutCalls(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(), ToolChoice: &pb.ToolChoice{Type: "tool"},
		Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "go"}}}
	if _, _, err := parseToolOutput("no json here", resolvePlan(req), nil); err == nil {
		t.Fatal("required choice without a call must fail")
	}
}

func TestParseToolOutputNativeCallsSkipExecuted(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(), Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "go"}}}
	plan := resolvePlan(req)
	text, calls, err := parseToolOutput("done", plan, []nativeCall{
		{ID: "native_1", Name: "get_weather", Arguments: `{"city":"Hangzhou"}`},
		{ID: "native_2", Name: "get_weather", Arguments: `{"city":"Beijing"}`, Executed: true},
	})
	if err != nil {
		t.Fatalf("native calls failed: %v", err)
	}
	if text != "done" || len(calls) != 1 || calls[0].ID != "native_1" {
		t.Fatalf("only the unexecuted native call must surface: %+v", calls)
	}
}

func TestParseToolOutputNativeEmptyArguments(t *testing.T) {
	tool := []*pb.ToolDefinition{{Name: "ping", ParametersSchema: `{"type":"object","properties":{}}`}}
	req := &pb.ChatRequest{Tools: tool, Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "go"}}}
	_, calls, err := parseToolOutput("", resolvePlan(req), []nativeCall{{ID: "c1", Name: "ping"}})
	if err != nil {
		t.Fatalf("empty native arguments must default to {}: %v", err)
	}
	if len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Fatalf("unexpected call: %+v", calls)
	}
}

func TestParseToolOutputParallelDisabled(t *testing.T) {
	req := &pb.ChatRequest{Tools: tools(), Extra: map[string]string{"parallel_tool_calls": "false"},
		Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "go"}}}
	_, _, err := parseToolOutput(
		`{"tool_calls":[{"name":"get_weather","arguments":{"city":"a"}},{"name":"get_weather","arguments":{"city":"b"}}]}`,
		resolvePlan(req), nil)
	if err == nil {
		t.Fatal("parallel disabled must reject multiple calls")
	}
}

func TestParseFrameSSE(t *testing.T) {
	ev, err := parseFrame(`data: {"type":"text-delta","delta":"你好"}`)
	if err != nil || ev == nil || ev.Delta != "你好" {
		t.Fatalf("frame parse failed: %v %+v", err, ev)
	}
	// 多行 data 合并 + CRLF + [DONE]
	if ev, err = parseFrame("data: {\"type\":\"text-delta\",\r\ndata: \"delta\":\"x\"}\r"); err != nil || ev == nil {
		t.Fatalf("multi-line data must merge: %v %+v", err, ev)
	}
	if ev, err = parseFrame("data: [DONE]"); err != nil || ev != nil {
		t.Fatalf("[DONE] must be ignored: %v %+v", err, ev)
	}
	if _, err = parseFrame("data: not-json"); err == nil {
		t.Fatal("invalid json must error")
	}
	if ev, err = parseFrame(": keep-alive"); err != nil || ev != nil {
		t.Fatalf("comment frame must be ignored: %v %+v", err, ev)
	}
}

func TestSplitFrameHandlesBothLineEndings(t *testing.T) {
	for _, raw := range []string{"a\r\n\r\nb", "a\n\nb"} {
		frame, rest, ok := splitFrame(raw)
		if !ok || frame != "a" || rest != "b" {
			t.Fatalf("splitFrame(%q) = %q,%q,%v", raw, frame, rest, ok)
		}
	}
}

func TestUsageOfPrefersTotalUsage(t *testing.T) {
	ev := &upstreamEvent{
		Usage:      &upstreamUsage{InputTokens: json.RawMessage(`5`), OutputTokens: json.RawMessage(`8`)},
		TotalUsage: &upstreamUsage{InputTokens: json.RawMessage(`{"total":11}`), OutputTokens: json.RawMessage(`{"total":22}`)},
	}
	u := usageOf(ev)
	if u == nil || u.InputTokens != 11 || u.OutputTokens != 22 {
		t.Fatalf("totalUsage must win: %+v", u)
	}
	if usageOf(&upstreamEvent{}) != nil {
		t.Fatal("absent usage must stay nil")
	}
	if usageOf(&upstreamEvent{Usage: &upstreamUsage{InputTokens: json.RawMessage(`5`)}}) != nil {
		t.Fatal("partial usage must not be reported")
	}
}

func TestNormalizeFinishReason(t *testing.T) {
	cases := map[string]string{
		"": "stop", "stop": "stop", "length": "length", "max_tokens": "length",
		"content_filter": "content_filter", "tool-calls": "tool_calls", "weird": "stop",
	}
	for in, want := range cases {
		if got := normalizeFinishReason(in); got != want {
			t.Fatalf("normalizeFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvePlanToolChoice(t *testing.T) {
	// 指定工具:只有该工具可被调用
	req := &pb.ChatRequest{Tools: tools(), ToolChoice: &pb.ToolChoice{Type: "tool", ToolName: "get_weather"}}
	plan := resolvePlan(req)
	if !plan.required || plan.choice != "get_weather" || len(plan.active) != 1 {
		t.Fatalf("named tool choice not resolved: %+v", plan)
	}
	// none:工具协议关闭
	req = &pb.ChatRequest{Tools: tools(), ToolChoice: &pb.ToolChoice{Type: "none"}}
	if plan = resolvePlan(req); len(plan.active) != 0 {
		t.Fatalf("tool_choice=none must disable tools: %+v", plan)
	}
	// 指定不存在的工具:active 为空但保留 required
	req = &pb.ChatRequest{Tools: tools(), ToolChoice: &pb.ToolChoice{Type: "tool", ToolName: "ghost"}}
	if plan = resolvePlan(req); len(plan.active) != 0 || !plan.required {
		t.Fatalf("unknown named tool must leave nothing callable: %+v", plan)
	}
}

func TestCollectNativeCallIgnoresUndeclared(t *testing.T) {
	plan := resolvePlan(&pb.ChatRequest{Tools: tools(), Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "go"}}})
	index := map[string]int{}
	var calls []nativeCall
	collectNativeCall(&upstreamEvent{Type: "tool-input-start", ToolCallID: "a", ToolName: "other"}, index, &calls, plan)
	if len(calls) != 0 {
		t.Fatalf("undeclared native tool must be ignored: %+v", calls)
	}
	collectNativeCall(&upstreamEvent{Type: "tool-input-start", ToolCallID: "b", ToolName: "get_weather"}, index, &calls, plan)
	collectNativeCall(&upstreamEvent{Type: "tool-input-delta", ToolCallID: "b", InputTextDelta: `{"city":`}, index, &calls, plan)
	collectNativeCall(&upstreamEvent{Type: "tool-input-delta", ToolCallID: "b", InputTextDelta: `"X"}`}, index, &calls, plan)
	markNativeExecuted(&upstreamEvent{ToolCallID: "b"}, index, calls)
	if len(calls) != 1 || calls[0].Arguments != `{"city":"X"}` || !calls[0].Executed {
		t.Fatalf("native accumulation wrong: %+v", calls)
	}
	if len(pendingNative(calls)) != 0 {
		t.Fatal("executed call must not be pending")
	}
}

func TestMapUpstreamStatus(t *testing.T) {
	cases := map[int]int32{400: 400, 413: 400, 422: 400, 401: 401, 403: 403, 402: 429, 429: 429, 500: 502, 503: 502}
	for in, want := range cases {
		if got := mapUpstreamStatus(in); got != want {
			t.Fatalf("mapUpstreamStatus(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestValidatePartsRejectsImages(t *testing.T) {
	ok := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{
		{Role: "user", Text: "hi", Parts: []*pb.ContentPart{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Parts: []*pb.ContentPart{{Type: "thinking", Text: "..."}}},
	}}
	if err := validateParts(ok.Messages); err != nil {
		t.Fatalf("text/thinking parts must pass: %v", err)
	}
	bad := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{
		{Role: "user", Parts: []*pb.ContentPart{{Type: "image", MediaType: "image/png", Data: "x"}}},
	}}
	if err := validateParts(bad.Messages); err == nil {
		t.Fatal("image part must be rejected instead of dropped")
	}
	if status := shared.ErrorStatus(invalidError("x")); status != 400 {
		t.Fatalf("invalid requests must map to 400, got %d", status)
	}
}

func TestCookieValueParsesHeader(t *testing.T) {
	header := "__client=abc.def_ghi; __session=xyz; __refresh_jid=777"
	got, err := cookieValue(header, "__client")
	if err != nil || got != "abc.def_ghi" {
		t.Fatalf("cookieValue = %q, %v", got, err)
	}
	// 直接粘贴单个值也接受。
	if got, err := cookieValue("  raw-value  ", "__client"); err != nil || got != "raw-value" {
		t.Fatalf("single value not accepted: %q, %v", got, err)
	}
	// 带引号与大小写差异。
	if got, err := cookieValue(`__CLIENT="quoted-value"`, "__client"); err != nil || got != "quoted-value" {
		t.Fatalf("quoted cookie not parsed: %q, %v", got, err)
	}
}

func TestCookieValueRejectsMissingOrEmpty(t *testing.T) {
	if _, err := cookieValue("__session=xyz", "__client"); err == nil {
		t.Fatal("missing __client must fail")
	}
	if _, err := cookieValue("__client=; __session=x", "__client"); err == nil {
		t.Fatal("empty __client must fail")
	}
}

func TestPickSessionIDPrefersLastActive(t *testing.T) {
	body := `{"response":{"last_active_session_id":"sess_active","sessions":[` +
		`{"id":"sess_old","status":"ended"},{"id":"sess_other","status":"active"}]}}`
	got, err := pickSessionID([]byte(body))
	if err != nil || got != "sess_active" {
		t.Fatalf("pickSessionID = %q, %v", got, err)
	}
}

func TestPickSessionIDFallsBackToActiveSession(t *testing.T) {
	body := `{"response":{"sessions":[{"id":"sess_ended","status":"ended"},{"id":"sess_live","status":"active"}]}}`
	got, err := pickSessionID([]byte(body))
	if err != nil || got != "sess_live" {
		t.Fatalf("pickSessionID = %q, %v", got, err)
	}
}

func TestPickSessionIDWithoutActiveSession(t *testing.T) {
	body := `{"response":{"last_active_session_id":"","sessions":[{"id":"sess_gone","status":"ended"}]}}`
	_, err := pickSessionID([]byte(body))
	if err == nil {
		t.Fatal("no active session must fail")
	}
	if status := shared.ErrorStatus(err); status != 401 {
		t.Fatalf("missing session must map to 401, got %d", status)
	}
	if _, err := pickSessionID([]byte("not json")); shared.ErrorStatus(err) != 502 {
		t.Fatalf("malformed response must map to 502, got %v", err)
	}
}

// testJWT 造一个只带 exp 的 JWT(仅供解析测试,不参与验签)。
func testJWT(exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp, 10) + `,"sub":"user_test"}`))
	return "header." + payload + ".sig"
}

func TestCredentialCacheReusesFreshToken(t *testing.T) {
	cred := &credential{SessionID: "sess_cache"}
	token := testJWT(time.Now().Add(time.Hour).Unix())
	cred.Token, cred.ClientCookie = token, "rotated-cookie"
	storeToken(cred)
	t.Cleanup(func() { tokenCache.Delete(cred.cacheKey()) })

	// 另一份凭据(同一账号、旧 JWT)应命中缓存,并回灌轮换后的 Cookie。
	reloaded := &credential{SessionID: "sess_cache", Token: "stale", ClientCookie: "old-cookie"}
	got, ok := cachedToken(reloaded)
	if !ok || got != token {
		t.Fatalf("expected cached token, got ok=%v token=%q", ok, got)
	}
	if reloaded.ClientCookie != "rotated-cookie" {
		t.Fatalf("rotated cookie not restored: %q", reloaded.ClientCookie)
	}
}

func TestCredentialCacheDropsExpiredToken(t *testing.T) {
	cred := &credential{SessionID: "sess_expired"}
	cred.Token = testJWT(time.Now().Add(-time.Minute).Unix())
	storeToken(cred)
	if _, ok := cachedToken(&credential{SessionID: "sess_expired"}); ok {
		t.Fatal("expired cached token must not be reused")
	}
}

func TestCacheKeyIsolatesAccounts(t *testing.T) {
	withAccount := &credential{accountID: "42", SessionID: "sess_x"}
	withSession := &credential{SessionID: "sess_x"}
	if withAccount.cacheKey() == withSession.cacheKey() {
		t.Fatal("account-scoped key must differ from session-scoped key")
	}
	if (&credential{Token: "abc"}).cacheKey() == (&credential{Token: "abd"}).cacheKey() {
		t.Fatal("token digests must differ")
	}
}

// blob 只应包含凭据字段:核心已单独持久化 account id,重复写入会让字段语义漂移。
func TestCredentialBlobOmitsInjectedFields(t *testing.T) {
	c := &credential{ClientCookie: "cookie", SessionID: "sess_1", Token: "tok", accountID: "42", proxyURL: "http://p"}
	blob := c.toBlob()
	for _, unwanted := range []string{"accountID", "proxyURL", "http://p"} {
		if strings.Contains(string(blob), unwanted) {
			t.Fatalf("blob leaked %q: %s", unwanted, blob)
		}
	}
	for _, want := range []string{"clientCookie", "sessionId", "token"} {
		if !strings.Contains(string(blob), want) {
			t.Fatalf("blob missing %q: %s", want, blob)
		}
	}
}
