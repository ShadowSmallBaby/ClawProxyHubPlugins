// telemetry.go — WorkBuddy 客户端埋点（POST /v2/report）构造与发送。
// 成长任务的进度判定只消费客户端埋点（实测不发对话也 +1），
// 事件字段照桌面端 5.5.4 载荷复刻；机器指纹进程内固定，模拟同一台客户端。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// postRaw POST 原始 JSON body。
func postRaw(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = shared.UpstreamClient("")
	}
	return client.Do(req)
}

const reportPath = "/v2/report"

// 事件码
const (
	evChatRequestSend        = "chat_request_send"
	evChatMessageSend        = "chat_message_send"
	evChatMessageResponse    = "chat_message_response"
	evChatRequestResponse    = "chat_request_response"
	evTaskCreated            = "agent_task_created"
	evTaskCreatedWithTpl     = "agent_task_created_with_template"
	evTemplateUsed           = "template_used"
	evExpertSummoned         = "expert_summoned"
	evExpertActualUse        = "expert_actual_use"
	evSkillInstalled         = "skill_installed"
	evSkillAction            = "skill_action"
	evSkillRequestSend       = "skill_request_send"
	evSkillInfo              = "skill_info"
	evChatToolAction         = "chat_tool_action"
	evDesignConversation     = "wbx_design_conversation_create"
	evDesignCanvasTaskCreate = "wbx_design_canvas_task_create"
	evAutomationCreated      = "automated_task_create_suc"
	evAppearanceSkinApply    = "appearance_skin_apply"
	evBuddyAppDiscoverClick  = "buddyapp_discover_click"
	evBuddyAppShow           = "buddyapp_show"
	evBuddyAppEnterClick     = "buddyapp_enter_click"
	evPlaybookPromptSend     = "playbook_prompt_send"
	evWebElementClick        = "web_element_click"
)

const (
	taskModeWorking = "working"
	taskModeDesign  = "design"
	releaseDate     = int64(1788787950918)
	commitHash      = "39046950a4209032e7a9c32d2e0b7c525219d813"
	extName         = "workbuddy-desktop"
	ideName         = "WorkBuddy"
	// Space 前端硬编码的《WorkBuddy 资料库介绍》文档 nodeId
	libraryIntroNodeID = "o0KWYeynteVv06UnAZqIFm"
)

var (
	fpOnce sync.Once
	fp     struct{ machineID, qimei36, sessionID string }
)

// fingerprint 进程内生成一次，模拟同一台桌面端。
func fingerprint() (string, string, string) {
	fpOnce.Do(func() {
		fp.machineID, fp.qimei36, fp.sessionID = uuidV4(), shared.RandHex(16), uuidV4()
	})
	return fp.machineID, fp.qimei36, fp.sessionID
}

func nowMS() int64 { return time.Now().UnixMilli() }

// uuidV4 标准 8-4-4-4-12 形态。
func uuidV4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// conversationIDs 一轮对话的三元 id。
type conversationIDs struct{ conversation, request, message string }

func newConversation() conversationIDs {
	return conversationIDs{uuidV4(), shared.RandHex(16), shared.RandHex(16)}
}

// commonFields 每条事件都带的客户端公共字段。
func commonFields(cred *credential) map[string]interface{} {
	machineID, qimei36, sessionID := fingerprint()
	nickname := cred.Account.Nickname
	ideVer := clientIDEVersion()
	return map[string]interface{}{
		"timezone": "Asia/Shanghai", "qimei36": qimei36,
		"userId": cred.Account.UID, "username": nickname, "userNickname": nickname,
		"product": "workbuddy-desktop", "releaseDate": releaseDate, "commit": commitHash,
		"os": "win32", "arch": "x64", "osVersion": "10.0.26200",
		"cpuModel": "Intel", "cpuCores": 16, "memorySize": 32,
		"vcsType": "unknown", "vcsRepo": "", "vcsBranchName": "", "vcsRevId": "",
		"extName": extName, "extVersion": ideVer,
		"ideName": ideName, "ideType": ideName,
		"machineId": machineID, "sessionId": sessionID, "ideVersion": ideVer,
	}
}

// ev 组装单条事件：公共字段 + 业务字段。
func ev(code string, cred *credential, fields map[string]interface{}) map[string]interface{} {
	e := map[string]interface{}{"eventCode": code, "timestamp": nowMS(), "reportDelay": 0}
	for k, v := range commonFields(cred) {
		e[k] = v
	}
	for k, v := range fields {
		e[k] = v
	}
	return e
}

func traceFields(ids conversationIDs, agentType string) map[string]interface{} {
	return map[string]interface{}{
		"traceId": ids.request, "rootRequestId": ids.request,
		"parentConversationId": ids.conversation, "agentName": "cli", "agentType": agentType,
	}
}

