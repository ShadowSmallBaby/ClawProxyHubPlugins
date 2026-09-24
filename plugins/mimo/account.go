// account.go — 账号档案（展示名 = userId 或固定名）。
package main

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// GetProfile 基本档案（展示名 = userId 或固定名）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return p.profileOf(c), nil
}

// profileOf 基础档案。
func (p *plugin) profileOf(c *credential) *pb.AccountProfile {
	name := shared.OrDefault(c.UserID, "mimo-account")
	return &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}}
}
