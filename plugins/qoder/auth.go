// auth.go — 认证：PAT→jobToken 交换/轮换（cosy 签名 + 私有 base64 编码）、登录/刷新。
package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// ---------- 自定义 base64（Qoder 私有字母表 + 字符重排） ----------

const customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
const customPad = "$"

var c2s = func() map[rune]byte {
	std := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	m := map[rune]byte{rune(customPad[0]): '='}
	for i := 0; i < 64; i++ {
		m[rune(customAlphabet[i])] = std[i]
	}
	return m
}()

var s2c = func() map[byte]rune {
	std := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	m := map[byte]rune{'=': rune(customPad[0])}
	for i := 0; i < 64; i++ {
		m[std[i]] = rune(customAlphabet[i])
	}
	return m
}()

// qEncode 明文 → 自定义 base64（标准 base64 后按 n/3 切片重排再换字母表）。
func qEncode(plain []byte) (string, error) {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	out := make([]rune, 0, len(rearranged))
	for _, ch := range rearranged {
		m, ok := s2c[byte(ch)]
		if !ok {
			return "", fmt.Errorf("char out of alphabet: %q", ch)
		}
		out = append(out, m)
	}
	return string(out), nil
}

// qDecode 自定义 base64 → 明文。
func qDecode(encoded string) ([]byte, error) {
	n := len(encoded)
	mapped := make([]byte, 0, n)
	for _, ch := range encoded {
		m, ok := c2s[ch]
		if !ok {
			return nil, fmt.Errorf("char out of custom alphabet: %q", ch)
		}
		mapped = append(mapped, m)
	}
	a := n / 3
	std := string(mapped[n-a:]) + string(mapped[a:n-a]) + string(mapped[:a])
	return base64.StdEncoding.DecodeString(std)
}

// ---------- cosy 请求签名 ----------

// signDate Bearer 类请求的 cosy-date（unix 秒字符串）。
func signDate() string { return fmt.Sprint(time.Now().Unix()) }

// authDate 认证类请求的 date 头（RFC1123 GMT，与真实 cosy 客户端一致；unix 秒会被服务端时效校验拒绝）。
func authDate() string { return time.Now().UTC().Format(http.TimeFormat) }

// signatureHeader MD5(appcode&secret&date) —— 认证类接口的 signature 头。
func signatureHeader(date string) string {
	sum := md5.Sum([]byte(appCode + "&" + signatureSecret + "&" + date))
	return hex.EncodeToString(sum[:])
}

// authHeaders 认证类接口公共头（Signature 签名）。
func authHeaders(c *credential, date, sig string) map[string]string {
	return map[string]string{
		"cosy-machinetoken": c.MachineToken,
		"cosy-machinetype":  c.MachineType,
		"login-version":     "v2",
		"appcode":           appCode,
		"accept":            "application/json",
		"accept-encoding":   "identity",
		"cosy-version":      cosyVersion,
		"cosy-clienttype":   "5",
		"date":              date,
		"signature":         sig,
		"content-type":      "application/json",
		"cosy-machineid":    c.MachineID,
		"User-Agent":        "Go-http-client/2.0",
	}
}

// postEncoded 认证类 POST：JSON → 自定义 base64 编码请求体。
func (p *plugin) postEncoded(ctx context.Context, c *credential, rawURL string, obj map[string]interface{}) (map[string]interface{}, error) {
	date := authDate()
	sig := signatureHeader(date)
	plain, _ := json.Marshal(obj)
	body, err := qEncode(plain)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range authHeaders(c, date, sig) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := shared.ReadLimited(resp.Body, 4<<20)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, &authError{msg: fmt.Sprintf("HTTP %d %s", resp.StatusCode, shared.Truncate(string(raw), 200))}
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d at %s: %s", resp.StatusCode, rawURL, shared.Truncate(string(raw), 200))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("non-json response: %w", err)
	}
	return out, nil
}

type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

