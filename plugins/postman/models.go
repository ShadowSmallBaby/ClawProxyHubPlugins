// 模型目录：Postman Agent Mode 服务端模型键（/_gw/config 返回）+ 通用别名。
// 对外展示 Postman 键；Chat 时把请求 model 解析回 modelKey，未知回退默认。
package main

import (
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// defaultModel 未识别请求 model 时的回退键。
const defaultModel = "GPT_54"

// postmanModels Postman 服务端模型键，顺序对齐 /_gw/config。
var postmanModels = []string{
	"GPT_55",
	"GPT_56_SOL",
	"GPT_56_TERRA",
	"GPT_56_LUNA",
	"GPT_54",
	"CLAUDE_OPUS_48_BEDROCK",
	"CLAUDE_OPUS_47_BEDROCK",
	"CLAUDE_OPUS_45_BEDROCK",
	"CLAUDE_46_SONNET_BEDROCK",
	"CLAUDE_45_SONNET_BEDROCK",
	"CLAUDE_45_HAIKU_BEDROCK",
}

// modelLabels 对外展示名。
var modelLabels = map[string]string{
	"GPT_55":                   "GPT-5.5",
	"GPT_56_SOL":               "GPT-5.6 Sol",
	"GPT_56_TERRA":             "GPT-5.6 Terra",
	"GPT_56_LUNA":              "GPT-5.6 Luna",
	"GPT_54":                   "GPT-5.4",
	"CLAUDE_OPUS_48_BEDROCK":   "Claude Opus 4.8",
	"CLAUDE_OPUS_47_BEDROCK":   "Claude Opus 4.7",
	"CLAUDE_OPUS_45_BEDROCK":   "Claude Opus 4.5",
	"CLAUDE_46_SONNET_BEDROCK": "Claude Sonnet 4.6",
	"CLAUDE_45_SONNET_BEDROCK": "Claude Sonnet 4.5",
	"CLAUDE_45_HAIKU_BEDROCK":  "Claude Haiku 4.5",
}

// thinkingSupported 支持 thinkingLevel 参数的模型键。
var thinkingSupported = map[string]bool{
	"GPT_55":                   true,
	"GPT_56_SOL":               true,
	"GPT_56_TERRA":             true,
	"GPT_56_LUNA":              true,
	"GPT_54":                   true,
	"CLAUDE_OPUS_48_BEDROCK":   true,
	"CLAUDE_OPUS_47_BEDROCK":   true,
	"CLAUDE_46_SONNET_BEDROCK": true,
}

// modelAliases 通用名 → Postman 键（小写匹配）。
var modelAliases = map[string]string{
	"gpt-5.4":           "GPT_54",
	"gpt-5.5":           "GPT_55",
	"gpt-5.6":           "GPT_56_SOL",
	"gpt-5.6-sol":       "GPT_56_SOL",
	"gpt-5.6-terra":     "GPT_56_TERRA",
	"gpt-5.6-luna":      "GPT_56_LUNA",
	"claude-opus-4.8":   "CLAUDE_OPUS_48_BEDROCK",
	"claude-opus-4.7":   "CLAUDE_OPUS_47_BEDROCK",
	"claude-opus-4.5":   "CLAUDE_OPUS_45_BEDROCK",
	"claude-sonnet-4.6": "CLAUDE_46_SONNET_BEDROCK",
	"claude-sonnet-4.5": "CLAUDE_45_SONNET_BEDROCK",
	"claude-haiku-4.5":  "CLAUDE_45_HAIKU_BEDROCK",
	"claude":            "CLAUDE_46_SONNET_BEDROCK",
	"claude-sonnet":     "CLAUDE_46_SONNET_BEDROCK",
	"claude-opus":       "CLAUDE_OPUS_48_BEDROCK",
	"claude-haiku":      "CLAUDE_45_HAIKU_BEDROCK",
	"postman":           "GPT_54",
	"postbot":           "GPT_54",
}

// resolveModelKey 把请求 model 解析为 Postman 模型键：直配 > 别名 > 默认。
func resolveModelKey(requested string) string {
	if requested == "" {
		return defaultModel
	}
	for _, k := range postmanModels {
		if k == requested {
			return requested
		}
	}
	if k, ok := modelAliases[strings.ToLower(requested)]; ok {
		return k
	}
	return defaultModel
}

// thinkingLevel reasoning 强度 → Postman thinkingLevel（low/medium/high）。
// low/minimal→low，high/max→high，其余→medium；模型不支持则返回空（不下发）。
func thinkingLevel(modelKey, effort string) string {
	if !thinkingSupported[modelKey] {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "minimal":
		return "low"
	case "high", "max":
		return "high"
	default:
		return "medium"
	}
}

// listModels 静态模型目录。
func listModels() *pb.ModelList {
	out := make([]*pb.ModelInfo, 0, len(postmanModels))
	for _, k := range postmanModels {
		out = append(out, &pb.ModelInfo{
			Id:             k,
			Label:          map[string]string{"zh": modelLabels[k], "en": modelLabels[k]},
			ContextWindow:  200000,
			SupportsTools:  true,
			SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: out}
}
