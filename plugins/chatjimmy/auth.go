// 匿名凭据、HTTP client 与登录/账号档案。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential 匿名凭据：上游免 KEY，blob 仅承载出站代理与展示名。
type credential struct {
	Label string `json:"label,omitempty"`

	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析（blob 允许为空 = 匿名直调）。
func credFrom(blob *pb.CredentialBlob) *credential {
	c := &credential{}
	if blob != nil && len(blob.GetBlob()) > 0 {
		_ = json.Unmarshal(blob.GetBlob(), c)
	}
	if blob != nil {
		c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	}
	return c
}

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
// 连接 15s / TLS 15s / 首字节 120s（上游整段生成后一次性返回，首字节偏慢）。
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

// Login 匿名一键建档：上游免 KEY，账号仅供核心路由与分组。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if req.MethodId != "anonymous" {
		return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
	}
	c := &credential{Label: "chatjimmy-anonymous"}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: c.Label, Healthy: true, Quota: map[string]string{}},
	}, nil
}

// GetProfile 基本档案（上游无余额接口）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c := credFrom(credBlob)
	return &pb.AccountProfile{
		DisplayName: shared.OrDefault(c.Label, "chatjimmy-anonymous"),
		Healthy:     true, Quota: map[string]string{},
	}, nil
}
