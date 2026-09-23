// account.go — 登录校验 / 资料与余额 / 模型目录 / 每日签到。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// ---------- 登录 ----------

// Login 四种方式：api_key（必填 + 可选访问令牌）/ password（账号密码）/ cred_file（cookie + user_id）/ oauth（预留）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	site, err := p.site(req.InstanceId)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	switch req.MethodId {
	case "api_key":
		return p.loginAPIKey(ctx, req, site)
	case "password":
		user, pass := strings.TrimSpace(req.Form["username"]), req.Form["password"]
		if user == "" || pass == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写用户名与密码"}}, nil
		}
		auth, err := p.loginPassword(ctx, site, user, pass)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "登录失败: " + err.Error()}}, nil
		}
		return p.loginViaSession(ctx, site, req.InstanceId, auth, user, pass, strings.TrimSpace(req.Form["token_name"]))
	case "cred_file":
		auth, err := parseCredFile(req.Form["content"])
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
		}
		return p.loginViaSession(ctx, site, req.InstanceId, auth, "", "", strings.TrimSpace(req.Form["token_name"]))
	case "oauth":
		return &pb.LoginResult{Error: &pb.Error{Code: 501, Message: "OAuth（LinuxDo）登录尚未开放，请先用其他方式"}}, nil
	}
	return nil, status.Error(codes.NotFound, "unknown auth method: "+req.MethodId)
}

// loginAPIKey api_key 经 /v1/models 校验；access_token 可选，给出即拉 /api/user/self 校验并补 user_id。
func (p *plugin) loginAPIKey(ctx context.Context, req *pb.LoginRequest, site *siteConfig) (*pb.LoginResult, error) {
	cred := &credential{
		APIKey:      strings.TrimSpace(req.Form["api_key"]),
		AccessToken: strings.TrimSpace(req.Form["access_token"]),
		instanceID:  req.InstanceId,
	}
	if cred.APIKey == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 API 密钥"}}, nil
	}
	if uid := strings.TrimSpace(req.Form["user_id"]); uid != "" {
		cred.UserID, _ = strconv.Atoi(uid)
	}

	// 1. api_key 校验：能列模型即有效
	if _, err := p.fetchModels(ctx, cred, site); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "API 密钥校验失败: " + err.Error()}}, nil
	}
	profile := &pb.AccountProfile{DisplayName: "newapi-account", Healthy: true, Quota: map[string]string{}}

	// 2. 管理面（可选）：拉自身信息补展示名 / user_id / 余额
	if cred.hasManagement() {
		self, err := p.fetchSelf(ctx, cred, site)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "系统访问令牌校验失败: " + err.Error()}}, nil
		}
		if cred.UserID == 0 {
			cred.UserID = self.ID
		}
		p.fillProfile(ctx, cred, site, self, profile)
	}
	blob, _ := json.Marshal(cred)
	return &pb.LoginResult{Blob: blob, Profile: profile}, nil
}

// ---------- 资料 / 余额 ----------