func usageFields(prompt, completion int) map[string]interface{} {
	return map[string]interface{}{
		"inputToken": prompt, "outputToken": completion, "totalToken": prompt + completion,
		"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": prompt,
	}
}

// ---------- chat 四连 ----------

type chatEventOpts struct {
	model     string
	modelName string
	mode      string
	expertID  string
	skillIDs  []string
	agentType string
}

func chatRequestSendEvent(cred *credential, ids conversationIDs, o chatEventOpts) map[string]interface{} {
	f := map[string]interface{}{
		"mode": shared.OrDefault(o.mode, "craft"), "conversationId": ids.conversation, "requestId": ids.request,
		"inputLength": 32, "requestModelId": o.model, "requestModelName": shared.OrDefault(o.modelName, o.model),
		"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false, "codebaseEnable": false,
		"maxToken": 0, "maxSteps": 500, "temperature": 0, "maxRetries": 0, "mentionContexts": []string{},
		"knowledgeId": []string{}, "knowledgeName": []string{}, "codebaseId": "", "mentionContextCount": 0,
		"command": "", "recommendId": "", "skillId": joinNonEmpty(o.skillIDs, ","),
		"skillCount": len(o.skillIDs), "totalCount": 0, "presentAt": nowMS(), "expertId": o.expertID,
		"codebuddy.session_id":              ids.conversation,
		"codebuddy.conversation_request_id": ids.request,
	}
	for k, v := range traceFields(ids, shared.OrDefault(o.agentType, "main")) {
		f[k] = v
	}
	return ev(evChatRequestSend, cred, f)
}

func chatMessageSendEvent(cred *credential, ids conversationIDs, o chatEventOpts) map[string]interface{} {
	f := map[string]interface{}{
		"conversationId": ids.conversation, "requestId": ids.request, "messageId": ids.message,
		"requestModelId": o.model, "requestModelName": shared.OrDefault(o.modelName, o.model),
		"historyCount": 0, "isContextTruncated": false, "currentStepCount": 1,
		"presentAt": nowMS(), "expertId": o.expertID,
	}
	for k, v := range traceFields(ids, shared.OrDefault(o.agentType, "main")) {
		f[k] = v
	}
	return ev(evChatMessageSend, cred, f)
}

func chatMessageResponseEvent(cred *credential, ids conversationIDs, o chatEventOpts) map[string]interface{} {
	now := nowMS()
	f := map[string]interface{}{
		"conversationId": ids.conversation, "requestId": ids.request, "messageId": ids.message,
		"requestModelId": o.model, "requestModelName": shared.OrDefault(o.modelName, o.model), "responseModelId": o.model,
		"isSuccessful": true, "messageErrorCode": "0", "finishReason": "stop",
		"firstTokenAt": now, "presentAt": now, "expertId": o.expertID,
	}
	for k, v := range usageFields(16, 32) {
		f[k] = v
	}
	for k, v := range traceFields(ids, shared.OrDefault(o.agentType, "main")) {
		f[k] = v
	}
	return ev(evChatMessageResponse, cred, f)
}

func chatRequestResponseEvent(cred *credential, ids conversationIDs, o chatEventOpts) map[string]interface{} {
	f := map[string]interface{}{
		"mode": shared.OrDefault(o.mode, "craft"), "conversationId": ids.conversation, "requestId": ids.request,
		"requestModelId": o.model, "requestModelName": shared.OrDefault(o.modelName, o.model), "toolCallCount": 0,
		"isSuccessful": true, "messageErrorCode": "0", "finishReason": "stop",
		"presentAt": nowMS(), "expertId": o.expertID,
	}
	for k, v := range usageFields(16, 32) {
		f[k] = v
	}
	for k, v := range traceFields(ids, shared.OrDefault(o.agentType, "main")) {
		f[k] = v
	}
	return ev(evChatRequestResponse, cred, f)
}

func joinNonEmpty(list []string, sep string) string {
	out, first := "", true
	for _, s := range list {
		if s == "" {
			continue
		}
		if !first {
			out += sep
		}
		out += s
		first = false
	}
	return out
}

// ---------- 桌面端 renderer 事件 ----------

func templateAction(tpl map[string]interface{}) string {
	id := fmt.Sprint(tpl["id"])
	if idx, ok := tpl["promptIndex"]; ok && idx != nil {
		return id + ":" + fmt.Sprint(idx)
	}
	return id
}

