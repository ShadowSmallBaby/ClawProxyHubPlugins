package main

import (
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"testing"
)

func TestResponsesReasoningSwitch(t *testing.T) {
	for _, tc := range []struct {
		raw, effort string
		enabled     bool
	}{
		{`{"effort":"high"}`, "high", true}, {`{"effort":"none"}`, "", false},
	} {
		effort, enabled := normalizePuterReasoning(&pb.ChatRequest{Extra: map[string]string{"responses_reasoning": tc.raw}}, "deepseek")
		if effort != tc.effort || enabled == nil || *enabled != tc.enabled {
			t.Fatalf("effort=%q enabled=%v", effort, enabled)
		}
	}
}
