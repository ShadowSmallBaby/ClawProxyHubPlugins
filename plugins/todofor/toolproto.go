// 客户端工具协议：教上游用严格文本块表达工具调用（上游不能自己执行客户端工具）。
// 移植自 todo2api internal/openai/toolproto.go，适配信封 ToolDefinition。
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	toolTag      = "TOOL_CALL"
	toolOpenTag  = "<" + toolTag + ">"
	toolCloseTag = "</" + toolTag + ">"
)

type wireToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolBlock struct {
	start int
	raw   string
}

// parsedToolCall 一次解析出的工具调用（id 由内容哈希稳定生成）。
type parsedToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON 字符串
}

// toolCallStreamFilter 流式过滤器：普通助手文本尽早放行，合法 TOOL_CALL 块扣留（避免协议文本泄漏给客户端）。
// 一个助手回合用一个过滤器。工具标签可能被切在任意帧边界。
type toolCallStreamFilter struct {
	pending string
	inBlock bool
	stopped bool
}

// Push 消费一段上游文本增量，返回可立即下发给客户端的文本。
func (f *toolCallStreamFilter) Push(fragment string) string {
	if fragment == "" || f.stopped {
		return ""
	}
	f.pending += fragment

	var out strings.Builder
	for {
		if f.inBlock {
			closeAt := strings.Index(f.pending[len(toolOpenTag):], toolCloseTag)
			if closeAt < 0 {
				return out.String()
			}
			closeAt += len(toolOpenTag)
			end := closeAt + len(toolCloseTag)
			candidate := f.pending[:end]
			if _, calls := parseToolCalls(candidate); len(calls) > 0 {
				f.pending = ""
				f.stopped = true
				return out.String()
			}
			// 闭合但格式非法：当普通模型输出处理。
			out.WriteString(candidate)
			f.pending = f.pending[end:]
			f.inBlock = false
			continue
		}

		openAt := strings.Index(f.pending, toolOpenTag)
		if openAt >= 0 {
			out.WriteString(f.pending[:openAt])
			f.pending = f.pending[openAt:]
			f.inBlock = true
			continue
		}

		keep := possibleToolTagPrefix(f.pending)
		emitEnd := len(f.pending) - keep
		out.WriteString(f.pending[:emitEnd])
		f.pending = f.pending[emitEnd:]
		return out.String()
	}
}

// Flush 回合结束时返回未决文本：合法工具块仍扣留，残缺标签 / 块当普通文本放行。
func (f *toolCallStreamFilter) Flush() string {
	if f.stopped {
		return ""
	}
	pending := f.pending
	f.pending = ""
	f.inBlock = false
	return pending
}

// possibleToolTagPrefix 返回 content 结尾可能是开标签前缀的字节数（跨帧标签保护）。
func possibleToolTagPrefix(content string) int {
	max := len(toolOpenTag) - 1
	if len(content) < max {
		max = len(content)
	}
	for size := max; size > 0; size-- {
		if strings.HasSuffix(content, toolOpenTag[:size]) {
			return size
		}
	}
	return 0
}

// buildToolSystemPrompt 渲染工具契约，作为 raw system 消息注入。
func buildToolSystemPrompt(tools []*pb.ToolDefinition) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("You have access to the following tools, but they are client-side tools. ")
	b.WriteString("You cannot execute them yourself and must not use any device, cloud, shell, or file tool as a substitute. ")
	b.WriteString("Never claim that you executed a client-side tool. When a tool is needed, output exactly one block with no Markdown fence or surrounding prose:\n")
	b.WriteString(toolOpenTag + "{\"name\":\"<tool>\",\"arguments\":{...}}" + toolCloseTag + "\n")
	b.WriteString("Then stop immediately. The client will execute it and send back the result. ")
	b.WriteString("After receiving a result, either request one more tool in the same format or provide the final answer. ")
	b.WriteString("Only provide a normal answer when no client-side tool is needed.\n\nTools:\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s: %s\n", t.GetName(), t.GetDescription())
		if schema := strings.TrimSpace(t.GetParametersSchema()); schema != "" {
			fmt.Fprintf(&b, "  parameters (JSON schema): %s\n", schema)
		}
	}
	return b.String()
}

// parseToolCalls 从助手回复提取合法工具块。首个合法块之后的文本按契约丢弃（契约要求请求工具后立即停止）。
func parseToolCalls(content string) (text string, calls []parsedToolCall) {
	blocks := findToolBlocks(content)
	firstStart := -1
	for _, block := range blocks {
		var wc wireToolCall
		if err := json.Unmarshal([]byte(block.raw), &wc); err != nil || wc.Name == "" {
			continue
		}
		args := strings.TrimSpace(string(wc.Arguments))
		if args == "" || args == "null" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			continue
		}
		if firstStart < 0 {
			firstStart = block.start
		}
		sum := sha256.Sum256([]byte(block.raw))
		calls = append(calls, parsedToolCall{
			ID:        fmt.Sprintf("call_%x", sum[:12]),
			Name:      wc.Name,
			Arguments: args,
		})
	}
	if len(calls) == 0 {
		return content, nil
	}
	return strings.TrimSpace(content[:firstStart]), calls
}

// findToolBlocks 扫描所有闭合的 TOOL_CALL 块（内容已 trim）。
func findToolBlocks(content string) []toolBlock {
	var blocks []toolBlock
	for offset := 0; offset < len(content); {
		open := strings.Index(content[offset:], toolOpenTag)
		if open < 0 {
			break
		}
		open += offset
		rawStart := open + len(toolOpenTag)
		closeAt := strings.Index(content[rawStart:], toolCloseTag)
		if closeAt < 0 {
			break
		}
		closeAt += rawStart
		blocks = append(blocks, toolBlock{start: open, raw: strings.TrimSpace(content[rawStart:closeAt])})
		offset = closeAt + len(toolCloseTag)
	}
	return blocks
}
