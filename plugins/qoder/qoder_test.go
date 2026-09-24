package main

import (
	"testing"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func TestToolChoiceForOpenAI(t *testing.T) {
	if toolChoiceForOpenAI(nil) != nil {
		t.Fatal("nil choice should map to nil")
	}
	if got := toolChoiceForOpenAI(&pb.ToolChoice{Type: "auto"}); got != "auto" {
		t.Fatalf("auto = %v", got)
	}
	if got := toolChoiceForOpenAI(&pb.ToolChoice{Type: "tool"}); got != "required" {
		t.Fatalf("tool without name = %v, want required", got)
	}
	got := toolChoiceForOpenAI(&pb.ToolChoice{Type: "tool", ToolName: "search"})
	m, ok := got.(map[string]interface{})
	if !ok || m["type"] != "function" {
		t.Fatalf("tool with name = %v", got)
	}
	if fn, _ := m["function"].(map[string]interface{}); fn["name"] != "search" {
		t.Fatalf("function name = %v", fn["name"])
	}
}

func TestBuildChatBody_ToolChoiceAndParallel(t *testing.T) {
	req := &pb.ChatRequest{
		Model:      "lite",
		Messages:   []*pb.EnvelopeMessage{{Role: "user", Text: "hi"}},
		Tools:      []*pb.ToolDefinition{{Name: "search", ParametersSchema: `{"type":"object"}`}},
		ToolChoice: &pb.ToolChoice{Type: "tool", ToolName: "search"},
		Extra:      map[string]string{"parallel_tool_calls": "false"},
	}
	body := buildChatBody(req, "rid", "sid")
	if body["tool_choice"] == nil {
		t.Fatal("tool_choice not forwarded")
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %v, want false", body["parallel_tool_calls"])
	}
}

func TestBuildChatBody_NoToolsNoChoice(t *testing.T) {
	req := &pb.ChatRequest{Model: "lite", Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "hi"}}}
	body := buildChatBody(req, "rid", "sid")
	if _, ok := body["tool_choice"]; ok {
		t.Fatal("tool_choice should be absent without tools")
	}
}
