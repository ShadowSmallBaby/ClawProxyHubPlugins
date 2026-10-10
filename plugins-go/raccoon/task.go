// task.go — 任务能力：桌面端登录奖励（desktop/v1/login/points/grant）。
//
// ⚠️ 不是每日签到：幂等一次性（已领过返回 granted:false，每号一次）；
// 每日积分是服务端按日自动发放，没有端点，不要实现成签到按钮。
package raccoon

import (
	"context"
	"encoding/json"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// loginRewardPoints 登录奖励默认额度（服务端未给出时兜底）。
const loginRewardPoints = 3000

func (p *plugin) ListTaskCapabilities(ctx context.Context, _ *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{
			{
				Id: "login_reward", Label: map[string]string{"zh": "领取桌面端登录奖励", "en": "Claim Login Reward"},
				Kind: "once", PerAccount: true, DefaultSchedule: "manual",
			},
		},
	}, nil
}

// RunTask login_reward：POST grant，幂等判据 granted。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	if req.CapabilityId != "login_reward" {
		return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
	}
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	data, err := p.bizRequest(ctx, cred, "POST", apiBase+desktopPfx+"/login/points/grant", nil)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "领取登录奖励失败："+err.Error())
	}
	if data["granted"] != true {
		return &pb.RunTaskResponse{Summary: "该账号已领取过桌面端登录奖励（每号一次）"}, nil
	}
	points := loginRewardPoints
	if popup, ok := data["popup"].(map[string]interface{}); ok {
		if v, ok := popup["points"].(float64); ok && v > 0 {
			points = int(v)
		}
	}
	return &pb.RunTaskResponse{Summary: "登录奖励领取成功，积分 +" + itoa(points)}, nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
