// account.go — 账号档案与积分（刷新凭据、profile-summary/quota 双接口归一化、签到状态块）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if cred.RefreshToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "没有 refreshToken，请重新登录"}}, nil
	}
	body := withKeyfrom(map[string]interface{}{"refreshToken": cred.RefreshToken}, cred, p.clientVersion())
	resp, err := postJSON(ctx, p.hc(cred), serverBase+pathRefresh, map[string]string{"Content-Type": "application/json"}, body)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: err.Error()}}, nil
	}
	data, err := envelope(resp)
	if err != nil {
		code := int32(503)
		if _, isAuth := err.(*upstreamAuthError); isAuth {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	var refreshed struct {
		AccessToken  string          `json:"accessToken"`
		RefreshToken string          `json:"refreshToken"`
		ExpiresAt    float64         `json:"expiresAt"`
		User         json.RawMessage `json:"user"`
	}
	if err := json.Unmarshal(data, &refreshed); err != nil || refreshed.AccessToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 503, Message: "refresh 响应缺少 accessToken"}}, nil
	}
	if refreshed.RefreshToken != "" {
		cred.RefreshToken = refreshed.RefreshToken
	}
	if len(refreshed.User) > 0 {
		cred.User = refreshed.User
	}
	cred.AccessToken, cred.ExpiresAt = refreshed.AccessToken, refreshed.ExpiresAt
	blob, _ := json.Marshal(cred)
	profile := &pb.AccountProfile{
		DisplayName: credentialName(cred), Healthy: true, Quota: map[string]string{},
	}
	// 刷新成功后顺带拉积分与签到状态，避免快照缺块
	p.fetchQuota(ctx, cred, profile)
	if sec := p.checkinSection(ctx, cred); sec != nil {
		profile.Sections = append(profile.Sections, sec)
	}
	return &pb.RefreshResult{Blob: blob, Profile: profile}, nil
}

// GetProfile 真实积分余额（profile-summary），失败降级为基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{
		DisplayName: credentialName(cred), Healthy: true, Quota: map[string]string{},
	}
	p.fetchQuota(ctx, cred, profile)
	if sec := p.checkinSection(ctx, cred); sec != nil {
		profile.Sections = append(profile.Sections, sec)
	}
	return profile, nil
}

// checkinSection 签到状态动态块（活动开启时才渲染；失败静默跳过）。
// 复用 RunTask checkin 的前两步：slot 找活动 → context 查状态，不执行签到动作。
func (p *plugin) checkinSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	q := url.Values{
		"placement":           {"desktop_sidebar"},
		"clientVersion":       {p.clientVersion()},
		"containerApiVersion": {"2"},
		"platform":            {"win32"},
	}
	slotReq, _ := http.NewRequestWithContext(ctx, "GET", serverBase+"/api/client-activities/slot?"+q.Encode(), nil)
	for k, v := range p.authHeaders(cred) {
		slotReq.Header.Set(k, v)
	}
	slotReq.Header.Set("Cache-Control", "no-store")
	resp, err := p.hc(cred).Do(slotReq)
	if err != nil {
		return nil
	}
	slotData, err := envelope(resp)
	if err != nil {
		return nil
	}
	var slot struct {
		Activity struct {
			ActivityCode   string `json:"activityCode"`
			ConfigRevision int    `json:"configRevision"`
		} `json:"activity"`
		ActivityCode   string `json:"activityCode"`
		ConfigRevision int    `json:"configRevision"`
	}
	_ = json.Unmarshal(slotData, &slot)
	code, revision := slot.Activity.ActivityCode, slot.Activity.ConfigRevision
	if code == "" {
		code, revision = slot.ActivityCode, slot.ConfigRevision
	}
	if code == "" {
		return nil // 无活动
	}
	ctxReq, _ := http.NewRequestWithContext(ctx, "GET",
		serverBase+"/api/client-activities/"+code+"/context?configRevision="+fmt.Sprint(revision), nil)
	for k, v := range p.authHeaders(cred) {
		ctxReq.Header.Set(k, v)
	}
	ctxReq.Header.Set("Cache-Control", "no-store")
	resp2, err := p.hc(cred).Do(ctxReq)
	if err != nil {
		return nil
	}
	ctxData, err := envelope(resp2)
	if err != nil {
		return nil
	}
	var state struct {
		State struct {
			ClaimedToday bool `json:"claimedToday"`
			ClaimedDays  int  `json:"claimedDays"`
			// 兼容旧字段形状
			TodayCheckedIn bool `json:"todayCheckedIn"`
			StreakDays     int  `json:"streakDays"`
		} `json:"state"`
		ClaimedToday   bool `json:"claimedToday"`
		ClaimedDays    int  `json:"claimedDays"`
		TodayCheckedIn bool `json:"todayCheckedIn"`
		StreakDays     int  `json:"streakDays"`
	}
	if json.Unmarshal(ctxData, &state) != nil {
		return nil
	}
	checked := state.State.ClaimedToday || state.ClaimedToday || state.State.TodayCheckedIn || state.TodayCheckedIn
	streak := state.State.ClaimedDays
	if streak == 0 {
		streak = state.ClaimedDays
	}
	if streak == 0 {
		streak = state.State.StreakDays
	}
	if streak == 0 {
		streak = state.StreakDays
	}
	status := "已签到"
	if !checked {
		status = "未签到"
	}
	entries := []*pb.SectionEntry{
		{Label: map[string]string{"zh": "今日签到", "en": "Today"}, Value: "status:" + status, Kind: "status"},
	}
	if streak > 0 {
		entries = append(entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "连签天数", "en": "Streak"}, Value: fmt.Sprintf("%d 天", streak),
		})
	}
	return &pb.ProfileSection{
		Id:      "checkin",
		Title:   map[string]string{"zh": "签到情况", "en": "Check-in"},
		Entries: entries,
	}
}

