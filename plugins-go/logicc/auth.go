// auth:登录(Login)+ Clerk JWT 刷新(Refresh)+ 档案(GetProfile)+ 凭据校验。
package logicc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// Login 两种方式:cookie(整条 Clerk Cookie,自动解析 Session ID)/ token(仅 Session JWT)。
// 校验:凭据能列出模型即视为有效。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	c := &credential{}
	switch req.MethodId {
	case "cookie":
		cookie, err := cookieValue(req.Form["cookie"], "__client")
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
		}
		c.ClientCookie = cookie
		sessionID, err := p.resolveSessionID(ctx, c)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: shared.ErrorStatus(err), Message: err.Error()}}, nil
		}
		c.SessionID = sessionID
	case "token":
		token := strings.TrimSpace(req.Form["token"])
		if token == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 Session JWT"}}, nil
		}
		c.Token = token
		if claims, err := decodeJWT(token); err == nil {
			c.SessionID, _ = claims["sid"].(string)
		}
	default:
		return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
	}

	// 会话方式先换一次 JWT,顺带校验凭据并落盘轮换后的 Cookie。
	if c.Token == "" {
		if _, err := p.refreshToken(ctx, c, true); err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
		}
	}
	if _, err := p.fetchModels(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "凭据校验失败: " + err.Error()}}, nil
	}
	return &pb.LoginResult{Blob: c.toBlob(), Profile: p.profile(c)}, nil
}

// Refresh 刷新凭据:仅 JWT 且已过期时无法自愈(需要 Cookie),返回 401 让核心标记失效。
func (p *plugin) Refresh(ctx context.Context, blob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(blob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	if c.ClientCookie == "" {
		if jwtFresh(c.Token) {
			return &pb.RefreshResult{Profile: p.profile(c)}, nil
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: 401,
			Message: "Session JWT 已过期,且凭据未包含 Cookie,无法自动刷新;请重新登录"}}, nil
	}
	if _, err := p.refreshToken(ctx, c, true); err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: shared.ErrorStatus(err), Message: err.Error()}}, nil
	}
	return &pb.RefreshResult{Blob: c.toBlob(), Profile: p.profile(c)}, nil
}

// GetProfile 上游无公开余额接口 → 基本档案。
func (p *plugin) GetProfile(ctx context.Context, blob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(blob)
	if err != nil {
		return nil, err
	}
	return p.profile(c), nil
}

// profile 展示名取自 Session JWT 的 sub(如 user_xxx),无则回退 session id。
func (p *plugin) profile(c *credential) *pb.AccountProfile {
	name := "logicc-" + shared.OrDefault(c.SessionID, "account")
	if claims, err := decodeJWT(c.Token); err == nil {
		if sub, ok := claims["sub"].(string); ok && sub != "" {
			name = "logicc-" + maskID(sub)
		}
	}
	return &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}}
}

// refreshToken 换取 Session JWT。Clerk 轮换 __client 时把新值写回凭据(随账号持久化)。
func (p *plugin) refreshToken(ctx context.Context, c *credential, force bool) (string, error) {
	if !force && jwtFresh(c.Token) {
		return c.Token, nil
	}
	if c.ClientCookie == "" {
		return "", shared.HTTPError{Code: 401, Message: "凭据缺少 Cookie,无法刷新"}
	}
	// 历史凭据或缺省的 Session ID 在此补齐(一次性,之后随凭据持久化)。
	if c.SessionID == "" {
		sessionID, err := p.resolveSessionID(ctx, c)
		if err != nil {
			return "", err
		}
		c.SessionID = sessionID
	}
	key := c.cacheKey()
	call, loaded := refreshFlights.LoadOrStore(key, &refreshCall{done: make(chan struct{})})
	fc := call.(*refreshCall)
	if loaded {
		// 跟随同账号进行中的刷新:等它结束后复用同一份结果。
		select {
		case <-fc.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return fc.value, fc.err
	}
	defer refreshFlights.Delete(key)
	fc.value, fc.err = p.exchangeToken(ctx, c)
	close(fc.done)
	if fc.err != nil {
		return "", fc.err
	}
	storeToken(c)
	return fc.value, nil
}

// exchangeToken 调 Clerk 会话令牌端点。匿名请求用独立 client:会话凭据走原文,
// 但出站代理仍按账号继承(与其它请求同一出口)。
func (p *plugin) exchangeToken(ctx context.Context, c *credential) (string, error) {
	endpoint := fmt.Sprintf("%s/v1/client/sessions/%s/tokens", p.clerkBase(), url.PathEscape(c.SessionID))
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", "__client="+c.ClientCookie)
	req.Header.Set("Origin", "https://app.logicc.com")
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return "", shared.HTTPError{Code: 502, Message: "凭据刷新请求失败: " + err.Error()}
	}
	defer resp.Body.Close()

	// Clerk 若轮换客户端凭据,保存响应中的新值,重启后仍使用新值。
	for _, cookie := range resp.Header.Values("Set-Cookie") {
		if v, ok := clientCookieValue(cookie); ok {
			if cleaned, cerr := cleanSecret(v, "__client Cookie"); cerr == nil {
				c.ClientCookie = cleaned
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", shared.HTTPError{Code: int32(resp.StatusCode),
			Message: fmt.Sprintf("凭据刷新失败(HTTP %d),请更新 Cookie:%s",
				resp.StatusCode, shared.Truncate(strings.TrimSpace(string(raw)), 200))}
	}
	var data struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return "", shared.HTTPError{Code: 502, Message: "Clerk 返回了无法解析的令牌响应"}
	}
	if !jwtFresh(data.JWT) {
		return "", shared.HTTPError{Code: 401, Message: "上游没有返回属于此账户的有效 Session JWT"}
	}
	if claims, err := decodeJWT(data.JWT); err == nil {
		if sid, _ := claims["sid"].(string); sid != "" && sid != c.SessionID {
			return "", shared.HTTPError{Code: 401, Message: "上游返回的 JWT 不属于该 Session ID"}
		}
	}
	c.Token = data.JWT
	return c.Token, nil
}

