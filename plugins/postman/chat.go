// 核心链路：统一信封 → Agent Mode 请求 → Postman 私有 SSE → StreamEvent。
// Postman 是服务端保留历史的会话模型（每轮只发最后一条 query + conversationId），
// 插件用内存映射「线程 → 会话」跨轮复用，工具调用透传给下游执行。
package main

import (
	"bufio"
	"context"
	"strings"
	"sync"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// wsCache 账号 → 已解析工作区 id（进程内缓存，避免每轮重复解析）。
var wsCache sync.Map

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	site, err := p.site(cred.instanceID)
	if err != nil {
		return stream.Send(shared.Failed(400, err.Error()))
	}

	wsID, err := p.workspaceID(ctx, cred, site)
	if err != nil {
		return stream.Send(shared.Failed(upstreamCode(err), "resolve workspace: "+err.Error()))
	}

	modelKey := resolveModelKey(req.GetModel())
	plan := p.planTurn(req)
	if plan.query == "" && plan.chatType != "TOOL_RESPONSE" && len(req.GetTools()) == 0 {
		return stream.Send(shared.Failed(400, "request has no content"))
	}

	body := buildChatBody(chatParams{
		Query:               plan.query,
		ConversationID:      plan.convID,
		WorkspaceID:         wsID,
		Product:             site.Product,
		ThinkingLevel:       thinkingLevel(modelKey, req.GetExtra()["reasoning_effort"]),
		ModelKey:            modelKey,
		ChatType:            plan.chatType,
		ToolCallID:          plan.toolCallID,
		ToolResponse:        plan.toolResponse,
		ToolResponseSummary: shared.Truncate(plan.toolResponse, 120),
		ThirdParty:          buildThirdParty(req.GetTools()),
	})

	resp, err := p.sendChat(ctx, cred, site, body)
	if err != nil {
		return stream.Send(shared.Failed(upstreamCode(err), err.Error()))
	}
	defer resp.Body.Close()

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.GetModel()},
	}}); err != nil {
		return err
	}

	st := &streamState{
		p:         p,
		threadKey: plan.threadKey,
		emit:      func(ev *pb.StreamEvent) { _ = stream.Send(ev) },
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		st.handleLine(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return stream.Send(shared.Failed(502, "read upstream stream: "+err.Error()))
	}

	finish := "stop"
	if st.sawTool {
		finish = "tool_calls"
	}
	return stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageFinish{
		MessageFinish: &pb.MessageFinish{FinishReason: finish},
	}})
}

// workspaceID 取账号工作区：凭据显式指定 > 进程缓存 > 上游解析。
func (p *plugin) workspaceID(ctx context.Context, cred *credential, site *siteConfig) (string, error) {
	if cred.WorkspaceID != "" {
		return cred.WorkspaceID, nil
	}
	key := cred.accountID
	if key != "" {
		if v, ok := wsCache.Load(key); ok {
			return v.(string), nil
		}
	}
	id, err := p.resolveWorkspaceID(ctx, cred, site)
	if err != nil {
		return "", err
	}
	if key != "" && id != "" {
		wsCache.Store(key, id)
	}
	return id, nil
}

// turnPlan 一回合的上游会话决策。
type turnPlan struct {
	threadKey    string
	chatType     string // USER_QUERY / TOOL_RESPONSE
	convID       string
	query        string
	toolCallID   string
	toolResponse string
}

