// sso.go — passToken → serviceToken 的 5 步小米 SSO 链（30 分钟缓存，401 强制刷新）。
// 灰度回调自动钉回生产网关（回调宿主的 sts 未被 account.xiaomi.com 白名单收编）。
package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

const accountHost = "account.xiaomi.com"

// sessionCache 进程级 service cookie 缓存（passToken sha256 → 会话）。
var (
	sessionMu     sync.Mutex
	sessionCookie string
	sessionKey    string
	sessionAt     time.Time
)

// serviceSession SSO 链产物。
type serviceSession struct {
	ServiceToken string
	UserID       string
	Extra        []string // mimopc_ph / mimopc_slh 等附加 cookie 值
}

// cookie 会话 → Cookie 头。
func (s *serviceSession) header() string {
	parts := []string{"serviceToken=" + s.ServiceToken}
	parts = append(parts, s.Extra...)
	if s.UserID != "" {
		parts = append(parts, "userId="+s.UserID)
	}
	return strings.Join(parts, "; ")
}

// getServiceCookie 取 service cookie（缓存命中直接回；force 绕过缓存）。
func (p *plugin) getServiceCookie(ctx context.Context, cfg apiConfig, cred *credential, force bool) (string, error) {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(cred.PassToken)))
	sessionMu.Lock()
	if !force && sessionCookie != "" && sessionKey == key && time.Since(sessionAt) < cookieTTL {
		cookie := sessionCookie
		sessionMu.Unlock()
		return cookie, nil
	}
	sessionMu.Unlock()

	sess, err := p.sso(ctx, cfg, cred)
	if err != nil {
		return "", err
	}
	cookie := sess.header()
	sessionMu.Lock()
	sessionCookie, sessionKey, sessionAt = cookie, key, time.Now()
	sessionMu.Unlock()
	return cookie, nil
}

// sso 5 步链：未鉴权 API 302 → passportapi SSO 两步 → mimopc SSO → sts 换 Set-Cookie。
func (p *plugin) sso(ctx context.Context, cfg apiConfig, cred *credential) (*serviceSession, error) {
	// jar 键对齐 Desktop cookie 名：passToken / userId / cUserId
	jar := map[string]string{}
	if cred.PassToken != "" {
		jar["passToken"] = cred.PassToken
	}
	if cred.UserID != "" {
		jar["userId"] = cred.UserID
	}
	if cred.CUserID != "" {
		jar["cUserId"] = cred.CUserID
	}
	ck := func() string {
		parts := make([]string, 0, len(jar))
		for k, v := range jar {
			parts = append(parts, k+"="+v)
		}
		return strings.Join(parts, "; ")
	}

	client := &http.Client{Transport: sdk.UpstreamClient(cred.proxyURL).Transport, Timeout: 30 * time.Second}

	// 1. 未鉴权 API → 302 serviceLogin，Location query 携带 sts 回调
	req, _ := http.NewRequestWithContext(ctx, "GET", cfg.APIBase+"/api/user/xiaomi/me", nil)
	req.Header.Set("User-Agent", cfg.APIUA)
	req.Header.Set("Cookie", ck())
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SSO step1: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || !strings.Contains(loc, "serviceLogin") {
		return nil, fmt.Errorf("SSO step1 failed: HTTP %d %s", resp.StatusCode, shared.Truncate(loc, 120))
	}
	stsCallback, err := queryParam(loc, "callback")
	if err != nil || stsCallback == "" {
		return nil, fmt.Errorf("SSO step1: no callback in 302")
	}
	stsCallback = normalizeGateway(stsCallback, cfg.APIBase)

	// 2. passportapi SSO phase1 → nonce / ssecurity
	body, status, err := getJSON(ctx, client, "https://"+accountHost+"/pass/serviceLogin?sid=passportapi&_json=true", ck())
	if err != nil {
		return nil, fmt.Errorf("SSO step2: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("SSO step2 failed: HTTP %d %s", status, shared.Truncate(body, 120))
	}
	var j1 struct {
		Nonce    string `json:"nonce"`
		Security string `json:"ssecurity"`
		Location string `json:"location"`
	}
	if err := json.Unmarshal([]byte(sanitizeJSONBody(body)), &j1); err != nil || j1.Nonce == "" {
		return nil, fmt.Errorf("SSO step2: no nonce in response")
	}

	// 3. phase2 (+clientSign) → 账号级 serviceToken
	sep := "&"
	if !strings.Contains(j1.Location, "?") {
		sep = "?"
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", j1.Location+sep+"clientSign="+clientSign(j1.Nonce, j1.Security), nil)
	req.Header.Set("User-Agent", ssoUA)
	req.Header.Set("Cookie", ck())
	resp, err = client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SSO step3: %w", err)
	}
	resp.Body.Close()
	absorbCookies(jar, resp)

	// 4. mimopc SSO → sts ticket
	q := url.Values{}
	q.Set("sid", "mimopc")
	q.Set("callback", stsCallback)
	q.Set("_json", "true")
	body, status, err = getJSON(ctx, client, "https://"+accountHost+"/pass/serviceLogin?"+q.Encode()+"&_json=true", ck())
	if err != nil {
		return nil, fmt.Errorf("SSO step4: %w", err)
	}
	var j3 struct {
		Location string `json:"location"`
	}
	if err := json.Unmarshal([]byte(sanitizeJSONBody(body)), &j3); err != nil {
		return nil, fmt.Errorf("SSO step4: bad response")
	}
	if !strings.Contains(j3.Location, "/api/sts") {
		return nil, fmt.Errorf("SSO step4 failed: %s", shared.Truncate(j3.Location, 120))
	}
	stsURL := normalizeGateway(j3.Location, cfg.APIBase)

	// 5. sts → Set-Cookie: serviceToken
	req, _ = http.NewRequestWithContext(ctx, "GET", stsURL, nil)
	req.Header.Set("User-Agent", cfg.APIUA)
	req.Header.Set("Cookie", ck())
	resp, err = client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SSO step5: %w", err)
	}
	resp.Body.Close()
	absorbCookies(jar, resp)

	if jar["serviceToken"] == "" {
		return nil, fmt.Errorf("SSO step5: no serviceToken issued (passToken 已过期？请重新登录 MiMo Desktop)")
	}
	sess := &serviceSession{ServiceToken: jar["serviceToken"], UserID: jar["userId"]}
	for _, k := range []string{"mimopc_ph", "mimopc_slh"} {
		if v := jar[k]; v != "" {
			sess.Extra = append(sess.Extra, k+"="+v)
		}
	}
	return sess, nil
}

