// account.go — 凭据（loomy_web_session Cookie）解析、账号档案与刷新。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential Loomy 会话凭据：单个 loomy_web_session Cookie（无 bearer / 无 CSRF）。
// Loomy 不签发 refresh token，Cookie 失效只能人工重登。
type credential struct {
	Cookie string `json:"cookie"` // 完整 Cookie 头，至少含 loomy_web_session
	Label  string `json:"label,omitempty"`

	proxyURL string `json:"-"`
}

// credFrom 解析凭据 blob（含代理配置），blob 需含非空 Cookie。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	if blob == nil || len(blob.GetBlob()) == 0 {
		return nil, fmt.Errorf("缺少 Loomy 凭据，请先登录")
	}
	c := &credential{}
	if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
		return nil, fmt.Errorf("凭据解析失败: %w", err)
	}
	if strings.TrimSpace(c.Cookie) == "" {
		return nil, fmt.Errorf("凭据缺少 loomy_web_session Cookie")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// normalizeCookie 规范化用户粘贴的 Cookie：只粘了 value 时补 loomy_web_session= 前缀。
func normalizeCookie(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "=") {
		return "loomy_web_session=" + s
	}
	return s
}

// GetProfile 拉取 points-summary（额度）+ me/profile（脱敏身份）；只读缓存不影响对话。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.AccountProfile{Healthy: false}, nil
	}
	prof := &pb.AccountProfile{
		DisplayName: shared.OrDefault(c.Label, "loomy"),
		Healthy:     true,
		Quota:       map[string]string{},
	}

	points, err := p.getJSON(ctx, c, apiPoints)
	if err != nil {
		// 只有真正的 401/403 才判失效；其余（网络/限流）保持 healthy。
		if isAuthErr(err) {
			prof.Healthy = false
		}
		return prof, nil
	}
	data := mapField(points, "data")
	permanent, okP := intField(data, "permanent")
	daily, okD := intField(data, "daily")
	if okP {
		prof.Quota["quota.credits"] = strconv.FormatInt(permanent, 10)
	}
	if okD {
		prof.Quota["quota.daily"] = strconv.FormatInt(daily, 10)
	}

	// 身份是装点信息，失败不影响额度读数。
	if me, err := p.getJSON(ctx, c, apiMe); err == nil {
		if phone := strField(mapField(me, "data"), "maskedPhone"); phone != "" {
			prof.Quota["quota.phone"] = phone
		}
	}
	if pf, err := p.getJSON(ctx, c, apiProfile); err == nil {
		if nick := strField(mapField(pf, "data"), "nickname"); nick != "" {
			prof.DisplayName = nick
		}
	}
	return prof, nil
}

// Refresh Loomy 无 refresh token：静态 Cookie 无可刷新态，原样返回不报错。
// Cookie 失效在对话时以 401 反馈，由核心提示重新登录。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	return &pb.RefreshResult{}, nil
}

// ---------- JSON 取值辅助 ----------

func mapField(obj map[string]interface{}, key string) map[string]interface{} {
	m, _ := obj[key].(map[string]interface{})
	return m
}

func strField(obj map[string]interface{}, key string) string {
	if obj == nil {
		return ""
	}
	s, _ := obj[key].(string)
	return s
}

// intField 兼容数字与数字字符串两种形态。
func intField(obj map[string]interface{}, key string) (int64, bool) {
	if obj == nil {
		return 0, false
	}
	switch v := obj[key].(type) {
	case float64:
		return int64(v), true
	case string:
		if v == "" {
			return 0, false
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}
