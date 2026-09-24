// account.go — 登录校验（whoami + metering）/ Refresh / GetProfile。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// puterUser whoami 的非密身份。
type puterUser struct {
	UUID     string `json:"uuid"`
	Username string `json:"username"`
}

// puterUsage metering/usage 的月度配额。
type puterUsage struct {
	AllowanceInfo struct {
		Remaining           float64 `json:"remaining"`
		MonthUsageAllowance float64 `json:"monthUsageAllowance"`
	} `json:"allowanceInfo"`
}

// fetchUser 校验 grant（不跟随重定向，凭据不外漏）。
func (p *plugin) fetchUser(ctx context.Context, cred *credential) (*puterUser, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, puterWhoamiURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+cred.AuthToken)
	httpReq.Header.Set("Accept", "application/json")
	client := *p.hcFor(cred)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Puter identity request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Puter identity status=%d", resp.StatusCode)
	}
	var user puterUser
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&user); err != nil {
		return nil, fmt.Errorf("invalid Puter identity response")
	}
	user.UUID = strings.TrimSpace(user.UUID)
	user.Username = strings.TrimSpace(user.Username)
	if user.UUID == "" || user.Username == "" {
		return nil, fmt.Errorf("missing Puter identity")
	}
	return &user, nil
}

// fetchUsage 月度配额（登录校验 + GetProfile 用）。
func (p *plugin) fetchUsage(ctx context.Context, cred *credential) (*puterUsage, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, puterUsageURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.AuthToken)
	resp, err := p.hcFor(cred).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send puter usage request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := shared.ReadLimitedResp(resp, 8192)
		return nil, fmt.Errorf("puter usage API error: status=%d, body=%s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var usage puterUsage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&usage); err != nil {
		return nil, fmt.Errorf("failed to decode puter usage response: %w", err)
	}
	return &usage, nil
}

// ---------- Login / Refresh / GetProfile ----------

// Login 校验 popup grant：whoami + metering 双验证后建档。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	token := strings.TrimSpace(req.Form["auth_token"])
	if token == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 auth_token（浏览器 Puter 弹窗授权后获取）"}}, nil
	}
	c := &credential{AuthToken: token}
	user, err := p.fetchUser(ctx, c)
	if err != nil {
		// 原始上游错误可能带凭据，不回显
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "Puter 校验失败，请重新获取 popup 授权"}}, nil
	}
	usage, err := p.fetchUsage(ctx, c)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "Puter 登录成功但用量接口校验失败，账号未保存"}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{
		Blob:    blob,
		Profile: p.buildProfile(user.Username, usage),
	}, nil
}

// Refresh 静态令牌类：whoami 校验即可，无可刷新态原样返回不报错。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	user, err := p.fetchUser(ctx, c)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "auth_token 已失效，请重新授权"}}, nil
	}
	return &pb.RefreshResult{Profile: &pb.AccountProfile{DisplayName: user.Username, Healthy: true, Quota: map[string]string{}}}, nil
}

// GetProfile 资料与月度配额（余额折算标准键）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	user, err := p.fetchUser(ctx, c)
	if err != nil {
		return nil, err
	}
	usage, err := p.fetchUsage(ctx, c)
	if err != nil {
		return &pb.AccountProfile{DisplayName: user.Username, Healthy: true, Quota: map[string]string{}}, nil
	}
	return p.buildProfile(user.Username, usage), nil
}

// buildProfile 月度配额 → 标准键（美元）。
func (p *plugin) buildProfile(username string, usage *puterUsage) *pb.AccountProfile {
	profile := &pb.AccountProfile{DisplayName: username, Healthy: true, Quota: map[string]string{}}
	if usage == nil {
		return profile
	}
	total := usage.AllowanceInfo.MonthUsageAllowance
	remaining := usage.AllowanceInfo.Remaining
	if total > 0 {
		used := total - remaining
		if used < 0 {
			used = 0
		}
		profile.Quota["credits"] = fmt.Sprintf("%.2f", remaining)
		profile.Quota["total_credits"] = fmt.Sprintf("%.2f", total)
		profile.Quota["used_credits"] = fmt.Sprintf("%.2f", used)
	}
	return profile
}
