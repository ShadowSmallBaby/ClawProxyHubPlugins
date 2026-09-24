// Postman Agent Mode 上游客户端：组 /_gw/chat 请求体、发起流式调用、解析工作区。
// 线协议逆向自 postman-web 12.23.0。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// excludedNativeTools 注入 thirdParty 时排除的 Postman 内置工具，避免模型跑去搜工作区而不用注入工具。
// 名字精确匹配（逆向自 ai-chat.js）；不能全量排除，否则报 "No tools found for tool search phrases"。
var excludedNativeTools = []string{
	"searchPostman", "searchInFiles", "getsOrListsWorkspaces", "getWorkspaceActivityLog",
	"createNewRequest", "createCollection", "createNewGRPCRequest", "createNewGraphQLRequest",
	"createNewMCPRequest", "createNewMQTTRequest", "createNewWebSocketRequest",
	"listCollections", "getFullCollection", "getCollection", "getCollectionRequest",
	"readFile", "createFile", "editFile", "listDirectory", "executeShellCommand",
	"navigateInApp", "openBrowserPage", "getTabDetails", "askUser",
}

// chatParams buildChatBody 的入参。
type chatParams struct {
	Query               string
	ConversationID      string
	WorkspaceID         string
	Product             string
	ThinkingLevel       string
	ModelKey            string
	ChatType            string // USER_QUERY / TOOL_RESPONSE
	ToolCallID          string
	ToolResponse        string
	ToolResponseSummary string
	ThirdParty          map[string]interface{} // nil = 不注入工具
}

// buildChatBody 组 Agent Mode 请求体。
func buildChatBody(pr chatParams) map[string]interface{} {
	chatType := shared.OrDefault(pr.ChatType, "USER_QUERY")
	input := map[string]interface{}{
		"chatType":       chatType,
		"query":          pr.Query,
		"toolResponse":   pr.ToolResponse,
		"useCase":        nil,
		"conversationId": nilIfEmpty(pr.ConversationID),
		"agent":          nil,
		"product":        pr.Product,
	}
	if pr.ThinkingLevel != "" {
		input["thinkingLevel"] = pr.ThinkingLevel
	}
	if pr.ModelKey != "" {
		input["modelKey"] = pr.ModelKey
	}
	if chatType == "TOOL_RESPONSE" {
		input["toolCallId"] = pr.ToolCallID
		input["toolResponseSummary"] = shared.OrDefault(pr.ToolResponseSummary, "Tool executed successfully")
	}

	hasThirdParty := len(pr.ThirdParty) > 0
	excluded := []string{}
	if hasThirdParty {
		excluded = excludedNativeTools
	}
	thirdParty := pr.ThirdParty
	if thirdParty == nil {
		thirdParty = map[string]interface{}{}
	}
	return map[string]interface{}{
		"input":    input,
		"platform": "WEB",
		"clientTools": map[string]interface{}{
			"nativeToolsHash": nativeToolsHash,
			"excludedTools":   excluded,
			"thirdParty":      thirdParty,
			"native":          []interface{}{},
		},
		"clientKBTerms":     map[string]interface{}{"nativeTermsHash": nil, "excludedKBTerms": []interface{}{}},
		"mandatoryContext":  map[string]interface{}{"workspaceId": pr.WorkspaceID},
		"selectedContext":   []interface{}{},
		"backgroundContext": []interface{}{},
		"availableSkills":   []interface{}{},
		"devModeOptions":    map[string]interface{}{},
	}
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// buildThirdParty 信封 tools → Postman thirdParty。server 名固定 "user"（工具名无前缀，模型直接调）。
// 内置工具被 excludedTools 屏蔽，模型只见这里注入的工具。
func buildThirdParty(tools []*pb.ToolDefinition) map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	serverTools := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		name := t.GetName()
		if name == "" || strings.HasPrefix(name, "namespace:") {
			continue
		}
		if name == "exec" {
			// exec：codex 通用执行器。必须给 code 参数 schema，否则 Postman 生成空 arguments。
			serverTools = append(serverTools, map[string]interface{}{
				"name":        "exec",
				"description": buildExecDescription(t.GetDescription()),
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"code": map[string]interface{}{
							"type":        "string",
							"description": "The JavaScript code to execute. Use await tools.<name>({...}) to call tools.",
						},
					},
					"required": []string{"code"},
				},
			})
			continue
		}
		serverTools = append(serverTools, map[string]interface{}{
			"name":        name,
			"description": t.GetDescription(),
			"parameters":  rawSchema(t.GetParametersSchema()),
		})
	}
	if len(serverTools) == 0 {
		return nil
	}
	return map[string]interface{}{
		"user": map[string]interface{}{
			// url 不带路径：Postman 侧仅作占位，工具由下游执行。
			"serverConfig": map[string]interface{}{"url": "http://localhost:8788"},
			"tools":        serverTools,
		},
	}
}

