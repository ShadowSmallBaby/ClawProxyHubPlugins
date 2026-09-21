// auth.go — 账号密码 / 凭据文件登录：站点会话 → 自举出 access_token + user_id + api_key。
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

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// sessionAuth 站点会话：旧版 New API 下发 session cookie，新版登录直接返回短期 JWT（Bearer）；两者都要 New-Api-User。
type sessionAuth struct {
	cookie    string
	bearer    string
	userID    int
	expiresAt int64 // JWT 到期 unix 秒，0 未知
}

// headers 会话请求头（浏览器 UA）。New API 会话鉴权强制要求 New-Api-User 且须与会话用户一致；
// 未知时按官方前端惯例填 -1（保证头存在，服务端会给出明确的"不匹配"而非"未提供"）。
func (s *sessionAuth) headers(site *siteConfig) map[string]string {
	h := map[string]string{"Content-Type": "application/json", "New-Api-User": "-1", "User-Agent": site.BrowserUA}
	if s.bearer != "" {
		h["Authorization"] = "Bearer " + s.bearer
	} else if s.cookie != "" {
		h["Cookie"] = "session=" + s.cookie
	}
	if s.userID > 0 {
		h["New-Api-User"] = strconv.Itoa(s.userID)
	}
	return h
}

// callJSON 管理面通用调用（任意头），解 {success,message,data} 信封。
func (p *plugin) callJSON(ctx context.Context, cred *credential, site *siteConfig, method, path string, headers map[string]string, body interface{}) (json.RawMessage, *http.Response, error) {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	resp, err := p.do(ctx, cred, method, site.BaseURL+path, headers, rd)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp, fmt.Errorf("upstream non-json (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if !env.Success {
		return nil, resp, &apiError{message: orDefault(env.Message, fmt.Sprintf("HTTP %d", resp.StatusCode))}
	}
	return env.Data, resp, nil
}

// loginPassword POST /api/user/login。旧版：data 为用户对象 + session cookie；
// 新版：data = {access_token, user:{id}}（JWT，短期有效，仅用于自举）。
func (p *plugin) loginPassword(ctx context.Context, site *siteConfig, username, password string) (*sessionAuth, error) {
	cred := &credential{}
	headers := (&sessionAuth{}).headers(site)
	data, resp, err := p.callJSON(ctx, cred, site, "POST", "/api/user/login", headers,
		map[string]string{"username": username, "password": password})
	if err != nil {
		return nil, err
	}
	var body struct {
		ID          int      `json:"id"`
		AccessToken string   `json:"access_token"`
		ExpiresAt   int64    `json:"access_expires_at"`
		User        selfInfo `json:"user"`
	}
	_ = json.Unmarshal(data, &body)
	auth := &sessionAuth{bearer: body.AccessToken, userID: body.User.ID, expiresAt: body.ExpiresAt}
	if auth.userID == 0 {
		auth.userID = body.ID
	}
	if auth.bearer != "" {
		return auth, nil
	}
	for _, c := range resp.Cookies() {
		if c.Name == "session" && c.Value != "" {
			auth.cookie = c.Value
			return auth, nil
		}
	}
	return nil, fmt.Errorf("登录成功但未返回 session cookie 或 access_token")
}

// parseCredFile 凭据文件：JSON {"session":"...","user_id":1}（也认 cookie / access_token 键）或原始 Cookie 头字符串。
func parseCredFile(raw string) (*sessionAuth, error) {
	raw = strings.TrimSpace(raw)
	auth := &sessionAuth{}
	if strings.HasPrefix(raw, "{") {
		var f struct {
			Session     string          `json:"session"`
			Cookie      string          `json:"cookie"`
			AccessToken string          `json:"access_token"`
			UserID      json.RawMessage `json:"user_id"`
		}
		if json.Unmarshal([]byte(raw), &f) != nil {
			return nil, fmt.Errorf("凭据文件不是合法 JSON")
		}
		auth.cookie = orDefault(f.Session, cookieValue(f.Cookie, "session"))
		auth.bearer = f.AccessToken
		auth.userID = int(rawNumber(f.UserID))
	} else {
		auth.cookie = cookieValue(raw, "session")
		if auth.cookie == "" && !strings.Contains(raw, "=") {
			auth.cookie = raw // 直接给 session 值
		}
	}
	if auth.cookie == "" && auth.bearer == "" {
		return nil, fmt.Errorf("凭据文件缺少 session（cookie）或 access_token")
	}
	return auth, nil
}