// jobTokenRequest jobToken 交换 / 轮换共用（needRefresh 选择续期路径）。
func (p *plugin) jobTokenRequest(ctx context.Context, c *credential, needRefresh bool) (map[string]interface{}, error) {
	inner, _ := json.Marshal(map[string]interface{}{
		"personalToken":      c.PAT,
		"securityOauthToken": c.SecurityOauthToken,
		"refreshToken":       c.RefreshToken,
		"needRefresh":        needRefresh,
		"authInfo":           map[string]interface{}{},
	})
	outer := map[string]interface{}{
		"payload":       string(inner),
		"encodeVersion": "1",
	}
	return p.postEncoded(ctx, c, gateway+"/algo/api/v3/user/jobToken?Encode=1", outer)
}

// exchangeJobToken 冷 PAT → jobToken 交换。
func (p *plugin) exchangeJobToken(ctx context.Context, c *credential) (map[string]interface{}, error) {
	return p.jobTokenRequest(ctx, c, false)
}

// refreshJobToken refreshToken 周期续期（PAT 仍必填）。
func (p *plugin) refreshJobToken(ctx context.Context, c *credential) (map[string]interface{}, error) {
	return p.jobTokenRequest(ctx, c, true)
}

// Login PAT 校验：冷交换 jobToken 即有效。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	pat := strings.TrimSpace(req.Form["pat"])
	if pat == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 PAT（qoder.cn/account/integrations 创建）"}}, nil
	}
	c := &credential{PAT: pat, MachineID: shared.RandHex(16), MachineToken: base64.RawURLEncoding.EncodeToString([]byte(shared.RandHex(25))), MachineType: shared.RandHex(9)}
	if err := p.refreshCred(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "PAT 校验失败: " + err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	name := shared.OrDefault(c.UserName, "qoder-account")
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}},
	}, nil
}

// refreshCred 冷交换（首次）或 refreshToken 轮换（已有会话），写回 cred。
func (p *plugin) refreshCred(ctx context.Context, c *credential) error {
	var jt map[string]interface{}
	var err error
	if c.RefreshToken != "" {
		// 先尝试轮换；被拒回退冷交换
		jt, err = p.refreshJobToken(ctx, c)
		if err != nil {
			jt, err = p.exchangeJobToken(ctx, c)
		}
	} else {
		jt, err = p.exchangeJobToken(ctx, c)
	}
	if err != nil {
		return err
	}
	name, _ := jt["name"].(string)
	id, _ := jt["id"].(string)
	userType, _ := jt["userType"].(string)
	sot, _ := jt["securityOauthToken"].(string)
	rt, _ := jt["refreshToken"].(string)
	exp := numberField(jt, "expireTime")
	c.UserID = shared.OrDefault(id, c.UserID)
	c.UserName = shared.OrDefault(name, c.UserName)
	c.UserType = shared.OrDefault(userType, c.UserType)
	c.SecurityOauthToken = sot
	if rt != "" {
		c.RefreshToken = rt
	}
	c.ExpireTime = int64(exp)
	return nil
}

// ensureFresh jobToken 到期前主动续期（提前 2 小时）。
func (p *plugin) ensureFresh(ctx context.Context, c *credential) error {
	if c.ExpireTime == 0 || time.Now().UnixMilli() < c.ExpireTime-int64(refreshMargin/time.Millisecond) {
		return nil
	}
	return p.refreshCred(ctx, c)
}

// numberField 从响应取数字字段（宽松解析）。
func numberField(m map[string]interface{}, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	if n, ok := v.(float64); ok {
		return n
	}
	if s, ok := v.(string); ok {
		var f float64
		fmt.Sscanf(s, "%f", &f)
		return f
	}
	return 0
}

// Refresh jobToken 轮换并写回 blob。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if err := p.refreshCred(ctx, c); err != nil {
		code := int32(503)
		if _, isAuth := err.(*authError); isAuth {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: &pb.AccountProfile{
		DisplayName: shared.OrDefault(c.UserName, "qoder-account"), Healthy: true, Quota: map[string]string{},
	}}, nil
}
