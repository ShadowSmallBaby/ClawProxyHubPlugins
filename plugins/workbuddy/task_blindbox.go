// task_blindbox.go — 开盲盒任务：查能量与配额，能量足够则一次开完。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// runBlindbox 查能量与配额，能量足够则一次开完，返回抽到的伙伴名。
func (p *plugin) runBlindbox(ctx context.Context, cred *credential) (*pb.RunTaskResponse, error) {
	data, err := p.actGet(ctx, cred, actEnergy, "盲盒能量")
	if err != nil {
		return nil, err
	}
	var energy struct {
		Balance int `json:"balance"`
	}
	_ = json.Unmarshal(data, &energy)

	draws := 0
	if q, err := p.actGet(ctx, cred, actQuota, "盲盒配额"); err == nil {
		var quota struct {
			Affordable int `json:"affordable"`
		}
		if json.Unmarshal(q, &quota) == nil {
			draws = quota.Affordable
		}
	}
	if draws <= 0 && energy.Balance >= 10 { // quota 拿不到时按能量本地推导（单次 10）
		draws = energy.Balance / 10
	}
	if draws <= 0 {
		return &pb.RunTaskResponse{Summary: fmt.Sprintf("跳过：能量不足（余额 %d）", energy.Balance)}, nil
	}

	res, err := p.actPost(ctx, cred, actBlindbox, map[string]interface{}{"count": draws}, "开盲盒")
	if err != nil {
		return nil, err
	}
	var opened struct {
		Count int `json:"count"`
		Items []struct {
			Template struct {
				Name string `json:"name"`
			} `json:"template"`
		} `json:"results"`
	}
	_ = json.Unmarshal(res, &opened)
	names := ""
	for i, item := range opened.Items {
		if i >= 5 {
			names += "…"
			break
		}
		if i > 0 {
			names += "、"
		}
		if item.Template.Name != "" {
			names += item.Template.Name
		} else {
			names += "未知伙伴"
		}
	}
	n := opened.Count
	if n == 0 {
		n = draws
	}
	return &pb.RunTaskResponse{Summary: fmt.Sprintf("开盲盒 ×%d：%s", n, names)}, nil
}