// cookieValue 从 "a=1; session=xxx" 形态取指定 cookie 值。
func cookieValue(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// bootstrapFromSession 会话 → 完整凭据：self 取 user_id → 尽力签发系统访问令牌 → 取/建一把 API 密钥。
// 系统令牌在新版站点需过安全验证，拿不到就靠会话（有密码则过期自动重登）。
func (p *plugin) bootstrapFromSession(ctx context.Context, site *siteConfig, auth *sessionAuth, username, password, tokenName string) (*credential, *selfInfo, error) {
	cred := &credential{Username: username, Password: password, TokenName: tokenName}
	cred.applySession(auth)
	// 1. 自身信息（补 user_id）
	data, err := p.managementOnce(ctx, cred, site, "GET", "/api/user/self")
	if err != nil {
		if cred.UserID == 0 {
			return nil, nil, fmt.Errorf("会话无效: %w（New API 要求 New-Api-User 与会话用户一致，凭据文件请用 JSON 形态附上 user_id）", err)
		}
		return nil, nil, fmt.Errorf("会话无效: %w", err)
	}
	var self selfInfo
	_ = json.Unmarshal(data, &self)
	if cred.UserID == 0 {
		cred.UserID = self.ID
	}
	// 2. 系统访问令牌（可选；GET /api/user/token 每次重新签发，data 为字符串，兼容 {token} 对象）
	if data, err := p.managementOnce(ctx, cred, site, "GET", "/api/user/token"); err == nil {
		var token string
		if json.Unmarshal(data, &token) != nil {
			var obj struct {
				Token       string `json:"token"`
				AccessToken string `json:"access_token"`
			}
			_ = json.Unmarshal(data, &obj)
			token = orDefault(obj.Token, obj.AccessToken)
		}
		cred.AccessToken = token
	}
	if cred.AccessToken == "" && cred.Password == "" && cred.SessionExp > 0 {
		return nil, nil, fmt.Errorf("站点未签发系统访问令牌，且凭据文件的 JWT 会过期；请改用账号密码登录")
	}
	// 3. API 密钥：指定名称则只认那把；否则首把启用的；都没有就新建。
	// 不探活（避免站点检测）：取不到明文也照常建档，profile 标 healthy=false，等刷新取得后再启用调度。
	cred.APIKey, cred.keyErr = p.pickOrCreateAPIKey(ctx, cred, site)
	return cred, &self, nil
}

// pickOrCreateAPIKey 按 cred.TokenName（空 = 任意）取首把启用且有明文的密钥；
// 指定名已存在却取不到明文直接报错（不重名新建）；没有就 POST 创建一把（名字取指定名或 cph，无限额度、永不过期）再取。
func (p *plugin) pickOrCreateAPIKey(ctx context.Context, cred *credential, site *siteConfig) (string, error) {
	name := cred.TokenName
	tokens := p.listTokens(ctx, cred, site)
	if key := p.plainToken(ctx, cred, site, tokens, name); key != "" {
		return key, nil
	}
	if name != "" && hasToken(tokens, name) {
		return "", fmt.Errorf("密钥「%s」存在但取不到明文（已禁用或站点不返回明文），请在站点上检查或换一把", name)
	}
	_, _, err := p.callJSON(ctx, cred, site, "POST", "/api/token/", managementHeaders(cred, site), map[string]interface{}{
		"name": orDefault(name, "cph"), "remain_quota": 0, "expired_time": -1, "unlimited_quota": true,
		"model_limits_enabled": false, "model_limits": "", "group": "",
	})
	if err != nil {
		return "", fmt.Errorf("创建 API 密钥失败: %w", err)
	}
	if key := p.plainToken(ctx, cred, site, p.listTokens(ctx, cred, site), name); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("站点未返回 API 密钥明文，无法用于对话调度；可在站点复制密钥后改用「API 密钥」方式添加")
}

func hasToken(tokens []apiToken, name string) bool {
	for _, t := range tokens {
		if t.Name == name {
			return true
		}
	}
	return false
}

// apiToken 令牌列表条目（列表兼容 {items:[...]} 与直接数组）。
type apiToken struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"`
	Status int    `json:"status"`
}