func rawSchema(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	var v interface{}
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

// buildExecDescription exec 描述：codex 原生描述前加 Postman 友好指引。
func buildExecDescription(nativeDesc string) string {
	return `This tool runs JavaScript that can access the user's computer: read/write files, run shell commands, browse directories. Call tools like: await tools.read_file({path: "C:/path"}); await tools.exec_command({command: "dir"}).

IMPORTANT OUTPUT RULES:
- Nested tools (exec_command, read_file, etc.) already return plain strings. Output them directly: text(result).
- NEVER wrap results with JSON.stringify(result) - that double-encodes the output and makes it unreadable.
- For file reading, prefer text(result) with the raw file contents.

` + nativeDesc
}

// sendChat 发起 Agent Mode 流式请求，返回原始响应（调用方负责关闭 Body）。
func (p *plugin) sendChat(ctx context.Context, cred *credential, site *siteConfig, body map[string]interface{}) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	u := site.Origin + "/_gw/chat?stream=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cred.Cookie != "" {
		req.Header.Set("Cookie", cred.Cookie)
	}
	if cred.APIKey != "" {
		req.Header.Set("x-api-key", cred.APIKey)
	}
	req.Header.Set("x-pstmn-req-service", "agent-mode-service")
	req.Header.Set("x-app-version", site.AppVersion)
	req.Header.Set("User-Agent", site.BrowserUA)
	req.Header.Set("Accept", "text/event-stream, application/json, text/plain, */*")
	req.Header.Set("Origin", site.Origin)
	req.Header.Set("Referer", site.Origin+"/")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")

	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &upstreamError{status: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	return resp, nil
}

// upstreamError 保留上游状态码，供 Chat 映射信封错误码。
type upstreamError struct {
	status int
	body   string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("postman upstream HTTP %d: %s", e.status, shared.Truncate(e.body, 300))
}

// upstreamCode 上游错误 → 信封错误码。
func upstreamCode(err error) int32 {
	if ue, ok := err.(*upstreamError); ok {
		switch {
		case ue.status == 401 || ue.status == 403:
			return 401
		case ue.status == 429:
			return 429
		}
	}
	return 502
}

// resolveWorkspaceID 解析账号首个工作区：apiKey 走 api.postman.com（纯 HTTP），cookie 走团队网关代理。
func (p *plugin) resolveWorkspaceID(ctx context.Context, cred *credential, site *siteConfig) (string, error) {
	if cred.APIKey != "" {
		return p.workspaceViaAPIKey(ctx, cred)
	}
	return p.workspaceViaGateway(ctx, cred, site)
}

func (p *plugin) workspaceViaAPIKey(ctx context.Context, cred *credential) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.postman.com/workspaces", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", cred.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", browserUA)
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &upstreamError{status: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	var out struct {
		Workspaces []struct {
			ID string `json:"id"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("parse workspaces: %w", err)
	}
	if len(out.Workspaces) == 0 {
		return "", fmt.Errorf("account has no workspaces")
	}
	return out.Workspaces[0].ID, nil
}

func (p *plugin) workspaceViaGateway(ctx context.Context, cred *credential, site *siteConfig) (string, error) {
	payload, _ := json.Marshal(map[string]string{"service": "workspaces", "method": "GET", "path": "/workspaces"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, site.Origin+"/_api/ws/proxy", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cred.Cookie)
	if cred.TeamID != "" {
		req.Header.Set("x-entity-team-id", cred.TeamID)
	}
	req.Header.Set("User-Agent", site.BrowserUA)
	req.Header.Set("Origin", site.Origin)
	req.Header.Set("Referer", site.Origin+"/")
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &upstreamError{status: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Workspaces []struct {
			ID string `json:"id"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("parse workspaces: %w", err)
	}
	if len(out.Data) > 0 {
		return out.Data[0].ID, nil
	}
	if len(out.Workspaces) > 0 {
		return out.Workspaces[0].ID, nil
	}
	return "", fmt.Errorf("account has no workspaces")
}
