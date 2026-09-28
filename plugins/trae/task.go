// task.go — 任务能力：每日签到（checkin_credits/status + claim，完整签到头）。
//
// ⚠️ claim 响应不含积分数，真实数值只在 status 端点的 credits 字里；
// 设备身份基于 uid 确定性派生（每账号独立稳定，规避设备级限流）。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (p *plugin) ListTaskCapabilities(ctx context.Context, _ *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{
			{
				Id: "checkin", Label: map[string]string{"zh": "每日签到", "en": "Daily Check-in"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 08:05",
			},
		},
	}, nil
}

// RunTask checkin：status 查状态 → claim 领取 → status 补查积分。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	if req.CapabilityId != "checkin" {
		return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
	}
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if cred.UID == "" {
		// 签到设备身份派生依赖 uid（缺失会报 9004），先补
		p.getUserInfo(ctx, cred)
		if cred.UID == "" {
			return nil, status.Error(codes.InvalidArgument, "缺少 uid，无法构造签到设备身份，请重新登录或导入 uid")
		}
	}

	// 1. 状态查询（今天签没签）
	data := p.postUG(ctx, cred, pathChkStatus, "{}")
	if data != nil && data["checked_in"] == true {
		return &pb.RunTaskResponse{Summary: fmt.Sprintf(
			"今日已签到（连签 %d 天）", int(numOf(data, "streak_days")))}, nil
	}

	// 2. 领取（body 为 {}，非 {"req_source":2}）
	resp, err := p.postUGRaw(ctx, cred, pathChkClaim, "{}")
	if err != nil {
		return nil, status.Error(codes.Unavailable, "签到请求失败："+err.Error())
	}
	var claim struct {
		Code    float64 `json:"code"`
		Message string  `json:"message"`
		Msg     string  `json:"msg"`
	}
	_ = json.Unmarshal(resp, &claim)
	if int(claim.Code) == 9074 {
		return nil, status.Error(codes.ResourceExhausted, "签到人数过多，请稍后再试")
	}
	if claim.Code != 0 {
		msg := claim.Message
		if msg == "" {
			msg = claim.Msg
		}
		if msg == "" {
			msg = fmt.Sprintf("签到失败（code=%d）", int(claim.Code))
		}
		return nil, status.Error(codes.Internal, msg)
	}

	// 3. ⚠️ claim 响应不含积分数：补查 status 拿真实 credits
	credit, streak := 0.0, 0
	if data := p.postUG(ctx, cred, pathChkStatus, "{}"); data != nil {
		credit = numOf(data, "credits")
		streak = int(numOf(data, "streak_days"))
	}
	summary := "签到成功"
	if credit > 0 {
		summary = fmt.Sprintf("签到成功，积分 +%g（连签 %d 天）", credit, streak)
	}
	return &pb.RunTaskResponse{Summary: summary}, nil
}

// postUGRaw Ug 信道 POST，返回原始 body（业务失败也返回，由调用方分类）。
func (p *plugin) postUGRaw(ctx context.Context, cred *credential, path, body string) ([]byte, error) {
	req, err := httpNewReq(ctx, "POST", ugHost+path, []byte(body))
	if err != nil {
		return nil, err
	}
	for k, v := range p.checkinHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readResp(resp), nil
}

var _ = json.Marshal
