package main

import (
	"strings"
	"testing"
)

func TestServiceForModel(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5":    "claude",
		"gpt-5":            "openai",
		"gemini-3-pro":     "google",
		"gemma-3":          "google",
		"grok-4":           "x-ai",
		"deepseek-v4":      "deepseek",
		"mistral-large":    "mistral",
		"openrouter:llama": "openrouter",
		"google:gemini-3":  "google",
		"Claude-Opus-5":    "claude",
	}
	for in, want := range cases {
		got, err := serviceForModel(in)
		if err != nil {
			t.Errorf("serviceForModel(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("serviceForModel(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := serviceForModel("unknown-model"); err == nil {
		t.Error("unknown model should error")
	}
}

func TestNormalizeStreamToolInputDepth(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"query":"hi"}`, `{"query":"hi"}`},
		{`"{\"file_path\":\"a.go\"}"`, `{"file_path":"a.go"}`},
		{`{"arguments":"{\"command\":\"ls\"}"}`, `{"command":"ls"}`},
		{`""`, `{}`},
		{`null`, `{}`},
	}
	for _, c := range cases {
		if got := normalizeStreamToolInputDepth(c.in, 4); got != c.want {
			t.Errorf("normalizeStreamToolInputDepth(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitLeadingHTTPStatus(t *testing.T) {
	if code, rest := splitLeadingHTTPStatus("400 {json}"); code != "400" || rest != "{json}" {
		t.Errorf("split = (%q,%q)", code, rest)
	}
	if code, _ := splitLeadingHTTPStatus("hello 400"); code != "" {
		t.Errorf("non-status prefix should not match, got %q", code)
	}
	if code, _ := splitLeadingHTTPStatus("400"); code != "400" {
		t.Errorf("bare status should match, got %q", code)
	}
	if code, _ := splitLeadingHTTPStatus("4000 more"); code != "" {
		t.Errorf("4-digit prefix should not match, got %q", code)
	}
}

func TestNormalizePuterUsage(t *testing.T) {
	usage := normalizePuterUsage(map[string]interface{}{
		"inputTokens":  float64(100),
		"outputTokens": float64(50),
		"cachedTokens": float64(10),
	})
	if usage == nil || usage.InputTokens != 100 || usage.OutputTokens != 50 || usage.CachedTokens != 10 {
		t.Errorf("usage = %+v", usage)
	}
	if normalizePuterUsage(map[string]interface{}{}) != nil {
		t.Error("empty usage should be nil")
	}
	if normalizePuterUsage(map[string]interface{}{"other": 1}) != nil {
		t.Error("usage without token fields should be nil")
	}
}

func TestErrorFieldBothShapes(t *testing.T) {
	var fromString errorField
	if err := fromString.UnmarshalJSON([]byte(`"400 bad"`)); err != nil || fromString.Message != "400 bad" {
		t.Errorf("string shape = %+v err=%v", fromString, err)
	}
	var fromObject errorField
	if err := fromObject.UnmarshalJSON([]byte(`{"code":"quota","message":"limit"}`)); err != nil || fromObject.Payload == nil || fromObject.Payload.Code != "quota" {
		t.Errorf("object shape = %+v err=%v", fromObject, err)
	}
}

func TestNormalizePuterStreamLine(t *testing.T) {
	if got := normalizePuterStreamLine("data: {\"type\":\"text\"}"); got != `{"type":"text"}` {
		t.Errorf("data prefix = %q", got)
	}
	if got := normalizePuterStreamLine("[DONE]"); got != "" {
		t.Errorf("[DONE] = %q", got)
	}
}

func TestCompactToolInput(t *testing.T) {
	if got := compactToolInput(`{ "a" : 1 }`); got != `{"a":1}` {
		t.Errorf("compact = %q", got)
	}
	if got := compactToolInput("not json"); got != "not json" {
		t.Errorf("invalid passthrough = %q", got)
	}
	if got := compactToolInput(""); got != "{}" {
		t.Errorf("empty = %q", got)
	}
}

func TestSplitLeadingHTTPStatusImmutability(t *testing.T) {
	line := "409 conflict"
	code, rest := splitLeadingHTTPStatus(line)
	if !strings.HasPrefix(line, code) || rest != "conflict" {
		t.Errorf("split = (%q,%q)", code, rest)
	}
}