// selfInfo GET /api/user/self 的关注字段。
type selfInfo struct {
	ID          int    `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Quota       int64  `json:"quota"`
	UsedQuota   int64  `json:"used_quota"`
	Group       string `json:"group"`
}

func (p *plugin) fetchSelf(ctx context.Context, cred *credential, site *siteConfig) (*selfInfo, error) {
	data, err := p.managementJSON(ctx, cred, site, "GET", "/api/user/self")
	if err != nil {
		return nil, err
	}
	var s selfInfo
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid /api/user/self response")
	}
	return &s, nil
}

// fillProfile 余额折算美元写标准键 + credits_json；签到状态动态块（失败静默）。
func (p *plugin) fillProfile(ctx context.Context, cred *credential, site *siteConfig, self *selfInfo, profile *pb.AccountProfile) {
	name := shared.OrDefault(self.DisplayName, self.Username)
	if name != "" {
		profile.DisplayName = name
	}
	unit := site.QuotaPerUnit
	remaining := float64(self.Quota) / unit
	used := float64(self.UsedQuota) / unit
	profile.Quota["credits"] = fmtUSD(remaining)
	profile.Quota["used_credits"] = fmtUSD(used)
	profile.Quota["total_credits"] = fmtUSD(remaining + used)
	if self.Group != "" {
		profile.Quota["group"] = self.Group
	}
	credits := map[string]string{
		"remaining": fmtUSD(remaining), "used": fmtUSD(used), "total": fmtUSD(remaining + used),
	}
	if b, err := json.Marshal(credits); err == nil {
		profile.CreditsJson = string(b)
	}
	if sec := p.checkinSection(ctx, cred, site); sec != nil {
		profile.Sections = append(profile.Sections, sec)
	}
}

func fmtUSD(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// Refresh 重拉资料与余额即"刷新"；凭据有变（会话续期 / 取得或更换 api_key）时回传新 Blob 持久化，否则 Blob 为空。
// 核心在对话 401 时也走这里：密钥被拒即重取一把（懒识别，平时不探活）。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	before, _ := json.Marshal(cred)
	oldKey := cred.APIKey
	profile, err := p.profileOf(ctx, cred)
	if err != nil {
		code := int32(503)
		if _, isAuth := err.(*authError); isAuth {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	res := &pb.RefreshResult{Profile: profile}
	if after, _ := json.Marshal(cred); string(after) != string(before) {
		res.Blob = after
	}
	switch {
	case oldKey == "" && cred.APIKey != "":
		res.Notification = &pb.TaskNotification{
			Title:   "New API · " + profile.DisplayName + " 已取得 API 密钥",
			Content: "刷新时已从站点取得 API 密钥明文，可到账号页启用调度并同步模型。",
			Level:   "info",
		}
	case oldKey != "" && cred.APIKey != oldKey:
		res.Notification = &pb.TaskNotification{
			Title:   "New API · " + profile.DisplayName + " API 密钥已更换",
			Content: "原密钥已失效，刷新时已自动切换到站点上另一把密钥；该密钥的模型 / 分组限制可能不同，请到账号页重新同步模型。",
			Level:   "warning",
		}
	}
	return res, nil
}

// GetProfile 有管理面凭据时拉余额与签到状态；否则仅校验 api_key 可用。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return p.profileOf(ctx, cred)
}

// profileOf 拉资料。api_key 模式只校验密钥；会话模式：密钥缺失或被网关拒绝时从站点重取一把
// （尊重指定的密钥名；取不到不算失败，healthy=false 让核心停用调度）。
func (p *plugin) profileOf(ctx context.Context, cred *credential) (*pb.AccountProfile, error) {
	site, err := p.site(cred.instanceID)
	if err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{DisplayName: "newapi-account", Healthy: true, Quota: map[string]string{}}
	_, keyErr := p.fetchModels(ctx, cred, site)
	if !cred.hasManagement() {
		if keyErr != nil {
			return nil, &authError{msg: "API 密钥校验失败: " + keyErr.Error()}
		}
		return profile, nil
	}
	self, err := p.fetchSelf(ctx, cred, site)
	if err != nil {
		return nil, err
	}
	if _, rejected := keyErr.(*authError); rejected {
		if key, kerr := p.pickOrCreateAPIKey(ctx, cred, site); key != "" {
			cred.APIKey = key
		} else {
			cred.APIKey = "" // 旧密钥已失效，清掉避免继续被调度
			profile.Healthy = false
			profile.Quota["api_key"] = "未取得: " + kerr.Error()
		}
	}
	p.fillProfile(ctx, cred, site, self, profile)
	return profile, nil
}

// ---------- 模型目录 ----------

// fetchModels GET /v1/models（无客户端上下文，固定 CLI 形态 modelsUA）；无密钥直接按 401 报。
func (p *plugin) fetchModels(ctx context.Context, cred *credential, site *siteConfig) ([]string, error) {
	if cred.APIKey == "" {
		return nil, &authError{msg: "该账号尚未取得 API 密钥（站点未返回明文），请刷新账号重试或改用「API 密钥」方式添加"}
	}
	resp, err := p.do(ctx, cred, "GET", site.BaseURL+"/v1/models", gatewayHeaders(cred, modelsUA), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, &authError{msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))}
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("invalid /v1/models response")
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// fetchPricingModels GET /api/pricing：站点定价表里的模型名（服务端已按用户可用分组过滤）。
// 无 api_key 或 /v1/models 被拒时的目录来源；管理面凭据可选（定价页多为公开）。
func (p *plugin) fetchPricingModels(ctx context.Context, cred *credential, site *siteConfig) ([]string, error) {
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": site.BrowserUA}
	if cred.hasManagement() {
		headers = managementHeaders(cred, site)
	}
	data, _, err := p.callJSON(ctx, cred, site, "GET", "/api/pricing", headers, nil)
	if err != nil {
		return nil, fmt.Errorf("/api/pricing: %w", err)
	}
	var items []struct {
		ModelName string `json:"model_name"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("invalid /api/pricing response")
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it.ModelName != "" && !seen[it.ModelName] {
			seen[it.ModelName] = true
			ids = append(ids, it.ModelName)
		}
	}
	return ids, nil
}

