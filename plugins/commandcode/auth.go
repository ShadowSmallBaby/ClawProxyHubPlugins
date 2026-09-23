// auth：登录（Login）+ 档案（GetProfile）+ 凭据 blob。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// credential CC 就是一个 user_ 前缀的 API Key；设备指纹随凭据持久化
// （登录时按当时的 salt/profile 生成，之后该账号永远同一台设备）。
type credential struct {
	Key         string       `json:"key"`
	Fingerprint *fingerprint `json:"fingerprint,omitempty"`

	proxyURL string `json:"-"` // 出站代理（核心注入，不参与序列化）
}

var keyRE = regexp.MustCompile(`user_[a-zA-Z0-9_-]+`)

// extractKey 从任意输入提取 user_ 前缀 key。
func extractKey(s string) string { return keyRE.FindString(s) }

func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if len(blob.GetBlob()) > 0 {
		if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
			return nil, fmt.Errorf("invalid credential: %w", err)
		}
	}
	if c.Key == "" {
		return nil, fmt.Errorf("credential missing key")
	}
	c.proxyURL = shared.ProxyURL(blob.GetProxy())
	return c, nil
}

// fingerprintFor 账号设备指纹：blob 持久化的优先；旧账号（blob 无指纹）按当前设置即时生成。
func (p *plugin) fingerprintFor(c *credential) fingerprint {
	if c.Fingerprint != nil {
		return *c.Fingerprint
	}
	cfg := p.ccConfigFrom()
	return generateFingerprint(c.Key, cfg.fingerprintSalt, cfg.profile)
}

// Login 单一方式 api_key：校验 user_ 前缀 + 能列模型即有效；
// 设备指纹按登录时的 salt/profile 生成并写入 blob（随账号持久化）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if req.MethodId != "api_key" {
		return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
	}
	key := extractKey(req.Form["key"])
	if key == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "请填写有效的 API Key（须含 user_ 前缀）"}}, nil
	}
	c := &credential{Key: key}
	if _, err := p.fetchModels(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "API Key 校验失败: " + err.Error()}}, nil
	}
	cfg := p.ccConfigFrom()
	fp := generateFingerprint(key, cfg.fingerprintSalt, cfg.profile)
	c.Fingerprint = &fp
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{
		Blob:    blob,
		Profile: &pb.AccountProfile{DisplayName: maskKey(key), Healthy: true, Quota: map[string]string{}},
	}, nil
}

// GetProfile CC 无余额接口 → 基本档案。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	return &pb.AccountProfile{DisplayName: maskKey(c.Key), Healthy: true, Quota: map[string]string{}}, nil
}

// maskKey 脱敏展示名：user_ + 前 4 位 + …
func maskKey(key string) string {
	body := strings.TrimPrefix(key, "user_")
	if len(body) <= 4 {
		return "commandcode-" + body
	}
	return "commandcode-" + body[:4] + "…"
}
