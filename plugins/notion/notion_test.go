package main

import "testing"

func TestNormalizeBlockID(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"  ":                                   "",
		"1234567890abcdef1234567890abcdef":     "12345678-90ab-cdef-1234-567890abcdef",
		"12345678-90ab-cdef-1234-567890abcdef": "12345678-90ab-cdef-1234-567890abcdef", // 已带连字符原样返回
		"not-a-uuid":                           "not-a-uuid",
	}
	for in, want := range cases {
		if got := normalizeBlockID(in); got != want {
			t.Errorf("normalizeBlockID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanContent(t *testing.T) {
	cases := map[string]string{
		"hello":                                          "hello",
		"<thinking>secret</thinking>answer":              "answer",
		"<thought>x</thought> visible":                   "visible",
		`<lang primary="zh" />text`:                      "text",
		"<thinking>a</thinking><thought>b</thought>done": "done",
	}
	for in, want := range cases {
		if got := cleanContent(in); got != want {
			t.Errorf("cleanContent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseNDJSONLine(t *testing.T) {
	// 格式1：markdown-chat 直接事件 → final
	if got := parseNDJSONLine(`{"type":"markdown-chat","value":"hi"}`); len(got) != 1 || got[0].kind != "final" || got[0].text != "hi" {
		t.Errorf("markdown-chat: %+v", got)
	}

	// 格式2a：Claude/GPT 增量 x + /value/
	if got := parseNDJSONLine(`{"type":"patch","v":[{"o":"x","p":"/steps/0/value/0","v":"tok"}]}`); len(got) != 1 || got[0].kind != "incremental" || got[0].text != "tok" {
		t.Errorf("claude incremental: %+v", got)
	}

	// 格式2b：Claude/GPT 完整 a + /value/- + text
	if got := parseNDJSONLine(`{"type":"patch","v":[{"o":"a","p":"/steps/0/value/-","v":{"type":"text","content":"full"}}]}`); len(got) != 1 || got[0].kind != "final" || got[0].text != "full" {
		t.Errorf("claude final: %+v", got)
	}

	// 格式2c：Gemini 增量 x + /s/ + /value
	if got := parseNDJSONLine(`{"type":"patch","v":[{"o":"x","p":"/a/s/0/value","v":"g"}]}`); len(got) != 1 || got[0].kind != "incremental" || got[0].text != "g" {
		t.Errorf("gemini incremental: %+v", got)
	}

	// 格式2d：Gemini 完整 a + /s/- + markdown-chat
	if got := parseNDJSONLine(`{"type":"patch","v":[{"o":"a","p":"/a/s/-","v":{"type":"markdown-chat","value":"gm"}}]}`); len(got) != 1 || got[0].kind != "final" || got[0].text != "gm" {
		t.Errorf("gemini final: %+v", got)
	}

	// 非法行 / 无关类型 → 空
	if got := parseNDJSONLine(`not json`); len(got) != 0 {
		t.Errorf("garbage should yield nothing: %+v", got)
	}
}

func TestParseRecordMap(t *testing.T) {
	raw := `{"thread_message":{"m1":{"value":{"value":{"step":{"type":"markdown-chat","value":"rm-text"}}}}}}`
	if got := parseRecordMap([]byte(raw)); got != "rm-text" {
		t.Errorf("parseRecordMap markdown-chat = %q", got)
	}
	rawAgent := `{"thread_message":{"m1":{"value":{"value":{"step":{"type":"agent-inference","value":[{"type":"text","content":"ai-text"}]}}}}}}`
	if got := parseRecordMap([]byte(rawAgent)); got != "ai-text" {
		t.Errorf("parseRecordMap agent-inference = %q", got)
	}
}

func TestCredentialValidate(t *testing.T) {
	if err := (&credential{Cookie: "t", SpaceID: "s", UserID: "u"}).validate(); err != nil {
		t.Errorf("valid credential rejected: %v", err)
	}
	if err := (&credential{SpaceID: "s", UserID: "u"}).validate(); err == nil {
		t.Error("missing cookie should error")
	}
	if err := (&credential{Cookie: "t", UserID: "u"}).validate(); err == nil {
		t.Error("missing space_id should error")
	}
}

func TestCookieHeader(t *testing.T) {
	if got := (&credential{Cookie: "abc"}).cookieHeader(); got != "token_v2=abc" {
		t.Errorf("bare token = %q", got)
	}
	full := "token_v2=abc; other=1"
	if got := (&credential{Cookie: full}).cookieHeader(); got != full {
		t.Errorf("full header = %q", got)
	}
}
