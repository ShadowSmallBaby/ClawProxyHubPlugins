// task.go — 任务能力：每日赠送额度签到（POST /web/api/points/first-login，幂等）。
//
// 官方在登录成功后调用 first-login 发放每日赠送池，重复调用返回
// alreadyProcessed=true（HTTP 200），故幂等。语义是「触发每日额度重置」
// 而非「+积分」：dailyBalance = dailyQuota - dailyConsumed，消耗后不回补。
// dailyQuota 仅 first-login 响应携带，未签到时不要硬编码。
package loomy

import (
	"context"
	"fmt"
	"strconv"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (p *plugin) ListTaskCapabilities(ctx context.Context, _ *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{
			{
				Id: "daily_quota", Label: map[string]string{"zh": "每日额度签到", "en": "Daily Quota"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 08:05",
			},
		},
	}, nil
}

// RunTask daily_quota：POST first-login 触发每日额度，alreadyProcessed 即今日已领。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	if req.CapabilityId != "daily_quota" {
		return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
	}
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	obj, err := p.postJSON(ctx, cred, "/web/api/points/first-login", map[string]interface{}{})
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	data := mapField(obj, "data")
	if data == nil {
		data = obj
	}
	if code := strField(obj, "code"); code != "" && code != "000000" {
		return &pb.RunTaskResponse{Summary: "签到失败：" + strField(obj, "message")}, nil
	}
	dailyQuota, hasQuota := intField(data, "dailyQuota")
	dailyBalance, _ := intField(data, "dailyBalance")
	if data["alreadyProcessed"] == true {
		if hasQuota {
			return &pb.RunTaskResponse{Summary: fmt.Sprintf(
				"今日额度已初始化（每日 %s/%s）", strconv.FormatInt(dailyBalance, 10), strconv.FormatInt(dailyQuota, 10))}, nil
		}
		return &pb.RunTaskResponse{Summary: "今日额度已初始化"}, nil
	}
	// 首次处理：本次新发额度 = dailyQuota - dailyConsumed；缺字段时回退 dailyQuota
	granted := dailyQuota
	if consumed, ok := intField(data, "dailyConsumed"); ok && hasQuota {
		g := dailyQuota - consumed
		if g < 0 {
			g = 0
		}
		granted = g
	}
	return &pb.RunTaskResponse{Summary: fmt.Sprintf("每日额度已发放 +%s（消耗后不回补）", strconv.FormatInt(granted, 10))}, nil
}
