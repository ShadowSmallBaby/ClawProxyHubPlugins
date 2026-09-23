// ima 插件 — 腾讯 ima（ima.qq.com）客户端协议反代（照 ima2api）。
// 上游：POST /cgi-bin/assistant/qa SSE + /cgi-bin/session_logic/init_session，免 KEY。
// 登录：微信扫码（auth.go）或粘贴 x-ima-cookie 导入（含 IMA-REFRESH-TOKEN 则自动续期）。
// 模型：启动时从官方 get_models 同步（免登录），失败沿用内置表。
// 工具：IMA 不支持原生 function calling，走 prompt 注入 + <function_call> 块解析。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
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

// settingStr 读插件设置（核心管理界面在线编辑），30s 内存缓存。
func (p *plugin) settingStr(key string) string {
	if r := p.host.Settings(pluginName); len(r) > 0 {
		var cfg map[string]string
		if json.Unmarshal(r, &cfg) == nil {
			return cfg[key]
		}
	}
	return ""
}

// ---------- 凭据 blob ----------

type credential struct {
	Cookie       string `json:"cookie"`                  // x-ima-cookie 完整值
	RefreshToken string `json:"refresh_token,omitempty"` // 长期刷新票据（不轮换）
	Name         string `json:"name,omitempty"`

	proxyURL string `json:"-"`
}

// credFrom 凭据 + 代理配置一起解析。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	c := &credential{}
	if blob == nil || len(blob.GetBlob()) == 0 {
		return nil, fmt.Errorf("缺少 ima 凭据，请先登录")
	}
	if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
		return nil, fmt.Errorf("凭据解析失败: %w", err)
	}
	if c.Cookie == "" {
		return nil, fmt.Errorf("凭据缺少 cookie")
	}
	c.proxyURL = shared.ProxyURL(blob.GetProxy())
	return c, nil
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

// ---------- 模型 ----------

// imaModel 上游模型条目（主模型 + think 子模型）。
type imaModel struct {
	ID        string // 插件模型 id（slug）
	Type      int64  // 上游 model_type
	UpID      string // 上游 model_id
	Name      string // 上游 model_name
	ThinkType int64
	ThinkUpID string
}

// builtinModels 内置模型表兜底（官方接口不可用时）。
var builtinModels = []imaModel{
	{ID: "hy3-preview", Type: 0, UpID: "official_0", Name: "Tencent Hy3 preview", ThinkType: 2, ThinkUpID: "official_2"},
	{ID: "deepseek-v4-flash", Type: 3, UpID: "official_3", Name: "DeepSeek V4-Flash", ThinkType: 1, ThinkUpID: "official_1"},
	{ID: "glm-5.2", Type: 3000, UpID: "official_3000", Name: "GLM-5.2", ThinkType: 3001, ThinkUpID: "official_3001"},
}

// currentModels 模型表（10 分钟缓存；官方接口免登录，缓存过期时后台拉取）。
func (p *plugin) currentModels(ctx context.Context) []imaModel {
	p.mu.Lock()
	fresh := p.modelsFound && time.Since(p.modelsAt) < 10*time.Minute
	models := p.models
	p.mu.Unlock()
	if fresh && len(models) > 0 {
		return models
	}
	if synced, err := p.fetchUpstreamModels(ctx); err == nil && len(synced) > 0 {
		p.mu.Lock()
		p.models, p.modelsAt, p.modelsFound = synced, time.Now(), true
		p.mu.Unlock()
		return synced
	}
	if len(models) > 0 {
		return models
	}
	return builtinModels
}

// fetchUpstreamModels POST /cgi-bin/model_manage/get_models（免登录）。
func (p *plugin) fetchUpstreamModels(ctx context.Context) ([]imaModel, error) {
	resp, err := p.imaPost(ctx, nil, nil, pathModels, map[string]interface{}{}, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := ioReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Code   int64 `json:"code"`
		Models []struct {
			ModelName     string `json:"model_name"`
			ModelType     int64  `json:"model_type"`
			ModelID       string `json:"model_id"`
			IsDefault     bool   `json:"is_default"`
			SubModelInfos map[string]struct {
				ModelType int64  `json:"model_type"`
				ModelID   string `json:"model_id"`
			} `json:"sub_model_infos"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Code != 0 {
		return nil, fmt.Errorf("get_models 返回异常")
	}
	var models []imaModel
	for _, m := range out.Models {
		if m.ModelName == "" {
			continue
		}
		mm := imaModel{
			ID: slugModelKey(m.ModelName), Type: m.ModelType,
			UpID: shared.OrDefault(m.ModelID, fmt.Sprintf("official_%d", m.ModelType)), Name: m.ModelName,
		}
		if think, ok := m.SubModelInfos["1"]; ok && think.ModelType != m.ModelType {
			mm.ThinkType = think.ModelType
			mm.ThinkUpID = shared.OrDefault(think.ModelID, fmt.Sprintf("official_%d", think.ModelType))
		}
		models = append(models, mm)
	}
	return models, nil
}

// ListModels 模型目录：主模型 + -think 变体（IMA 思考是官方 sub_model）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	var models []*pb.ModelInfo
	for _, m := range p.currentModels(ctx) {
		models = append(models, &pb.ModelInfo{
			Id: m.ID, Label: map[string]string{"zh": m.Name, "en": m.ID},
			SupportsTools: true, SupportsStream: true,
		})
		if m.ThinkUpID != "" {
			models = append(models, &pb.ModelInfo{
				Id: m.ID + "-think", Label: map[string]string{"zh": m.Name + " (Think)", "en": m.ID + "-think"},
				SupportsTools: true, SupportsStream: true,
			})
		}
	}
	return &pb.ModelList{Models: models}, nil
}

// resolveModel 请求 id → 上游 (model_type, model_id)；-think 后缀走思考子模型，未知回退默认。
func resolveModel(models []imaModel, requested string) (int64, string) {
	if len(models) == 0 {
		return builtinModels[0].Type, builtinModels[0].UpID
	}
	if requested != "" {
		lower := strings.ToLower(requested)
		if m := strings.TrimSuffix(lower, "-think"); m != lower {
			for _, mm := range models {
				if mm.ID == m && mm.ThinkUpID != "" {
					return mm.ThinkType, mm.ThinkUpID
				}
			}
		}
		for _, mm := range models {
			if strings.EqualFold(mm.ID, requested) || mm.Name == requested {
				return mm.Type, mm.UpID
			}
		}
	}
	def := models[0]
	for _, mm := range models {
		if strings.Contains(mm.Name, "Hy3") {
			def = mm
			break
		}
	}
	return def.Type, def.UpID
}

// slugModelKey 模型名 → 插件 id（与 ima2api slugModelKey 同构）。
func slugModelKey(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
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
func cachedSession(key string) string {
	sessionMu.Lock()
	defer sessionMu.Unlock()
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

// cjkRe 判断文本是否含汉字（对话时据此给模型加中文回复提示）。
var cjkRe = regexp.MustCompile(`\p{Han}`)