func (p *plugin) listTokens(ctx context.Context, cred *credential, site *siteConfig) []apiToken {
	data, _, err := p.callJSON(ctx, cred, site, "GET", "/api/token/?p=1&size=50", managementHeaders(cred, site), nil)
	if err != nil {
		return nil
	}
	var wrapped struct {
		Items []apiToken `json:"items"`
	}
	if json.Unmarshal(data, &wrapped) == nil && len(wrapped.Items) > 0 {
		return wrapped.Items
	}
	var items []apiToken
	_ = json.Unmarshal(data, &items)
	return items
}

// plainToken 取首把启用密钥的明文（name 非空时只看同名的），不探活。
// 列表 / 单查均脱敏（新版站点），明文走 POST /api/token/{id}/key；旧版站点 GET /api/token/{id} 直接给明文，作兜底。
func (p *plugin) plainToken(ctx context.Context, cred *credential, site *siteConfig, tokens []apiToken, name string) string {
	for _, t := range tokens {
		if t.Status != 1 || (name != "" && t.Name != name) {
			continue
		}
		if key := normalizeKey(t.Key); key != "" {
			return key
		}
		if t.ID <= 0 {
			continue
		}
		if key := p.tokenPlainKey(ctx, cred, site, t.ID); key != "" {
			return key
		}
	}
	return ""
}

// tokenPlainKey 按 id 取明文：POST /{id}/key → {key}；GET /{id} → 旧版整条记录。
func (p *plugin) tokenPlainKey(ctx context.Context, cred *credential, site *siteConfig, id int) string {
	idPath := "/api/token/" + strconv.Itoa(id)
	if data, _, err := p.callJSON(ctx, cred, site, "POST", idPath+"/key", managementHeaders(cred, site), nil); err == nil {
		var obj struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(data, &obj)
		if key := normalizeKey(obj.Key); key != "" {
			return key
		}
	}
	if data, _, err := p.callJSON(ctx, cred, site, "GET", idPath, managementHeaders(cred, site), nil); err == nil {
		var full apiToken
		_ = json.Unmarshal(data, &full)
		if key := normalizeKey(full.Key); key != "" {
			return key
		}
	}
	return ""
}

// normalizeKey 补 sk- 前缀；含 * 的脱敏值视为无效。
func normalizeKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" || strings.Contains(k, "*") {
		return ""
	}
	return "sk-" + strings.TrimPrefix(k, "sk-")
}

// loginViaSession 账号密码 / 凭据文件的共同收尾：自举凭据 + 资料。
// 未取得 API 密钥时仍建档：healthy=false（核心以停用调度入库，任务照常），并在资料里说明原因。
func (p *plugin) loginViaSession(ctx context.Context, site *siteConfig, instanceID int64, auth *sessionAuth, username, password, tokenName string) (*pb.LoginResult, error) {
	cred, self, err := p.bootstrapFromSession(ctx, site, auth, username, password, tokenName)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	cred.instanceID = instanceID
	profile := &pb.AccountProfile{DisplayName: "newapi-account", Healthy: cred.APIKey != "", Quota: map[string]string{}}
	p.fillProfile(ctx, cred, site, self, profile)
	if cred.keyErr != nil {
		profile.Quota["api_key"] = "未取得: " + cred.keyErr.Error()
	}
	blob, _ := json.Marshal(cred)
	return &pb.LoginResult{Blob: blob, Profile: profile}, nil
}
