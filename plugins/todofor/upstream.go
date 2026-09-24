// todofor.ai 上游客户端：REST（建 todo / 拉消息 / 计费 / 项目 / Agent）+ 前端 WebSocket 订阅。
// 移植自 todo2api internal/upstream，裁剪为插件所需子集（单账号，无池化）。
package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"github.com/gorilla/websocket"
)

const defaultBaseURL = "https://api.todofor.ai/api/v1"

// httpError 保留上游错误响应，供调用方区分账号失效与请求非法。
type httpError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
	Code       string
	Body       string
}

func (e *httpError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Body
	}
	return fmt.Sprintf("upstream %s %s: HTTP %d %s", e.Method, e.Path, e.StatusCode, shared.Truncate(msg, 200))
}

func newHTTPError(method, path string, status int, data []byte) *httpError {
	body := strings.TrimSpace(string(data))
	var env struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Error   *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &env)
	msg, code := env.Message, env.Code
	if env.Error != nil {
		if env.Error.Message != "" {
			msg = env.Error.Message
		}
		if env.Error.Code != "" {
			code = env.Error.Code
		}
	}
	return &httpError{Method: method, Path: path, StatusCode: status, Message: msg, Code: code, Body: body}
}

// client 单账号上游客户端（apiKey + 复用 HTTP transport）。
type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(baseURL, apiKey string, hc *http.Client) *client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: hc}
}

// ---------- 数据结构 ----------

// block 消息内容块（text / tool / bash ...）。
type block struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// runMeta 一条上游消息附带的计量操作。
type runMeta struct {
	Cost   float64 `json:"cost"`
	Type   string  `json:"type"`
	Extras struct {
		Model            string `json:"model"`
		InputTokens      int    `json:"inputTokens"`
		OutputTokens     int    `json:"outputTokens"`
		CacheReadTokens  int    `json:"cacheReadTokens"`
		CacheWriteTokens int    `json:"cacheWriteTokens"`
		ContextTokens    int    `json:"contextTokens"`
	} `json:"extras"`
}

type message struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Blocks  []block   `json:"blocks"`
	RunMeta []runMeta `json:"runMeta"`
}

type todo struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Status    string `json:"status"`
}

// agentSettings 上游 AgentSettings 模板（仅保留插件会用到 / 需回传的字段）。
type agentSettings struct {
	ID                string           `json:"id,omitempty"`
	Name              string           `json:"name,omitempty"`
	OwnerID           string           `json:"ownerId,omitempty"`
	Model             string           `json:"model,omitempty"`
	SystemMessage     string           `json:"systemMessage,omitempty"`
	SystemMessageMode string           `json:"systemMessageMode,omitempty"`
	Permissions       *toolPermissions `json:"permissions,omitempty"`
	SpecID            string           `json:"specId,omitempty"`
	Color             string           `json:"color,omitempty"`
}

type toolPermissions struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny,omitempty"`
}

// billingUsage 账号计费状态（用作 GetProfile 余额）。
type billingUsage struct {
	TotalBalance        float64 `json:"totalBalance"`
	ManualBalance       float64 `json:"manualBalance"`
	SubscriptionBalance float64 `json:"subscriptionBalance"`
	Tier                string  `json:"tier"`
}

type modelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	OwnedBy       string `json:"owned_by,omitempty"`
	ContextLength int64  `json:"context_length,omitempty"`
}

// ---------- REST ----------