// ListModels 无凭据（核心刷新目录）时返回空；有 api_key 按 /v1/models（反映该密钥实际可用范围），
// 无密钥或被拒时回退 /api/pricing（站点定价表）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	if len(credBlob.GetBlob()) == 0 {
		return &pb.ModelList{}, nil
	}
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	site, err := p.site(cred.instanceID)
	if err != nil {
		return nil, err
	}
	ids, err := p.fetchModels(ctx, cred, site)
	if err != nil {
		pricing, perr := p.fetchPricingModels(ctx, cred, site)
		if perr != nil {
			return nil, fmt.Errorf("%v; %v", err, perr)
		}
		ids = pricing
	}
	models := make([]*pb.ModelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": id}, SupportsTools: true, SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: models}, nil
}

// ---------- 签到 ----------

// checkinStatus GET /api/user/checkin 的 data.stats（站点未开签到时返回 nil）。
type checkinStatus struct {
	Enabled bool `json:"enabled"`
	Stats   struct {
		CheckedInToday bool  `json:"checked_in_today"`
		CheckinCount   int   `json:"checkin_count"`
		TotalCheckins  int   `json:"total_checkins"`
		TotalQuota     int64 `json:"total_quota"`
	} `json:"stats"`
}

func (p *plugin) fetchCheckin(ctx context.Context, cred *credential, site *siteConfig) (*checkinStatus, error) {
	data, err := p.managementJSON(ctx, cred, site, "GET", "/api/user/checkin")
	if err != nil {
		return nil, err
	}
	var st checkinStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("invalid checkin response")
	}
	return &st, nil
}

// checkinSection 签到状态动态块（站点未开签到 / 请求失败静默不渲染）。
func (p *plugin) checkinSection(ctx context.Context, cred *credential, site *siteConfig) *pb.ProfileSection {
	st, err := p.fetchCheckin(ctx, cred, site)
	if err != nil || !st.Enabled {
		return nil
	}
	today := "未签到"
	if st.Stats.CheckedInToday {
		today = "已签到"
	}
	entries := []*pb.SectionEntry{
		{Label: map[string]string{"zh": "今日签到", "en": "Today"}, Value: "status:" + today, Kind: "status"},
		{Label: map[string]string{"zh": "本月签到", "en": "This month"}, Value: fmt.Sprintf("%d 次", st.Stats.CheckinCount)},
		{Label: map[string]string{"zh": "累计签到", "en": "Total"}, Value: fmt.Sprintf("%d 次", st.Stats.TotalCheckins)},
		{Label: map[string]string{"zh": "累计奖励", "en": "Total reward"}, Value: "$" + fmtUSD(float64(st.Stats.TotalQuota)/site.QuotaPerUnit)},
	}
	return &pb.ProfileSection{
		Id: "checkin", Title: map[string]string{"zh": "签到情况", "en": "Check-in"}, Entries: entries,
	}
}

// ListTaskCapabilities 按实例签到类型裁剪：instance_id>0 且 checkin_mode=none 时不声明 checkin（核心不建任务）。
func (p *plugin) ListTaskCapabilities(ctx context.Context, req *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	if id := req.GetInstanceId(); id > 0 {
		if site, err := p.site(id); err == nil && site.CheckinMode == checkinNone {
			return &pb.TaskCapabilities{}, nil
		}
	}
	return &pb.TaskCapabilities{
		Capabilities: []*pb.TaskCapability{{
			Id: "checkin", Label: map[string]string{"zh": "每日签到", "en": "Daily Check-in"},
			Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:05",
		}},
	}, nil
}

// RunTask checkin：按实例 checkin_mode 分派（api / login / refresh / site / none）。
func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	if req.CapabilityId != "checkin" {
		return nil, status.Error(codes.NotFound, "unknown capability: "+req.CapabilityId)
	}
	cred, err := credFrom(req.Credential)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	site, err := p.site(cred.instanceID)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	switch site.CheckinMode {
	case checkinNone:
		return &pb.RunTaskResponse{Summary: "实例签到类型为「无签到」，已跳过"}, nil
	case checkinSite:
		return p.checkinBySite(req, site), nil
	case checkinLogin:
		return p.checkinByLogin(ctx, cred, site)
	case checkinRefresh:
		return p.checkinByRefresh(ctx, cred, site)
	}
	return p.checkinByAPI(ctx, cred, site)
}

// checkinByAPI 通用签到 POST /api/user/checkin；「今日已签到」「签到功能未启用」按成功结果汇报。
func (p *plugin) checkinByAPI(ctx context.Context, cred *credential, site *siteConfig) (*pb.RunTaskResponse, error) {
	if !cred.hasManagement() {
		return p.checkinManualOnly(cred, site), nil
	}
	data, err := p.managementJSON(ctx, cred, site, "POST", "/api/user/checkin")
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			switch {
			case strings.Contains(ae.message, "已签到"):
				return &pb.RunTaskResponse{Summary: "今日已签到（上游确认）"}, nil
			case strings.Contains(ae.message, "未启用"):
				return &pb.RunTaskResponse{Summary: "站点未开启签到"}, nil
			}
		}
		return nil, taskErr(err)
	}
	var result struct {
		QuotaAwarded int64 `json:"quota_awarded"`
	}
	_ = json.Unmarshal(data, &result)
	return &pb.RunTaskResponse{
		Summary: fmt.Sprintf("签到成功，额度 +$%s", fmtUSD(float64(result.QuotaAwarded)/site.QuotaPerUnit)),
	}, nil
}

