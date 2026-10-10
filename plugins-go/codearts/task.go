// task.go — 任务能力：每日签到（ops/delivery + claim + confirm，HMAC 签名）。
//
// 前置：账户必须是积分计费模式（is_credit_package）；幂等靠活动列表
// claimable/status 预检（本协议无幂等键）；confirm 漏掉积分停待确认。
package codearts

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// claimedStatuses 活动 status 中「已领取/已确认/已核销」的取值。
var claimedStatuses = map[string]bool{"CLAIMED": true, "CONFIRMED": true, "CONSUMED": true}

func (p *plugin) ListTaskCapabilities(ctx context.Context, _ *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{
			{
				Id: "checkin", Label: map[string]string{"zh": "每日签到得积分", "en": "Daily Check-in"},
				Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:10",
			},
		},
	}, nil
}

// RunTask checkin 四步：账户类型 → 活动列表 → claim → confirm。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	if req.CapabilityId != "checkin" {
		return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
	}
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// 1. 账户类型（积分计费前置）
	obj, err := p.snapRequest(ctx, cred, "GET", snapBase+pathPackageInfo, nil, nil)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "账户信息查询失败："+err.Error())
	}
	data, err := unwrapSnapEnvelope(obj)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "账户信息查询失败："+err.Error())
	}
	pkg, _ := data["package"].(map[string]interface{})
	if pkg == nil {
		pkg = map[string]interface{}{}
	}
	if !boolField(pkg, "is_credit_package") {
		summary := "非积分计费账户，不在积分活动范围"
		if boolField(pkg, "is_token_package") {
			summary = "Token 计费账户，不在积分活动范围"
		}
		return &pb.RunTaskResponse{Summary: summary}, nil
	}

	// 2. 活动列表（找 USER_LOGIN 每日签到项）
	obj2, err := p.snapRequest(ctx, cred, "GET", snapBase+pathOpsDelivery+"?channel=IDE", nil, nil)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "活动列表查询失败："+err.Error())
	}
	data2, err := unwrapSnapEnvelope(obj2)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "活动列表查询失败："+err.Error())
	}
	items, _ := data2["items"].([]interface{})
	campaignID, actAmount, claimable, actStatus := "", 0.0, false, ""
	found := false
	for _, raw := range items {
		m, _ := raw.(map[string]interface{})
		if m == nil || strField(m, "type") != "USER_LOGIN" {
			continue
		}
		found = true
		// ⚠️ campaignId 服务端下发的是数字，须兼容数字/字符串
		if v, ok := m["campaignId"].(float64); ok {
			campaignID = fmt.Sprintf("%d", int64(v))
		} else {
			campaignID = strField(m, "campaignId")
		}
		claimable = boolField(m, "claimable")
		actStatus = strField(m, "status")
		// 可领积数字段名是 benefitAmount，不是 amount
		actAmount = numField(m, "benefitAmount")
		if actAmount == 0 {
			actAmount = numField(m, "amount")
		}
		break
	}
	if !found {
		return &pb.RunTaskResponse{Summary: "未找到每日签到活动"}, nil
	}
	if !claimable {
		if claimedStatuses[actStatus] {
			return &pb.RunTaskResponse{Summary: "今天已领取"}, nil
		}
		return &pb.RunTaskResponse{Summary: "当前不可领取（status=" + actStatus + "）"}, nil
	}
	if campaignID == "" {
		return nil, status.Error(codes.Internal, "活动缺少 campaignId，无法领取")
	}

	// 3. 领取
	claimBody := mustJSON(map[string]interface{}{"campaignId": campaignID, "channel": "IDE"})
	obj3, err := p.snapRequest(ctx, cred, "POST", snapBase+pathOpsClaim, claimBody, nil)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "领取失败："+err.Error())
	}
	data3, err := unwrapSnapEnvelope(obj3)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "领取失败："+err.Error())
	}

	// 4. 领取确认（响应 id 非 null 时补；confirm 失败不判整体失败，
	// 积分已进入待确认态，报 failed 会让用户重复点击）
	if data3["id"] != nil {
		confirmBody := mustJSON(map[string]interface{}{"campaignId": campaignID})
		_, _ = p.snapRequest(ctx, cred, "POST", snapBase+pathOpsConfirm, confirmBody, nil)
	}

	// 积分多级回退：领取响应 → 活动条目
	credit := numField(data3, "benefitAmount")
	if credit == 0 {
		credit = numField(data3, "credit")
	}
	if credit == 0 {
		credit = numField(data3, "credits")
	}
	if credit == 0 {
		credit = actAmount
	}
	summary := "签到成功"
	if credit > 0 {
		summary = fmt.Sprintf("签到成功，积分 +%s", fmt.Sprintf("%g", credit))
	}
	return &pb.RunTaskResponse{Summary: summary}, nil
}

var _ = json.Marshal
