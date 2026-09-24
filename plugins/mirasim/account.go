// account.go — 凭据 blob 解析、设备密钥/身份派生、JWT claim、账号档案与配额快照。
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

type credential struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresAt        int64  `json:"expires_at,omitempty"` // access token 到期（unix 毫秒），0 未知
	AccountID        string `json:"account_id,omitempty"`
	Email            string `json:"email,omitempty"`
	Plan             string `json:"plan,omitempty"`
	DevicePrivateKey string `json:"device_private_key,omitempty"` // Ed25519 PKCS#8 PEM

	// 运行态（不序列化）
	privateKey ed25519.PrivateKey `json:"-"`
	publicB64  string             `json:"-"` // 公钥 PKIX DER 的标准 base64
	deviceID   string             `json:"-"` // base64url(sha256(publicB64))[:22]
	relayAcct  string             `json:"-"` // sub-account（仅 token 含 account_id 时）
	proxyURL   string             `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析，并加载设备密钥/派生身份。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.AccessToken == "" && c.RefreshToken == "" {
		return nil, fmt.Errorf("credential missing tokens")
	}
	if err := c.loadSigner(); err != nil {
		return nil, err
	}
	c.deriveIdentity()
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// loadSigner 解析 device 私钥，派生 publicB64 与 deviceID。
func (c *credential) loadSigner() error {
	if c.DevicePrivateKey == "" {
		return fmt.Errorf("credential missing device key")
	}
	block, _ := pem.Decode([]byte(c.DevicePrivateKey))
	if block == nil {
		return fmt.Errorf("device key not valid PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse device key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok || len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("device key is not Ed25519")
	}
	c.privateKey = priv
	publicDER, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return fmt.Errorf("marshal device public key: %w", err)
	}
	c.publicB64 = base64.StdEncoding.EncodeToString(publicDER)
	digest := sha256.Sum256([]byte(c.publicB64))
	c.deviceID = base64.RawURLEncoding.EncodeToString(digest[:])[:22]
	return nil
}

// deriveIdentity 从 access token JWT 补身份字段（account_id/email/plan/sub-account）。
func (c *credential) deriveIdentity() {
	claims := decodeJWTClaims(c.AccessToken)
	if claims == nil {
		return
	}
	for _, key := range []string{"account_id", "accountId", "user_id", "userId", "sub"} {
		if v := claimString(claims, key); v != "" {
			c.AccountID = v
			break
		}
	}
	if v := claimString(claims, "email"); v != "" {
		c.Email = v
	}
	if v := claimString(claims, "plan"); v != "" {
		c.Plan = v
	}
	// sub-account 仅取 account_id/accountId，区别于文件命名用的 AccountID。
	c.relayAcct = shared.OrDefault(claimString(claims, "account_id"), claimString(claims, "accountId"))
}

// newDeviceKeyPEM 生成 Ed25519 PKCS#8 PEM（登录时随凭据持久化）。
func newDeviceKeyPEM() (string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// decodeJWTClaims 解 JWT payload（不校验签名，仅取 claim）。
func decodeJWTClaims(token string) map[string]interface{} {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]interface{}
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func claimString(claims map[string]interface{}, key string) string {
	v, ok := claims[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// GetProfile 基本档案 + 配额快照。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	s := p.settings(credBlob.GetInstanceId())
	rc := p.relay(c, s)
	profile := p.profileOf(c)
	if err := rc.ensureAccess(ctx); err == nil {
		p.fillQuota(ctx, rc, profile)
	}
	return profile, nil
}

// profileOf 基础档案（展示名 + 健康）。
func (p *plugin) profileOf(c *credential) *pb.AccountProfile {
	name := shared.OrDefault(c.Email, shared.OrDefault(c.AccountID, "mirasim-account"))
	quota := map[string]string{}
	if c.Plan != "" {
		quota["plan"] = c.Plan
	}
	return &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: quota}
}

// fillQuota GET /v1/limits → account-wide / model-scoped 两组 Section（失败静默降级）。
func (p *plugin) fillQuota(ctx context.Context, rc *relayClient, profile *pb.AccountProfile) {
	raw, code, err := rc.controlDo(ctx, "GET", limitsPath)
	if err != nil || code == 404 || code == 405 {
		return // 无配额路由：不报桶
	}
	if code < 200 || code >= 300 {
		return
	}
	var payload struct {
		Windows []struct {
			Name        string   `json:"name"`
			Budget      *float64 `json:"budget"`
			Used        *float64 `json:"used"`
			ResetAt     *float64 `json:"reset_at"`
			ModelScoped bool     `json:"model_scoped"`
		} `json:"windows"`
		Degraded bool `json:"degraded"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return
	}
	var accountEntries, scopedEntries []*pb.SectionEntry
	for _, w := range payload.Windows {
		name := strings.TrimSpace(w.Name)
		if name == "" || w.Budget == nil || w.Used == nil || *w.Budget < 0 {
			continue
		}
		if math.IsNaN(*w.Budget) || math.IsInf(*w.Budget, 0) || math.IsNaN(*w.Used) || math.IsInf(*w.Used, 0) {
			continue
		}
		usedPct := 0.0
		if *w.Budget > 0 {
			usedPct = quotaUsedPercent(*w.Used / *w.Budget * 100)
		}
		desc := fmt.Sprintf("%.1f%% used", usedPct)
		if payload.Degraded {
			desc += " · service degraded"
		}
		entry := &pb.SectionEntry{
			Label: map[string]string{"en": name, "zh": name},
			Value: desc,
			Kind:  "text",
		}
		if w.ModelScoped {
			scopedEntries = append(scopedEntries, entry)
		} else {
			accountEntries = append(accountEntries, entry)
			profile.Quota["used_"+name] = fmt.Sprintf("%.1f", usedPct)
		}
	}
	var secs []*pb.ProfileSection
	if len(accountEntries) > 0 {
		secs = append(secs, &pb.ProfileSection{
			Id: "account_limits", Title: map[string]string{"zh": "账号额度", "en": "Account limits"},
			Entries: accountEntries,
		})
	}
	if len(scopedEntries) > 0 {
		secs = append(secs, &pb.ProfileSection{
			Id: "model_limits", Title: map[string]string{"zh": "模型额度", "en": "Model limits"},
			Entries: scopedEntries,
		})
	}
	if len(secs) > 0 {
		profile.Sections = secs
	}
}

// quotaUsedPercent 一位小数、≥99% 饱和到 100。
func quotaUsedPercent(v float64) float64 {
	v = roundPercent(v)
	if v >= 99 {
		return 100
	}
	return v
}

func roundPercent(v float64) float64 {
	return math.Max(0, math.Min(100, math.Round(v*10)/10))
}
