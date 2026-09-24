package main

import (
	"strings"
	"testing"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func TestResolveModelKey(t *testing.T) {
	cases := map[string]string{
		"":                         defaultModel,
		"GPT_55":                   "GPT_55",
		"claude-sonnet-4.6":        "CLAUDE_46_SONNET_BEDROCK",
		"CLAUDE-SONNET-4.6":        "CLAUDE_46_SONNET_BEDROCK", // 大小写不敏感
		"claude":                   "CLAUDE_46_SONNET_BEDROCK",
		"totally-unknown-model-id": defaultModel,
	}
	for in, want := range cases {
		if got := resolveModelKey(in); got != want {
			t.Errorf("resolveModelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThinkingLevel(t *testing.T) {
	if got := thinkingLevel("GPT_54", "low"); got != "low" {
		t.Errorf("low = %q", got)
	}
	if got := thinkingLevel("GPT_54", "high"); got != "high" {
		t.Errorf("high = %q", got)
	}
	if got := thinkingLevel("GPT_54", ""); got != "medium" {
		t.Errorf("default = %q", got)
	}
	// 不支持 thinking 的模型返回空（不下发）。
	if got := thinkingLevel("CLAUDE_45_HAIKU_BEDROCK", "high"); got != "" {
		t.Errorf("unsupported model should be empty, got %q", got)
	}
}

func TestStreamTextAndReasoning(t *testing.T) {
	var events []*pb.StreamEvent
	st := &streamState{p: &plugin{}, emit: func(ev *pb.StreamEvent) { events = append(events, ev) }}
	st.handleLine(`data: {"eventType":"textChunk","data":{"textContent":"hello"}}`)
	st.handleLine(`data: {"eventType":"thinkingChunk","data":{"thinkingContent":"pondering"}}`)
	st.handleLine(`data: [DONE]`)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if d := events[0].GetContentDelta(); d == nil || d.GetText() != "hello" {
		t.Errorf("event0 not content 'hello': %+v", events[0])
	}
	if d := events[1].GetReasoningDelta(); d == nil || d.GetText() != "pondering" {
		t.Errorf("event1 not reasoning 'pondering': %+v", events[1])
	}
}

func TestStreamToolCallChunks(t *testing.T) {
	var events []*pb.StreamEvent
	p := &plugin{}
	st := &streamState{p: p, threadKey: "thread-1", emit: func(ev *pb.StreamEvent) { events = append(events, ev) }}
	// 先 conversation（会话 id），后分块工具调用。
	st.handleLine(`data: {"eventType":"conversation","data":{"id":"conv-abc"}}`)
	st.handleLine(`data: {"eventType":"toolCall","data":{"toolCalls":[{"id":"call_1","function":{"name":"exec","arguments":"{\"co"}}]}}`)
	st.handleLine(`data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"id":"call_1","function":{"arguments":"de\":1}"}}]}}`)

	if !st.sawTool {
		t.Fatal("sawTool should be true")
	}
	// 首块：id+name；后续两块：仅 arguments。
	if len(events) != 3 {
		t.Fatalf("expected 3 tool events, got %d", len(events))
	}
	if d := events[0].GetToolCallDelta(); d == nil || d.GetId() != "call_1" || d.GetName() != "exec" {
		t.Errorf("event0 not tool head: %+v", events[0])
	}
	if d := events[1].GetToolCallDelta(); d == nil || d.GetId() != "" || d.GetArgumentsDelta() != `{"co` {
		t.Errorf("event1 not args delta: %+v", events[1])
	}
	// pendingCall 登记到位（含正确会话）。
	if reg, ok := p.lookupPending("call_1", "thread-1"); !ok || reg.convID != "conv-abc" {
		t.Errorf("pending call not registered: %+v ok=%v", reg, ok)
	}
	// 会话线程映射登记。
	if conv, ok := p.lookupThread("thread-1"); !ok || conv != "conv-abc" {
		t.Errorf("thread conv not remembered: %q ok=%v", conv, ok)
	}
}

func TestInjectSystemContextTruncates(t *testing.T) {
	system := strings.Repeat("A", maxQueryLen*2)
	out := injectSystemContext(system, "hi")
	if len(out) > maxQueryLen {
		t.Errorf("injected query %d exceeds limit %d", len(out), maxQueryLen)
	}
	if !strings.Contains(out, "TRUNCATED") {
		t.Error("expected truncation marker")
	}
	if !strings.Contains(out, "[User request]\nhi") {
		t.Error("expected user request preserved")
	}
	// 短 system 不加标记。
	if got := injectSystemContext("short", "hi"); !strings.Contains(got, "short") || strings.Contains(got, "TRUNCATED") {
		t.Errorf("short system mishandled: %q", got)
	}
	// 空 system 原样返回 query。
	if got := injectSystemContext("", "hi"); got != "hi" {
		t.Errorf("empty system should return query, got %q", got)
	}
}

func TestBuildThirdParty(t *testing.T) {
	tools := []*pb.ToolDefinition{
		{Name: "exec", Description: "run code"},
		{Name: "search", Description: "web search", ParametersSchema: `{"type":"object","properties":{"q":{"type":"string"}}}`},
		{Name: "namespace:skip"}, // 跳过
	}
	tp := buildThirdParty(tools)
	if tp == nil {
		t.Fatal("thirdParty should not be nil")
	}
	user, ok := tp["user"].(map[string]interface{})
	if !ok {
		t.Fatal("missing user server")
	}
	list, ok := user["tools"].([]map[string]interface{})
	if !ok || len(list) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(list))
	}
	// exec 必须带 code 参数 schema。
	execTool := list[0]
	params, _ := execTool["parameters"].(map[string]interface{})
	props, _ := params["properties"].(map[string]interface{})
	if _, has := props["code"]; !has {
		t.Error("exec tool missing code parameter")
	}
	if desc, _ := execTool["description"].(string); !strings.Contains(desc, "run code") {
		t.Error("exec description should embed native description")
	}
	// 空 tools → nil。
	if buildThirdParty(nil) != nil {
		t.Error("nil tools should yield nil thirdParty")
	}
}

func TestMessageExtractors(t *testing.T) {
	msgs := []*pb.EnvelopeMessage{
		{Role: "system", Text: "you are an agent"},
		{Role: "user", Text: "first question"},
		{Role: "assistant", Text: "answer"},
		{Role: "user", Text: "second question"},
	}
	if got := lastUserText(msgs); got != "second question" {
		t.Errorf("lastUserText = %q", got)
	}
	if got := systemContext(msgs); got != "you are an agent" {
		t.Errorf("systemContext = %q", got)
	}
	// threadKey 取首条 user，稳定且非空。
	k1 := threadKeyOf(msgs)
	k2 := threadKeyOf(msgs[:2])
	if k1 == "" || k1 != k2 {
		t.Errorf("threadKey unstable: %q vs %q", k1, k2)
	}
	// 末尾工具结果检测。
	if lastToolOutput(msgs) != nil {
		t.Error("no trailing tool output expected")
	}
	toolMsgs := append(msgs, &pb.EnvelopeMessage{Role: "tool", ToolCallId: "call_9", Text: "result"})
	out := lastToolOutput(toolMsgs)
	if out == nil || out.callID != "call_9" || out.content != "result" {
		t.Errorf("lastToolOutput = %+v", out)
	}
}
