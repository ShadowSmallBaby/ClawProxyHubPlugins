// challenge.go 管理插件级验证服务配置；站点协议确认后由签到适配调用。
package newapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared/challenge"
)

func challengeSettingsSchema() string {
	options := []map[string]string{{"const": "disabled", "title": "关闭"}}
	for _, adapter := range challenge.Adapters() {
		if adapter.Supports(challenge.TurnstileToken) {
			options = append(options, map[string]string{"const": adapter.ID, "title": adapter.Name["zh"]})
		}
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"challenge_adapter": map[string]any{"type": "string", "title": "验证服务", "default": "disabled", "oneOf": options,
			"description": "服务客户端配置；本站签到协议尚待验证，当前不会自动调用服务"},
		"challenge_service_url": map[string]any{"type": "string", "title": "验证服务地址", "default": "",
			"description": "HTTP(S) 基础 URL，可含路径前缀；地址从插件运行环境访问，非当前浏览器所在设备"},
		"challenge_service_token": map[string]any{"type": "string", "title": "验证服务密钥", "format": "password", "default": "",
			"description": "可选 Bearer 密钥，用于服务前置网关；隐藏输入不改变设置存储方式"},
		"challenge_timeout_seconds": map[string]any{"type": "integer", "title": "验证超时（秒）", "default": 90, "minimum": 5, "maximum": 300},
	}}
	raw, _ := json.Marshal(schema)
	return string(raw)
}

// challengeClient 每次从插件级设置创建客户端，不使用实例合并配置或站点缓存。
func (p *plugin) challengeClient(ctx context.Context) (challenge.Client, error) {
	read := p.loadChallengeSettings
	if read == nil {
		if p.host == nil {
			return nil, &challenge.Error{Code: challenge.InvalidConfig, Message: "无法读取插件验证服务配置"}
		}
		read = func(ctx context.Context) ([]byte, error) { return p.host.SettingsContext(ctx, pluginName) }
	}
	raw, err := read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &challenge.Error{Code: challenge.InvalidConfig, Message: "无法读取插件验证服务配置"}
	}
	return challengeClientFromSettings(raw)
}

func challengeClientFromSettings(raw []byte) (challenge.Client, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, &challenge.Error{Code: challenge.InvalidConfig, Message: "验证服务配置格式无效"}
	}
	var adapter string
	if value, ok := fields["challenge_adapter"]; ok {
		if json.Unmarshal(value, &adapter) != nil {
			return nil, &challenge.Error{Code: challenge.InvalidConfig, Message: "验证服务选择无效"}
		}
	}
	if adapter == "" || adapter == "disabled" {
		return nil, nil
	}
	var config struct {
		URL     string `json:"challenge_service_url"`
		Token   string `json:"challenge_service_token"`
		Timeout *int   `json:"challenge_timeout_seconds"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return nil, &challenge.Error{Code: challenge.InvalidConfig, Message: "验证服务配置格式无效"}
	}
	seconds := 90
	if config.Timeout != nil {
		seconds = *config.Timeout
	}
	if seconds < 5 || seconds > 300 {
		return nil, &challenge.Error{Code: challenge.InvalidConfig, Message: "验证超时应为 5–300 秒"}
	}
	return challenge.NewClient(challenge.Config{Adapter: adapter, BaseURL: config.URL, AccessToken: config.Token, Timeout: time.Duration(seconds) * time.Second})
}
