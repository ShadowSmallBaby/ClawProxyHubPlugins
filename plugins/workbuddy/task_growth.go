// task_growth.go — 成长任务：状态机推进（批量接取 → 埋点推进 → 领奖）。进度埋点见 task_progress.go。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// runGrowthTasks 状态机推进：批量接取 → 埋点推进 → 领奖。
// 每个阶段改变状态就重拉列表，让刚接取的任务本轮可推进、刚推进完成的本轮可领奖。
func (p *plugin) runGrowthTasks(ctx context.Context, cred *credential) (*pb.RunTaskResponse, error) {
	tasks, err := p.growthTasks(ctx, cred)
	if err != nil {
		return nil, err
	}

	// 1. 批量接取 single 型未接任务
	var toAccept []string
	for _, t := range tasks {
		if t.action() == "accept" {
			toAccept = append(toAccept, t.Code)
		}
	}
	if len(toAccept) > 0 {
		if _, err := p.actPost(ctx, cred, actTasksAccept, map[string]interface{}{"task_codes": toAccept}, "任务接取"); err != nil {
			return nil, err
		}
		tasks, err = p.growthTasks(ctx, cred)
		if err != nil {
			return nil, err
		}
	}

	// 2. 埋点推进（有已知链路的进行中任务）；单个失败只记录，不中断其它
	advanced, advanceFailed := 0, []string{}
	for _, t := range tasks {
		if t.action() != "advance" {
			continue
		}
		n, err := p.advanceTask(ctx, cred, t)
		if err != nil {
			advanceFailed = append(advanceFailed, shared.OrDefault(t.Title, t.Code))
			continue
		}
		advanced += n
	}
	if advanced > 0 {
		if tasks, err = p.growthTasks(ctx, cred); err != nil {
			return nil, err
		}
	}

	// 3. 领取已完成任务的奖励
	claimed, credit := 0, 0
	for _, t := range tasks {
		if t.action() != "claim" {
			continue
		}
		res, err := p.actPost(ctx, cred, "/activity/growth/tasks/"+t.Code+"/claim", map[string]interface{}{}, "任务领奖")
		if err != nil {
			continue // 单个失败不中断
		}
		var claim struct {
			Already bool `json:"already_claimed"`
			Credit  int  `json:"credit"`
		}
		_ = json.Unmarshal(res, &claim)
		if !claim.Already {
			claimed++
			credit += claim.Credit
		}
	}

	summary := fmt.Sprintf("接取 %d / 推进 %d / 领奖 %d（积分 +%d）", len(toAccept), advanced, claimed, credit)
	if len(advanceFailed) > 0 {
		summary += fmt.Sprintf("；%d 项推进失败", len(advanceFailed))
	}
	// 执行后的任务快照：结构化明细持久化到 task_runs，账号详情弹窗直接渲染
	detail := map[string]interface{}{"items": tasks}
	if b, err := json.Marshal(detail); err == nil {
		return &pb.RunTaskResponse{Summary: summary, DetailJson: string(b)}, nil
	}
	return &pb.RunTaskResponse{Summary: summary}, nil
}

type growthTask struct {
	Code         string `json:"task_code"`
	Title        string `json:"title"`
	TaskType     string `json:"task_type"`
	AcceptStatus string `json:"accept_status"`
	Locked       bool   `json:"locked"`
	RewardCredit int    `json:"reward_credit"`
	RewardEnergy int    `json:"reward_energy"`
	Progress     struct {
		Current int `json:"current"`
		Target  int `json:"target"`
	} `json:"progress"`
}

// action 下一步动作：accept / advance / claim / none。
func (t growthTask) action() string {
	switch {
	case t.AcceptStatus == "completed":
		return "claim"
	case t.AcceptStatus == "not_accepted" && t.TaskType == "single":
		return "accept"
	case (t.AcceptStatus == "accepted" || t.AcceptStatus == "in_progress") && !t.Locked && taskAdvanceable(t):
		return "advance"
	}
	return "none"
}

func (p *plugin) growthTasks(ctx context.Context, cred *credential) ([]growthTask, error) {
	data, err := p.actGet(ctx, cred, actTasksList, "成长任务")
	if err != nil {
		return nil, err
	}
	var list struct {
		Tasks []growthTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("成长任务解析失败: %w", err)
	}
	return list.Tasks, nil
}