// planTurn 依据信封历史 + 内存会话映射决定本轮策略：
//   - 末尾是工具结果且匹配到已登记调用 → TOOL_RESPONSE 复用会话
//   - 新用户消息且线程已有会话       → USER_QUERY 复用会话（多轮）
//   - 否则                          → USER_QUERY 新建会话，注入 system 上下文
func (p *plugin) planTurn(req *pb.ChatRequest) turnPlan {
	msgs := req.GetMessages()
	plan := turnPlan{
		threadKey: threadKeyOf(msgs),
		chatType:  "USER_QUERY",
		query:     lastUserText(msgs),
	}

	// 工具续轮：末尾若干条 tool 结果，取最近一条匹配已登记调用的。
	if out := lastToolOutput(msgs); out != nil {
		if reg, ok := p.lookupPending(out.callID, plan.threadKey); ok {
			plan.chatType = "TOOL_RESPONSE"
			plan.convID = reg.convID
			plan.toolCallID = out.callID
			plan.toolResponse = out.content
			p.deletePending(out.callID)
			return plan
		}
		// 未匹配（进程重启丢失登记 / 陈旧结果）：拼进 query 走 USER_QUERY，禁止复用旧会话。
		if plan.query == "" {
			plan.query = "Continue based on the tool result:\n" + out.content
		} else {
			plan.query = plan.query + "\n\n[Tool result for " + out.callID + "]:\n" + out.content
		}
		return plan
	}

	// 新用户消息：线程已有会话则复用（多轮），否则新建 + 注入 system 上下文。
	if conv, ok := p.lookupThread(plan.threadKey); ok {
		plan.convID = conv
		return plan
	}
	plan.query = injectSystemContext(systemContext(msgs), plan.query)
	return plan
}

// 首轮 system 注入用的固定包裹文本。
const (
	sysPrefixHead = "[System instructions from the host agent]\n"
	sysPrefixMid  = "\n\n[User request]\n"
	sysTruncMark  = "\n\n[TRUNCATED: system context omitted]\n"
)

// injectSystemContext 首轮把 system 指令拼到 query 前（截断到 Agent Mode 上限）。
func injectSystemContext(system, query string) string {
	system = strings.TrimSpace(system)
	if system == "" {
		return query
	}
	// 预算需扣除固定包裹文本，保证总长不超上限。
	budget := maxQueryLen - len(query) - len(sysPrefixHead) - len(sysPrefixMid) - len(sysTruncMark)
	if budget < 0 {
		budget = 0
	}
	if len(system) > budget {
		// 保留开头（身份 / 工具提示）与结尾，中间截断。
		head := int(float64(budget) * 0.7)
		tail := budget - head
		system = system[:head] + sysTruncMark + system[len(system)-tail:]
	}
	return sysPrefixHead + system + sysPrefixMid + query
}

// ---------- 会话映射（内存） ----------

func (p *plugin) lookupThread(threadKey string) (string, bool) {
	if threadKey == "" {
		return "", false
	}
	p.convMu.Lock()
	defer p.convMu.Unlock()
	conv, ok := p.convByThread[threadKey]
	return conv, ok && conv != ""
}

func (p *plugin) rememberThread(threadKey, convID string) {
	if threadKey == "" || convID == "" {
		return
	}
	p.convMu.Lock()
	defer p.convMu.Unlock()
	if p.convByThread == nil {
		p.convByThread = map[string]string{}
	}
	p.convByThread[threadKey] = convID
}

func (p *plugin) rememberPending(callID, threadKey, convID string) {
	if callID == "" || convID == "" {
		return
	}
	p.convMu.Lock()
	defer p.convMu.Unlock()
	if p.pendingCall == nil {
		p.pendingCall = map[string]pendingToolCall{}
	}
	p.pendingCall[callID] = pendingToolCall{threadKey: threadKey, convID: convID}
}

// lookupPending 取已登记调用；线程键不一致视为未匹配（避免跨会话续错）。
func (p *plugin) lookupPending(callID, threadKey string) (pendingToolCall, bool) {
	p.convMu.Lock()
	defer p.convMu.Unlock()
	reg, ok := p.pendingCall[callID]
	if !ok {
		return pendingToolCall{}, false
	}
	if reg.threadKey != "" && threadKey != "" && reg.threadKey != threadKey {
		return pendingToolCall{}, false
	}
	return reg, true
}

func (p *plugin) deletePending(callID string) {
	p.convMu.Lock()
	defer p.convMu.Unlock()
	delete(p.pendingCall, callID)
}
