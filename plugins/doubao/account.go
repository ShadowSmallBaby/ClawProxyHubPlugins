// account.go — 账号/凭证：豆包会话凭据解析与基本档案。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential 豆包会话凭据：认证 Cookie + 设备指纹参数（对齐官方客户端会话）。
type credential struct {
	Label    string            `json:"label,omitempty"`
	Cookies  map[string]string `json:"cookies"`
	MsToken  string            `json:"ms_token,omitempty"`
	DeviceID string            `json:"device_id,omitempty"`
	WebID    string            `json:"web_id,omitempty"`
	Fp       string            `json:"fp,omitempty"`
	BotID    string            `json:"bot_id,omitempty"`

	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析（blob 需含至少一个 Cookie）。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if blob == nil || len(blob.GetBlob()) == 0 {
		return nil, fmt.Errorf("缺少豆包凭据，请先登录")
	}
	if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
		return nil, fmt.Errorf("凭据解析失败: %w", err)
	}
	if len(c.Cookies) == 0 {
		return nil, fmt.Errorf("凭据缺少 cookies")
	}
	if c.DeviceID == "" {
		c.DeviceID = defaultDeviceID
	}
	if c.WebID == "" {
		c.WebID = defaultWebID
	}
	if c.Fp == "" {
		c.Fp = defaultFp
	}
	if c.BotID == "" {
		c.BotID = defaultBotID
	}
	if blob != nil {
		c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	}
	return c, nil
}

// GetProfile 基本档案（豆包无余额接口；凭据有效性以对话时的风控反馈为准）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.AccountProfile{Healthy: false}, nil
	}
	return &pb.AccountProfile{
		DisplayName: shared.OrDefault(c.Label, "doubao-cookie"),
		Healthy:     true, Quota: map[string]string{},
	}, nil
}