func taskCreatedEvent(cred *credential, ids conversationIDs, o chatEventOpts, taskMode string,
	tpl, expert map[string]interface{}, skillNames []string) map[string]interface{} {
	hasTpl, hasExpert := tpl != nil, expert != nil
	skills := nonEmpty(skillNames)
	return ev(evTaskCreated, cred, map[string]interface{}{
		"source": "desktop", "name": taskMode, "task_target": "local", "mode": shared.OrDefault(o.mode, "craft"),
		"requestModelId": o.model, "has_repo": false, "repo_type": "", "workspace_type": "empty",
		"has_connector": false, "connector_types": []string{}, "has_mention": false, "mention_types": []string{},
		"has_template": hasTpl, "action": ternaryStr(hasTpl, templateAction(tpl), ""),
		"template_name":  mapStr(tpl, "name"),
		"conversationId": ids.conversation, "requestId": ids.request, "messageId": "",
		"requestModelName": shared.OrDefault(o.modelName, o.model),
		"has_expert":       hasExpert, "expert_id": mapStr(expert, "id"), "expert_name": mapStr(expert, "name"),
		"expert_industry_id": mapStr(expert, "industryId"),
		"has_skill":          len(skills) > 0, "skill_names": skills,
	})
}

func taskCreatedWithTemplateEvent(cred *credential, ids conversationIDs, tpl map[string]interface{}) map[string]interface{} {
	return ev(evTaskCreatedWithTpl, cred, map[string]interface{}{
		"mode": "desktop", "isCustomModel": true, "id": templateAction(tpl),
		"name": mapStr(tpl, "name"), "requestId": ids.request,
	})
}

func templateUsedEvent(cred *credential, tpl map[string]interface{}, taskMode string) map[string]interface{} {
	return ev(evTemplateUsed, cred, map[string]interface{}{
		"template_id": fmt.Sprint(tpl["id"]), "task_mode": taskMode,
	})
}

func expertSummonedEvent(cred *credential, expert map[string]interface{}) map[string]interface{} {
	return ev(evExpertSummoned, cred, map[string]interface{}{
		"id": mapStr(expert, "id"), "name": mapStr(expert, "name"),
		"expertTitle": mapStr(expert, "title"), "type": mapStr(expert, "industryId"),
		"expertType": shared.OrDefault(mapStr(expert, "expertType"), "agent"),
	})
}

func expertActualUseEvent(cred *credential, ids conversationIDs, expert map[string]interface{}) map[string]interface{} {
	return ev(evExpertActualUse, cred, map[string]interface{}{
		"mode": "desktop", "id": mapStr(expert, "id"), "name": mapStr(expert, "name"),
		"expertTitle": mapStr(expert, "title"), "type": mapStr(expert, "industryId"),
		"expertType": shared.OrDefault(mapStr(expert, "expertType"), "agent"),
		"source":     shared.OrDefault(mapStr(expert, "source"), "openapi"), "version": mapStr(expert, "version"), "cost": 0,
		"conversationId": ids.conversation, "requestId": ids.request,
	})
}

func skillInstalledEvent(cred *credential, skill map[string]interface{}) map[string]interface{} {
	return ev(evSkillInstalled, cred, map[string]interface{}{
		"name": mapStr(skill, "name"), "skillId": mapStr(skill, "id"), "skillVersion": mapStr(skill, "version"),
	})
}

func skillActionEvent(cred *credential, skill map[string]interface{}) map[string]interface{} {
	return ev(evSkillAction, cred, map[string]interface{}{
		"name": mapStr(skill, "name"), "skillId": mapStr(skill, "id"), "skillVersion": mapStr(skill, "version"),
		"action": "install", "channelType": "wecom",
	})
}

func skillRequestSendEvent(cred *credential, ids conversationIDs, skills []map[string]interface{}) map[string]interface{} {
	ids2, names := "", ""
	for i, s := range skills {
		if i > 0 {
			ids2 += ","
			names += ","
		}
		ids2 += mapStr(s, "id")
		names += mapStr(s, "name")
	}
	return ev(evSkillRequestSend, cred, map[string]interface{}{
		"ext1": "", "requestId": ids.request, "skills": ids2, "skillNames": names, "isOfficial": 0,
	})
}

func chatToolActionEvent(cred *credential, ids conversationIDs, toolName string, o chatEventOpts) map[string]interface{} {
	f := map[string]interface{}{
		"conversationId": ids.conversation, "requestId": ids.request, "messageId": ids.message,
		"toolName": toolName, "toolCallSuccessful": true, "toolStatus": "success",
		"requestModelId": o.model, "requestModelName": shared.OrDefault(o.modelName, o.model),
		"toolErrorCode": "0", "toolErrorCodeKey": "Success", "toolErrorMessage": "", "type": "main",
		"presentAt": nowMS(),
	}
	for k, v := range traceFields(ids, "main") {
		f[k] = v
	}
	return ev(evChatToolAction, cred, f)
}

