// loomy_test.go — 纯函数单测：模型解析 / SSE 增量提取 / 信封折叠。
package main

import (
	"strings"
	"testing"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func TestResolveModel(t *testing.T) {
	cases := map[string]string{
		"kimi":          "Kimi-k2.6",     // 别名
		"GLM-5.3-Flash": "GLM-5.3-Flash", // 规范 id 直通
		"doubao":        "doubao-seed-2.0-mini",
		"unknown-xyz":   "", // 未知
		"":              "",
	}
	for in, want := range cases {
		if got := resolveModel(in); got != want {
			t.Errorf("resolveModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentOf(t *testing.T) {
	obj := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"delta":         map[string]interface{}{"content": "你好"},
			"finish_reason": nil,
		}},
	}
	if got := contentOf(obj); got != "你好" {
		t.Errorf("contentOf = %q, want 你好", got)
	}
	// 空 choices 不 panic
	if got := contentOf(map[string]interface{}{}); got != "" {
		t.Errorf("contentOf(empty) = %q, want empty", got)
	}
	// finish_reason
	fin := map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": "length"}}}
	if got := finishReasonOf(fin); got != "length" {
		t.Errorf("finishReasonOf = %q, want length", got)
	}
}

func TestBuildContent(t *testing.T) {
	req := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{
		{Role: "system", Text: "你是测试助手"},
		{Role: "user", Text: "第一个问题"},
		{Role: "assistant", Text: "第一个回答"},
		{Role: "user", Text: "第二个问题"},
	}}

	first := buildContent(req, false)
	if !strings.Contains(first, identityDirective) {
		t.Error("首轮应含身份指令")
	}
	for _, want := range []string{"你是测试助手", "第一个问题", "第一个回答", "第二个问题"} {
		if !strings.Contains(first, want) {
			t.Errorf("折叠内容缺少 %q", want)
		}
	}

	// 重试轮用加压指令
	if retry := buildContent(req, true); !strings.Contains(retry, "上一轮") {
		t.Error("重试轮应含加压指令")
	}
}

func TestHasUserText(t *testing.T) {
	empty := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{{Role: "system", Text: "x"}}}
	if hasUserText(empty) {
		t.Error("无 user 文本应返回 false")
	}
	ok := &pb.ChatRequest{Messages: []*pb.EnvelopeMessage{{Role: "user", Text: "hi"}}}
	if !hasUserText(ok) {
		t.Error("有 user 文本应返回 true")
	}
}
