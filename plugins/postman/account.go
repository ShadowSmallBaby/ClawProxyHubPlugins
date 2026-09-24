// 账号能力：Login（api_key / cookie）、Refresh（静态密钥，原样返回）、
// GetProfile（展示名 + 健康），ListModels（静态目录）。
// Postman Agent Mode 无独立余额端点（额度只在对话 SSE 的 usage 事件里给），故不填 quota。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	c := credential{instanceID: req.GetInstanceId()}
	switch req.GetMethodId() {
	case "api_key":
		c.APIKey = strings.TrimSpace(req.GetForm()["api_key"])
		if c.APIKey == "" {
			return loginErr("API 密钥不能为空"), nil
		}
	case "cookie":
		c.Cookie = strings.TrimSpace(req.GetForm()["cookie"])
		if c.Cookie == "" {
			return loginErr("Cookie 不能为空"), nil
		}
		c.TeamID = strings.TrimSpace(req.GetForm()["team_id"])
	default:
		return loginErr("unsupported method: " + req.GetMethodId()), nil
	}
	c.WorkspaceID = strings.TrimSpace(req.GetForm()["workspace_id"])

	// 校验凭据并解析工作区（api_key 走 api.postman.com，cookie 走团队网关）。
	if c.WorkspaceID == "" {
		if c.APIKey != "" {
			id, err := p.workspaceViaAPIKey(ctx, &c)
			if err != nil {
				return loginErr("API 密钥校验失败: " + err.Error()), nil
			}
			c.WorkspaceID = id
		} else {
			site, err := p.site(req.GetInstanceId())
			if err != nil {
				return loginErr("Cookie 登录需先配置实例团队子域: " + err.Error()), nil
			}
			id, err := p.workspaceViaGateway(ctx, &c, site)
			if err != nil {
				return loginErr("Cookie 校验失败: " + err.Error()), nil
			}
			c.WorkspaceID = id
		}
	}

	blob, err := json.Marshal(c)
	if err != nil {
		return loginErr(err.Error()), nil
	}
	profile, _ := p.profile(ctx, &c)
	return &pb.LoginResult{Blob: blob, Profile: profile}, nil
}

func loginErr(msg string) *pb.LoginResult {
	return &pb.LoginResult{Error: &pb.Error{Code: 1, Message: msg}}
}

// Refresh 静态密钥类：无可刷新态，原样返回（blob 空 = 无变更），尽力附上最新展示名。
func (p *plugin) Refresh(ctx context.Context, blob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	cred, err := credFrom(blob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 1, Message: err.Error()}}, nil
	}
	profile, _ := p.profile(ctx, cred)
	return &pb.RefreshResult{Profile: profile}, nil
}

func (p *plugin) GetProfile(ctx context.Context, blob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	cred, err := credFrom(blob)
	if err != nil {
		return &pb.AccountProfile{Healthy: false}, nil
	}
	profile, err := p.profile(ctx, cred)
	if err != nil {
		return &pb.AccountProfile{DisplayName: displayName(cred), Healthy: false}, nil
	}
	return profile, nil
}

func (p *plugin) ListModels(ctx context.Context, blob *pb.CredentialBlob) (*pb.ModelList, error) {
	return listModels(), nil
}

// profile 展示名 + 健康：api_key 走 api.postman.com/me 取用户名，否则脱敏显示。
func (p *plugin) profile(ctx context.Context, cred *credential) (*pb.AccountProfile, error) {
	name := displayName(cred)
	if cred.APIKey != "" {
		if u, err := p.fetchMe(ctx, cred); err == nil && u != "" {
			name = u
		} else if err != nil {
			return &pb.AccountProfile{DisplayName: name, Healthy: false}, err
		}
	}
	return &pb.AccountProfile{DisplayName: name, Healthy: true}, nil
}

// displayName 无用户名时的脱敏展示。
func displayName(cred *credential) string {
	if cred.APIKey != "" {
		key := cred.APIKey
		if len(key) > 10 {
			return key[:8] + "***"
		}
		return "Postman API Key"
	}
	return "Postman session"
}

// fetchMe api.postman.com/me → 用户名（username > email）。
func (p *plugin) fetchMe(ctx context.Context, cred *credential) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.postman.com/me", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", cred.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", browserUA)
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("me HTTP %d", resp.StatusCode)
	}
	var out struct {
		User struct {
			Username string `json:"username"`
			Email    string `json:"email"`
		} `json:"user"`
	}
	if json.Unmarshal(data, &out) != nil {
		return "", fmt.Errorf("parse me")
	}
	if out.User.Username != "" {
		return out.User.Username, nil
	}
	return out.User.Email, nil
}
