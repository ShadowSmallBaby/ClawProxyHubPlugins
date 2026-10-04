package main

import (
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"testing"
)

func TestResponsesReasoningEffort(t *testing.T) {
	req := &pb.ChatRequest{Extra: map[string]string{"responses_reasoning": `{"effort":"high"}`}}
	if got := reasoningEffort(req); got != "high" {
		t.Fatalf("Responses effort lost: %q", got)
	}
}
