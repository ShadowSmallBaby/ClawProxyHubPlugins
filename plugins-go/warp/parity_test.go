// 验证流式边界、工具参数和凭据持久化。
package warp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func warpFixture(t *testing.T, raw string, out proto.Message) {
	t.Helper()
	if err := protojson.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err)
	}
}

func TestMCPToolNameAndParametersPreserved(t *testing.T) {
	for _, name := range []string{"read_file", "Bash", "custom-tool"} {
		var msg warpapi.Message_ToolCall
		warpFixture(t, `{"toolCallId":"call1","callMcpTool":{"name":"`+name+`","args":{"custom":42}}}`, &msg)
		call, ok := parseWarpToolCall(&msg)
		if !ok || call.name != name || call.input != `{"custom":42}` {
			t.Fatalf("MCP call = %#v, %v", call, ok)
		}
	}
}

func TestNativeToolMapping(t *testing.T) {
	for _, tc := range []struct{ raw, name, input string }{
		{`{"runShellCommand":{"command":"pwd"}}`, "Bash", `{"command":"pwd"}`},
		{`{"readFiles":{"files":[{"name":"a.go"}]}}`, "Read", `{"file_path":"a.go"}`},
		{`{"grep":{"queries":["TODO"],"path":"src"}}`, "Grep", `{"path":"src","pattern":"TODO"}`},
	} {
		var msg warpapi.Message_ToolCall
		warpFixture(t, tc.raw, &msg)
		call, ok := parseWarpToolCall(&msg)
		if !ok || call.name != tc.name || call.input != tc.input {
			t.Fatalf("native call = %#v, %v", call, ok)
		}
	}
}

func TestSSETerminationVariants(t *testing.T) {
	var finished warpapi.ResponseEvent
	warpFixture(t, `{"finished":{"done":{}}}`, &finished)
	raw, err := proto.Marshal(&finished)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "\n", "\n\n", "\r\n\r\n"} {
		t.Run("suffix_"+strings.ReplaceAll(suffix, "\n", "LF"), func(t *testing.T) {
			var events []*pb.StreamEvent
			parser := newWarpStreamParser(func(e *pb.StreamEvent) { events = append(events, e) })
			if err := parser.scan(strings.NewReader("data: " + base64.RawURLEncoding.EncodeToString(raw) + suffix)); err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].GetMessageFinish() == nil {
				t.Fatalf("events = %v", events)
			}
		})
	}
}

func TestBase64Alphabets(t *testing.T) {
	raw := []byte{255, 255}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		got, err := decodeWarpPayload(enc.EncodeToString(raw))
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("decode = %x, %v", got, err)
		}
	}
}

func TestSSEClientSendFailure(t *testing.T) {
	var finished warpapi.ResponseEvent
	warpFixture(t, `{"finished":{"done":{}}}`, &finished)
	raw, _ := proto.Marshal(&finished)
	sendErr := errors.New("client disconnected")
	var parser *warpStreamParser
	parser = newWarpStreamParser(func(*pb.StreamEvent) { parser.sendErr = sendErr })
	err := parser.scan(strings.NewReader("data: " + base64.RawURLEncoding.EncodeToString(raw) + "\n\n"))
	if !errors.Is(err, sendErr) {
		t.Fatalf("send error lost: %v", err)
	}
}

