// profile.go — 账号档案与积分明细组装（签到/旅行/盲盒/成长/积分包动态块）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// profileSections 动态块组装（Refresh 与 GetProfile 共用）：签到 / 旅行 / 盲盒 / 成长计划 / 积分包。
func (p *plugin) profileSections(ctx context.Context, cred *credential, creditsJSON string) []*pb.ProfileSection {
	var secs []*pb.ProfileSection
	if sec := p.checkinSection(ctx, cred); sec != nil {
		secs = append(secs, sec)
	}
	if sec := p.travelSection(ctx, cred); sec != nil {
		secs = append(secs, sec)
	}
	if sec := p.blindboxSection(ctx, cred); sec != nil {
		secs = append(secs, sec)
	}
	if sec := p.growthSection(ctx, cred); sec != nil {
		secs = append(secs, sec)
	}
	if creditsJSON != "" {
		secs = append(secs, packagesSection(creditsJSON))
	}
	return secs
}

// GetProfile 积分明细（get-user-resource）：
// 总积分 = TotalDosage；已用/剩余 = 各积分包 Cycle* 累加（以上游给的数为准，不自己算差值）。
// 失败降级为基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{
		DisplayName: shared.OrDefault(cred.Account.Nickname, cred.Account.UID), Healthy: true, Quota: map[string]string{},
	}
	credits := p.fetchCredits(ctx, cred)
	if credits != "" {
		profile.CreditsJson = credits
		var c struct {
			Total     string `json:"total"`
			Used      string `json:"used"`
			Remaining string `json:"remaining"`
		}
		if json.Unmarshal([]byte(credits), &c) == nil {
			profile.Quota["total_credits"] = c.Total
			profile.Quota["used_credits"] = c.Used
			profile.Quota["credits"] = c.Remaining
		}
	}
	profile.Sections = append(profile.Sections, p.profileSections(ctx, cred, credits)...)
	return profile, nil
}

// growthSection 成长计划明细动态块：每条任务一行（状态徽章 + 进度 + 奖励），失败静默跳过。
func (p *plugin) growthSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	tasks, err := p.growthTasks(ctx, cred)
	if err != nil || len(tasks) == 0 {
		return nil
	}
	sec := &pb.ProfileSection{
		Id:    "growth_tasks",
		Title: map[string]string{"zh": "成长计划", "en": "Growth Plan"},
		Columns: []*pb.SectionColumn{
			{Key: "title", Title: map[string]string{"zh": "任务", "en": "Task"}},
			{Key: "status", Title: map[string]string{"zh": "状态", "en": "Status"}, Kind: "status"},
			{Key: "progress", Title: map[string]string{"zh": "进度", "en": "Progress"}},
			{Key: "reward", Title: map[string]string{"zh": "奖励", "en": "Reward"}},
		},
	}
	for _, t := range tasks {
		progress := "-"
		if t.Progress.Target > 0 {
			progress = fmt.Sprintf("%d / %d", t.Progress.Current, t.Progress.Target)
		}
		sec.Items = append(sec.Items, &pb.SectionRow{Cells: map[string]string{
			"title":    shared.OrDefault(t.Title, t.Code),
			"status":   "status:" + growthAcceptStatus(t.AcceptStatus),
			"progress": progress,
			"reward":   rewardText(t),
		}})
	}
	return sec
}

// growthAcceptStatus accept_status 五态 → 中文。
func growthAcceptStatus(s string) string {
	switch s {
	case "not_accepted":
		return "未接取"
	case "accepted":
		return "已接取"
	case "in_progress":
		return "进行中"
	case "completed":
		return "待领奖"
	case "claimed":
		return "已领奖"
	}
	return s
}

// rewardText 任务奖励文案（积分 / 能量 / 伙伴）。
func rewardText(t growthTask) string {
	parts := ""
	if t.RewardCredit > 0 {
		parts += fmt.Sprintf("积分+%d", t.RewardCredit)
	}
	if t.RewardEnergy > 0 {
		if parts != "" {
			parts += " "
		}
		parts += fmt.Sprintf("能量+%d", t.RewardEnergy)
	}
	if parts == "" {
		return "-"
	}
	return parts
}

