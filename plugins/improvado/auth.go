// 凭据解析、HTTP client 与登录/账号档案。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential 凭据 blob：Cookie 头原文（浏览器导出）+ workspace/provider/effort。
type credential struct {
	Cookie      string `json:"cookie"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	Provider    string `json:"provider,omitempty"` // 默认 codex-cli
	Effort      string `json:"effort,omitempty"`   // 默认 xhigh

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{Provider: "codex-cli", Effort: "xhigh"}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.Provider == "" {
		c.Provider = "codex-cli"
	}
	if c.Effort == "" {
		c.Effort = "xhigh"
	}
	if c.Cookie == "" {
		return nil, fmt.Errorf("credential missing cookie")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// hc 凭据对应的 HTTP client；流式对话整体不设超时（长回复合法）。
func (p *plugin) hc(cred *credential) *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	if cred != nil && cred.proxyURL != "" {
		if u, err := url.Parse(cred.proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: transport}
}

// Login Cookie 校验：格式检查（真实校验走首次 Chat）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	cookie := strings.TrimSpace(req.Form["cookie"])
	ws := strings.TrimSpace(req.Form["workspace_id"])
	if cookie == "" || ws == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 Cookie 与 Workspace ID"}}, nil
	}
	if _, err := parseWorkspaceID(ws); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "Workspace ID 必须为数字"}}, nil
	}
	c := &credential{
		Cookie:      cookie,
		WorkspaceID: ws,
		Provider:    shared.OrDefault(strings.TrimSpace(req.Form["provider"]), "codex-cli"),
		Effort:      shared.OrDefault(strings.TrimSpace(req.Form["effort"]), "xhigh"),
	}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: "improvado-ws-" + ws, Healthy: true, Quota: map[string]string{}},
	}, nil
}

// parseWorkspaceID Workspace ID 必须为数字。
func parseWorkspaceID(s string) (int64, error) {
	var n int64
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		n = n*10 + int64(ch-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("empty")
	}
	return n, nil
}

// GetProfile 基本档案（上游无余额接口）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return &pb.AccountProfile{DisplayName: "improvado-ws-" + c.WorkspaceID, Healthy: true, Quota: map[string]string{}}, nil
}
