package cnb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"google.golang.org/grpc"
)

type testStream struct {
	grpc.ServerStream
	events []*pb.StreamEvent
	fail   bool
}

func (s *testStream) Context() context.Context { return context.Background() }
func (s *testStream) Send(e *pb.StreamEvent) error {
	if s.fail && len(s.events) > 0 {
		return errors.New("client disconnected")
	}
	s.events = append(s.events, e)
	return nil
}

func TestEndpoint(t *testing.T) {
	for _, repo := range []string{"org/repo", "org/group/repo"} {
		got, e := (site{BaseURL: "https://api.cnb.cool/", Repo: repo}).endpoint()
		if e != nil || got != "https://api.cnb.cool/"+repo+"/-/ai/chat/completions" {
			t.Fatalf("%s: %s %v", repo, got, e)
		}
	}
	for _, repo := range []string{"", "repo", "org/../repo", "org/repo?x=1", "org//repo", "org/%2e%2e"} {
		if _, e := (site{BaseURL: "https://api.cnb.cool", Repo: repo}).endpoint(); e == nil {
			t.Fatalf("accepted %q", repo)
		}
	}
}

func TestChatStream(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		failure    bool
	}{
		{"tools", `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}` + "\n\n" + `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" + `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}}` + "\n\ndata: [DONE]\n\n", 200, false},
		{"truncated", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", 200, true},
		{"malformed", "data: broken\n\n", 200, true},
		{"empty", "", 200, true}, {"unauthorized", "secret must not be echoed", 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/org/repo/-/ai/chat/completions" || r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("wrong upstream route/auth")
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid body")
				}
				if body["stream"] != true {
					t.Error("stream disabled")
				}
				if _, ok := body["reasoning_effort"]; ok {
					t.Error("reasoning leaked")
				}
				if len(body["tools"].([]any)) != 1 {
					t.Error("tool definitions lost")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			r := &pb.ChatRequest{Model: "test", Credential: &pb.CredentialBlob{Blob: []byte(`{"token":"test-token"}`)}, Extra: map[string]string{"reasoning_effort": "high"}, Tools: []*pb.ToolDefinition{{Name: "lookup", ParametersSchema: `{"type":"object"}`}}}
			s := &testStream{}
			p := &plugin{host: &sdk.Host{}}
			if err := p.chat(r, s, site{BaseURL: server.URL, Repo: "org/repo"}); err != nil {
				t.Fatal(err)
			}
			last := s.events[len(s.events)-1]
			if tc.failure {
				if last.GetTaskFailed() == nil {
					t.Fatal("missing failure")
				}
				if strings.Contains(last.String(), "secret must") {
					t.Fatal("leaked error body")
				}
				return
			}
			finish := last.GetMessageFinish()
			if finish == nil || finish.FinishReason != "tool_calls" || finish.Usage.InputTokens != 7 || finish.Usage.CachedTokens != 3 {
				t.Fatalf("bad finish: %v", last)
			}
			args := ""
			for _, e := range s.events {
				if d := e.GetToolCallDelta(); d != nil {
					if d.Id != "call_1" {
						t.Fatal("tool identity lost")
					}
					args += d.ArgumentsDelta
				}
			}
			if args != `{"q":"hi"}` {
				t.Fatal(args)
			}
			s = &testStream{fail: true}
			if err := p.chat(r, s, site{BaseURL: server.URL, Repo: "org/repo"}); err == nil {
				t.Fatal("client send error swallowed")
			}
		})
	}
}