// do 发一个 JSON 请求并解出 out。X-API-Key 鉴权，>=300 转 httpError。
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return newHTTPError(method, path, resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// models 拉上游模型目录。
func (c *client) models(ctx context.Context) ([]modelInfo, error) {
	var resp struct {
		Data []modelInfo `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/models", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// firstProject 取账号首个项目 id。
func (c *client) firstProject(ctx context.Context) (string, error) {
	var items []struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/projects", nil, &items); err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", fmt.Errorf("account has no projects")
	}
	id := items[0].Project.ID
	if id == "" {
		id = items[0].ID
	}
	if id == "" {
		return "", fmt.Errorf("account's first project has no id")
	}
	return id, nil
}

// firstAgent 取账号首个 AgentSettings 模板（原文一并保留供透传）。
func (c *client) firstAgent(ctx context.Context) (agentSettings, error) {
	var raws []json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/agents", nil, &raws); err != nil {
		return agentSettings{}, err
	}
	if len(raws) == 0 {
		return agentSettings{}, fmt.Errorf("account has no agent settings")
	}
	return decodeAgent(raws[0])
}

// agent 取指定 AgentSettings 模板。
func (c *client) agent(ctx context.Context, agentID string) (agentSettings, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/agents/"+url.PathEscape(agentID), nil, &raw); err != nil {
		return agentSettings{}, err
	}
	return decodeAgent(raw)
}

func decodeAgent(raw json.RawMessage) (agentSettings, error) {
	var a agentSettings
	if err := json.Unmarshal(raw, &a); err != nil {
		return agentSettings{}, err
	}
	return a, nil
}

// billing 拉账号计费状态。
func (c *client) billing(ctx context.Context) (billingUsage, error) {
	var usage billingUsage
	if err := c.do(ctx, http.MethodGet, "/billing/usage", nil, &usage); err != nil {
		return billingUsage{}, err
	}
	return usage, nil
}

// createTodoReq 建 / 续 todo 请求体。
type createTodoReq struct {
	TodoID        string        `json:"todoId,omitempty"`
	ProjectID     string        `json:"projectId"`
	Content       string        `json:"content"`
	AgentSettings agentSettings `json:"agentSettings"`
}

// createTodo 起一段新对话，返回创建的 todo。
func (c *client) createTodo(ctx context.Context, projectID, content string, agent agentSettings) (*todo, error) {
	body := createTodoReq{ProjectID: projectID, Content: content, AgentSettings: agent}
	var t todo
	path := "/projects/" + url.PathEscape(projectID) + "/todos"
	if err := c.do(ctx, http.MethodPost, path, body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// messages 拉 todo 的消息列表。
func (c *client) messages(ctx context.Context, todoID string) ([]message, error) {
	var resp struct {
		Messages []message `json:"messages"`
	}
	path := "/todos/" + url.PathEscape(todoID) + "/messages"
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// getTodo 查当前运行状态（WebSocket 漏掉终止事件时的 REST 兜底）。
func (c *client) getTodo(ctx context.Context, todoID string) (*todo, error) {
	var t todo
	path := "/todos/" + url.PathEscape(todoID)
	if err := c.do(ctx, http.MethodGet, path, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ---------- WebSocket 订阅 ----------

// wsEvent 一条前端 WebSocket 信封。
type wsEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// subscription 已连接的前端 WebSocket。刻意在建 / 续 todo 之前打开，避免漏掉早期运行事件。
type subscription struct {
	c     *client
	tabID string
	conn  *websocket.Conn
}

// prepareSubscription 打开共享前端 WebSocket，apiKey 作为子协议发送（照官方 CLI）。
func (c *client) prepareSubscription(ctx context.Context) (*subscription, error) {
	tabID, err := newTabID()
	if err != nil {
		return nil, err
	}
	wsURL, err := frontendWSURL(c.baseURL, tabID)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Subprotocols:     []string{c.apiKey},
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		if resp != nil {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, newHTTPError(http.MethodGet, wsURL, resp.StatusCode, data)
		}
		return nil, fmt.Errorf("frontend ws dial: %w", err)
	}
	conn.SetReadLimit(5 * 1024 * 1024)
	return &subscription{c: c, tabID: tabID, conn: conn}, nil
}

// subscribe 把本 tab 绑到 todoID（HTTP），再把 WebSocket 信封转发到 out，直至 ctx 取消或 socket 关闭。
func (s *subscription) subscribe(ctx context.Context, todoID string, out chan<- wsEvent) error {
	if err := s.c.subscribeTodo(ctx, todoID, s.tabID); err != nil {
		return err
	}
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("frontend ws read: %w", err)
		}
		var ev wsEvent
		if json.Unmarshal(data, &ev) != nil || ev.Type == "" {
			continue
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *subscription) close() {
	if s != nil && s.conn != nil {
		s.conn.Close()
	}
}

// subscribeTodo POST /todos/{id}/subscribe 绑定 tab。
func (c *client) subscribeTodo(ctx context.Context, todoID, tabID string) error {
	body, _ := json.Marshal(map[string]string{"todoId": todoID})
	path := "/todos/" + url.PathEscape(todoID) + "/subscribe"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("X-Tab-ID", tabID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("subscribe todo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return newHTTPError(http.MethodPost, path, resp.StatusCode, data)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// frontendWSURL 由 REST base URL 推出前端 WebSocket 地址（https→wss，剥 /api/v1 加 /ws/v1/frontend）。
func frontendWSURL(baseURL, tabID string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v1") + "/ws/v1/frontend"
	u.RawPath = ""
	q := u.Query()
	q.Set("tabId", tabID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// newTabID 生成 UUIDv4 作为 frontend tab id。
func newTabID() (string, error) {
	var id [16]byte
	if _, err := crand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate tab id: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

// runnerModelID 把 OpenAI 代理模型 id 转成 AgentSettings 期望的 provider:author/model 形态。
func runnerModelID(modelID string) string {
	provider, _, ok := strings.Cut(modelID, "/")
	if !ok || provider == "" || strings.Contains(provider, ":") {
		return modelID
	}
	return provider + ":" + modelID
}

// shortModelID 取 "/" 后的短名（模型目录对外展示用）。
func shortModelID(id string) string {
	if _, runner, ok := strings.Cut(id, ":"); ok {
		id = runner
	}
	if _, short, ok := strings.Cut(id, "/"); ok && short != "" {
		return short
	}
	return id
}