// blindboxSection 盲盒情况动态块（能量余额 + 可开次数），失败静默跳过。
func (p *plugin) blindboxSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	data, err := p.actGet(ctx, cred, actEnergy, "盲盒能量")
	if err != nil {
		return nil
	}
	var energy struct {
		Balance int `json:"balance"`
	}
	_ = json.Unmarshal(data, &energy)
	openable := 0
	if q, err := p.actGet(ctx, cred, actQuota, "盲盒配额"); err == nil {
		var quota struct {
			Affordable int `json:"affordable"`
		}
		if json.Unmarshal(q, &quota) == nil {
			openable = quota.Affordable
		}
	}
	return &pb.ProfileSection{
		Id:    "blindbox",
		Title: map[string]string{"zh": "盲盒情况", "en": "Blind Box"},
		Entries: []*pb.SectionEntry{
			{Label: map[string]string{"zh": "能量余额", "en": "Energy"}, Value: fmt.Sprintf("%d", energy.Balance)},
			{Label: map[string]string{"zh": "可开次数", "en": "Openable"}, Value: fmt.Sprintf("%d", openable)},
		},
	}
}

// travelSection 猫猫旅行情况动态块（Buddy + 状态 + 奖励），失败静默跳过。
func (p *plugin) travelSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	data, err := p.actGet(ctx, cred, actTravelStat, "猫猫旅行状态")
	if err != nil {
		return nil
	}
	var st struct {
		State        string `json:"state"`
		BuddyID      int64  `json:"buddy_id"`
		ArriveAt     int64  `json:"arrive_at"`
		ServerNow    int64  `json:"server_now"`
		RewardCredit int    `json:"reward_credit"`
		Location     struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"location"`
	}
	if json.Unmarshal(data, &st) != nil {
		return nil
	}
	// 无猫（buddy_id=0 且 state 空）不渲染
	if st.BuddyID == 0 && (st.State == "" || st.State == "idle") {
		return nil
	}
	status := "空闲"
	if st.State == "traveling" {
		status = "在途"
		if st.ArriveAt > st.ServerNow {
			minutes := (st.ArriveAt - st.ServerNow + 59) / 60
			status = fmt.Sprintf("在途（约 %d 分钟后到达）", minutes)
		} else {
			status = "已到达，待领奖"
		}
	}
	entries := []*pb.SectionEntry{
		{Label: map[string]string{"zh": "状态", "en": "State"}, Value: "status:" + status, Kind: "status"},
	}
	if st.Location.Name != "" {
		entries = append(entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "目的地", "en": "Destination"}, Value: st.Location.Name,
		})
	}
	if st.RewardCredit > 0 {
		entries = append(entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "预计奖励", "en": "Reward"}, Value: fmt.Sprintf("积分+%d", st.RewardCredit),
		})
	}
	return &pb.ProfileSection{
		Id:      "travel",
		Title:   map[string]string{"zh": "旅行情况", "en": "Buddy Travel"},
		Entries: entries,
	}
}

// checkinSection 签到状态动态块（活动开启时才渲染；失败静默跳过）。
func (p *plugin) checkinSection(ctx context.Context, cred *credential) *pb.ProfileSection {
	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+"/v2/billing/meter/checkin-activity-status", p.headers(cred, true), map[string]interface{}{})
	if err != nil {
		return nil
	}
	data, err := envelope(resp)
	if err != nil {
		return nil
	}
	var st struct {
		Active          bool   `json:"active"`
		TodayCheckedIn  bool   `json:"today_checked_in"`
		StreakDays      int    `json:"streak_days"`
		DailyCredit     int    `json:"daily_credit"`
		WeekCheckinDays int    `json:"week_checkin_days"`
		EndAt           string `json:"end_at"`
	}
	if json.Unmarshal(data, &st) != nil || !st.Active {
		return nil
	}
	status := "已签到"
	if !st.TodayCheckedIn {
		status = "未签到"
	}
	sec := &pb.ProfileSection{
		Id:    "checkin",
		Title: map[string]string{"zh": "签到状态", "en": "Check-in"},
		Entries: []*pb.SectionEntry{
			{Label: map[string]string{"zh": "今日签到", "en": "Today"}, Value: "status:" + status, Kind: "status"},
			{Label: map[string]string{"zh": "连签天数", "en": "Streak"}, Value: fmt.Sprintf("%d 天", st.StreakDays)},
		},
	}
	if st.DailyCredit > 0 {
		sec.Entries = append(sec.Entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "每日积分", "en": "Daily Credit"}, Value: fmt.Sprintf("%d", st.DailyCredit),
		})
	}
	if st.WeekCheckinDays > 0 {
		sec.Entries = append(sec.Entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "本周签到", "en": "Week"}, Value: fmt.Sprintf("%d 天", st.WeekCheckinDays),
		})
	}
	if st.EndAt != "" {
		sec.Entries = append(sec.Entries, &pb.SectionEntry{
			Label: map[string]string{"zh": "活动截止", "en": "Ends"}, Value: st.EndAt,
		})
	}
	return sec
}

// packagesSection 积分包明细动态块：每个积分包一行（剩余/已用/到期 + 临期徽章）。
func packagesSection(creditsJSON string) *pb.ProfileSection {
	var c struct {
		Packages []struct {
			Total     string `json:"total"`
			Used      string `json:"used"`
			Remaining string `json:"remaining"`
			ExpiresAt string `json:"expiresAt"`
		} `json:"packages"`
	}
	if json.Unmarshal([]byte(creditsJSON), &c) != nil || len(c.Packages) == 0 {
		return nil
	}
	sec := &pb.ProfileSection{
		Id:    "packages",
		Title: map[string]string{"zh": "积分包", "en": "Credit Packages"},
		Columns: []*pb.SectionColumn{
			{Key: "remaining", Title: map[string]string{"zh": "剩余", "en": "Remaining"}},
			{Key: "used", Title: map[string]string{"zh": "已用", "en": "Used"}},
			{Key: "total", Title: map[string]string{"zh": "总积分", "en": "Total"}},
			{Key: "expiresAt", Title: map[string]string{"zh": "到期", "en": "Expires"}},
			{Key: "status", Title: map[string]string{"zh": "状态", "en": "Status"}, Kind: "status"},
		},
	}
	for _, pk := range c.Packages {
		sec.Items = append(sec.Items, &pb.SectionRow{Cells: map[string]string{
			"remaining": pk.Remaining,
			"used":      pk.Used,
			"total":     pk.Total,
			"expiresAt": pk.ExpiresAt,
			"status":    "status:" + packageExpiryStatus(pk.ExpiresAt),
		}})
	}
	return sec
}

// packageExpiryStatus 按到期时间判定：active / expiringSoon（7 天内）/ expired / unknown。
func packageExpiryStatus(expiresAt string) string {
	if expiresAt == "" {
		return "unknown"
	}
	t, err := time.Parse("2006-01-02 15:04:05", expiresAt)
	if err != nil {
		return "unknown"
	}
	// 上游时间按 UTC+8（与同条记录毫秒字段交叉验算确认）
	loc := time.FixedZone("CST", 8*3600)
	expires := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
	until := time.Until(expires)
	switch {
	case until <= 0:
		return "expired"
	case until <= 7*24*time.Hour:
		return "expiringSoon"
	default:
		return "active"
	}
}

// fetchCredits 查询积分明细并解析为 credits_json；失败返回空串（安全降级）。
func (p *plugin) fetchCredits(ctx context.Context, cred *credential) string {
	body := map[string]interface{}{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca",
		"Status":                     []int{0, 3},
		"PackageStartTimeRangeBegin": "2024-12-01 21:25:00",
		"PackageStartTimeRangeEnd":   time.Now().Format("2006-01-02 15:04:05"),
	}
	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+"/v2/billing/meter/get-user-resource", p.headers(cred, true), body)
	if err != nil {
		return ""
	}
	data, err := envelope(resp)
	if err != nil {
		return ""
	}
	var resource struct {
		Response struct {
			Data struct {
				TotalDosage json.Number `json:"TotalDosage"`
				Accounts    []struct {
					Used      string `json:"CycleCapacityUsedPrecise"`
					Total     string `json:"CycleCapacitySizePrecise"`
					Remaining string `json:"CycleCapacityRemainPrecise"`
					StartsAt  string `json:"CycleStartTime"`
					ExpiresAt string `json:"CycleEndTime"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if json.Unmarshal(data, &resource) != nil {
		return ""
	}
	d := resource.Response.Data
	if d.TotalDosage == "" && len(d.Accounts) == 0 {
		return ""
	}
	type pkg struct {
		Total     string `json:"total,omitempty"`
		Used      string `json:"used"`
		Remaining string `json:"remaining,omitempty"`
		ExpiresAt string `json:"expiresAt,omitempty"`
	}
	var packages []pkg
	usedSum, remainSum := 0.0, 0.0
	for _, a := range d.Accounts {
		pk := pkg{Used: a.Used}
		if a.Total != "" {
			pk.Total = a.Total
		}
		if a.Remaining != "" {
			pk.Remaining = a.Remaining
		}
		if a.ExpiresAt != "" {
			pk.ExpiresAt = a.ExpiresAt
		}
		if v, err := strconv.ParseFloat(a.Used, 64); err == nil {
			usedSum += v
		}
		if v, err := strconv.ParseFloat(a.Remaining, 64); err == nil {
			remainSum += v
		}
		packages = append(packages, pk)
	}
	out := map[string]interface{}{
		"total":     d.TotalDosage.String(),
		"used":      trimFloat(usedSum),
		"remaining": trimFloat(remainSum),
	}
	if len(packages) > 0 {
		out["packages"] = packages
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}