// clientCookieValue 从 Set-Cookie 头取出 __client 的值。
func clientCookieValue(header string) (string, bool) {
	name, rest, ok := strings.Cut(header, "=")
	if !ok || strings.TrimSpace(name) != "__client" {
		return "", false
	}
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest), true
}

// cleanSecret 校验单个凭据值:合法的 Clerk 值只含 token 与 base64url 字符。
func cleanSecret(value, field string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("请填写 %s", field)
	}
	if len(v) > 20000 {
		return "", fmt.Errorf("%s 格式不正确(过长)", field)
	}
	for _, r := range v {
		if !(r == '_' || r == '-' || r == '.' || r == '~' || r == '+' || r == '/' || r == '=' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return "", fmt.Errorf("%s 请填写凭据值,不要填写完整 Cookie 请求头", field)
		}
	}
	return v, nil
}

// cookieValue 从整条 Cookie 头中取出指定名 cookie 的值;也接受直接粘贴的单个值。
func cookieValue(raw, name string) (string, error) {
	// 不含分隔符即视为直接粘贴的值,交由 cleanSecret 校验。
	if !strings.ContainsAny(raw, ";=") {
		return cleanSecret(raw, name)
	}
	for _, part := range strings.FieldsFunc(strings.ReplaceAll(raw, ";", "\n"), func(r rune) bool { return r == '\n' || r == '\r' }) {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		value := strings.Trim(strings.TrimSpace(val), `"`)
		if value == "" {
			return "", fmt.Errorf("Cookie 中 %s 的值为空", name)
		}
		return value, nil
	}
	return "", fmt.Errorf("Cookie 中缺少 %s,请复制 clerk.logicc.com 的完整 Cookie 头", name)
}

// resolveSessionID 调 Clerk 客户端接口取当前活跃会话 id。
func (p *plugin) resolveSessionID(ctx context.Context, c *credential) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.clerkBase()+"/v1/client", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", "__client="+c.ClientCookie)
	req.Header.Set("Origin", "https://app.logicc.com")
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return "", shared.HTTPError{Code: 502, Message: "读取 Clerk 会话失败: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", shared.HTTPError{Code: int32(resp.StatusCode),
			Message: fmt.Sprintf("Cookie 校验失败(HTTP %d),请重新复制:%s",
				resp.StatusCode, shared.Truncate(strings.TrimSpace(string(raw)), 200))}
	}
	return pickSessionID(shared.ReadLimitedResp(resp, 1<<20))
}

// pickSessionID 从 Clerk /v1/client 响应中挑出 Session ID:优先最近活跃会话,回退首个活跃会话。
func pickSessionID(raw []byte) (string, error) {
	var data struct {
		Response struct {
			LastActiveSessionID string `json:"last_active_session_id"`
			Sessions            []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"sessions"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", shared.HTTPError{Code: 502, Message: "Clerk 返回了无法解析的会话响应"}
	}
	if id := strings.TrimSpace(data.Response.LastActiveSessionID); strings.HasPrefix(id, "sess_") {
		return id, nil
	}
	for _, s := range data.Response.Sessions {
		if strings.HasPrefix(s.ID, "sess_") && s.Status == "active" {
			return s.ID, nil
		}
	}
	return "", shared.HTTPError{Code: 401, Message: "该 Cookie 下没有活跃的 Session ID,请先在 clerk.logicc.com 登录"}
}

// maskID 脱敏展示:保留前 12 位。
func maskID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "..."
}