func TestToolResultRoundAndCaseSensitiveTools(t *testing.T) {
	history := []*pb.EnvelopeMessage{
		{Role: "assistant", ToolCalls: []*pb.ToolCall{{Id: "c", Name: "read_file"}}},
		{Role: "tool", ToolCallId: "c", Text: "result"},
	}
	result := buildWarpToolResult(history[1])
	if result.GetToolCallId() != "c" || result.GetCallMcpTool().GetSuccess().GetResults()[0].GetText().GetText() != "result" {
		t.Fatalf("result = %v", result)
	}
	if len(latestWarpToolResults(history)) != 1 {
		t.Fatal("current result missing")
	}
	history = append(history, &pb.EnvelopeMessage{Role: "assistant", Text: "done"}, &pb.EnvelopeMessage{Role: "user", Text: "next"})
	if len(latestWarpToolResults(history)) != 0 {
		t.Fatal("old tool result replayed")
	}
	req := &pb.ChatRequest{Model: "auto", Messages: history, Tools: []*pb.ToolDefinition{{Name: "Read"}, {Name: "read"}}}
	_, raw, err := buildWarpRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var built warpapi.Request
	if err := proto.Unmarshal(raw, &built); err != nil {
		t.Fatal(err)
	}
	if len(built.GetMcpContext().GetServers()[0].GetTools()) != 2 {
		t.Fatal("case-sensitive tools collapsed")
	}
	req.ToolChoice = &pb.ToolChoice{Type: "none"}
	_, raw, err = buildWarpRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(raw, &built); err != nil {
		t.Fatal(err)
	}
	if built.GetMcpContext() != nil || built.GetSettings().GetSupportsParallelToolCalls() {
		t.Fatal("tool_choice none ignored")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRefreshCredentialRoundTrip(t *testing.T) {
	p := &plugin{settingsJSON: []byte(`{"firebase_api_key":"test-key"}`), settingsAt: time.Now(), hc: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "securetoken.googleapis.com" {
			t.Errorf("unexpected request: %s", r.URL.Host)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id_token":"new-access","refresh_token":"rotated-refresh","expires_in":"3600"}`)), Header: make(http.Header)}, nil
	})}}
	c := &credential{RefreshToken: "old-refresh"}
	if err := p.refreshJWT(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := credFrom(&pb.CredentialBlob{Blob: raw})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccessToken != "new-access" || loaded.RefreshToken != "rotated-refresh" {
		t.Fatal("refreshed tokens not persisted")
	}
	if left := time.Until(loaded.AccessExpiresAt); left < 59*time.Minute || left > 61*time.Minute {
		t.Fatalf("expires_in ignored: %s", left)
	}
}

// MCP 声明与模型发现保留上游真实能力，不静默裁掉工具。
func TestMCPContextValidationAndToolFence(t *testing.T) {
	tools := make([]*pb.ToolDefinition, 300)
	for i := range tools {
		tools[i] = &pb.ToolDefinition{Name: fmt.Sprintf("tool_%d", i), ParametersSchema: `{"type":"object","properties":{"value":{"type":"string"}}}`}
	}
	req := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "hello"}}, Tools: tools}
	_, raw, err := buildWarpRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var built warpapi.Request
	if err := proto.Unmarshal(raw, &built); err != nil {
		t.Fatal(err)
	}
	if got := len(built.GetMcpContext().GetServers()[0].GetTools()); got != 300 {
		t.Fatalf("tools truncated: %d", got)
	}
	supported := built.GetSettings().GetSupportedTools()
	if len(supported) != 1 || supported[0] != warpapi.ToolType_CALL_MCP_TOOL {
		t.Fatalf("unsupported native tools advertised: %v", supported)
	}
	tools[0].ParametersSchema = `{"type":`
	if _, _, err = buildWarpRequest(req); err == nil {
		t.Fatal("invalid schema silently ignored")
	}
}

func TestModelDiscoveryContextWindowAndAccountIsolation(t *testing.T) {
	p := &plugin{hc: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/client/login" {
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		model := "m1"
		if r.Header.Get("Authorization") == "Bearer access2" {
			model = "m2"
		}
		body := `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"defaultId":"` + model + `","choices":[{"id":"` + model + `","displayName":"Model","contextWindow":{"max":1000000,"default":128000}}]}}}]}}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	for _, id := range []string{"1", "2"} {
		raw, _ := json.Marshal(&credential{RefreshToken: "refresh" + id, AccessToken: "access" + id, AccessExpiresAt: time.Now().Add(time.Hour)})
		result, err := p.ListModels(context.Background(), &pb.CredentialBlob{Blob: raw})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.GetModels()) != 1 || result.GetModels()[0].GetId() != "m"+id || result.GetModels()[0].GetContextWindow() != 1000000 {
			t.Fatalf("models = %v", result)
		}
	}
}
