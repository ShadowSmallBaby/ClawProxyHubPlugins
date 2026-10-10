// main.go — 入口 + Handshake/Login + 设置缓存。
//
// codearts 插件：华为云 CodeArts Agent 反代。凭据为 IAM AK/SK/security_token
// （AK 导入，无 OAuth），模型与对话走 snap-access 网关 SDK-HMAC-SHA256 签名；
// 业务积分（每日签到）走 snap-manager 统计与 ops 端点。
package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const pluginName = "codearts"

const settingsTTL = 30 * time.Second

// New 由平台入口注入版本，桌面与 Android 复用业务实现。
func New(version string) sdk.Plugin {
	if version == "" {
		version = "dev"
	}
	return &plugin{version: version}
}

type plugin struct {
	version string
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu           sync.Mutex
	settingsJSON []byte // 插件设置缓存（30s）
	settingsAt   time.Time
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// ---------- 设置 ----------

// settingStr 读插件设置（核心管理界面在线编辑），30s 内存缓存。
func (p *plugin) settingStr(key string) string {
	p.mu.Lock()
	fresh := p.settingsJSON != nil && time.Since(p.settingsAt) < settingsTTL
	raw := p.settingsJSON
	p.mu.Unlock()
	if !fresh {
		if r := p.host.Settings(pluginName); r != nil {
			raw = r
		} else {
			raw = []byte("{}")
		}
		p.mu.Lock()
		p.settingsJSON, p.settingsAt = raw, time.Now()
		p.mu.Unlock()
	}
	var cfg map[string]string
	if json.Unmarshal(raw, &cfg) == nil {
		return cfg[key]
	}
	return ""
}

// ---------- Manifest / 登录 ----------

const settingsSchema = `{
	"type": "object",
	"properties": {}
}`

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: p.version, Author: "cph",
		Label:           map[string]string{"zh": "CodeArts", "en": "CodeArts"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "account", "tasks"},
		Endpoints:       []string{"chat_completions"},
		SettingsSchema:  settingsSchema,
		AuthMethods: []*pb.AuthMethod{
			{
				Id:           "ak_import",
				Label:        map[string]string{"zh": "AK/SK 导入", "en": "AK/SK Import"},
				Capabilities: []string{"profile", "refreshable"},
				Fields: []*pb.AuthField{
					{
						Name:        "access_key",
						Label:       map[string]string{"zh": "Access Key", "en": "Access Key"},
						Type:        "text",
						Required:    true,
						Placeholder: "IAM 用户 Access Key（myhuaweicloud.com → 我的凭证）",
					},
					{
						Name:     "secret_key",
						Label:    map[string]string{"zh": "Secret Key", "en": "Secret Key"},
						Type:     "textarea",
						Required: true,
					},
					{
						Name:        "security_token",
						Label:       map[string]string{"zh": "Security Token（可选）", "en": "Security Token (optional)"},
						Type:        "textarea",
						Required:    false,
						Placeholder: "临时凭据的 security token，永久 AK 留空",
					},
				},
			},
		},
	}}, nil
}

// Login 单方式 ak_import：AK/SK（+可选 security_token），建档前校验（拉模型目录）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if req.MethodId != "ak_import" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "unknown auth method: " + req.MethodId}}, nil
	}
	c := &credential{
		AccessKey:     req.Form["access_key"],
		SecretKey:     req.Form["secret_key"],
		SecurityToken: req.Form["security_token"],
		Label:         "codearts",
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "Access Key / Secret Key 不能为空"}}, nil
	}
	blob, _ := json.Marshal(c)
	if _, err := p.ListModels(ctx, &pb.CredentialBlob{Blob: blob}); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "AK/SK 校验失败：" + err.Error()}}, nil
	}
	prof, _ := p.GetProfile(ctx, &pb.CredentialBlob{Blob: blob})
	return &pb.LoginResult{Blob: blob, Profile: prof}, nil
}
