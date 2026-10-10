// 请求净化：规范系统提示词，移除工具 Schema 的描述性元数据。
package devin

import (
	"encoding/json"
	"regexp"
	"strings"
)

// identityReplacements 按顺序规范系统提示词中的身份描述。
var identityReplacements = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`(?i)You are Claude Code, Anthropic's official CLI for Claude`), "You are an AI coding assistant"},
	{regexp.MustCompile(`(?i)Claude Code is available as a CLI in the terminal, desktop app`), "The assistant is available as a CLI in the terminal, desktop tool"},
	{regexp.MustCompile(`(?i)Fast mode for Claude Code uses Claude Opus`), "Fast mode uses the faster output model"},
	{regexp.MustCompile(`(?i)The most recent Claude models are the Claude 5 family`), "The most recent models are the latest family"},
	{regexp.MustCompile(`(?i)default to the latest and most capable Claude models`), "default to the latest and most capable models"},
	// 精简安全说明，保留授权范围。
	{regexp.MustCompile(`(?s)IMPORTANT: Assist with authorized security testing.*?defensive use cases`), "IMPORTANT: Assist with authorized security testing and educational contexts. Refuse harmful requests. Dual-use tools require clear authorization context"},
}

// sanitizeSystemPrompt 规范身份描述，保留功能指令。
func sanitizeSystemPrompt(prompt string) string {
	for _, r := range identityReplacements {
		prompt = r.pattern.ReplaceAllString(prompt, r.replace)
	}
	return prompt
}

// sanitizeToolSchema 移除描述性元数据，保留 Schema 约束和业务值。
func sanitizeToolSchema(schema json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(stripSchemaValue(value, false))
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func stripSchemaValue(value any, propertyNames bool) any {
	switch typed := value.(type) {
	case []any:
		for i, item := range typed {
			typed[i] = stripSchemaValue(item, false)
		}
		return typed
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for k, child := range typed {
			if isNLAnnotation(k) && !propertyNames {
				continue
			}
			if isSchemaLiteral(k) && !propertyNames {
				cleaned[k] = child
				continue
			}
			cleaned[k] = stripSchemaValue(child, k == "properties")
		}
		return cleaned
	default:
		return value
	}
}

// isSchemaLiteral 业务值而非可递归清理的 Schema 定义。
func isSchemaLiteral(key string) bool {
	switch key {
	case "const", "default", "enum", "example", "examples":
		return true
	default:
		return false
	}
}

// isNLAnnotation 判断描述性元数据字段。
func isNLAnnotation(key string) bool {
	switch key {
	case "description", "title", "$comment":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(key), "x-")
	}
}
