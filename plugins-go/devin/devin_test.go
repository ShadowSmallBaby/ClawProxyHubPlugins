// 指纹稳定性 + 净化正确性 + 解析器状态机单测。
package devin

import (
	"encoding/json"
	"io"
	"testing"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	devinproto "github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/devin/devinproto"
	"google.golang.org/protobuf/proto"
)

func TestFingerprintStable(t *testing.T) {
	a := fingerprintFromToken("devin-session-token$abc")
	b := fingerprintFromToken("devin-session-token$abc")
	c := fingerprintFromToken("devin-session-token$xyz")
	if a != b {
		t.Fatalf("fingerprint not stable for same token")
	}
	if a == c {
		t.Fatalf("fingerprint collides across tokens")
	}
	if len(a) != 732 { // 366 bytes hex
		t.Fatalf("fingerprint length = %d, want 732", len(a))
	}
}

func TestSanitizeSystemPrompt(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude. Do work."
	out := sanitizeSystemPrompt(in)
	if out != "You are an AI coding assistant. Do work." {
		t.Fatalf("sanitize = %q", out)
	}
	if len(identityReplacements) == 0 {
		t.Fatal("no replacements")
	}
}

func TestSanitizeToolSchema(t *testing.T) {
	in := []byte(`{"type":"object","properties":{"path":{"type":"string","description":"file path","x-meta":1}},"description":"read a file","enum":["a"]}`)
	out, err := sanitizeToolSchema(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if len(s) == 0 {
		t.Fatal("empty schema")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if _, ok := m["description"]; ok {
		t.Fatalf("top-level description not stripped: %s", s)
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[devinproto.ExaCodeiumCommonPb_StopReason]string{
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_MAX_TOKENS:    "max_tokens",
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL: "tool_use",
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_ERROR:         "error",
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_UNSPECIFIED:   "stop",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Fatalf("mapStopReason(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestDecoderTextAndStop(t *testing.T) {
	resp := &devinproto.GetChatMessageResponse{
		DeltaText:  proto.String("hello"),
		StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum(),
	}
	frame, err := proto.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var events []*pb.StreamEvent
	s := &stream{ev: &fakeChatServer{sink: &events}, d: newDecoder("m1")}
	done, err := s.feed(frame)
	if err != nil || done {
		t.Fatalf("feed: done=%v err=%v", done, err)
	}
	if err := s.finish(io.EOF); err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 {
		t.Fatalf("events = %d, want >=3", len(events))
	}
	if events[0].GetMessageStart() == nil || events[1].GetContentDelta().GetText() != "hello" {
		t.Fatalf("unexpected events: %v", events)
	}
	fin := events[len(events)-1].GetMessageFinish()
	if fin == nil || fin.GetFinishReason() != "stop" {
		t.Fatalf("finish = %v", fin)
	}
}

// fakeChatServer 收集事件。
type fakeChatServer struct {
	sink *[]*pb.StreamEvent
	pb.ClawPlugin_ChatServer
}

func (f *fakeChatServer) Send(ev *pb.StreamEvent) error {
	*f.sink = append(*f.sink, ev)
	return nil
}
