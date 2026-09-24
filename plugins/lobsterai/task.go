// task.go — 任务能力：每日签到（slot 找活动 → context 查状态 → action 幂等签到）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (p *plugin) ListTaskCapabilities(ctx context.Context, _ *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{
			{
				Id: "checkin", Label: map[string]string{"zh": "每日签到", "en": "Daily Check-in"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:05",
			},
		},
	}, nil
}

// RunTask checkin 三步：slot 找活动 → context 查状态 → action 幂等签到。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	if req.CapabilityId != "checkin" {
		return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
	}
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// 1. 活动位
	q := url.Values{
		"placement":           {"desktop_sidebar"},
		"clientVersion":       {p.clientVersion()},
		"containerApiVersion": {"2"},
		"platform":            {"win32"},
	}
	slotReq, _ := http.NewRequestWithContext(ctx, "GET", serverBase+"/api/client-activities/slot?"+q.Encode(), nil)
	for k, v := range p.authHeaders(cred) {
		slotReq.Header.Set(k, v)
	}
	slotReq.Header.Set("Cache-Control", "no-store")
	resp, err := p.hc(cred).Do(slotReq)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	slotData, err := envelope(resp)
	if err != nil {
		return activityResult(err)
	}
	var slot struct {
		Activity struct {
			ActivityCode   string `json:"activityCode"`
			ConfigRevision int    `json:"configRevision"`
		} `json:"activity"`
		ActivityCode   string `json:"activityCode"`
		ConfigRevision int    `json:"configRevision"`
	}
	_ = json.Unmarshal(slotData, &slot)
	code, revision := slot.Activity.ActivityCode, slot.Activity.ConfigRevision
	if code == "" {
		code, revision = slot.ActivityCode, slot.ConfigRevision
	}
	if code == "" {
		return &pb.RunTaskResponse{Summary: "当前无签到活动"}, nil
	}

	// 2. 活动状态（今天签没签）
	ctxReq, _ := http.NewRequestWithContext(ctx, "GET",
		serverBase+"/api/client-activities/"+code+"/context?configRevision="+fmt.Sprint(revision), nil)
	for k, v := range p.authHeaders(cred) {
		ctxReq.Header.Set(k, v)
	}
	ctxReq.Header.Set("Cache-Control", "no-store")
	resp2, err := p.hc(cred).Do(ctxReq)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	ctxData, err := envelope(resp2)
	if err != nil {
		return activityResult(err)
	}
	var state struct {
		State struct {
			ClaimedToday bool `json:"claimedToday"`
			ClaimedDays  int  `json:"claimedDays"`
			// 兼容旧字段形状
			TodayCheckedIn bool `json:"todayCheckedIn"`
			StreakDays     int  `json:"streakDays"`
		} `json:"state"`
		ClaimedToday   bool `json:"claimedToday"`
		ClaimedDays    int  `json:"claimedDays"`
		TodayCheckedIn bool `json:"todayCheckedIn"`
		StreakDays     int  `json:"streakDays"`
	}
	_ = json.Unmarshal(ctxData, &state)
	checked := state.State.ClaimedToday || state.ClaimedToday || state.State.TodayCheckedIn || state.TodayCheckedIn
	if checked {
		streak := state.State.ClaimedDays
		if streak == 0 {
			streak = state.ClaimedDays
		}
		if streak == 0 {
			streak = state.State.StreakDays
		}
		if streak == 0 {
			streak = state.StreakDays
		}
		return &pb.RunTaskResponse{Summary: fmt.Sprintf("今日已签到（连签 %d 天）", streak)}, nil
	}

	// 3. 执行签到（幂等键防重复发积分）
	actionURL := fmt.Sprintf("%s/api/client-activities/%s/actions/check_in", serverBase, code)
	actionBody := map[string]interface{}{
		"configRevision": revision,
		"idempotencyKey": shared.RandUUID(),
		"payload":        map[string]interface{}{},
	}
	resp3, err := postJSON(ctx, p.hc(cred), actionURL, p.authHeaders(cred), actionBody)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	actionData, err := envelope(resp3)
	if err != nil {
		return activityResult(err)
	}
	var result struct {
		Result struct {
			Replayed bool `json:"replayed"`
			Rewards  []struct {
				Amount int    `json:"amount"`
				Type   string `json:"type"`
			} `json:"rewards"`
		} `json:"result"`
	}
	_ = json.Unmarshal(actionData, &result)
	summary := "签到成功"
	if len(result.Result.Rewards) > 0 {
		summary = fmt.Sprintf("签到成功，积分 +%d", result.Result.Rewards[0].Amount)
	}
	if result.Result.Replayed {
		summary += "（幂等重放，未重复发分）"
	}
	return &pb.RunTaskResponse{Summary: summary}, nil
}
