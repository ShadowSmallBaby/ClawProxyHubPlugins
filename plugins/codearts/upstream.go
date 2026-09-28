// upstream.go — snap-access HTTP：签名请求 helper + 模型/对话端点常量。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

const (
	// snapBase 模型与积分共用同一 snap-access 网关。
	snapBase        = "https://snap-access.cn-north-4.myhuaweicloud.com"
	chatAPIBase     = snapBase + "/api/v2"
	pathBuiltin     = "/v1/model/builtin"
	pathGatewayCfg  = "https://opengw.developer.huaweicloud.com/api/v1/gateway/config"
	pathChat        = "/chat/completions"
	pathPackageInfo = "/snap-manager/v1/statistics/plugin"
	pathOpsDelivery = "/v1/ops/delivery"
	pathOpsClaim    = "/v1/ops/claim"
	pathOpsConfirm  = "/v1/ops/confirm"
)

// CODEARTS_BENEFIT_FALLBACK benefit（免费额度）模型兜底表：调时必须带
// maas_type: benefit 签名头，缺该头后端按非 benefit 通道处理报 404。
// 判定不能靠名字猜测（deepseek-v4-flash 带后缀/不带 benefit 属性相反）。
var benefitModels = map[string]bool{
	"glm-5.3-flash":       true,
	"deepseek-v4.1-flash": true,
}

// codeartsBenefit 判定模型是否 benefit（内存集合 ∪ 兜底表）。
func (p *plugin) codeartsBenefit(model string) bool {
	if benefitModels[model] {
		return true
	}
	if v := p.settingStr("benefit_models"); v != "" {
		var extra []string
		if json.Unmarshal([]byte(v), &extra) == nil {
			if slices.Contains(extra, model) {
				return true
			}
		}
	}
	return false
}

// snapRequest 发一次带 SDK-HMAC-SHA256 签名的 snap-access 请求。
// Agent-Type/X-Language 在签名后追加（不参与签名，进了会验签失败）。
func (p *plugin) snapRequest(ctx context.Context, cred *credential, method, fullURL string, body []byte, extraSigned map[string]string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	signHuawei(req, cred.AccessKey, cred.SecretKey, cred.SecurityToken, body, extraSigned)
	// 运行头：签名后追加（服务端路由所需，但不参与签名）
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	req.Header.Set("X-Sdk-Date", req.Header.Get("X-Sdk-Date")) // 保持既有签名头
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}
	return obj, nil
}

// unwrapSnapEnvelope 拆 snap 信封：ops/* 返回 {code,message,data}（code!==0 即失败），
// statistics/plugin 直接返回裸对象。
func unwrapSnapEnvelope(obj map[string]interface{}) (map[string]interface{}, error) {
	if code, ok := obj["code"].(float64); ok {
		if code != 0 {
			msg, _ := obj["message"].(string)
			if msg == "" {
				msg, _ = obj["msg"].(string)
			}
			if msg == "" {
				msg = fmt.Sprintf("业务码 %v", code)
			}
			return nil, fmt.Errorf("%s", msg)
		}
		if data, ok := obj["data"].(map[string]any); ok {
			return data, nil
		}
		return nil, fmt.Errorf("响应缺少 data 字段")
	}
	return obj, nil // 裸对象形态
}

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (p *plugin) hc(cred *credential) *http.Client {
	return sdk.UpstreamClient(cred.proxyURL)
}
