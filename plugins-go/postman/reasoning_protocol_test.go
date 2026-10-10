package postman

import (
	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/requestutil"
	"testing"
)

func TestResponsesThinkingLevel(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"effort":"xhigh"}`, "high"}, {`{"effort":"none"}`, ""},
	} {
		effort := requestutil.ReasoningEffort(map[string]string{"responses_reasoning": tc.raw})
		if got := thinkingLevel("GPT_54", effort); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}
