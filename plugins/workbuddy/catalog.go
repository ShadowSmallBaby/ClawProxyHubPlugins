// catalog.go — 成长任务模拟所需的目录接口：模板 / 专家 / 技能 / 主题。
// 任务列表不带模板/专家/技能 id，这里对接桌面端实际使用的列表接口。
package main

import (
	"context"
	"encoding/json"
	"fmt"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	scenesPath        = "/v2/as/support/scenes?locale=zh-cn"
	expertListPath    = "/portal/operation-platform/market/expert/list"
	skillListPath     = "/v2/operation-platform/market/skill/list"
	skillInstallPath  = "/v2/user-asset/skill/install"
	appearanceResPath = "/v2/operation-platform/appearance/resources"
	appearanceSetPath = "/v2/user-asset/appearance/set"
	expertTypeAgent   = "agent"
	expertTypeTeam    = "team"
)

// catalogListTemplates 模板场景（无提示词的剔除，不能用于"使用模板"）。
func (p *plugin) catalogListTemplates(ctx context.Context, cred *credential) ([]map[string]interface{}, error) {
	data, err := p.actGet(ctx, cred, scenesPath, "模板列表")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Scenes []struct {
			ID      interface{}   `json:"id"`
			Name    string        `json:"name"`
			Mode    string        `json:"mode"`
			Prompts []interface{} `json:"prompts"`
		} `json:"scenes"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, fmt.Errorf("模板列表解析失败")
	}
	out := []map[string]interface{}{}
	for _, s := range payload.Scenes {
		if s.ID == nil || len(s.Prompts) == 0 {
			continue
		}
		out = append(out, map[string]interface{}{
			"id": fmt.Sprint(s.ID), "name": s.Name, "mode": shared.OrDefault(s.Mode, "working"),
		})
	}
	return out, nil
}

// catalogListExperts 专家市场列表；expertType 服务端过滤之外再兜底。
func (p *plugin) catalogListExperts(ctx context.Context, cred *credential, keyword, expertType string) ([]map[string]interface{}, error) {
	body := map[string]interface{}{"page": 1, "page_size": 100, "sort_by": "reco_rank"}
	if keyword != "" {
		body["keyword"] = keyword
	}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	data, err := p.actPost(ctx, cred, expertListPath, body, "专家列表")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Experts []struct {
			ExpertID    interface{} `json:"expert_id"`
			DisplayName string      `json:"display_name_zh"`
			AgentName   string      `json:"agent_name"`
			Profession  string      `json:"profession_zh"`
			ExpertType  string      `json:"expert_type"`
			Categories  []string    `json:"categories"`
			Source      string      `json:"source"`
			Version     string      `json:"version"`
		} `json:"experts"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, fmt.Errorf("专家列表解析失败")
	}
	out := []map[string]interface{}{}
	for _, e := range payload.Experts {
		if e.ExpertID == nil {
			continue
		}
		industry := ""
		if len(e.Categories) > 0 {
			industry = e.Categories[0]
		}
		out = append(out, map[string]interface{}{
			"id": fmt.Sprint(e.ExpertID), "name": shared.OrDefault(e.DisplayName, e.AgentName),
			"title": e.Profession, "expertType": shared.OrDefault(e.ExpertType, expertTypeAgent),
			"industryId": industry, "source": shared.OrDefault(e.Source, "openapi"), "version": e.Version,
		})
	}
	if expertType != "" {
		filtered := out[:0]
		for _, e := range out {
			if mapStr(e, "expertType") == expertType {
				filtered = append(filtered, e)
			}
		}
		out = filtered
	}
	return out, nil
}

