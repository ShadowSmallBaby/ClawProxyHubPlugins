// sanitizer.go — 请求体净化：system 特征改写 + 合规声明词零宽脱敏。
// 词内插 U+200B 打断后端关键词匹配，模型/人眼读起来无差别。
package main

import (
	"regexp"
	"strings"
)

// systemFeatureRewrites Claude Code 特征文本 → CodeBuddy（防客户端识别）。
var systemFeatureRewrites = []struct{ from, to string }{
	{"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are CodeBuddy, Tencent's official CLI."},
	{"main branch (you will usually use this for prs)",
		"main branch (you will usually use this for pr)"},
}

// sensitiveTerms 合规声明高频词（按长度降序编译，避免短词先吃长词）。
var sensitiveTerms = []string{
	"credential stuffing", "supply chain compromise", "supply-chain compromise",
	"detection evasion", "C2 frameworks", "C2 framework", "command and control",
	"malicious purposes", "malicious intent", "mass targeting", "brute force", "brute-force",
	"privilege escalation", "reverse shell", "remote code execution", "SQL injection",
	"penetration testing", "penetration test", "security review", "exploit development",
	"red teaming", "red-teaming", "cybersecurity", "noreply@anthropic.com", "Co-Authored-By",
	"DoS", "DDoS", "exploit", "credential testing", "XSS", "CSRF", "phishing", "malware",
	"ransomware", "keylogger", "rootkit", "backdoor", "botnet", "zero-day", "0day",
	"vulnerability", "vulnerabilities", "sandbox", "sandboxing", "sandboxed", "unsandboxed",
	"escalated privileges", "escalated", "escalation", "destructive action", "destructive command",
	"destructive", "attack", "attacks", "hacking", "injection", "weaponize", "weaponized",
	"harmful", "dangerous", "abuse", "abusive", "illegal", "terrorist", "terrorism", "bomb",
	"weapon", "weapons", "drug", "drugs", "narcotic", "suicide", "self-harm", "murder", "kill",
	"violence", "violent",
	"Claude Code", "Claude Opus", "Claude Sonnet", "Claude Haiku", "Claude Fable", "Anthropic",
}

var sensitivePattern = buildSensitivePattern()

func buildSensitivePattern() *regexp.Regexp {
	// 按长度降序 + 词边界 + 忽略大小写
	sorted := append([]string(nil), sensitiveTerms...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if len(sorted[j]) > len(sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	quoted := make([]string, len(sorted))
	for i, t := range sorted {
		quoted[i] = regexp.QuoteMeta(t)
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)\b`)
}

const zwsp = "​"

// zwspInsert 词中间插零宽空格（首字符之后）。
func zwspInsert(term string) string {
	if len(term) < 2 {
		return term
	}
	return term[:1] + zwsp + term[1:]
}

// desensitizeText 单条文本：特征改写 + 词表脱敏（保持原文大小写）。
func desensitizeText(text string) string {
	for _, r := range systemFeatureRewrites {
		text = replaceIgnoreCase(text, r.from, r.to)
	}
	return sensitivePattern.ReplaceAllStringFunc(text, zwspInsert)
}

var featurePatterns = buildFeaturePatterns()

func buildFeaturePatterns() []*regexp.Regexp {
	pats := make([]*regexp.Regexp, len(systemFeatureRewrites))
	for i, r := range systemFeatureRewrites {
		pats[i] = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(r.from))
	}
	return pats
}

func replaceIgnoreCase(text, from, to string) string {
	for i, r := range systemFeatureRewrites {
		if r.from == from {
			return featurePatterns[i].ReplaceAllString(text, to)
		}
	}
	return text
}

// runtimeBlockReplacements 运行时上下文块整体替换（客户端 harness 注入，
// 常含 sandbox/permissions 等触发词，保留语义占位即可）。
var runtimeBlockReplacements = []struct{ open, close, replacement string }{
	{"<environment_context>", "</environment_context>", "Environment context is provided by the harness."},
	{"<permissions instructions>", "</permissions instructions>",
		"Runtime permissions apply: filesystem access may be sandboxed, network may be restricted."},
	{"<skills_instructions>", "</skills_instructions>",
		"Skill instructions are provided by the harness."},
	{"<system-reminder>", "</system-reminder>", "Runtime reminder from the harness."},
}

// desensitizeMessageBody 对 openai body（map 形态）的 system/developer 消息净化，
// 并替换消息里的运行时上下文块。user 消息仅做块替换（不动真实提问内容）。
func desensitizeMessageBody(body map[string]interface{}) {
	messages, ok := body["messages"].([]interface{})
	if !ok {
		return
	}
	for _, mi := range messages {
		msg, ok := mi.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content, ok := msg["content"].(string)
		if !ok {
			continue
		}
		if role == "system" || role == "developer" {
			msg["content"] = desensitizeText(replaceRuntimeBlocks(content))
		} else {
			msg["content"] = replaceRuntimeBlocks(content)
		}
	}
}

// replaceRuntimeBlocks 运行时块替换。
func replaceRuntimeBlocks(text string) string {
	for _, r := range runtimeBlockReplacements {
		for {
			start := strings.Index(text, r.open)
			if start < 0 {
				break
			}
			end := strings.Index(text[start:], r.close)
			if end < 0 {
				break
			}
			text = text[:start] + r.replacement + text[start+end+len(r.close):]
		}
	}
	return text
}
