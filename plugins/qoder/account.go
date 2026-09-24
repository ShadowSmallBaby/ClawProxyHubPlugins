// account.go — 账号/凭证：凭据解析（PAT + 机器身份）与基本档案。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

type credential struct {
	PAT                string `json:"pat"`
	RefreshToken       string `json:"refreshToken,omitempty"`
	SecurityOauthToken string `json:"securityOauthToken,omitempty"`
	ExpireTime         int64  `json:"expireTime,omitempty"` // jobToken 到期（unix 毫秒），0 为未知
	UserID             string `json:"userId,omitempty"`
	UserName           string `json:"userName,omitempty"`
	UserType           string `json:"userType,omitempty"`

	// 会话态（机器身份，整个凭据生命周期稳定）
	MachineID    string `json:"machineId,omitempty"`
	MachineToken string `json:"machineToken,omitempty"`
	MachineType  string `json:"machineType,omitempty"`

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
	if c.PAT == "" {
		return nil, fmt.Errorf("credential missing PAT")
	}
	if c.MachineID == "" {
		c.MachineID = shared.RandHex(16)
		c.MachineToken = base64.RawURLEncoding.EncodeToString([]byte(shared.RandHex(25)))
		c.MachineType = shared.RandHex(9)
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// GetProfile 基本档案（user_status 需额外接口，固定展示名 + 健康）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return &pb.AccountProfile{DisplayName: shared.OrDefault(c.UserName, "qoder-account"), Healthy: true, Quota: map[string]string{}}, nil
}
