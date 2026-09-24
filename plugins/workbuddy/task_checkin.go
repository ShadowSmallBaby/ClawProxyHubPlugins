// task_checkin.go — 每日签到任务：查活动状态，未签则 POST daily-checkin。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// runCheckin 每日签到：先查活动状态，未签则 POST daily-checkin。
func (p *plugin) runCheckin(ctx context.Context, cred *credential) (*pb.RunTaskResponse, error) {
	// 1. 活动状态
	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+"/v2/billing/meter/checkin-activity-status",
		p.headers(cred, true), map[string]interface{}{})
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	data, err := envelope(resp)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var st struct {
		Active         bool `json:"active"`
		TodayCheckedIn bool `json:"today_checked_in"`
		StreakDays     int  `json:"streak_days"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, status.Error(codes.Internal, "status parse: "+err.Error())
	}
	if !st.Active {
		return &pb.RunTaskResponse{Summary: "当前无签到活动"}, nil
	}
	if st.TodayCheckedIn {
		return &pb.RunTaskResponse{Summary: fmt.Sprintf("今日已签到（连签 %d 天）", st.StreakDays)}, nil
	}

	// 2. 执行签到
	resp2, err := postJSON(ctx, p.hc(cred), upstreamBase+"/v2/billing/meter/daily-checkin",
		p.headers(cred, true), map[string]interface{}{})
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	data2, err := envelope(resp2)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var result struct {
		Credit      int  `json:"credit"`
		StreakDays  int  `json:"streak_days"`
		IsStreakDay bool `json:"is_streak_day"`
	}
	_ = json.Unmarshal(data2, &result)
	return &pb.RunTaskResponse{
		Summary: fmt.Sprintf("签到成功，积分 +%d（连签 %d 天）", result.Credit, result.StreakDays),
	}, nil
}
