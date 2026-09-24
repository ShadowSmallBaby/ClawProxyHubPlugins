// 登录/刷新/校验（凭据解析与档案见 account.go）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
// 连接 15s / TLS 15s / 首字节 60s，流式对话整体不设超时（长回复合法）。
func (p *plugin) hc(cred *credential) *http.Client {
	key := ""
	if cred != nil {
		key = cred.proxyURL
	}
	if c, ok := proxyClients.Load(key); ok {
		return c.(*http.Client)
	}
	c := sdk.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
}

// validate userInfo 校验 + 回吐轮换后的 ptKey（code==0 即有效）。
func (p *plugin) validate(ctx context.Context, c *credential) error {
	resp, err := p.postJSON(ctx, c, "userInfo", map[string]interface{}{})
	if err != nil {
		return err
	}
	code, ok := resp["code"].(float64)
	if !ok || code != 0 {
		msg, _ := resp["msg"].(string)
		return fmt.Errorf("userInfo 校验失败 (code=%.0f): %s", code, shared.OrDefault(msg, "unknown"))
	}
	if data, ok := resp["data"].(map[string]interface{}); ok {
		if k, _ := data["ptKey"].(string); k != "" {
			c.PtKey = k // 轮换
		}
		if c.UserID == "" {
			if id, _ := data["userId"].(string); id != "" {
				c.UserID = id
			}
		}
	}
	return nil
}

// Login 浏览器授权（oauth）或 pt_key + userId 手填校验：userInfo 成功即有效。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "oauth":
		return p.loginOAuth(ctx, req)
	case "pt_key":
		ptKey := strings.TrimSpace(req.Form["ptKey"])
		userID := strings.TrimSpace(req.Form["userId"])
		if ptKey == "" || userID == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 pt_key 与 用户 ID"}}, nil
		}
		c := &credential{
			PtKey:        ptKey,
			UserID:       userID,
			ColorBaseURL: strings.TrimSpace(req.Form["colorBaseUrl"]),
			Tenant:       strings.TrimSpace(req.Form["tenant"]),
			LoginType:    strings.TrimSpace(req.Form["loginType"]),
			OrgFullName:  strings.TrimSpace(req.Form["orgFullName"]),
		}
		if err := p.validate(ctx, c); err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "校验失败: " + err.Error()}}, nil
		}
		return loginDone(c), nil
	}
	return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
}

// loginDone 凭据序列化为登录结果（含基础档案）。
func loginDone(c *credential) *pb.LoginResult {
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: "joycode-" + c.UserID, Healthy: true, Quota: map[string]string{}},
	}
}

// Refresh userInfo 轮换 ptKey，写回 blob。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if err := p.validate(ctx, c); err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: &pb.AccountProfile{
		DisplayName: "joycode-" + c.UserID, Healthy: true, Quota: map[string]string{},
	}}, nil
}
