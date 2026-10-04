package main

import (
	"strings"
	"testing"
)

func TestNormalizeWarpModel(t *testing.T) {
	cases := map[string]string{
		"":               "auto-open",
		"auto":           "auto-open",
		"auto-efficient": "auto-open",
		"auto-genius":    "auto-open",
		"AUTO-OPEN":      "auto-open",
		"claude-opus-5":  "claude-opus-5",
	}
	for in, want := range cases {
		if got := normalizeWarpModel(in); got != want {
			t.Errorf("normalizeWarpModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeWarpPayload(t *testing.T) {
	// "hi" 的 base64 变体都要解出同一字节
	for _, data := range []string{"aGk", "aGk="} {
		got, err := decodeWarpPayload(data)
		if err != nil {
			t.Fatalf("decodeWarpPayload(%q) error: %v", data, err)
		}
		if string(got) != "hi" {
			t.Errorf("decodeWarpPayload(%q) = %q, want %q", data, got, "hi")
		}
	}
	if _, err := decodeWarpPayload(""); err == nil {
		t.Error("decodeWarpPayload(\"\") should error")
	}
}

func TestApplyContentUpdateDedup(t *testing.T) {
	s := newWarpStreamState()
	// append 增量
	if got := s.applyContentUpdate("m", "hello ", false, false); got != "hello " {
		t.Errorf("append delta = %q", got)
	}
	// 完整 snapshot 与已发内容前缀一致 → 只发后缀
	if got := s.applyContentUpdate("m", "hello world", false, true); got != "world" {
		t.Errorf("snapshot suffix = %q, want %q", got, "world")
	}
	// 重复完整 snapshot → 空
	if got := s.applyContentUpdate("m", "hello world", false, true); got != "" {
		t.Errorf("duplicate snapshot = %q, want empty", got)
	}
}

func TestApplyContentUpdateConflictKeepsLatest(t *testing.T) {
	s := newWarpStreamState()
	s.applyContentUpdate("m", "abc", false, false)
	// 冲突替换不作为重复输出
	if got := s.applyContentUpdate("m", "xyz", false, true); got != "" {
		t.Errorf("conflict snapshot should emit nothing, got %q", got)
	}
}

func TestAcceptToolCallDedup(t *testing.T) {
	s := newWarpStreamState()
	if !s.acceptToolCall("id1") {
		t.Error("first accept should pass")
	}
	if s.acceptToolCall("id1") {
		t.Error("duplicate accept should fail")
	}
	if !s.acceptToolCall("") {
		t.Error("empty id always passes")
	}
}

func TestFinishReason(t *testing.T) {
	s := newWarpStreamState()
	if s.finishReason() != "end_turn" {
		t.Errorf("no tool call = %q", s.finishReason())
	}
	s.sawToolCall = true
	if s.finishReason() != "tool_use" {
		t.Errorf("tool call = %q", s.finishReason())
	}
}

func TestIsIncompleteToolCall(t *testing.T) {
	if !isIncompleteToolCall("Bash", "{}") {
		t.Error("empty bash command should be incomplete")
	}
	if isIncompleteToolCall("Bash", `{"command":"ls"}`) {
		t.Error("valid bash command should be complete")
	}
	if !isIncompleteToolCall("Edit", `{"file_path":"a.go"}`) {
		t.Error("edit without old/new should be incomplete")
	}
	if isIncompleteToolCall("Edit", `{"file_path":"a.go","old_string":"x","new_string":"y"}`) {
		t.Error("valid edit should be complete")
	}
}

func TestNormalizeToolNameFallback(t *testing.T) {
	cases := map[string]string{
		"run_shell_command":  "Bash",
		"str_replace_editor": "Edit",
		"view":               "Read",
		"ripgreptool":        "Grep",
		"file_glob_v2":       "Glob",
		"write_file":         "Write",
		"unknown_tool":       "unknown_tool",
	}
	for in, want := range cases {
		if got := normalizeToolNameFallback(in); got != want {
			t.Errorf("normalizeToolNameFallback(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDerivedWarpToolCallIDStable(t *testing.T) {
	a := derivedWarpToolCallID("Bash", `{"command":"ls"}`)
	b := derivedWarpToolCallID("bash ", `{"command":"ls"}`)
	if a != b {
		t.Errorf("derived id should be stable across name case: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "warp_anon_") {
		t.Errorf("derived id prefix = %q", a)
	}
}
