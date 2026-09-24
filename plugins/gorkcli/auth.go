// auth.go — 认证/登录：xAI OIDC refresh_token 换 access_token，登录与刷新。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Login 两种方式：refresh_token（直填）/ auth_file（Grok CLI 凭据 JSON）。校验即刷新一次。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	c := &credential{}
	switch req.MethodId {
	case "refresh_token":
		c.RefreshToken = strings.TrimSpace(req.Form["refresh_token"])
		if c.RefreshToken == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 refresh_token"}}, nil
		}
	case "auth_file":
		if err := c.applyAuthJSON(req.Form["auth_json"]); err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
		}
	default:
		return nil, status.Error(codes.NotFound, "unknown auth method: "+req.MethodId)
	}
	if err := p.doRefresh(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "凭据校验失败: " + err.Error()}}, nil
	}
	return loginDone(c), nil
}

// loginDone 凭据 → 建档结果（展示名用脱敏邮箱）。
func loginDone(c *credential) *pb.LoginResult {
	blob, _ := json.Marshal(c)
	name := maskEmail(c.Email)
	if name == "" {
		name = "grok-cli"
	}
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}},
	}
}

// Refresh 用 refresh_token 刷新 access_token（gRPC 刷新接口）。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if c.RefreshToken == "" {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "没有 refresh_token，请重新登录"}}, nil
	}
	if err := p.doRefresh(ctx, c); err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	name := maskEmail(c.Email)
	if name == "" {
		name = "grok-cli"
	}
	return &pb.RefreshResult{Blob: blob, Profile: &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}}}, nil
}

// doRefresh POST auth.x.ai/oauth2/token（grant_type=refresh_token）→ 更新 access_token/过期/身份。
func (p *plugin) doRefresh(ctx context.Context, c *credential) error {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.clientID()},
		"refresh_token": {c.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("refresh failed: HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var tok struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		IDToken      string  `json:"id_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh response missing access_token")
	}
	c.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		c.ExpiresAt = float64(time.Now().Unix()) + tok.ExpiresIn
	}
	// 从 access_token（或 id_token）JWT 补身份
	claims := decodeJWT(tok.AccessToken)
	if len(claims) == 0 {
		claims = decodeJWT(tok.IDToken)
	}
	if email := firstStr(claims, "email"); email != "" {
		c.Email = email
	}
	if uid := firstStr(claims, "sub", "user_id"); uid != "" {
		c.UserID = uid
	}
	if exp, ok := claims["exp"].(float64); ok && c.ExpiresAt == 0 {
		c.ExpiresAt = exp
	}
	return nil
}
