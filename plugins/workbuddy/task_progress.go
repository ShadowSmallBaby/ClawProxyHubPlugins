// task_progress.go — 用客户端埋点推进成长任务进度。
// 服务端按 /v2/report 埋点判定任务进度，这里为每个任务码定义"客户端做这件事时
// 会上报什么"，不真正发起模型请求（实测不需要，且省积分）。
package main

import (
	"context"
	"fmt"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	defaultModel     = "glm-5.2"
	defaultModelName = "GLM-5.2"
	roundInterval    = time.Second // 相邻两轮间隔，避免同毫秒批量到达被合并
	maxRoundsPerRun  = 5           // 单次执行最多推进轮数
	libraryIntroName = "WorkBuddy 资料库介绍"
)

// simulateChatOpts 一轮模拟对话的参数。
type simulateChatOpts struct {
	taskMode  string // working / design
	template  map[string]interface{}
	expert    map[string]interface{}
	skills    []map[string]interface{}
	agentType string
	preEvents []map[string]interface{}
}

// remaining 剩余推进次数（封顶 maxRoundsPerRun）。
func remaining(t growthTask) int {
	target := t.Progress.Target
	if target <= 0 {
		target = 1
	}
	n := target - t.Progress.Current
	if n < 1 {
		n = 1
	}
	if n > maxRoundsPerRun {
		n = maxRoundsPerRun
	}
	return n
}

// simulateChat 模拟"新建任务并完成一轮对话"的完整上报。
// 顺序与桌面端一致：新建任务（及模板/专家/技能附属事件）→ 发送 → 响应。
func (p *plugin) simulateChat(ctx context.Context, cred *credential, o simulateChatOpts) error {
	p.ensureIdentity() // 事件公共字段里的版本号要用当前设置
	ids := newConversation()
	o.agentType = shared.OrDefault(o.agentType, "main")
	co := chatEventOpts{model: defaultModel, modelName: defaultModelName, agentType: o.agentType}
	if o.template != nil {
		co.mode = mapStr(o.template, "mode")
	}
	if o.expert != nil {
		co.expertID = mapStr(o.expert, "id")
	}
	for _, s := range o.skills {
		co.skillIDs = append(co.skillIDs, mapStr(s, "id"))
	}

	skillNames := make([]string, 0, len(o.skills))
	for _, s := range o.skills {
		skillNames = append(skillNames, mapStr(s, "name"))
	}
	events := append([]map[string]interface{}{}, o.preEvents...)
	events = append(events, taskCreatedEvent(cred, ids, co, shared.OrDefault(o.taskMode, taskModeWorking), o.template, o.expert, skillNames))
	if o.template != nil {
		events = append(events, taskCreatedWithTemplateEvent(cred, ids, o.template))
		events = append(events, templateUsedEvent(cred, o.template, shared.OrDefault(o.taskMode, taskModeWorking)))
	}
	if o.expert != nil {
		events = append(events, expertActualUseEvent(cred, ids, o.expert))
	}
	if len(o.skills) > 0 {
		events = append(events, skillRequestSendEvent(cred, ids, o.skills))
	}
	if o.taskMode == taskModeDesign {
		events = append(events, designConversationCreateEvent(cred, ids))
	}
	events = append(events, chatRequestSendEvent(cred, ids, co), chatMessageSendEvent(cred, ids, co))
	if !p.reportEvents(ctx, cred, events) {
		return fmt.Errorf("埋点上报失败")
	}

	end := []map[string]interface{}{}
	for _, s := range o.skills {
		// 桌面端：模型调用 Skill 工具 → chat_tool_action + skill_info（成长埋点）
		end = append(end, chatToolActionEvent(cred, ids, "Skill", co))
		end = append(end, skillInfoEvent(cred, ids, s, co))
	}
	if o.taskMode == taskModeDesign {
		end = append(end, chatToolActionEvent(cred, ids, "ardot/create_design", co))
		end = append(end, designCanvasTaskCreateEvent(cred, ids, "移动端登录页"))
	}
	end = append(end, chatMessageResponseEvent(cred, ids, co), chatRequestResponseEvent(cred, ids, co))
	if !p.reportEvents(ctx, cred, end) {
		return fmt.Errorf("埋点上报失败")
	}
	return nil
}

func (p *plugin) chatRounds(ctx context.Context, cred *credential, rounds int, o simulateChatOpts) error {
	for i := 0; i < rounds; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(roundInterval):
			}
		}
		if err := p.simulateChat(ctx, cred, o); err != nil {
			return err
		}
	}
	return nil
}

