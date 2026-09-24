// task.go — 任务能力编排：声明能力 + 按 CapabilityId 分发（签到/盲盒/旅行/成长）。
package main

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (p *plugin) ListTaskCapabilities(ctx context.Context, _ *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{
			{
				Id: "checkin", Label: map[string]string{"zh": "每日签到", "en": "Daily Check-in"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:10",
			},
			{
				Id: "blindbox", Label: map[string]string{"zh": "开盲盒", "en": "Blind Box"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:20",
			},
			{
				Id: "travel", Label: map[string]string{"zh": "猫猫旅行", "en": "Buddy Travel"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:30",
			},
			{
				Id: "growth_tasks", Label: map[string]string{"zh": "成长任务", "en": "Growth Tasks"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:40",
			},
		},
	}, nil
}

// RunTask 按能力分发：签到 / 盲盒 / 旅行 / 成长任务（各任务实现见 task_*.go）。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	switch req.CapabilityId {
	case "checkin":
		return p.runCheckin(ctx, cred)
	case "blindbox":
		return p.runBlindbox(ctx, cred)
	case "travel":
		return p.runTravel(ctx, cred)
	case "growth_tasks":
		return p.runGrowthTasks(ctx, cred)
	}
	return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
}
