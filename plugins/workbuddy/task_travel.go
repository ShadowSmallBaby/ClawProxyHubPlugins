// task_travel.go — 猫猫旅行任务：领奖与出发同轮完成（到点领奖后立即改派）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// travelState 旅行状态快照。
type travelState struct {
	State        string `json:"state"`
	RecordID     int64  `json:"record_id"`
	BuddyID      int64  `json:"buddy_id"`
	ArriveAt     int64  `json:"arrive_at"`
	ServerNow    int64  `json:"server_now"`
	DailyLimited bool   `json:"daily_limit_reached"`
	Reward       int    `json:"reward_credit"`
}

// arrived 到点可领奖：显式 arrived，或 traveling 已过到达时刻。
func (st travelState) arrived() bool {
	return st.State == "arrived" || (st.State == "traveling" && st.ArriveAt > 0 && st.ServerNow >= st.ArriveAt)
}

// travelStatus 查询旅行状态。
func (p *plugin) travelStatus(ctx context.Context, cred *credential) (travelState, error) {
	var st travelState
	data, err := p.actGet(ctx, cred, actTravelStat, "猫猫旅行状态")
	if err != nil {
		return st, err
	}
	_ = json.Unmarshal(data, &st)
	return st, nil
}

// runTravel 领奖与出发同轮完成：到点先领奖再立即改派，避免定时器一天只推进一步而空耗一趟旅行。
func (p *plugin) runTravel(ctx context.Context, cred *credential) (*pb.RunTaskResponse, error) {
	st, err := p.travelStatus(ctx, cred)
	if err != nil {
		if isBuddyStateError(err) {
			return &pb.RunTaskResponse{Summary: "跳过：无可派出的 Buddy"}, nil
		}
		return nil, err
	}

	// 在途未到点：只报剩余时间
	if st.State == "traveling" && st.ArriveAt > st.ServerNow {
		minutes := (st.ArriveAt - st.ServerNow + 59) / 60
		return &pb.RunTaskResponse{Summary: fmt.Sprintf("跳过：在途，约 %d 分钟后到达", minutes)}, nil
	}

	// 到点：领奖后重查状态并立即再出发
	if st.arrived() {
		if st.RecordID == 0 {
			return &pb.RunTaskResponse{Summary: "跳过：缺少旅行记录，无法领奖"}, nil
		}
		res, err := p.actPost(ctx, cred, actTravelWin, map[string]interface{}{"record_id": st.RecordID}, "猫猫旅行领奖")
		if err != nil {
			return nil, err
		}
		var claim struct {
			Reward int `json:"reward_credit"`
		}
		_ = json.Unmarshal(res, &claim)
		claimMsg := fmt.Sprintf("旅行归来领奖，积分 +%d", claim.Reward)

		// 领奖已落地；再出发失败只降级为提示，不让整次任务失败而重复领奖
		next, err := p.travelStatus(ctx, cred)
		if err != nil {
			return &pb.RunTaskResponse{Summary: claimMsg + "；再出发状态查询失败，已跳过"}, nil
		}
		departMsg, derr := p.tryDepart(ctx, cred, next)
		if derr != nil {
			departMsg = "再出发失败：" + derr.Error()
		}
		return &pb.RunTaskResponse{Summary: claimMsg + "；" + departMsg}, nil
	}

	// 空闲：直接出发
	if st.State == "" || st.State == "idle" {
		msg, err := p.tryDepart(ctx, cred, st)
		if err != nil {
			return nil, err
		}
		return &pb.RunTaskResponse{Summary: msg}, nil
	}

	return &pb.RunTaskResponse{Summary: "跳过：旅行状态未知（" + st.State + "）"}, nil
}

// tryDepart 派出 Buddy 旅行（无猫先领养）；账号状态类阻碍返回原因摘要而非错误。
func (p *plugin) tryDepart(ctx context.Context, cred *credential, st travelState) (string, error) {
	if st.DailyLimited {
		return "今日旅行已达上限", nil
	}
	if err := p.ensureBuddy(ctx, cred, st.BuddyID); err != nil {
		if isBuddyStateError(err) {
			return "无可派出的 Buddy（领养门槛未达成）", nil
		}
		return "", err
	}
	locID, err := p.travelLocation(ctx, cred)
	if err != nil {
		return "", err
	}
	if _, err := p.actPost(ctx, cred, actTravelGo, map[string]interface{}{"location_id": locID}, "猫猫旅行出发"); err != nil {
		low := strings.ToLower(err.Error())
		if strings.Contains(low, "already traveling") {
			return "已在途（状态重查确认）", nil
		}
		if isBuddyStateError(err) {
			return "无可派出的 Buddy", nil
		}
		return "", err
	}
	return "已派出 Buddy 旅行", nil
}

// ensureBuddy 确保账号有可派出的 Buddy：协议幂等先调，无猫则领养。
func (p *plugin) ensureBuddy(ctx context.Context, cred *credential, buddyID int64) error {
	if _, err := p.actPost(ctx, cred, actBuddyAgree, map[string]interface{}{"agree": true}, "Buddy 协议"); err != nil {
		return err
	}
	if buddyID != 0 {
		return nil
	}
	data, err := p.actGet(ctx, cred, actBuddyInfo, "Buddy 档案")
	if err != nil {
		return err
	}
	var info struct {
		Buddy struct {
			ID int64 `json:"id"`
		} `json:"buddy"`
	}
	_ = json.Unmarshal(data, &info)
	if info.Buddy.ID != 0 {
		return nil
	}
	_, err = p.actPost(ctx, cred, actBuddyFirst, map[string]interface{}{}, "Buddy 领养")
	return err
}

// travelLocation 出发目的地：上游 config 排序第一，拿不到回退 1。
func (p *plugin) travelLocation(ctx context.Context, cred *credential) (int, error) {
	data, err := p.actGet(ctx, cred, actTravelCfg, "猫猫旅行配置")
	if err != nil {
		return 1, nil // 目的地解析失败不挡出发
	}
	var cfg struct {
		Locations []struct {
			ID   int `json:"id"`
			Sort int `json:"sort"`
		} `json:"locations"`
	}
	if json.Unmarshal(data, &cfg) != nil || len(cfg.Locations) == 0 {
		return 1, nil
	}
	return cfg.Locations[0].ID, nil
}
