// CNB 仓库 AI 网关：流水线 Token 认证与实例配置。
package cnb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const pluginName = "cnb"
const defaultModels = "deepseek-v4.1-flash,glm-5.3-flash,kimi-k3"

type plugin struct {
	pb.UnimplementedClawPluginServer
	version string
	host    *sdk.Host
}

func New(version string) sdk.Plugin {
	if version == "" {
		version = "dev"
	}
	return &plugin{version: version}
}
func (p *plugin) SetHost(h *sdk.Host) { p.host = h }

func (p *plugin) Handshake(_ context.Context, r *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if r.GetProtocolVersion() != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{Code: 1, Message: "protocol mismatch"}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name: pluginName, Version: p.version, Author: "cph", Label: map[string]string{"zh": "CNB", "en": "CNB"}, ProtocolVersion: sdk.ProtocolVersion,
		Capabilities: []string{"chat", "models", "login", sdk.CapabilityInstances}, Endpoints: []string{"chat_completions", "messages", "responses"},
		SettingsSchema: `{"type":"object","properties":{}}`,
		InstanceSchema: `{"type":"object","properties":{"repo":{"type":"string","title":"CNB 仓库路径","description":"org/repo；实例地址留空使用 https://api.cnb.cool"},"models":{"type":"string","title":"模型列表","description":"逗号分隔；留空使用参考项目的兜底目录，实际权限以上游为准"}},"required":["repo"]}`,
		AuthMethods:    []*pb.AuthMethod{{Id: "token", Label: map[string]string{"zh": "流水线 Token", "en": "Pipeline token"}, Fields: []*pb.AuthField{{Name: "token", Type: "password", Required: true, Label: map[string]string{"zh": "CNB_TOKEN", "en": "CNB_TOKEN"}}}}},
	}}, nil
}

type site struct {
	BaseURL string `json:"base_url"`
	Repo    string `json:"repo"`
	Models  string `json:"models"`
}

func (p *plugin) site(id int64) site {
	s := site{BaseURL: "https://api.cnb.cool", Models: defaultModels}
	if p.host != nil {
		_ = json.Unmarshal(p.host.InstanceSettings(pluginName, id), &s)
	}
	if s.BaseURL == "" {
		s.BaseURL = "https://api.cnb.cool"
	}
	if s.Models == "" {
		s.Models = defaultModels
	}
	return s
}

var slugPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (s site) endpoint() (string, error) {
	u, err := url.Parse(strings.TrimRight(s.BaseURL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid CNB instance URL")
	}
	parts := strings.Split(strings.Trim(s.Repo, "/"), "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("CNB repo must be org/repo")
	}
	for _, part := range parts {
		if !slugPart.MatchString(part) || part == "." || part == ".." {
			return "", fmt.Errorf("invalid CNB repo path")
		}
	}
	return strings.TrimRight(u.String(), "/") + "/" + strings.Join(parts, "/") + "/-/ai/chat/completions", nil
}

type credential struct {
	Token string `json:"token"`
}

func credFrom(b *pb.CredentialBlob) (credential, error) {
	var c credential
	if json.Unmarshal(b.GetBlob(), &c) != nil || strings.TrimSpace(c.Token) == "" || strings.ContainsAny(c.Token, "\r\n") {
		return c, fmt.Errorf("missing or invalid CNB token")
	}
	return c, nil
}
func (p *plugin) Login(_ context.Context, r *pb.LoginRequest) (*pb.LoginResult, error) {
	if r.GetMethodId() != "token" {
		return nil, fmt.Errorf("unsupported auth method")
	}
	if _, err := p.site(r.GetInstanceId()).endpoint(); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(credential{Token: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.Form["token"]), "Bearer "))})
	if _, err := credFrom(&pb.CredentialBlob{Blob: raw}); err != nil {
		return nil, err
	}
	// CNB 没有无消耗的认证接口，保存凭据不宣称远端已校验。
	return &pb.LoginResult{Blob: raw, Profile: &pb.AccountProfile{DisplayName: "CNB · " + p.site(r.GetInstanceId()).Repo}}, nil
}
func (p *plugin) ListModels(_ context.Context, b *pb.CredentialBlob) (*pb.ModelList, error) {
	out := &pb.ModelList{}
	seen := map[string]bool{}
	for id := range strings.SplitSeq(p.site(b.GetInstanceId()).Models, ",") {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out.Models = append(out.Models, &pb.ModelInfo{Id: id, Label: map[string]string{"zh": id, "en": id}, SupportsTools: true, SupportsStream: true})
		}
	}
	return out, nil
}
