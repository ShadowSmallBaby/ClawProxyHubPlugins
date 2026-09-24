// auth.go — 登录（Zen / Go tier API Key 校验）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// Login 两种方式：api_key（Zen tier）/ go_key（Go tier）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	tier := "zen"
	if req.MethodId == "go_key" {
		tier = "go"
	} else if req.MethodId != "api_key" {
		return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
	}
	key := strings.TrimSpace(req.Form["key"])
	if key == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写 API Key"}}, nil
	}
	c := &credential{Tier: tier, Key: key}

	// 校验：能列模型即有效（匿名 public key 对免费模型也放行）
	if _, err := p.listModels(ctx, c, tierBase(tier), key); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "API Key 校验失败: " + err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	name := "opencode-" + tier
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: name, Healthy: true, Quota: map[string]string{}},
	}, nil
}

func tierBase(tier string) string {
	if tier == "go" {
		return goBase
	}
	return zenBase
}

// GetProfile 基本档案（Zen 无余额接口）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return &pb.AccountProfile{DisplayName: "opencode-" + c.Tier, Healthy: true, Quota: map[string]string{}}, nil
}