// catalogFindExpert 按关键字找一位专家。
func (p *plugin) catalogFindExpert(ctx context.Context, cred *credential, keyword string) (map[string]interface{}, error) {
	body := map[string]interface{}{"page": 1, "page_size": 20, "keyword": keyword, "sort_by": "reco_rank"}
	data, err := p.actPost(ctx, cred, expertListPath, body, "专家列表")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Experts []struct {
			ExpertID    interface{} `json:"expert_id"`
			DisplayName string      `json:"display_name_zh"`
			AgentName   string      `json:"agent_name"`
			Profession  string      `json:"profession_zh"`
			ExpertType  string      `json:"expert_type"`
			Categories  []string    `json:"categories"`
			Source      string      `json:"source"`
			Version     string      `json:"version"`
		} `json:"experts"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Experts) == 0 {
		return nil, fmt.Errorf("未找到专家：%s", keyword)
	}
	e := payload.Experts[0]
	industry := ""
	if len(e.Categories) > 0 {
		industry = e.Categories[0]
	}
	return map[string]interface{}{
		"id": fmt.Sprint(e.ExpertID), "name": shared.OrDefault(e.DisplayName, e.AgentName),
		"title": e.Profession, "expertType": shared.OrDefault(e.ExpertType, expertTypeAgent),
		"industryId": industry, "source": shared.OrDefault(e.Source, "openapi"), "version": e.Version,
	}, nil
}

// catalogListSkills 技能市场列表。
func (p *plugin) catalogListSkills(ctx context.Context, cred *credential) ([]map[string]interface{}, error) {
	data, err := p.actPost(ctx, cred, skillListPath, map[string]interface{}{"page": 1, "page_size": 20}, "技能列表")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Skills []struct {
			SkillID     interface{} `json:"skill_id"`
			Name        string      `json:"name"`
			DisplayName string      `json:"display_name_zh"`
			Version     string      `json:"version"`
		} `json:"skills"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, fmt.Errorf("技能列表解析失败")
	}
	out := []map[string]interface{}{}
	for _, s := range payload.Skills {
		if s.SkillID == nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"id": fmt.Sprint(s.SkillID), "name": shared.OrDefault(s.DisplayName, s.Name), "version": s.Version,
		})
	}
	return out, nil
}

// catalogInstallSkill 注册技能到云端用户资产（best-effort，失败由调用方决定忽略）。
func (p *plugin) catalogInstallSkill(ctx context.Context, cred *credential, skill map[string]interface{}) error {
	_, err := p.actPost(ctx, cred, skillInstallPath, map[string]interface{}{
		"items": []map[string]interface{}{{
			"source": "market", "asset_id": mapStr(skill, "id"), "version": mapStr(skill, "version"),
		}},
	}, "技能安装")
	return err
}

// catalogFindTheme 按关键字找主题（platform 固定 client）。
func (p *plugin) catalogFindTheme(ctx context.Context, cred *credential, keyword string) (map[string]interface{}, error) {
	data, err := p.actPost(ctx, cred, appearanceResPath, map[string]interface{}{
		"platform": "client", "kind": "theme", "version": clientIDEVersion(), "lang": "zh-cn",
	}, "主题列表")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Resources []struct {
			ID       interface{} `json:"id"`
			Name     string      `json:"name"`
			VipLevel string      `json:"vip_level"`
			Series   string      `json:"series"`
		} `json:"resources"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, fmt.Errorf("主题列表解析失败")
	}
	for _, r := range payload.Resources {
		if r.ID != nil && containsStr(r.Name, keyword) {
			return map[string]interface{}{
				"resourceKey": fmt.Sprint(r.ID), "name": r.Name,
				"vipLevel": shared.OrDefault(r.VipLevel, "free"), "series": shared.OrDefault(r.Series, "craft"),
			}, nil
		}
	}
	return nil, fmt.Errorf("未找到主题：%s", keyword)
}

// catalogSetTheme 主题选择同步云端（判定不依赖它，仅保持跨端一致；best-effort）。
func (p *plugin) catalogSetTheme(ctx context.Context, cred *credential, theme map[string]interface{}) error {
	_, err := p.actPost(ctx, cred, appearanceSetPath, map[string]interface{}{
		"kind": "theme", "resource_key": mapStr(theme, "resourceKey"),
	}, "主题设置")
	return err
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
