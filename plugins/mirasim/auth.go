// auth.go — mrs-sig-v2 Ed25519 请求签名、邮件/凭据导入登录、access token 刷新。
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// signingInput 规范签名 payload 的输入。
type signingInput struct {
	method        string
	path          string
	timestamp     string
	nonce         string
	deviceID      string
	clientVersion string
	credential    string // ticket 或 access token（明文，进 payload 前哈希）
	metadata      map[string]string
	body          []byte
}

func sha256Hex(v []byte) string {
	d := sha256.Sum256(v)
	return hex.EncodeToString(d[:])
}

// canonicalMetadata key 小写、丢空值、按 key 排序、key:value 换行连接。
func canonicalMetadata(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	normalized := make(map[string]string, len(metadata))
	for k, v := range metadata {
		k = strings.ToLower(k)
		if v == "" {
			continue
		}
		normalized[k] = v
	}
	keys := make([]string, 0, len(normalized))
	for k := range normalized {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+":"+normalized[k])
	}
	return strings.Join(pairs, "\n")
}

// canonicalSignaturePayload 10 行以 \n 连接；空 metadata 贡献空行而非 sha256("")。
func canonicalSignaturePayload(in signingInput) []byte {
	metaCanonical := canonicalMetadata(in.metadata)
	metaDigest := ""
	if metaCanonical != "" {
		metaDigest = sha256Hex([]byte(metaCanonical))
	}
	payload := strings.Join([]string{
		signatureVersion,
		strings.ToUpper(strings.TrimSpace(in.method)),
		in.path,
		in.timestamp,
		in.nonce,
		in.deviceID,
		in.clientVersion,
		sha256Hex([]byte(in.credential)),
		metaDigest,
		sha256Hex(in.body),
	}, "\n")
	return []byte(payload)
}

// signHeaders 组签名头：Ed25519 签规范 payload，metadata 非空项也 Set 进头（后续封密）。
func (c *credential) signHeaders(in signingInput) (http.Header, error) {
	nonceBytes := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonceBytes); err != nil {
		return nil, fmt.Errorf("generate signature nonce: %w", err)
	}
	in.nonce = base64.RawURLEncoding.EncodeToString(nonceBytes)
	in.timestamp = strconv.FormatInt(time.Now().UnixMilli(), 10)
	in.deviceID = c.deviceID
	signature := ed25519.Sign(c.privateKey, canonicalSignaturePayload(in))

	headers := make(http.Header, len(in.metadata)+5)
	for k, v := range in.metadata {
		if v != "" {
			headers.Set(k, v)
		}
	}
	headers.Set("x-mirasim-device", in.deviceID)
	headers.Set("x-mirasim-ts", in.timestamp)
	headers.Set("x-mirasim-nonce", in.nonce)
	headers.Set("x-mirasim-sig", base64.RawURLEncoding.EncodeToString(signature))
	if in.clientVersion != "" {
		headers.Set("x-mirasim-client", in.clientVersion)
	}
	return headers, nil
}

// Login 两方式：email（发码 → 验码换 token）多步；credential_file（贴 token JSON）一步。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	s := p.settings(req.InstanceId)
	switch req.MethodId {
	case "email":
		return p.loginEmail(ctx, req, s)
	case "credential_file":
		return p.loginCredentialFile(ctx, req, s)
	}
	return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "unknown auth method: " + req.MethodId}}, nil
}

