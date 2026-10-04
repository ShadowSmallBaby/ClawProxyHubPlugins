// 验证 HTTP 帧封装、流尾事件和多模态信封。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	dp "github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins/devin/devinproto"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"google.golang.org/protobuf/proto"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func connectFrame(flags byte, payload []byte) []byte {
	frame := make([]byte, 5, len(payload)+5)
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	return append(frame, payload...)
}

func TestConnectStreamWire(t *testing.T) {
	request := []byte{10, 1, 'x'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/connect+proto" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Equal(raw, connectFrame(0, request)) {
			t.Errorf("request = %x", raw)
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.Write(connectFrame(0, []byte{1, 2, 3}))
		w.Write(connectFrame(2, []byte(`{}`)))
	}))
	defer server.Close()
	stream, err := nextStream(context.Background(), server.Client(), "test", server.URL, "/chat", request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	frame, err := stream.recv()
	if err != nil || !bytes.Equal(frame, []byte{1, 2, 3}) {
		t.Fatalf("frame %x, %v", frame, err)
	}
	if _, err = stream.recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("trailer: %v", err)
	}
}

func TestConnectStreamErrors(t *testing.T) {
	tests := []struct {
		name   string
		frame  []byte
		status int32
	}{
		{"quota trailer", connectFrame(2, []byte(`{"error":{"code":"resource_exhausted","message":"quota"}}`)), 429},
		{"truncated header", []byte{0, 0}, 502},
		{"truncated body", []byte{0, 0, 0, 0, 3, 1}, 502},
		{"oversized", []byte{0, 0xff, 0xff, 0xff, 0xff}, 502},
		{"unsupported compression", connectFrame(1, []byte{0}), 502},
		{"malformed trailer", connectFrame(2, []byte(`{`)), 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &streamReader{body: bufio.NewReader(bytes.NewReader(tt.frame))}
			_, err := s.recv()
			if err == nil || errors.Is(err, io.EOF) || shared.ErrorStatus(err) != tt.status {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDecoderRetainsTrailingUsageAndSignature(t *testing.T) {
	var events []*pb.StreamEvent
	s := &stream{ev: &fakeChatServer{sink: &events}, d: newDecoder("requested")}
	responses := []*dp.GetChatMessageResponse{
		{DeltaThinking: proto.String("think"), ActualModelUid: proto.String("actual"), Usage: &dp.ExaCodeiumCommonPb_ModelUsageStats{InputTokens: proto.Uint64(12)}},
		{DeltaSignature: proto.String("signature")},
		{DeltaText: proto.String("answer"), StopReason: dp.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
		{Usage: &dp.ExaCodeiumCommonPb_ModelUsageStats{OutputTokens: proto.Uint64(7), CacheReadTokens: proto.Uint64(3)}},
	}
	for _, r := range responses {
		done, err := s.feed(mustMarshal(r))
		if done || err != nil {
			t.Fatalf("premature finish: %v %v", done, err)
		}
	}
	if err := s.finish(io.EOF); err != nil {
		t.Fatal(err)
	}
	if events[0].GetMessageStart().GetModel() != "actual" {
		t.Fatal("actual model lost")
	}
	if events[2].GetReasoningDelta().GetSignature() != "signature" {
		t.Fatal("signature lost")
	}
	usage := events[len(events)-1].GetMessageFinish().GetUsage()
	if usage.GetInputTokens() != 12 || usage.GetOutputTokens() != 7 || usage.GetCachedTokens() != 3 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestDecoderTrailerFailureOverridesStop(t *testing.T) {
	var events []*pb.StreamEvent
	s := &stream{ev: &fakeChatServer{sink: &events}, d: newDecoder("m")}
	s.feed(mustMarshal(&dp.GetChatMessageResponse{DeltaText: proto.String("partial"), StopReason: dp.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()}))
	s.finish(connectEndError([]byte(`{"error":{"code":"unavailable","message":"failed"}}`)))
	for _, e := range events {
		if e.GetMessageFinish() != nil {
			t.Fatal("failure reported as success")
		}
	}
	if events[len(events)-1].GetTaskFailed().GetError().GetCode() != 503 {
		t.Fatalf("events = %v", events)
	}
}

func TestRequestSystemPartsAndCurrentImages(t *testing.T) {
	req := &pb.ChatRequest{Model: "m", Messages: []*pb.EnvelopeMessage{
		{Role: "system", Text: "first"}, {Role: "system", Text: "second"},
		{Role: "user", Text: "old", Parts: []*pb.ContentPart{{Type: "text", Text: "old"}, {Type: "image", Data: "aA==", MediaType: "image/png"}}},
		{Role: "assistant", Text: "reply"},
		{Role: "user", Text: "new", Parts: []*pb.ContentPart{{Type: "text", Text: "new"}, {Type: "image", Url: "data:image/png;base64,aA=="}}},
	}}
	built, err := buildChatRequest(req, "token")
	if err != nil {
		t.Fatal(err)
	}
	if built.GetPrompt() != "first\n\nsecond" {
		t.Fatalf("system = %q", built.GetPrompt())
	}
	if len(built.GetChatMessagePrompts()) != 3 {
		t.Fatal("system leaked into history")
	}
	old, current := built.GetChatMessagePrompts()[0], built.GetChatMessagePrompts()[2]
	if strings.Count(old.GetPrompt(), "old") != 1 || len(old.GetImages()) != 0 {
		t.Fatalf("history = %v", old)
	}
	if current.GetPrompt() != "new" || len(current.GetImages()) != 1 || current.GetImages()[0].GetBase64Data() != "aA==" {
		t.Fatalf("current = %v", current)
	}
	again, err := buildChatRequest(req, "token")
	if err != nil {
		t.Fatal(err)
	}
	if built.GetMetadata().GetF() != again.GetMetadata().GetF() {
		t.Fatal("request fingerprint changed")
	}
}
