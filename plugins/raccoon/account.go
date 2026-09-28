// account.go — 账号档案与积分（user_info 身份 + points/v1/balance 分池明细）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// GetProfile 身份（昵称/手机号）+ 积分分池（各池独立来源，有效期与回补规则不同）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.AccountProfile{Healthy: false}, nil
	}
	prof := &pb.AccountProfile{
		DisplayName: shared_orDefault(c.Label, "raccoon"),
		Healthy:     true,
		Quota:       map[string]string{},
	}
	p.fetchUserInfo(ctx, c, prof)
	p.fetchQuota(ctx, c, prof)
	return prof, nil
}

// fetchUserInfo 拉身份（失败不影响额度读数；只用于展示）。
func (p *plugin) fetchUserInfo(ctx context.Context, c *credential, prof *pb.AccountProfile) {
	data, err := p.bizRequest(ctx, c, "GET", apiBase+authPrefix+"/user_info", nil)
	if err != nil {
		if be, ok := err.(*bizError); ok && (be.code == 401 || be.code == 403) {
			prof.Healthy = false
		}
		return
	}
	if name := strField(data, "name"); name != "" {
		// ⚠️ name 是服务端自动生成的默认名，多账号消歧靠 phone
		prof.DisplayName = name
	}
	if phone := strField(data, "phone"); phone != "" {
		prof.Quota["phone"] = phone
	}
	if id := strField(data, "id"); id != "" {
		prof.Quota["user_id"] = id
	}
}

// fetchQuota 积分分池（available_points 核心字段，缺它即响应形状不对）。
func (p *plugin) fetchQuota(ctx context.Context, c *credential, prof *pb.AccountProfile) {
	data, err := p.bizRequest(ctx, c, "GET", apiBase+pointsPrefix+"/balance", nil)
	if err != nil {
		if be, ok := err.(*bizError); ok && (be.code == 401 || be.code == 403) {
			prof.Healthy = false
		}
		return
	}
	available, ok := data["available_points"].(float64)
	if !ok {
		return
	}
	prof.Quota["credits"] = strconv.FormatFloat(available, 'f', -1, 64)
	var pkgs []map[string]string
	addPkg := func(name, key string, min float64) {
		if v, ok := data[key].(float64); ok && v > min {
			pkgs = append(pkgs, map[string]string{"label": name, "remaining": strconv.FormatFloat(v, 'f', -1, 64)})
		}
	}
	addPkg("奖励积分", "reward_points", 0)
	addPkg("每日积分", "daily_points", 0)
	addPkg("会员积分", "monthly_points", 0)
	addPkg("充值积分", "topup_points", 0)
	if len(pkgs) == 0 {
		pkgs = append(pkgs, map[string]string{"label": "可用积分", "remaining": strconv.FormatFloat(available, 'f', -1, 64)})
	}
	if b, err := json.Marshal(pkgs); err == nil {
		prof.CreditsJson = string(b)
	}
}

// shared_orDefault 空串回退（本地小助手，避免引 shared 包）。
func shared_orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// strField 从原始 JSON 字段表按名取字符串。
func strField(obj map[string]interface{}, key string) string {
	if obj == nil {
		return ""
	}
	s, _ := obj[key].(string)
	return s
}

// numField 兼容数字与数字字符串两种形态。
func numField(obj map[string]interface{}, key string) float64 {
	if obj == nil {
		return 0
	}
	switch v := obj[key].(type) {
	case float64:
		return v
	case string:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return 0
}

func mustJSON2(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

var _ = fmt.Sprintf
