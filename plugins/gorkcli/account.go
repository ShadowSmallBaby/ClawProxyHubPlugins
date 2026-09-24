// account.go — 账号/凭证：凭据 JSON 解析（别名兼容）、JWT 解码、邮箱脱敏、档案。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// applyAuthJSON 解析 Grok CLI 凭据 JSON（兼容 snake/camel 别名）；缺 refresh_token 报错。
func (c *credential) applyAuthJSON(raw string) error {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return fmt.Errorf("invalid auth json: %w", err)
	}
	c.RefreshToken = firstStr(m, "refresh_token", "refreshToken", "oauth_refresh_token")
	c.AccessToken = firstStr(m, "access_token", "accessToken")
	c.UserID = firstStr(m, "user_id", "userId")
	c.Email = firstStr(m, "email")
	if cid := firstStr(m, "oidc_client_id", "client_id", "clientId"); cid != "" {
		c.ClientID = cid
	}
	if exp, ok := m["expires_at"].(float64); ok {
		c.ExpiresAt = exp
	}
	if c.RefreshToken == "" {
		return fmt.Errorf("auth json missing refresh_token")
	}
	return nil
}

// decodeJWT 解 JWT payload（中段 base64url），容错返回空 map（不验签）。
func decodeJWT(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return map[string]any{}
	}
	return claims
}

// firstStr 从 map 里按 keys 顺序取第一个非空字符串。
func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// maskEmail 邮箱脱敏：本地部分保留前 2 字符 + ***（无 @ 原样返回）。
func maskEmail(e string) string {
	at := strings.IndexByte(e, '@')
	if at < 0 {
		return e
	}
	local, domain := e[:at], e[at:]
	n := 2
	if len(local) < n {
		n = len(local)
	}
	return local[:n] + "***" + domain
}

// GetProfile 基本档案（xAI 无余额接口；展示名用脱敏邮箱）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	name := maskEmail(c.Email)
	if name == "" {
		name = "grok-cli"
	}
	return &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}}, nil
}