// advanceTask 按任务码分发埋点链路；未知码返回错误由调用方记失败。
func (p *plugin) advanceTask(ctx context.Context, cred *credential, t growthTask) (int, error) {
	switch t.Code {
	case "black_cat", "Model_chat_GLM5.2": // 每日 1 次 GLM-5.2 对话
		return 1, p.chatRounds(ctx, cred, 1, simulateChatOpts{})

	case "chat_5", "first_buddy", "RichMeow_Chat": // 普通对话补齐剩余次数
		return remaining(t), p.chatRounds(ctx, cred, remaining(t), simulateChatOpts{})

	case "template_5": // 每轮换一个不同模板（按已完成数偏移）
		templates, err := p.catalogListTemplates(ctx, cred)
		if err != nil {
			return 0, err
		}
		if len(templates) == 0 {
			return 0, fmt.Errorf("模板列表为空")
		}
		n := remaining(t)
		for i := 0; i < n; i++ {
			tpl := templates[(t.Progress.Current+i)%len(templates)]
			if i > 0 {
				select {
				case <-ctx.Done():
					return i, ctx.Err()
				case <-time.After(roundInterval):
				}
			}
			tpl["promptIndex"] = 0
			if err := p.simulateChat(ctx, cred, simulateChatOpts{
				taskMode: mapStr(tpl, "mode"), template: tpl,
			}); err != nil {
				return i, err
			}
		}
		return n, nil

	case "expert_5": // 召唤不同的普通专家各对话一轮
		experts, err := p.catalogListExperts(ctx, cred, "", expertTypeAgent)
		if err != nil {
			return 0, err
		}
		if len(experts) == 0 {
			return 0, fmt.Errorf("专家列表为空")
		}
		return p.expertRounds(ctx, cred, t, experts)

	case "Expert_team_use_3": // 召唤专家团
		teams, err := p.catalogListExperts(ctx, cred, "", expertTypeTeam)
		if err != nil {
			return 0, err
		}
		if len(teams) == 0 {
			return 0, fmt.Errorf("专家团列表为空")
		}
		return p.expertRounds(ctx, cred, t, teams)

	case "Expert_lighthouse": // 指定专家（轻量云）
		expert, err := p.catalogFindExpert(ctx, cred, "轻量云")
		if err != nil {
			return 0, err
		}
		return p.expertRounds(ctx, cred, t, []map[string]interface{}{expert})

	case "skill_1": // 安装一个技能（云端注册 best-effort）并在对话中使用
		skills, err := p.catalogListSkills(ctx, cred)
		if err != nil {
			return 0, err
		}
		if len(skills) == 0 {
			return 0, fmt.Errorf("技能列表为空")
		}
		skill := skills[0]
		if err := p.catalogInstallSkill(ctx, cred, skill); err != nil {
			// 桌面端同样 best-effort：云端注册失败不阻断
		}
		err = p.simulateChat(ctx, cred, simulateChatOpts{
			skills:    []map[string]interface{}{skill},
			preEvents: []map[string]interface{}{skillInstalledEvent(cred, skill), skillActionEvent(cred, skill)},
		})
		return ternaryInt(err == nil, 1, 0), err

	case "create_canvas": // 设计创意模式新建任务
		return 1, p.chatRounds(ctx, cred, 1, simulateChatOpts{taskMode: taskModeDesign})

	case "automation_1": // 对话里创建本地定时任务
		err := p.simulateChat(ctx, cred, simulateChatOpts{
			preEvents: []map[string]interface{}{automationCreatedEvent(cred, "每周五自动生成周报")},
		})
		return ternaryInt(err == nil, 1, 0), err

	case "Hp_Appearance": // 使用指定主题（和平精英）
		theme, err := p.catalogFindTheme(ctx, cred, "和平精英")
		if err != nil {
			return 0, err
		}
		if err := p.catalogSetTheme(ctx, cred, theme); err != nil {
			// 判定只看埋点，云端同步失败不阻断
		}
		if !p.reportEvents(ctx, cred, []map[string]interface{}{appearanceSkinApplyEvent(cred, theme)}) {
			return 0, fmt.Errorf("埋点上报失败")
		}
		return 1, nil

	case "Buddy_App", "Buddy_App_QQ": // 「发现应用」进入企鹅教师助手
		events := buddyAppEnterEvents(cred, "cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
		if !p.reportEvents(ctx, cred, events) {
			return 0, fmt.Errorf("埋点上报失败")
		}
		return 1, nil

	case "playbook_prompt": // 灵感页「做同款」
		ids := newConversation()
		pre := []map[string]interface{}{playbookPromptSendEvent(cred, ids, "playbook-demo", "灵感案例")}
		err := p.simulateChat(ctx, cred, simulateChatOpts{preEvents: pre})
		return ternaryInt(err == nil, 1, 0), err

	case "Library_read": // 资料库打开介绍文档
		if !p.reportEvents(ctx, cred, []map[string]interface{}{libraryDocIntroClickEvent(cred)}) {
			return 0, fmt.Errorf("埋点上报失败")
		}
		return 1, nil
	}
	return 0, fmt.Errorf("任务「%s」没有已知的埋点链路（需在客户端真实操作）", shared.OrDefault(t.Title, t.Code))
}

// expertRounds 召唤专家并各对话一轮（专家召唤事件作为前置）。
func (p *plugin) expertRounds(ctx context.Context, cred *credential, t growthTask, experts []map[string]interface{}) (int, error) {
	n := remaining(t)
	for i := 0; i < n; i++ {
		expert := experts[(t.Progress.Current+i)%len(experts)]
		if i > 0 {
			select {
			case <-ctx.Done():
				return i, ctx.Err()
			case <-time.After(roundInterval):
			}
		}
		err := p.simulateChat(ctx, cred, simulateChatOpts{
			expert:    expert,
			preEvents: []map[string]interface{}{expertSummonedEvent(cred, expert)},
		})
		if err != nil {
			return i, err
		}
	}
	return n, nil
}

// taskAdvanceable 是否有已知埋点链路可推进。
func taskAdvanceable(t growthTask) bool {
	switch t.Code {
	case "black_cat", "Model_chat_GLM5.2", "chat_5", "first_buddy", "RichMeow_Chat",
		"template_5", "expert_5", "Expert_team_use_3", "Expert_lighthouse",
		"skill_1", "create_canvas", "automation_1", "Hp_Appearance",
		"Buddy_App", "Buddy_App_QQ", "playbook_prompt", "Library_read":
		return true
	}
	return false
}

func ternaryInt(cond bool, a, b int) int {
	if cond {
		return a
	}
	return b
}
