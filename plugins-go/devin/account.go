// 资料 / 余额（SeatManagement/GetUserStatus JSON Connect）。
package devin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	devinproto "github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins-go/devin/devinproto"
	"google.golang.org/protobuf/proto"
)

// fetchProfile 拉用户状态 + 模型列表验活，写 profile。
func (p *plugin) fetchProfile(ctx context.Context, c *credential, profile *pb.AccountProfile) error {
	// 验活：拉一次模型列表（unary，顺带校验 token 有效性）。
	if _, err := p.fetchModelConfigs(ctx, c); err != nil {
		return err
	}
	var root map[string]any
	body := map[string]any{"metadata": metadataJSON(c.Token)}
	if err := connectJSONUnary(ctx, p.hc(c), c.Token, p.baseURL(), seatStatusPath, body, &root); err != nil {
		return fmt.Errorf("GetUserStatus: %w", err)
	}
	us, _ := root["userStatus"].(map[string]any)
	if us == nil {
		us, _ = root["user_status"].(map[string]any)
	}
	if us == nil {
		return fmt.Errorf("GetUserStatus: empty userStatus")
	}
	if name := strAny(us["name"]); name != "" {
		profile.DisplayName = name
	}
	if email := strAny(us["email"]); email != "" {
		profile.DisplayName = email
	}
	quota := map[string]string{}
	if v := numAny(us["userUsedPromptCredits"]); v != "" {
		quota["used_credits"] = v
	}
	if ps, _ := us["planStatus"].(map[string]any); ps != nil {
		if v := numAny(ps["availablePromptCredits"]); v != "" {
			quota["credits"] = v
		}
		if v := numAny(ps["availableFlexCredits"]); v != "" {
			quota["flex_credits"] = v
		}
		if v := numAny(ps["availableFlowCredits"]); v != "" {
			quota["flow_credits"] = v
		}
		if b, err := json.Marshal(ps); err == nil {
			profile.CreditsJson = string(b)
		}
	}
	if len(quota) > 0 {
		profile.Quota = quota
	}
	return nil
}

// fetchModelConfigs 获取账号可用模型，供模型同步和登录验活共用。
func (p *plugin) fetchModelConfigs(ctx context.Context, c *credential) ([]*devinproto.ExaCodeiumCommonPb_ClientModelConfig, error) {
	req := &devinproto.GetCascadeModelConfigsRequest{Metadata: metadataFor(c.Token)}
	var resp devinproto.GetCascadeModelConfigsResponse
	if err := connectUnary(ctx, p.hc(c), c.Token, p.baseURL(), connectProtoPath+"GetCascadeModelConfigs", mustMarshal(req), &resp); err != nil {
		return nil, fmt.Errorf("GetCascadeModelConfigs: %w", err)
	}
	if len(resp.GetClientModelConfigs()) == 0 {
		return nil, fmt.Errorf("GetCascadeModelConfigs returned an empty list")
	}
	return resp.GetClientModelConfigs(), nil
}

// metadataJSON JSON Connect 的 metadata（camelCase 键）。
func metadataJSON(token string) map[string]any {
	return map[string]any{
		"api_key":           token,
		"extension_name":    clientName,
		"extension_version": clientVersion,
		"ide_name":          clientName,
		"ide_version":       clientVersion,
		"locale":            "en",
		"os":                "windows",
	}
}

// strAny 首个非空字符串。
func strAny(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// numAny 首个可转数字字符串的值。
func numAny(values ...any) string {
	for _, v := range values {
		switch n := v.(type) {
		case float64:
			return strconv.FormatFloat(n, 'f', -1, 64)
		case string:
			if n != "" {
				return n
			}
		}
	}
	return ""
}

// mustMarshal proto 消息 → bytes（构造期错误直接 panic，消息由生成器保证合法）。
func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
