package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

type protocolStream struct {
	pb.ClawPlugin_ChatServer
	events []*pb.StreamEvent
}

func (s *protocolStream) Context() context.Context { return context.Background() }
func (s *protocolStream) Send(ev *pb.StreamEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func TestChatProtocolRouting(t *testing.T) {
	for _, tc := range []struct{ source, mode, path, response string }{
		{"responses", "", "/v1/responses", `data: {"type":"response.completed","response":{"output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"hello"}]}]}}` + "\n\n"},
		{"responses", "native", "/v1/responses", `data: {"type":"response.completed","response":{"output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"hello"}]}]}}` + "\n\n"},
		{"responses", "chat", "/v1/chat/completions", `data: {"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"},
		{"chat_completions", "native", "/v1/chat/completions", `data: {"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"},
		{"messages", "native", "/v1/messages", `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" + `data: {"type":"message_stop"}` + "\n\n"},
	} {
		t.Run(tc.source+"/"+tc.mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != tc.path || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				field := "messages"
				if tc.path == "/v1/responses" {
					field = "input"
				}
				if body[field] == nil || string(body["stream"]) != "true" || string(body["model"]) != `"test-model"` {
					t.Errorf("unexpected body: %v", body)
				}
				if tc.path == "/v1/responses" && (!strings.Contains(string(body["reasoning"]), `"high"`) || !strings.Contains(string(body["text"]), `"json_object"`)) {
					t.Errorf("Responses options lost: %v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			cfg, _ := json.Marshal(map[string]string{"base_url": server.URL, "responses_mode": tc.mode})
			p := &plugin{loadSettings: func(int64) []byte { return cfg }}
			stream := &protocolStream{}
			err := p.Chat(&pb.ChatRequest{
				Source: tc.source, Model: "test-model",
				Credential: &pb.CredentialBlob{Blob: []byte(`{"api_key":"test-key"}`)},
				Messages:   []*pb.EnvelopeMessage{{Role: "user", Text: "hi"}},
				Extra:      map[string]string{"responses_reasoning": `{"effort":"high"}`, "responses_text": `{"format":{"type":"json_object"}}`},
			}, stream)
			if err != nil {
				t.Fatal(err)
			}
			var text string
			finished := false
			for _, ev := range stream.events {
				if ev.GetTaskFailed() != nil {
					t.Fatalf("failed: %v", ev)
				}
				text += ev.GetContentDelta().GetText()
				finished = finished || ev.GetMessageFinish() != nil
			}
			if calls != 1 || text != "hello" || !finished {
				t.Fatalf("calls=%d text=%q finished=%v", calls, text, finished)
			}
		})
	}
}

func TestInvalidResponsesMode(t *testing.T) {
	p := &plugin{loadSettings: func(int64) []byte { return []byte(`{"base_url":"https://example.invalid","responses_mode":"typo"}`) }}
	if _, err := p.site(0); err == nil {
		t.Fatal("expected invalid mode error")
	}
}

func TestNativeResponsesFailureDoesNotRetryChat(t *testing.T) {
	for _, status := range []int{404, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/responses" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			cfg, _ := json.Marshal(map[string]string{"base_url": server.URL})
			p := &plugin{loadSettings: func(int64) []byte { return cfg }}
			stream := &protocolStream{}
			if err := p.Chat(&pb.ChatRequest{Source: "responses", Model: "test", Credential: &pb.CredentialBlob{Blob: []byte(`{"api_key":"test"}`)}}, stream); err != nil {
				t.Fatal(err)
			}
			wantCode := int32(502)
			if status == 429 {
				wantCode = 429
			}
			if calls != 1 || len(stream.events) != 1 || stream.events[0].GetTaskFailed().GetError().GetCode() != wantCode {
				t.Fatalf("calls=%d events=%v", calls, stream.events)
			}
		})
	}
}
