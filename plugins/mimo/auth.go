// auth.go — 登录（凭据导入 passToken）与刷新。
package main

import (
	"context"
	"encoding/json"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// Login 凭据导入一步：贴 Desktop cookie 库 / 自导 JSON（兼容 passToken 与 pass_token 两种键）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if req.MethodId != "credential_file" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "unknown auth method: " + req.MethodId}}, nil
	}
	c, err := parseCredentialJSON(req.Form["content"])
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	// 实证：跑一次 SSO，验证 passToken 有效且能换到 serviceToken
	cfg := p.settings(req.InstanceId)
	if _, err := p.getServiceCookie(ctx, cfg, c, true); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "passToken 校验失败: " + err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{Blob: blob, Profile: p.profileOf(c)}, nil
}

// parseCredentialJSON 宽松解析：兼容 passToken/pass_token、userId/user_id、cUserId/c_user_id。
func parseCredentialJSON(content string) (*credential, error) {
	var loose map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &loose); err != nil {
		return nil, err
	}
	c := &credential{}
	for k, v := range loose {
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue
		}
		switch strings.ToLower(k) {
		case "passtoken", "pass_token":
			c.PassToken = strings.TrimSpace(s)
		case "userid", "user_id":
			c.UserID = strings.TrimSpace(s)
		case "cuserid", "c_user_id":
			c.CUserID = strings.TrimSpace(s)
		}
	}
	if c.PassToken == "" {
		return nil, errMissingPassToken
	}
	return c, nil
}

var errMissingPassToken = &fieldError{msg: "凭据需含 passToken（MiMo Desktop cookie 库导出）"}

type fieldError struct{ msg string }

func (e *fieldError) Error() string { return e.msg }

// Refresh 校验 passToken 仍能换到 serviceToken（passToken 长期有效，正常原样返回）。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	cfg := p.settings(credBlob.GetInstanceId())
	if _, err := p.getServiceCookie(ctx, cfg, c, false); err != nil {
		code := int32(503)
		if strings.Contains(err.Error(), "过期") {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: err.Error()}}, nil
	}
	return &pb.RefreshResult{Profile: p.profileOf(c)}, nil
}
