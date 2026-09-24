// account.go — 凭据 blob 解析、账号档案与计费余额快照。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

type credential struct {
	APIKey    string `json:"api_key"`
	ProjectID string `json:"project_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Email     string `json:"email,omitempty"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	c.APIKey = strings.TrimSpace(c.APIKey)
	if c.APIKey == "" {
		return nil, fmt.Errorf("credential missing api_key")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// displayName 账号展示名（邮箱 > api_key 缩写）。
func (c *credential) displayName() string {
	if c.Email != "" {
		return c.Email
	}
	return "todofor-" + shortID(c.APIKey)
}

// GetProfile 真实余额（/billing/usage），失败降级为基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	cli := newClient(p.baseURL(), c.APIKey, p.hc(c))
	profile := &pb.AccountProfile{DisplayName: c.displayName(), Healthy: true, Quota: map[string]string{}}
	p.fillBalance(ctx, cli, profile)
	return profile, nil
}

// fillBalance 拉计费余额写标准键（美元数字字符串），失败降级（原因进宿主日志）。
func (p *plugin) fillBalance(ctx context.Context, cli *client, profile *pb.AccountProfile) {
	usage, err := cli.billing(ctx)
	if err != nil {
		if p.host != nil {
			p.host.Log("warn", "todofor balance: /billing/usage failed: "+err.Error())
		}
		return
	}
	// remaining/total 是核心前端积分列的标准键（美元数字字符串）。
	profile.Quota["remaining"] = formatUSD(usage.TotalBalance)
	profile.Quota["total"] = formatUSD(usage.TotalBalance)
	if b, err := json.Marshal(map[string]interface{}{
		"totalBalance":        usage.TotalBalance,
		"manualBalance":       usage.ManualBalance,
		"subscriptionBalance": usage.SubscriptionBalance,
		"tier":                usage.Tier,
		"remaining":           formatUSD(usage.TotalBalance),
		"total":               formatUSD(usage.TotalBalance),
	}); err == nil {
		profile.CreditsJson = string(b)
	}
}
