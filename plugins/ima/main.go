// ima 插件 — 腾讯 ima（ima.qq.com）客户端协议反代。
// 上游：POST /cgi-bin/assistant/qa SSE + /cgi-bin/session_logic/init_session，免 KEY。
// 登录：微信扫码（auth.go）或粘贴 x-ima-cookie 导入（含 IMA-REFRESH-TOKEN 则自动续期）。
// 模型：官方 get_models 同步（免登录），失败沿用内置表。
// 工具：IMA 不支持原生 function calling，走 prompt 注入 + <function_call> 块解析。
// 分层：main（骨架/设置/会话缓存）/ auth（登录+凭据）/ model（模型目录）/
// upstream（协议收敛）/ envelope（question 拼装）/ chat（编排+过滤器）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const pluginName = "ima"

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu          sync.Mutex
	models      []imaModel
	modelsAt    time.Time
	modelsFound bool
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

// settingStr 读插件设置（核心管理界面在线编辑）。GetSettings 每次 RPC 直查，
// 核心侧无缓存；改配置即时生效，无需插件层再加缓存。
func (p *plugin) settingStr(key string) string {
	if r := p.host.Settings(pluginName); len(r) > 0 {
		var cfg map[string]string
		if json.Unmarshal(r, &cfg) == nil {
			return cfg[key]
		}
	}
	return ""
}

// ---------- Manifest / 登录 ----------

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "ima", "en": "ima"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages"},
		SettingsSchema: `{
			"type": "object",
			"properties": {
				"user_agent": {"type": "string", "title": "User-Agent", "description": "上游请求 User-Agent，留空使用内置 okhttp 形态", "default": ""},
				"web_version": {"type": "string", "title": "WEB-VERSION", "description": "ima Web 客户端版本号（cookie 的 WEB-VERSION），上游据它判定模型权限；留空用内置。官方 Web 升级后新模型报失效可在此更新", "default": ""}
			}
		}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "qr", Label: map[string]string{"zh": "微信扫码登录", "en": "WeChat QR Login"},
				Callback: "auto",
				// qr 凭据同样带 IMA-REFRESH-TOKEN，标记让核心调度 Refresh 自动续期
				Capabilities: []string{"refreshable"},
			},
			{
				Id: "cookie", Label: map[string]string{"zh": "Cookie 导入", "en": "Cookie Header"},
				Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{{
					Name: "content", Label: map[string]string{"zh": "x-ima-cookie", "en": "x-ima-cookie"},
					Type: "textarea", Required: true,
					Placeholder: "粘贴请求头 x-ima-cookie 完整值（含 IMA-REFRESH-TOKEN 则自动续期）",
				}},
			},
		},
	}}, nil
}

// ---------- 刷新 / 档案 ----------

// Refresh 用 refresh_token 换新 IMA-TOKEN（实测票据不轮换，可长期使用）；无票据则原样回档。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if c.RefreshToken != "" {
		valid, err := p.refreshToken(ctx, c)
		if err != nil {
			return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: "刷新失败：" + err.Error()}}, nil
		}
		quota := map[string]string{}
		if valid > 0 {
			quota["token_valid_seconds"] = fmt.Sprintf("%d", valid)
		}
		blob, _ := json.Marshal(c)
		return &pb.RefreshResult{Blob: blob, Profile: &pb.AccountProfile{
			DisplayName: credentialName(c), Healthy: true, Quota: quota,
		}}, nil
	}
	// 无 refresh_token：探测会话确认有效即可
	if err := p.probeSession(ctx, c); err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
	}
	return &pb.RefreshResult{Profile: &pb.AccountProfile{DisplayName: credentialName(c), Healthy: true, Quota: map[string]string{}}}, nil
}

// GetProfile 账号全貌（init_session 探测；ima 无余额接口）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	if err := p.probeSession(ctx, c); err != nil {
		return &pb.AccountProfile{DisplayName: credentialName(c), Healthy: false}, nil
	}
	return &pb.AccountProfile{DisplayName: credentialName(c), Healthy: true, Quota: map[string]string{}}, nil
}

// ---------- 会话缓存 ----------

// sessionEntry 会话缓存：按 账号+对话 绑定，避免串台。
type sessionEntry struct {
	id string
	ts time.Time
}

var (
	sessionMu   sync.Mutex
	sessionPool = map[string]*sessionEntry{}
)

// cachedSession 取缓存会话（30 分钟 TTL）。
// 每次存取顺带清扫过期项，控制池上限，防多账号长跑内存无界增长。
func cachedSession(key string) string {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	gcSessionsLocked()
	e, ok := sessionPool[key]
	if !ok {
		return ""
	}
	if time.Since(e.ts) > 30*time.Minute {
		delete(sessionPool, key)
		return ""
	}
	e.ts = time.Now()
	return e.id
}

func storeSession(key, id string) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	sessionPool[key] = &sessionEntry{id: id, ts: time.Now()}
}

// gcSessionsLocked 清扫全部过期项（须持 sessionMu）。
func gcSessionsLocked() {
	now := time.Now()
	for k, e := range sessionPool {
		if now.Sub(e.ts) > 30*time.Minute {
			delete(sessionPool, k)
		}
	}
}
