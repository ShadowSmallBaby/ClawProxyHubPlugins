// model.go — 模型目录（batch_get_detail_param 多通道）。
package trae

import (
	"context"
	"encoding/json"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// fallbackModels 兜底目录（数值是估值，远端可用时完全采信远端）。
var fallbackModels = []struct{ id, name string }{
	{"DeepSeek-V4-Flash-Official", "DeepSeek V4 Flash Official"},
	{"Doubao-Seed-2.1-Pro", "Doubao Seed 2.1 Pro"},
	{"seed-code-pro-0430", "Seed Code Pro 0430"},
	{"Doubao-Seed-2.1-Turbo", "Doubao Seed 2.1 Turbo"},
	{"Doubao-Seed-2.0-Code", "Doubao Seed 2.0 Code"},
	{"glm-5.2", "GLM-5.2"},
	{"glm-5-turbo", "GLM-5 Turbo"},
	{"glm-5", "GLM-5"},
	{"DeepSeek-V4-Pro", "DeepSeek V4 Pro"},
	{"DeepSeek-V4-Flash", "DeepSeek V4 Flash"},
	{"kimi-k3", "Kimi K3"},
	{"kimi-k2.7-code", "Kimi K2.7 Code"},
	{"kimi-k2.6", "Kimi K2.6"},
	{"minimax-m3", "MiniMax M3"},
	{"qwen-3.7-plus", "Qwen 3.7 Plus"},
	{"sagitta", "Sagitta"},
	{"aquila", "Aquila"},
}

// modelChannel 模型 → 提供它的聊天通道（Chat 时据此路由；⚠️ 模型只在列出
// 它的通道里可调用，跨通道发 4001）。

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	// batch_get_detail_param：一次传多个 functions，响应 function_configs[]
	// 为每通道各自一套目录（单通道端点看不到 agent 专有模型）
	body, _ := json.Marshal(map[string]interface{}{
		"functions": []string{"solo_agent", "solo_work_lite", "solo_agent_remote"},
	})
	req, err := httpNewReq(ctx, "POST", agentHost+pathBatchModel, body)
	if err != nil {
		return nil, err
	}
	for k, v := range p.soloHeaders(cred, false) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return p.fallbackList(), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return p.fallbackList(), nil
	}
	models := p.parseBatchModelList(readResp(resp))
	if len(models) == 0 {
		return p.fallbackList(), nil
	}
	return &pb.ModelList{Models: models}, nil
}

// parseBatchModelList 解析多通道目录：usage=chat_completion 的条目，
// 剔除 display_config.is_custom_model === true（选中即报 4001）。
func (p *plugin) parseBatchModelList(raw []byte) []*pb.ModelInfo {
	var obj struct {
		FunctionConfigs []struct {
			Function       string `json:"function"`
			ConfigInfoList []struct {
				ModelDetail string `json:"model_detail"`
			} `json:"config_info_list"`
		} `json:"function_configs"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	seen := map[string]bool{}
	var models []*pb.ModelInfo
	for _, fc := range obj.FunctionConfigs {
		for _, ci := range fc.ConfigInfoList {
			// model_detail 是 JSON 字符串，须二次 parse
			var md struct {
				ID            string `json:"id"`
				ModelName     string `json:"model_name"`
				Usage         string `json:"usage"`
				DisplayConfig *struct {
					IsCustomModel bool `json:"is_custom_model"`
				} `json:"display_config"`
				IsInvisibleToUser bool `json:"is_invisible_to_user"`
			}
			if json.Unmarshal([]byte(ci.ModelDetail), &md) != nil {
				continue
			}
			if md.ID == "" || seen[md.ID] {
				continue
			}
			// ⚠️ is_custom_model 是「仅可见但不可调用」的权威判据；
			// is_invisible_to_user 与「能不能调用」无关，官方隐藏但可调用
			//（glm-5.1 即如此），不并进可用性判定
			if md.DisplayConfig != nil && md.DisplayConfig.IsCustomModel {
				continue
			}
			if md.Usage != "" && md.Usage != "chat_completion" {
				continue
			}
			seen[md.ID] = true
			modelChannel.Store(md.ID, fc.Function)
			models = append(models, &pb.ModelInfo{
				Id:    md.ID,
				Label: map[string]string{"en": shared.OrDefault(md.ModelName, md.ID)},
				// SOLO 通道纯文本（未见图片能力），OpenAI 兼容
				SupportsTools: true, SupportsStream: true,
			})
		}
	}
	return models
}

// fallbackList 远端不可用时的兜底目录。
func (p *plugin) fallbackList() *pb.ModelList {
	var models []*pb.ModelInfo
	for _, m := range fallbackModels {
		models = append(models, &pb.ModelInfo{
			Id: m.id, Label: map[string]string{"en": m.name},
			SupportsTools: true, SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: models}
}