// loginEmail 两步：State 空 = 发验证码并要求填码；State 非空 = 验码换 token。
func (p *plugin) loginEmail(ctx context.Context, req *pb.LoginRequest, s map[string]string) (*pb.LoginResult, error) {
	adminURL := p.adminURL(s)
	proxy := sdk.ProxyURL(nil) // 登录期无账号分组代理
	if len(req.State) == 0 {
		email := strings.TrimSpace(req.Form["email"])
		if !validEmail(email) {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写有效邮箱"}}, nil
		}
		if err := postAdminJSON(ctx, adminURL, proxy, "/auth/code", map[string]string{"email": email}, nil); err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "发送验证码失败: " + err.Error()}}, nil
		}
		state, _ := json.Marshal(map[string]string{"email": email})
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "input_form",
			Prompt: map[string]string{
				"zh": fmt.Sprintf("验证码已发往 %s，请填入", email),
				"en": fmt.Sprintf("Code sent to %s, enter it below", email),
			},
			State: state,
			Fields: []*pb.AuthField{{
				Name: "code", Label: map[string]string{"zh": "验证码", "en": "Code"},
				Type: "text", Required: true, Placeholder: "6 位验证码",
			}},
		}}, nil
	}
	var st struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(req.State, &st) != nil || st.Email == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "登录状态失效，请重新发起"}}, nil
	}
	code := strings.TrimSpace(req.Form["code"])
	if code == "" || len(code) > 64 || strings.ContainsAny(code, "\r\n\x00") {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写验证码"}}, nil
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := postAdminJSON(ctx, adminURL, proxy, "/auth/verify", map[string]string{"email": st.Email, "code": code}, &payload); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "验证码校验失败: " + err.Error()}}, nil
	}
	if payload.AccessToken == "" || payload.RefreshToken == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "登录响应缺少 token（该账号可能无法保活）"}}, nil
	}
	return p.finalizeLogin(ctx, &credential{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
	}, s)
}

// loginCredentialFile 直接导入 token JSON（含 access_token + refresh_token）。
func (p *plugin) loginCredentialFile(ctx context.Context, req *pb.LoginRequest, s map[string]string) (*pb.LoginResult, error) {
	var c credential
	if err := json.Unmarshal([]byte(req.Form["content"]), &c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "凭据必须是 JSON"}}, nil
	}
	if c.AccessToken == "" || c.RefreshToken == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "凭据需含 access_token 与 refresh_token"}}, nil
	}
	return p.finalizeLogin(ctx, &credential{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
	}, s)
}

// finalizeLogin 生成设备密钥 → 派生身份 → 实证（ListModels 能铸票）→ 建档。
func (p *plugin) finalizeLogin(ctx context.Context, c *credential, s map[string]string) (*pb.LoginResult, error) {
	keyPEM, err := newDeviceKeyPEM()
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 500, Message: "生成设备密钥失败: " + err.Error()}}, nil
	}
	c.DevicePrivateKey = keyPEM
	c.ExpiresAt = resolveAccessExpiry(c.AccessToken, 0)
	if err := c.loadSigner(); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 500, Message: err.Error()}}, nil
	}
	c.deriveIdentity()
	rc := p.relay(c, s)
	// 实证：拉一次模型目录，验证 token+设备密钥能铸票并被中继接受
	if _, code, err := rc.controlDo(ctx, "GET", modelsPath); err != nil || code >= 400 {
		msg := "凭据校验失败"
		if err != nil {
			msg += ": " + err.Error()
		} else {
			msg += fmt.Sprintf("（HTTP %d）", code)
		}
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: msg}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{Blob: blob, Profile: p.profileOf(c)}, nil
}

// validEmail 简单邮箱校验。
func validEmail(email string) bool {
	if len(email) == 0 || len(email) > 254 || strings.ContainsAny(email, "\r\n\x00 ") {
		return false
	}
	at := strings.IndexByte(email, '@')
	return at > 0 && at < len(email)-1 && strings.IndexByte(email[at+1:], '.') >= 0
}

// postAdminJSON 认证服务 POST（私有 client，20s，body 含登录密钥不入日志）。out 非 nil 时解析成功响应。
func postAdminJSON(ctx context.Context, adminURL, proxy, path string, reqBody map[string]string, out interface{}) error {
	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", adminURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: sdk.UpstreamClient(proxy).Transport, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw := shared.ReadLimited(resp.Body, 64<<10)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if detail := adminErrorDetail(raw); detail != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, detail)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("响应非 JSON")
		}
	}
	return nil
}

// adminErrorDetail 从 {"detail":"..."} 取错误信息（限长、过滤控制字符）。
func adminErrorDetail(raw []byte) string {
	var e struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	d := strings.TrimSpace(e.Detail)
	if len(d) > 512 {
		d = d[:512]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, d)
}

// Refresh access token 刷新并回填配额。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	s := p.settings(credBlob.GetInstanceId())
	rc := p.relay(c, s)
	if err := rc.refreshAccess(ctx); err != nil {
		code := int32(503)
		if strings.Contains(err.Error(), "失效") {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	profile := p.profileOf(c)
	p.fillQuota(ctx, rc, profile)
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: profile}, nil
}
