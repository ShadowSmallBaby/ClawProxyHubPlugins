// account.go — 账号档案与真实余额（/users/<id>/balance，microUSD 折算美元）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// GetProfile 真实余额（/users/<id>/balance），失败降级为基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{DisplayName: credentialName(c), Healthy: true, Quota: map[string]string{}}
	p.fetchBalance(ctx, c, profile)
	return profile, nil
}

// fetchBalance 拉真实余额写标准键（美元数字字符串），失败降级为基本档案（原因进宿主日志）。
// /users/me 补 userId，/users/<id>/balance 给余额（照官方 ClineAccountBalance）。
func (p *plugin) fetchBalance(ctx context.Context, c *credential, profile *pb.AccountProfile) {
	if err := p.ensureToken(ctx, c); err != nil {
		p.host.Log("warn", "cline balance: token not ready: "+err.Error())
		return
	}
	userID := c.UserID
	if userID == "" {
		me, err := p.accountJSON(ctx, c, "GET", "/users/me", nil)
		if err != nil {
			p.host.Log("warn", "cline balance: /users/me failed: "+err.Error())
			return
		}
		userID = rawString(me["id"])
		c.UserID = userID // 缓存到凭据（随 Refresh 回传 Blob 持久化）
	}
	if userID == "" {
		p.host.Log("warn", "cline balance: /users/me returned no id")
		return
	}
	data, err := p.accountJSON(ctx, c, "GET", "/users/"+urlPathEscape(userID)+"/balance", nil)
	if err != nil {
		p.host.Log("warn", "cline balance: /users/<id>/balance failed: "+err.Error())
		return
	}
	balance, _ := data["balance"].(float64)
	if balance == 0 {
		if n, ok := data["balance"].(json.Number); ok {
			balance, _ = n.Float64()
		}
	}
	// 照官方 normalizeCreditBalance：balance 单位是百万分之一美元（microUSD），
	// 展示前 / 1_000_000 折成美元。免费账号 balance=0 也照写（积分栏显 0 而非空缺）。
	dollars := balance / 1_000_000
	// remaining/total 是核心前端积分列的标准键（Accounts.vue 读 credits.remaining/total）
	profile.Quota["remaining"] = formatUSD(dollars)
	profile.Quota["total"] = formatUSD(dollars)
	// CreditsJson：microUSD 原值 + 折算后的美元值（remaining/total 同值，前端积分列标准键）
	if b, err := json.Marshal(map[string]interface{}{
		"balance": balance, "dollars": dollars,
		"remaining": formatUSD(dollars), "total": formatUSD(dollars),
	}); err == nil {
		profile.CreditsJson = string(b)
	}
}

// accountJSON 账号面调用（{success, data} 信封），headers 与对话同源。
func (p *plugin) accountJSON(ctx context.Context, c *credential, method, path string, body []byte) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range p.headers(c, fmt.Sprintf("sess_account_%d", time.Now().UnixMilli())) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("account auth failed: HTTP 401")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var env struct {
		Success bool                   `json:"success"`
		Error   string                 `json:"error"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("non-json response")
	}
	if !env.Success {
		return nil, fmt.Errorf("%s", shared.OrDefault(env.Error, "account request failed"))
	}
	return env.Data, nil
}

// rawString 从原始 JSON 字段表按名取字符串。
func rawString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// urlPathEscape 路径段转义（userId 含特殊字符时防注入）。
func urlPathEscape(s string) string {
	var b strings.Builder
	for _, ch := range s {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// formatUSD 余额 → 美元数字字符串（保留两位小数以内）。
func formatUSD(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}
