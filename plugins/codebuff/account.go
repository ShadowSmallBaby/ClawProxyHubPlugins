// 账号档案：session 拉全貌、额度缓存与刷新。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if err := p.fetchProfile(ctx, c); err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: profileOf(c)}, nil
}

// GetProfile 账号全貌（GET /freebuff/session），失败降级为基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	if err := p.fetchProfile(ctx, c); err != nil {
		return profileOf(c), nil
	}
	return profileOf(c), nil
}

// fetchProfile 拉账号全貌写缓存：GET /freebuff/session（带 include-unused 头拿 rateLimits）。
func (p *plugin) fetchProfile(ctx context.Context, c *credential) error {
	headers := p.authHeaders(c)
	headers[hdrMultiSession] = "1"
	headers[hdrIncludeUnused] = "1"
	if c.InstanceID != "" {
		headers[hdrInstance] = c.InstanceID
	}
	resp, err := postJSON(ctx, p.hc(c), apiBase+"/freebuff/session", headers, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("token 已失效（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var s sessionResp
	if json.Unmarshal(raw, &s) != nil {
		return fmt.Errorf("invalid session response")
	}
	if s.Status != "" && s.Status != "none" {
		c.AccessTier = s.AccessTier
		if id := shared.OrDefault(s.InstanceID, s.InstanceIDAlt); id != "" {
			c.InstanceID = id
		}
	}
	if s.Freebucks.Daily != nil {
		c.FreebucksD = s.Freebucks.Daily.Limit
		c.Freebucks = s.Freebucks.Daily.Remaining
	}
	if s.Message != "" && s.Status == "disabled" {
		return fmt.Errorf("账号无可用免费额度（disabled）")
	}
	return nil
}

// sessionResp GET /freebuff/session 响应（字段多形状兼容）。
type sessionResp struct {
	Status        string `json:"status"`
	InstanceID    string `json:"instance_id"`
	InstanceIDAlt string `json:"instanceId"`
	AccessTier    string `json:"access_tier"`
	Message       string `json:"message"`
	Freebucks     struct {
		Daily *struct {
			Limit     float64 `json:"limit"`
			Spent     float64 `json:"spent"`
			Remaining float64 `json:"remaining"`
			ResetAt   string  `json:"resetAt"`
		} `json:"daily"`
	} `json:"freebucks"`
}

// profileOf 缓存 → 标准档案（remaining/total 是核心积分列标准键）。
func profileOf(c *credential) *pb.AccountProfile {
	quota := map[string]string{}
	if c.FreebucksD > 0 || c.Freebucks > 0 {
		quota["remaining"] = formatNum(c.Freebucks)
		quota["total"] = formatNum(c.FreebucksD)
		if b, err := json.Marshal(map[string]interface{}{
			"daily_remaining": c.Freebucks, "daily_limit": c.FreebucksD,
			"remaining": formatNum(c.Freebucks), "total": formatNum(c.FreebucksD),
		}); err == nil {
			return &pb.AccountProfile{
				DisplayName: credentialName(c), Healthy: true, Quota: quota,
				CreditsJson: string(b),
			}
		}
	}
	return &pb.AccountProfile{DisplayName: credentialName(c), Healthy: true, Quota: quota}
}

// formatNum 数字 → 精简字符串（去尾零）。
func formatNum(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}