// clientSign 小米 SSO clientSign：sha1("nonce=<nonce>[&ssecurity]")，URL 编码后附到回调。
func clientSign(nonce, ssecurity string) string {
	payload := "nonce=" + nonce
	if strings.TrimSpace(ssecurity) != "" {
		payload += "&" + ssecurity
	}
	digest := sha1.Sum([]byte(payload))
	b64 := base64.StdEncoding.EncodeToString(digest[:])
	encoded := url.QueryEscape(b64)
	return encoded
}

// sanitizeJSONBody 小米 _json 响应偶有前导 & / START&&& 前缀，剥掉再解。
func sanitizeJSONBody(raw string) string {
	t := strings.TrimSpace(raw)
	for strings.HasPrefix(t, "&") {
		t = t[1:]
	}
	t = strings.TrimPrefix(t, "START&&&")
	return t
}

// getJSON GET 请求回 body + status。
func getJSON(ctx context.Context, client *http.Client, rawURL, cookie string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", ssoUA)
	req.Header.Set("Accept", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if len(buf) > 1<<20 {
			break
		}
	}
	return string(buf), resp.StatusCode, nil
}

// absorbCookies 把响应 Set-Cookie 收进 jar（只收名值对）。
func absorbCookies(jar map[string]string, resp *http.Response) {
	for _, v := range resp.Header.Values("Set-Cookie") {
		c := strings.TrimSpace(strings.SplitN(v, ";", 2)[0])
		if k, val, ok := strings.Cut(c, "="); ok && strings.TrimSpace(val) != "" {
			jar[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
}

// normalizeGateway 灰度回调钉回生产网关（回调宿主的 sts 未白名单化）。
func normalizeGateway(rawURL, apiBase string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	want, err := url.Parse(apiBase)
	if err != nil {
		return rawURL
	}
	if u.Host != want.Host {
		u.Host = want.Host
		u.Scheme = "https"
	}
	return u.String()
}

// queryParam 取 URL query 参数。
func queryParam(rawURL, key string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return u.Query().Get(key), nil
}
