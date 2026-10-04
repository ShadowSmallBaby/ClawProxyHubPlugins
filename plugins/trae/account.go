// account.go — 凭据解析、账号档案与刷新（ExchangeToken 轮换）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/proto"
	"net/http"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential TRAE 会话凭据：Cloud-IDE-JWT token + 设备指纹。
// machine_id 登录后绝不变（换机器触发风控）；refresh_token ExchangeToken 轮换。
type credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"` // 毫秒时间戳字符串
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	Phone        string `json:"phone,omitempty"` // 脱敏手机号（多账号消歧最有效）
	Email        string `json:"email,omitempty"` // 脱敏邮箱（短信登录账号为空）
	MachineID    string `json:"machine_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Label        string `json:"label,omitempty"`

	proxyURL string `json:"-"`
}

// credFrom 解析凭据 blob（含代理配置），blob 需含非空 access_token。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	if blob == nil || len(blob.GetBlob()) == 0 {
		return nil, fmt.Errorf("缺少 TRAE 凭据，请先登录")
	}
	c := &credential{}
	if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
		return nil, fmt.Errorf("凭据解析失败: %w", err)
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return nil, fmt.Errorf("凭据缺少 access_token")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// displayName 展示名：手机号优先 → 邮箱 → ScreenName → uid。
// ScreenName 是 passport 按 uid 自动生成的默认名（用户268…形态雷同），不能直接用。
func (c *credential) displayName() string {
	if c.Phone != "" {
		return c.Phone
	}
	if c.Email != "" {
		return c.Email
	}
	if c.Nickname != "" {
		return c.Nickname
	}
	return shared.OrDefault(c.UID, "trae")
}

// GetUserInfo 补齐 uid/昵称/脱敏手机号（POST，X-Cloudide-Token 头）。
func (p *plugin) getUserInfo(ctx context.Context, c *credential) {
	body, _ := json.Marshal(map[string]interface{}{"ReqSource": "IDE", "IDEVersion": ideVersion})
	req, err := httpNewReq(ctx, "POST", oauthHost+pathUserInfo, body)
	if err != nil {
		return
	}
	for k, v := range oauthHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Cloudide-Token", c.AccessToken)
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	result, ok := resultField(readResp(resp))
	if !ok {
		return
	}
	uid := strOf(result, "UserID")
	if uid == "" {
		uid = strOf(result, "userId")
	}
	if uid == "" {
		uid = strOf(result, "uid")
	}
	if uid != "" && c.UID == "" {
		c.UID = uid
	}
	// ⚠️ 字段名是 NonPlainTextMobile / NonPlainTextEmail（不是 Mobile/Phone/Email）
	if phone := strOf(result, "NonPlainTextMobile"); phone != "" {
		c.Phone = phone
	}
	if email := strOf(result, "NonPlainTextEmail"); email != "" {
		c.Email = email
	}
	if sn := strOf(result, "ScreenName"); sn != "" && c.Nickname == "" {
		c.Nickname = sn
	}
}

// GetProfile 用户信息 + 签到状态 + 积分余额。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.AccountProfile{Healthy: false}, nil
	}
	p.getUserInfo(ctx, c)
	prof := &pb.AccountProfile{
		DisplayName: c.displayName(),
		Healthy:     true,
		Quota:       map[string]string{},
	}
	if c.UID != "" {
		prof.Quota["uid"] = c.UID
	}
	// 签到状态（credits 是签到奖励额度）
	if data := p.postUG(ctx, c, pathChkStatus, "{}"); data != nil {
		prof.Quota["checkin_credits"] = fmt.Sprintf("%g", numOf(data, "credits"))
		if data["checked_in"] == true {
			prof.Quota["checked_in"] = "true"
		}
	} // 积分余额：remain = ∑(credits_limit - credits_amount)
	if packs := p.fetchCredits(ctx, c); len(packs) > 0 {
		if b, err := json.Marshal(packs); err == nil {
			prof.CreditsJson = string(b)
		}
	}
	return prof, nil
}

// fetchCredits 积分余额（ide_user_ent_usage，require_usage: true）。
func (p *plugin) fetchCredits(ctx context.Context, c *credential) []map[string]string {
	body, _ := json.Marshal(map[string]interface{}{"require_usage": true, "req_source": 2})
	req, err := httpNewReq(ctx, "POST", ugHost+pathEntUsage, body)
	if err != nil {
		return nil
	}
	for k, v := range p.checkinHeaders(c) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var obj struct {
		UserEntitlementPackList []struct {
			CreditsLimit  float64 `json:"credits_limit"`
			CreditsAmount float64 `json:"credits_amount"`
			PackName      string  `json:"pack_name"`
		} `json:"user_entitlement_pack_list"`
	}
	if json.Unmarshal(readResp(resp), &obj) != nil {
		return nil
	}
	var packs []map[string]string
	for _, pk := range obj.UserEntitlementPackList {
		remain := pk.CreditsLimit - pk.CreditsAmount
		if remain <= 0 {
			continue
		}
		packs = append(packs, map[string]string{
			"label":     shared.OrDefault(pk.PackName, "积分包"),
			"remaining": fmt.Sprintf("%g", remain),
		})
	}
	return packs
}

// Refresh ExchangeToken 换新凭据：access + refreshToken 都轮换，旧 refresh 即刻失效。
// 保留全部身份字段（machine_id/device_id/uid/nickname/enterprise_id）。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if c.RefreshToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "没有 refresh_token，请重新登录"}}, nil
	}
	body, _ := json.Marshal(map[string]interface{}{
		"ClientID": clientID, "RefreshToken": c.RefreshToken, "ClientSecret": "-", "UserID": "",
	})
	req, err := httpNewReq(ctx, "POST", oauthHost+pathExchange, body)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: err.Error()}}, nil
	}
	for k, v := range oauthHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: err.Error()}}, nil
	}
	defer resp.Body.Close()
	raw := readResp(resp)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "登录态已过期，请重新登录"}}, nil
	}
	result, ok := resultField(raw)
	if !ok {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: "ExchangeToken 响应解析失败"}}, nil
	}
	token := strOf(result, "Token")
	if token == "" {
		token = strOf(result, "token")
	}
	if token == "" {
		token = strOf(result, "accessToken")
	}
	if token == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: "refresh_failed: no token in response — re-login required"}}, nil
	}
	c.AccessToken = token
	if rt := strOf(result, "RefreshToken"); rt != "" {
		c.RefreshToken = rt
	}
	// tokenExpireAt：>1e12 毫秒 / 秒级 → 毫秒字符串；TokenExpireDuration 相对秒兜底
	if exp := numOf(result, "TokenExpireAt"); exp > 1e12 {
		c.ExpiresAt = fmt.Sprintf("%.0f", exp)
	} else if exp > 0 {
		c.ExpiresAt = fmt.Sprintf("%.0f", exp*1000)
	} else if d := numOf(result, "TokenExpireDuration"); d > 0 {
		c.ExpiresAt = fmt.Sprintf("%d", time.Now().UnixMilli()+int64(d*1000))
	}
	blob, _ := json.Marshal(c)
	prof, _ := p.GetProfile(ctx, updatedCredential(credBlob, blob))
	return &pb.RefreshResult{Blob: blob, Profile: prof}, nil
}

// httpNewReq 构造带 JSON body 的请求。
func httpNewReq(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
}

func updatedCredential(original *pb.CredentialBlob, blob []byte) *pb.CredentialBlob {
	c := proto.Clone(original).(*pb.CredentialBlob)
	c.Blob = blob
	return c
}