func skillInfoEvent(cred *credential, ids conversationIDs, skill map[string]interface{}, o chatEventOpts) map[string]interface{} {
	return ev(evSkillInfo, cred, map[string]interface{}{
		"id": mapStr(skill, "name"), "skillId": mapStr(skill, "id"), "skillVersion": mapStr(skill, "version"),
		"toolStatus": "success", "fileCount": 1, "source": extName,
		"conversationId": ids.conversation, "requestId": ids.request, "messageId": ids.message,
		"requestModelId": o.model, "requestModelName": shared.OrDefault(o.modelName, o.model), "traceId": ids.request,
	})
}

func designConversationCreateEvent(cred *credential, ids conversationIDs) map[string]interface{} {
	return ev(evDesignConversation, cred, map[string]interface{}{
		"conversationId": ids.conversation, "id": "", "total": 1, "source": "design_tab",
		"imageMode": "", "designStyleId": "", "designStyleName": "",
	})
}

func designCanvasTaskCreateEvent(cred *credential, ids conversationIDs, name string) map[string]interface{} {
	return ev(evDesignCanvasTaskCreate, cred, map[string]interface{}{
		"conversationId": ids.conversation, "requestId": ids.request, "source": "design_tab",
		"isCustomModel": false, "name": name, "inputLength": 32, "id": shared.RandHex(16),
		"cost": 1500, "isSuccessful": true,
	})
}

func automationCreatedEvent(cred *credential, name string) map[string]interface{} {
	return ev(evAutomationCreated, cred, map[string]interface{}{
		"mode": "LOCAL", "name": name, "source": "chat", "expertId": "", "expertMarketplace": "",
		"connectorIds": "", "connectorCount": 0,
	})
}

func appearanceSkinApplyEvent(cred *credential, theme map[string]interface{}) map[string]interface{} {
	return ev(evAppearanceSkinApply, cred, map[string]interface{}{
		"action": "apply", "source": "settings_close", "id": mapStr(theme, "resourceKey"),
		"vipLevel": shared.OrDefault(mapStr(theme, "vipLevel"), "free"), "series": shared.OrDefault(mapStr(theme, "series"), "craft"),
		"type": shared.OrDefault(mapStr(theme, "editionType"), "free"),
	})
}

// buddyAppEnterEvents 「发现应用」→ 曝光 → 进入（三连）。
func buddyAppEnterEvents(cred *credential, appID, appName string) []map[string]interface{} {
	return []map[string]interface{}{
		ev(evBuddyAppDiscoverClick, cred, map[string]interface{}{"position": "sidebar"}),
		ev(evBuddyAppShow, cred, map[string]interface{}{"position": "switcher"}),
		ev(evBuddyAppEnterClick, cred, map[string]interface{}{
			"elementId": appID, "elementName": appName, "position": "switcher", "isFirstPage": "1",
		}),
	}
}

func playbookPromptSendEvent(cred *credential, ids conversationIDs, playbookID, name string) map[string]interface{} {
	return ev(evPlaybookPromptSend, cred, map[string]interface{}{
		"id": playbookID, "name": name, "type": "other", "promptLength": 30, "isOfficial": 1,
		"skills": "", "skillNames": "", "expertId": "", "expertName": "",
		"categoryId": "", "categoryName": "", "query": "", "source": "playbook_detail",
		"conversationId": ids.conversation,
	})
}

// libraryDocIntroClickEvent 资料库打开介绍文档（网页端形状，不带完整公共字段）。
func libraryDocIntroClickEvent(cred *credential) map[string]interface{} {
	machineID, _, _ := fingerprint()
	return map[string]interface{}{
		"eventCode": evWebElementClick, "timestamp": nowMS(), "reportDelay": 0,
		"pageURL":   "https://www.workbuddy.cn/space/d/" + libraryIntroNodeID,
		"elementId": "library_doc_intro_click", "elementName": "Workbuddy资料库介绍",
		"os": "Win32", "arch": "", "osVersion": "10.0",
		"userAgent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
		"machineId": machineID, "userId": cred.Account.UID,
		"userNickname": cred.Account.Nickname, "enterpriseId": "",
	}
}

// ---------- 发送 ----------

// reportEvents POST /v2/report；任何失败只返回 false，埋点不拖垮主链路。
func (p *plugin) reportEvents(ctx context.Context, cred *credential, events []map[string]interface{}) bool {
	if len(events) == 0 {
		return true
	}
	raw, err := json.Marshal(events)
	if err != nil {
		return false
	}
	resp, err := postRaw(ctx, p.hc(cred), upstreamBase+reportPath, p.headers(cred, true), raw)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e struct {
		Code interface{} `json:"code"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	switch v := e.Code.(type) {
	case float64:
		return v == 0
	case string:
		return v == "0"
	}
	return resp.StatusCode == 200
}

// ---------- 小工具 ----------

func mapStr(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return ""
}

func nonEmpty(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func ternaryStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