// checkinByLogin 登录即签到：用留存的账号密码重新登录（登录动作触发签到），再读余额汇报。
// 非账号密码授权的账号没有密码可用，直接失败并在摘要里说明。
func (p *plugin) checkinByLogin(ctx context.Context, cred *credential, site *siteConfig) (*pb.RunTaskResponse, error) {
	if cred.Username == "" || cred.Password == "" {
		return &pb.RunTaskResponse{Error: &pb.Error{Code: 412,
			Message: "实例签到类型为「登录即签到」，但该账号的授权方式（API 密钥 / 凭据文件）未留存账号密码，无法重新登录触发签到；请改用「账号密码」方式重新添加该账号"}}, nil
	}
	if err := p.relogin(ctx, cred, site); err != nil {
		return nil, taskErr(err)
	}
	self, err := p.fetchSelf(ctx, cred, site)
	if err != nil {
		return nil, taskErr(err)
	}
	blob, _ := json.Marshal(cred) // 会话已刷新，顺带持久化
	return &pb.RunTaskResponse{
		Changed: true, Blob: blob,
		Summary: fmt.Sprintf("登录签到完成，当前余额 $%s", fmtUSD(float64(self.Quota)/site.QuotaPerUnit)),
	}, nil
}

// checkinByRefresh 刷新即签到：请求 /api/user/self 即触发签到，再读一次比较额度变化。
func (p *plugin) checkinByRefresh(ctx context.Context, cred *credential, site *siteConfig) (*pb.RunTaskResponse, error) {
	if !cred.hasManagement() {
		return p.checkinManualOnly(cred, site), nil
	}
	before, err := p.fetchSelf(ctx, cred, site)
	if err != nil {
		return nil, taskErr(err)
	}
	after, err := p.fetchSelf(ctx, cred, site)
	if err != nil {
		return nil, taskErr(err)
	}
	delta := float64(after.Quota-before.Quota) / site.QuotaPerUnit
	balance := fmtUSD(float64(after.Quota) / site.QuotaPerUnit)
	if delta > 0 {
		return &pb.RunTaskResponse{Summary: fmt.Sprintf("刷新签到成功，额度 +$%s，当前余额 $%s", fmtUSD(delta), balance)}, nil
	}
	return &pb.RunTaskResponse{Summary: fmt.Sprintf("已刷新（额度无变化，可能今日已签到），当前余额 $%s", balance)}, nil
}

// checkinManualOnly 仅 API 密钥的账号无管理面凭据，无法自动签到：与「站点签到」同样只做摘要 + 站内提醒。
func (p *plugin) checkinManualOnly(cred *credential, site *siteConfig) *pb.RunTaskResponse {
	name := shared.OrDefault(site.InstanceName, site.BaseURL)
	target := shared.OrDefault(site.CheckinURL, site.BaseURL)
	return &pb.RunTaskResponse{
		Summary: "该账号仅配置了 API 密钥（无系统访问令牌 / 账号密码），无法自动签到，请前往站点手动签到：" + target,
		Notification: &pb.TaskNotification{
			Title:   fmt.Sprintf("New API · %s 该签到了（需手动）", name),
			Content: fmt.Sprintf("账号 #%s 仅有 API 密钥，无法自动签到；请前往 %s 手动签到，或补填系统访问令牌 / 改用账号密码添加以启用自动签到", cred.accountID, target),
			Level:   "warning",
		},
	}
}

// checkinBySite 站点签到：无法自动完成，摘要提示地址 + 站内通知提醒。
func (p *plugin) checkinBySite(req *pb.RunTaskRequest, site *siteConfig) *pb.RunTaskResponse {
	target := shared.OrDefault(site.CheckinURL, "（实例未填写签到站地址）")
	name := shared.OrDefault(site.InstanceName, site.BaseURL)
	return &pb.RunTaskResponse{
		Summary: "站点签到需人工完成，请前往签到站签到：" + target,
		Notification: &pb.TaskNotification{
			Title:   fmt.Sprintf("New API · %s 该签到了", name),
			Content: fmt.Sprintf("账号 #%s 请前往 %s 完成签到", req.Credential.GetAccountId(), target),
			Level:   "warning",
		},
	}
}

// taskErr 管理面错误 → gRPC 状态：鉴权失败 Unauthenticated，其余 Unavailable。
func taskErr(err error) error {
	if _, isAuth := err.(*authError); isAuth {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}