// fetchQuota 拉取积分并写入标准键（数字字符串），失败静默降级。
// 两接口并发取数：/api/user/quota 给免费池上限/已用（多形状归一化），
// /api/user/profile-summary 补真实剩余与分池明细（creditItems）。
func (p *plugin) fetchQuota(ctx context.Context, cred *credential, profile *pb.AccountProfile) {
	quota := p.fetchQuotaPool(ctx, cred)
	summary := p.fetchQuotaSummary(ctx, cred)

	if summary == nil {
		summary = &quotaSummary{}
	}
	// 真实可用 = profile-summary 的 totalCreditsRemaining（主数字）。
	// quota 接口的数字只是免费池（limit/used），池子用尽或过期后恒为 0，
	// 不作为"总积分/已用"透出（会与真实可用自相矛盾），独立放 free_* 键。
	if summary.Remaining != 0 {
		profile.Quota["credits"] = trimFloat(summary.Remaining)
	}
	if len(summary.Packages) > 0 {
		profile.Quota["packages"] = fmt.Sprintf("%d", len(summary.Packages))
	}
	// credits_json：真实可用 + 积分包 + 免费池附注
	if len(summary.Packages) > 0 || summary.Remaining != 0 {
		out := map[string]interface{}{}
		if summary.Remaining != 0 {
			out["remaining"] = trimFloat(summary.Remaining)
		}
		if len(summary.Packages) > 0 {
			out["packages"] = summary.Packages
		}
		// 免费池附注（quota 接口数字）：上限同时作为列表列的"总积分"
		if quota.Total != 0 || quota.Used != 0 {
			out["free_limit"] = trimFloat(quota.Total)
			out["free_used"] = trimFloat(quota.Used)
			if quota.Total != 0 {
				out["total"] = trimFloat(quota.Total)
			}
		}
		if b, err := json.Marshal(out); err == nil {
			profile.CreditsJson = string(b)
		}
	}
	// 积分包明细动态块（分池 creditItems：剩余 + 到期），有包才定义
	if len(summary.Packages) > 0 {
		sec := &pb.ProfileSection{
			Id:    "packages",
			Title: map[string]string{"zh": "积分包", "en": "Credit Packages"},
			Columns: []*pb.SectionColumn{
				{Key: "remaining", Title: map[string]string{"zh": "剩余", "en": "Remaining"}},
				{Key: "label", Title: map[string]string{"zh": "积分包", "en": "Package"}},
				{Key: "expiresAt", Title: map[string]string{"zh": "到期", "en": "Expires"}},
			},
		}
		for _, pk := range summary.Packages {
			sec.Items = append(sec.Items, &pb.SectionRow{Cells: pk})
		}
		profile.Sections = append(profile.Sections, sec)
	}
}

// quotaSummary profile-summary 的解析结果（真实剩余 + 分池明细）。
type quotaSummary struct {
	Remaining float64
	Packages  []map[string]string
}

