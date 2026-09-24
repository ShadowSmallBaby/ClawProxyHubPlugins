// Package main — zcode 账号：coding-plan API Key 解析（z/login → customerInfo →
// api_keys → copy secretKey）+ 模型目录 + 配额快照 + 刷新。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	zcodeAPIBase = "https://zcode.z.ai/api/v1"
	zaiHost      = "https://api.z.ai"
	bigmodelHost = "https://bigmodel.cn"
	apiKeyName   = "zcode-api-key"
	// 默认机构 / 项目（UTF-8 字面量对齐上游 DEFAULT_ORG_MARKER）
	defaultOrgMarker     = "默认机构"
	defaultProjectMarker = "默认项目"
)

// requestBizApi 业务面 API（code=0/200 视为成功，其余抛错）。
func (p *plugin) requestBizApi(ctx context.Context, url, authorization string, method, body string) (json.RawMessage, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc(nil).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("biz api HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var env struct {
		Code json.Number     `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code.String() != "" {
		if c := env.Code.String(); c != "0" && c != "200" {
			return nil, fmt.Errorf("biz api error %s: %s", c, env.Msg)
		}
	}
	return env.Data, nil
}

// resolveCodingPlanKey OAuth accessToken → 上游 API Key（zai 双密钥 / bigmodel 直串）。
func (p *plugin) resolveCodingPlanKey(ctx context.Context, c *credential, accessToken string) error {
	if c.Provider == "zai" {
		// z/login：accessToken → bizToken
		body := `{"token":` + quote(accessToken) + `}`
		data, err := p.requestBizApi(ctx, zaiHost+"/api/auth/z/login", "", "POST", body)
		if err != nil {
			return fmt.Errorf("z/login: %w", err)
		}
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(data, &tok) != nil || tok.AccessToken == "" {
			return fmt.Errorf("z/login 返回缺少 access_token")
		}
		authz := "Bearer " + tok.AccessToken
		orgID, projID, err := p.resolveCustomerInfo(ctx, zaiHost, authz)
		if err != nil {
			return err
		}
		apiKey, err := p.findOrCreateApiKey(ctx, zaiHost, authz, orgID, projID)
		if err != nil {
			return err
		}
		secret, err := p.getSecretKey(ctx, zaiHost, authz, orgID, projID, apiKey)
		if err != nil || secret == "" {
			return fmt.Errorf("zai secretKey 缺失（签名必需）")
		}
		c.APIKey = apiKey + "." + secret
		return nil
	}

	// bigmodel：accessToken 直接作 Authorization
	orgID, projID, err := p.resolveCustomerInfo(ctx, bigmodelHost, accessToken)
	if err != nil {
		return err
	}
	apiKey, err := p.findOrCreateApiKey(ctx, bigmodelHost, accessToken, orgID, projID)
	if err != nil {
		return err
	}
	if secret, err := p.getSecretKey(ctx, bigmodelHost, accessToken, orgID, projID, apiKey); err == nil && secret != "" {
		apiKey = apiKey + "." + secret
	}
	c.APIKey = apiKey
	return nil
}

// resolveCustomerInfo 默认机构 + 默认项目 id。
func (p *plugin) resolveCustomerInfo(ctx context.Context, host, authz string) (orgID, projID string, err error) {
	data, err := p.requestBizApi(ctx, host+"/api/biz/customer/getCustomerInfo", authz, "GET", "")
	if err != nil {
		return "", "", fmt.Errorf("customerInfo: %w", err)
	}
	var info struct {
		Organizations []struct {
			OrganizationID   string `json:"organizationId"`
			ID               string `json:"id"`
			OrganizationName string `json:"organizationName"`
			Name             string `json:"name"`
			Projects         []struct {
				ProjectID   string `json:"projectId"`
				ID          string `json:"id"`
				ProjectName string `json:"projectName"`
				Name        string `json:"name"`
			} `json:"projects"`
		} `json:"organizations"`
	}
	if json.Unmarshal(data, &info) != nil || len(info.Organizations) == 0 {
		return "", "", fmt.Errorf("customerInfo: 无可用机构")
	}
	org := info.Organizations[0]
	for _, o := range info.Organizations {
		if strings.Contains(o.OrganizationName, defaultProjectMarker) || strings.Contains(o.Name, defaultProjectMarker) {
			org = o
			break
		}
	}
	orgID = shared.OrDefault(shared.OrDefault(org.OrganizationID, org.ID), "")
	if len(org.Projects) == 0 {
		return "", "", fmt.Errorf("customerInfo: 默认机构下无项目")
	}
	proj := org.Projects[0]
	for _, pr := range org.Projects {
		if strings.Contains(pr.ProjectName, defaultProjectMarker) || strings.Contains(pr.Name, defaultProjectMarker) {
			proj = pr
			break
		}
	}
	projID = shared.OrDefault(proj.ProjectID, proj.ID)
	return orgID, projID, nil
}

// findOrCreateApiKey 名为 zcode-api-key 的 Key（无则创建）。
func (p *plugin) findOrCreateApiKey(ctx context.Context, host, authz, orgID, projID string) (string, error) {
	listURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", host, orgID, projID)
	if data, err := p.requestBizApi(ctx, listURL, authz, "GET", ""); err == nil {
		var keys []struct {
			Name   string `json:"name"`
			APIKey string `json:"apiKey"`
		}
		if json.Unmarshal(data, &keys) == nil {
			for _, k := range keys {
				if k.Name == apiKeyName && k.APIKey != "" {
					return k.APIKey, nil
				}
			}
		}
	}
	data, err := p.requestBizApi(ctx, listURL, authz, "POST", `{"name":`+quote(apiKeyName)+`}`)
	if err != nil {
		return "", fmt.Errorf("api key create: %w", err)
	}
	var created struct {
		APIKey string `json:"apiKey"`
	}
	if json.Unmarshal(data, &created) != nil || created.APIKey == "" {
		return "", fmt.Errorf("api key create 返回缺少 apiKey")
	}
	return created.APIKey, nil
}

// getSecretKey API Key copy 接口取 secretKey（zai 必需；bigmodel 可缺）。
func (p *plugin) getSecretKey(ctx context.Context, host, authz, orgID, projID, apiKey string) (string, error) {
	url := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys/copy/%s", host, orgID, projID, apiKey)
	data, err := p.requestBizApi(ctx, url, authz, "GET", "")
	if err != nil {
		return "", err
	}
	var copied struct {
		SecretKey string `json:"secretKey"`
	}
	json.Unmarshal(data, &copied)
	return copied.SecretKey, nil
}

// ---------- 模型目录 ----------

// glmModel 静态目录条目（与上游 MODELS 同源，规格经 ZCode 3.11.2 校准）。
type glmModel struct {
	ID            string
	Label         string
	CtxWindow     int32
	MaxOut        int32
	Reasoning     bool
	StartPlanOnly bool
}

var glmModels = []glmModel{
	{ID: "glm-4.5-air", Label: "GLM 4.5 Air", CtxWindow: 131072, MaxOut: 98304, Reasoning: true},
	{ID: "glm-4.6", Label: "GLM 4.6", CtxWindow: 200000, MaxOut: 131072, Reasoning: true},
	{ID: "glm-4.6v", Label: "GLM 4.6V", CtxWindow: 131072, MaxOut: 32768},
	{ID: "glm-4.7", Label: "GLM 4.7", CtxWindow: 200000, MaxOut: 131072, Reasoning: true},
	{ID: "glm-5", Label: "GLM 5", CtxWindow: 200000, MaxOut: 64000, Reasoning: true},
	{ID: "glm-5-turbo", Label: "GLM 5 Turbo", CtxWindow: 200000, MaxOut: 64000, Reasoning: true},
	{ID: "glm-5v-turbo", Label: "GLM 5V Turbo", CtxWindow: 200000, MaxOut: 131072},
	{ID: "glm-5.1", Label: "GLM 5.1", CtxWindow: 200000, MaxOut: 64000, Reasoning: true},
	{ID: "glm-5.2", Label: "GLM 5.2", CtxWindow: 1000000, MaxOut: 128000, Reasoning: true},
	{ID: "glm-5.3", Label: "GLM 5.3", CtxWindow: 1000000, MaxOut: 128000, Reasoning: true},
	// start-plan 网关专属变体（体验套餐用）
	{ID: "glm-5.3-flash", Label: "GLM 5.3 Flash", CtxWindow: 1000000, MaxOut: 128000, Reasoning: true, StartPlanOnly: true},
}

// listModels 拉上游模型目录（静态表；凭据有效性与 GetProfile 走 billing）。
func (p *plugin) listModels(ctx context.Context, c *credential) ([]*pb.ModelInfo, error) {
	var out []*pb.ModelInfo
	for _, m := range glmModels {
		if m.StartPlanOnly && c != nil && c.Plan != "start-plan" {
			continue
		}
		out = append(out, &pb.ModelInfo{
			Id:             m.ID,
			Label:          map[string]string{"en": m.Label},
			ContextWindow:  m.CtxWindow,
			SupportsTools:  true,
			SupportsStream: true,
		})
	}
	return out, nil
}

// quote JSON 字符串字面量。
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
