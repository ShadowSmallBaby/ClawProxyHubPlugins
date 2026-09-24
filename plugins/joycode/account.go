// account.go — 账号/凭证：凭据解析与基本档案。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential JoyCode 登录态（pt_key + userId + gateway 路由上下文）。
type credential struct {
	PtKey         string `json:"ptKey"`
	UserID        string `json:"userId"`
	ColorBaseURL  string `json:"colorBaseUrl,omitempty"`
	MasterBaseURL string `json:"masterBaseUrl,omitempty"`
	Tenant        string `json:"tenant,omitempty"`
	LoginType     string `json:"loginType,omitempty"`
	OrgFullName   string `json:"orgFullName,omitempty"`

	// 出站代理（核心注入，不参与序列化）
	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.PtKey == "" {
		return nil, fmt.Errorf("credential missing ptKey")
	}
	if c.UserID == "" {
		return nil, fmt.Errorf("credential missing userId")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// GetProfile 基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return &pb.AccountProfile{DisplayName: "joycode-" + c.UserID, Healthy: true, Quota: map[string]string{}}, nil
}