// fetchQuotaSummary GET /api/user/profile-summary → 真实剩余 + 分池明细；失败返回 nil。
func (p *plugin) fetchQuotaSummary(ctx context.Context, cred *credential) *quotaSummary {
	req, _ := http.NewRequestWithContext(ctx, "GET", serverBase+pathProfileSum, nil)
	for k, v := range p.authHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil
	}
	data, err := envelope(resp)
	if err != nil {
		return nil
	}
	var raw struct {
		// envelope 已解包 data 层，creditItems 就在根上；
		// 兼容上游又包一层 data 的形状
		Data struct {
			TotalCreditsRemaining float64 `json:"totalCreditsRemaining"`
			CreditItems           []struct {
				Type             string  `json:"type"`
				Label            string  `json:"label"`
				CreditsRemaining float64 `json:"creditsRemaining"`
				ExpiresAt        string  `json:"expiresAt"`
			} `json:"creditItems"`
		} `json:"data"`
		TotalCreditsRemaining float64 `json:"totalCreditsRemaining"`
		CreditItems           []struct {
			Type             string  `json:"type"`
			Label            string  `json:"label"`
			CreditsRemaining float64 `json:"creditsRemaining"`
			ExpiresAt        string  `json:"expiresAt"`
		} `json:"creditItems"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	s := &quotaSummary{}
	// 兼容平铺（envelope 已解包）与再包一层 data 两种形状（照 normalize_credit_summary）
	items := raw.CreditItems
	s.Remaining = raw.TotalCreditsRemaining
	if len(items) == 0 && s.Remaining == 0 {
		items = raw.Data.CreditItems
		s.Remaining = raw.Data.TotalCreditsRemaining
	}
	for _, it := range items {
		expiry := it.ExpiresAt
		if t, err := time.Parse(time.RFC3339, it.ExpiresAt); err == nil {
			expiry = t.Format("2006-01-02")
		}
		s.Packages = append(s.Packages, map[string]string{
			"remaining": trimFloat(it.CreditsRemaining),
			"label":     shared.OrDefault(it.Label, shared.OrDefault(it.Type, "积分包")),
			"expiresAt": expiry,
		})
	}
	return s
}

// quotaPool /api/user/quota 的总积分/已用（多形状字段归一化，照 _QUOTA_FIELD_SHAPES）。
type quotaPool struct {
	Total float64
	Used  float64
}

// fetchQuotaPool GET /api/user/quota → 归一化的总积分/已用；失败返回零值。
func (p *plugin) fetchQuotaPool(ctx context.Context, cred *credential) quotaPool {
	req, _ := http.NewRequestWithContext(ctx, "GET", serverBase+pathQuota, nil)
	for k, v := range p.authHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return quotaPool{}
	}
	data, err := envelope(resp)
	if err != nil {
		return quotaPool{}
	}
	// 兼容 data/quota 包一层与平铺（照 normalize_auth_quota）
	var outer map[string]json.RawMessage
	if json.Unmarshal(data, &outer) != nil {
		return quotaPool{}
	}
	body := data
	for _, key := range []string{"quota", "data"} {
		var wrapped map[string]json.RawMessage
		if json.Unmarshal(body, &wrapped) == nil {
			if inner, ok := wrapped[key]; ok {
				var probe map[string]json.RawMessage
				if json.Unmarshal(inner, &probe) == nil {
					body = inner
					break
				}
			}
		}
	}
	// 多形状字段对：服务端会下发多套字段，按顺序取第一组非零
	shapes := [][2]string{
		{"limit", "used"},
		{"freeCreditsTotal", "freeCreditsUsed"},
		{"monthlyCreditsLimit", "monthlyCreditsUsed"},
		{"dailyCreditsLimit", "dailyCreditsUsed"},
		{"creditsLimit", "creditsUsed"},
	}
	var out quotaPool
	for _, shape := range shapes {
		// data 里混有 bool/string 字段，逐字段用 json.Number 按名取，避免整体 unmarshal 失败
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			continue
		}
		total, used := numField(fields, shape[0]), numField(fields, shape[1])
		if total != 0 || used != 0 {
			out.Total, out.Used = total, used
			break
		}
	}
	return out
}

// numField 从原始 JSON 字段表里按名取数字（bool/string 忽略，取不到返回 0）。
func numField(fields map[string]json.RawMessage, name string) float64 {
	raw, ok := fields[name]
	if !ok {
		return 0
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		return 0
	}
	return n
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
