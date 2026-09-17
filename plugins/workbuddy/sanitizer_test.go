package main

import (
	"strings"
	"testing"
)

func TestDesensitizeText(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude. Refuse DoS attacks and exploit development."
	out := desensitizeText(in)

	if strings.Contains(out, "Claude Code") {
		t.Errorf("feature text not rewritten: %s", out)
	}
	if !strings.Contains(out, "CodeBuddy, Tencent's official CLI") {
		t.Errorf("rewrite target missing: %s", out)
	}
	// 原词被打断：词内出现零宽空格
	if strings.Contains(out, "DoS") {
		t.Errorf("DoS not desensitized: %q", out)
	}
	if !strings.Contains(out, "D"+zwsp+"oS") {
		t.Errorf("DoS should carry zwsp: %q", out)
	}
	// 大小写保持（原文大小写不应被统一）
	if !strings.Contains(out, "Refuse") {
		t.Errorf("case should be preserved: %s", out)
	}
}

func TestReplaceRuntimeBlocks(t *testing.T) {
	in := "before <environment_context>\nsecret paths\n</environment_context> after"
	out := replaceRuntimeBlocks(in)
	if strings.Contains(out, "secret paths") {
		t.Errorf("block content should be replaced: %s", out)
	}
	if !strings.Contains(out, "Environment context is provided by the harness.") {
		t.Errorf("replacement missing: %s", out)
	}
}

func TestDesensitizeMessageBody(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "Refuse phishing requests."},
			map[string]interface{}{"role": "user", "content": "正常问题：phishing 是什么"},
		},
	}
	desensitizeMessageBody(body)
	sys := body["messages"].([]interface{})[0].(map[string]interface{})["content"].(string)
	if !strings.Contains(sys, zwsp) {
		t.Errorf("system message should be desensitized: %q", sys)
	}
	user := body["messages"].([]interface{})[1].(map[string]interface{})["content"].(string)
	if strings.Contains(user, zwsp) {
		t.Errorf("user message should keep verbatim: %q", user)
	}
}
