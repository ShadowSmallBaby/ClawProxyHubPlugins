package main

import (
	"strings"
	"testing"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func TestExtractToolCalls(t *testing.T) {
	in := `好的<tool_call>{"name":"get_weather","arguments":{"city":"北京"}}</tool_call>`
	calls, rest := extractToolCalls(in)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "北京") {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
	if rest != "好的" {
		t.Fatalf("rest = %q, want 好的", rest)
	}
}

func TestExtractToolCalls_None(t *testing.T) {
	calls, rest := extractToolCalls("普通回答")
	if calls != nil || rest != "普通回答" {
		t.Fatalf("unexpected: calls=%v rest=%q", calls, rest)
	}
}

func TestBuildPrompt_SingleTurnPlain(t *testing.T) {
	req := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "你好"}}}
	if got := buildPrompt(req); got != "你好" {
		t.Fatalf("single-turn should stay plain, got %q", got)
	}
}

func TestBuildPrompt_MultiTurn(t *testing.T) {
	req := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{
		{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}, {Role: "user", Text: "q2"},
	}}
	got := buildPrompt(req)
	for _, want := range []string{"[user]: q1", "[assistant]: a1", "[user]: q2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("multi-turn prompt missing %q in:\n%s", want, got)
		}
	}
}

func TestBuildPrompt_WithTools(t *testing.T) {
	req := &pb.ChatRequest{
		Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "hi"}},
		Tools:    []*pb.ToolDefinition{{Name: "search", Description: "web search", ParametersSchema: `{"type":"object"}`}},
	}
	got := buildPrompt(req)
	if !strings.Contains(got, "search") || !strings.Contains(got, "<tool_call>") {
		t.Fatalf("tool prompt missing tool defs / format:\n%s", got)
	}
}
