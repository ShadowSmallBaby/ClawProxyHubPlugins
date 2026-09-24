// joycode 插件 — JoyCode（京东 AI 编程助手，joycode-api.jd.com）私有协议反代。
// 认证：JD pt_key + userId（IDE 登录态）；对话走 color gateway（HMAC 签名）OpenAI 兼容端点。
// 刷新：userInfo 接口回吐轮换后的 ptKey。
package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const (
	pluginName    = "joycode"
	clientVersion = "2.7.5"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"JoyColor/2.7.5"
	// colorGatewaySig colorGateway 签名（逆向自 JoyCode 2.7.5）
	colorGatewayAppID = "joycode_ide"
	colorGatewayPath  = "/api"
	colorHMACKey      = "0691a3f0b37b4a85aeb63ad0fc7db3ed"

	defaultColorBaseURL = "https://api-ai.jd.com"
	defaultBaseURL      = "https://joycode-api.jd.com"
)

// version 插件版本：打包时经 -ldflags "-X main.version=..." 注入（源码直跑为 dev）。
var version = "dev"

// colorEndpoint 端点 key → (functionId, direct v2 路径)。
// gateway 模式靠 query 的 functionId 路由；direct 模式用 v2 路径。
type colorEndpoint struct {
	functionID string
	v2Path     string
}

var colorEndpoints = map[string]colorEndpoint{
	"chat":     {"chat_completions", "/api/saas/openai/v2/chat/completions"},
	"models":   {"joycode_modelList", "/api/saas/models/v2/modelList"},
	"userInfo": {"joycode_userInfo", "/api/saas/user/v2/userInfo"},
}

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer
	host *sdk.Host

	mu      sync.Mutex
	oauth   map[string]*oauthSession // state → 进行中的授权会话（单条，覆盖旧的）
	oauthCB *callbackServer          // 进行中的本地回调 server（懒起，完成/超时即关）
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }

func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: version, Author: "cph",
		Label:           map[string]string{"zh": "JoyCode", "en": "JoyCode"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh"},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema:  `{"type": "object", "properties": {}}`,
		AuthMethods: []*pb.AuthMethod{
			{
				Id: "oauth", Label: map[string]string{"zh": "浏览器登录", "en": "Browser Login"}, Capabilities: []string{"refreshable"},
				Callback: "auto_wait", // 本机访问自动回调，服务器部署转手动粘贴
			},
			{
				Id: "pt_key", Label: map[string]string{"zh": "pt_key 登录态", "en": "pt_key"}, Capabilities: []string{"refreshable"},
				Fields: []*pb.AuthField{
					{Name: "ptKey", Label: map[string]string{"zh": "pt_key", "en": "pt_key"}, Type: "password", Required: true, Placeholder: "JD pt_key cookie"},
					{Name: "userId", Label: map[string]string{"zh": "用户 ID", "en": "User ID"}, Type: "text", Required: true, Placeholder: "userId"},
					{Name: "colorBaseUrl", Label: map[string]string{"zh": "Color 网关地址", "en": "Color Base URL"}, Type: "text", Placeholder: "https://api-ai.jd.com（留空用默认）"},
					{Name: "tenant", Label: map[string]string{"zh": "租户", "en": "Tenant"}, Type: "text", Placeholder: "JOYCODE（留空用默认）"},
					{Name: "loginType", Label: map[string]string{"zh": "登录类型", "en": "Login Type"}, Type: "text", Placeholder: "N_PIN_PC（留空用默认）"},
					{Name: "orgFullName", Label: map[string]string{"zh": "组织全称", "en": "Org Full Name"}, Type: "text", Placeholder: "企业版填写"},
				},
			},
		},
	}}, nil
}
