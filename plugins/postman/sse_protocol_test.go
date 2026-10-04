package main

import (
	"io"
	"strings"
	"testing"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func TestMultilineSSE(t *testing.T) {
	var text string
	st := &streamState{emit: func(ev *pb.StreamEvent) { text += ev.GetContentDelta().GetText() }}
	err := sdk.ReadSSE(strings.NewReader("\ufeffdata: {\r\ndata: \"eventType\":\"textChunk\",\"data\":{\"textContent\":\"hello\"}}\r\n\r\ndata: [DONE]\r\n\r\n"), 4<<20, func(line string) error {
		st.handleLine(line)
		if st.done {
			return io.EOF
		}
		return nil
	})
	if err != nil || text != "hello" || !st.done {
		t.Fatalf("text=%q done=%t err=%v", text, st.done, err)
	}
}
