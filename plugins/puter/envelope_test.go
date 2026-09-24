package main

import (
	"testing"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func TestNormalizePuterReasoning(t *testing.T) {
	mk := func(effort string) *pb.ChatRequest {
		return &pb.ChatRequest{Extra: map[string]string{"reasoning_effort": effort}}
	}
	if e, on := normalizePuterReasoning(mk("high"), "deepseek"); e != "high" || on == nil || !*on {
		t.Fatalf("deepseek high = %q %v", e, on)
	}
	if e, on := normalizePuterReasoning(mk("none"), "deepseek"); e != "" || on == nil || *on {
		t.Fatalf("deepseek none = %q %v", e, on)
	}
	if e, on := normalizePuterReasoning(mk(""), "deepseek"); e != "" || on != nil {
		t.Fatalf("deepseek empty = %q %v", e, on)
	}
	if e, on := normalizePuterReasoning(mk("high"), "openrouter"); e != "" || on != nil {
		t.Fatal("non-deepseek must not set thinking")
	}
}

func TestParseParallelToolCalls(t *testing.T) {
	f := func(v string) *bool {
		return parseParallelToolCalls(&pb.ChatRequest{Extra: map[string]string{"parallel_tool_calls": v}})
	}
	if p := f("true"); p == nil || !*p {
		t.Fatal("true")
	}
	if p := f("false"); p == nil || *p {
		t.Fatal("false")
	}
	if p := f(""); p != nil {
		t.Fatal("empty should be nil")
	}
}

func TestMergeAdjacentAssistantMessages(t *testing.T) {
	in := []puterMessage{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a1"},
		{Role: "assistant", Content: "a2"},
	}
	out := mergeAdjacentAssistantMessages(in)
	if len(out) != 2 {
		t.Fatalf("want 2, got %d", len(out))
	}
	if out[1].Content != "a1\na2" {
		t.Fatalf("merged content = %q", out[1].Content)
	}
}

func TestSplitMultiToolCalls(t *testing.T) {
	in := []puterMessage{
		{Role: "assistant", ToolCalls: []puterTool{{ID: "c1"}, {ID: "c2"}}},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "tool", ToolCallID: "c2", Content: "r2"},
	}
	out := splitMultiToolCalls(in)
	if len(out) != 4 {
		t.Fatalf("want 4 (a→t→a→t), got %d: %+v", len(out), out)
	}
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "c1" || out[1].Role != "tool" || out[1].ToolCallID != "c1" {
		t.Fatalf("first pair wrong: %+v %+v", out[0], out[1])
	}
}

func TestConvertMessages_EchoReasoning(t *testing.T) {
	msgs := []*pb.EnvelopeMessage{{Role: "assistant", Text: "hi"}}
	if out := convertMessages(msgs, true); len(out) != 1 || out[0].ReasoningContent != missingDeepSeekReasoningFallback {
		t.Fatalf("echoReasoning fallback missing: %+v", out)
	}
	if out := convertMessages(msgs, false); out[0].ReasoningContent != "" {
		t.Fatal("non-echo must not set reasoning")
	}
}
