// account.go — 凭据（AK/SK）解析、账号档案与刷新。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

// credential CodeArts IAM 凭据：AK/SK（+ 可选临时 security_token）。
// 无 OAuth/refresh token：凭据失效只能人工重新生成 AK。
type credential struct {
	AccessKey     string `json:"access_key"`
	SecretKey     string `json:"secret_key"`
	SecurityToken string `json:"security_token,omitempty"`
	Label         string `json:"label,omitempty"`

	proxyURL string `json:"-"`
}

// credFrom 解析凭据 blob（含代理配置），blob 需含非空 AK/SK。
func credFrom(blob *pb.CredentialBlob) (*credential, error) {
	if blob == nil || len(blob.GetBlob()) == 0 {
		return nil, fmt.Errorf("缺少 CodeArts 凭据，请先导入 AK/SK")
	}
	c := &credential{}
	if err := json.Unmarshal(blob.GetBlob(), c); err != nil {
		return nil, fmt.Errorf("凭据解析失败: %w", err)
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, fmt.Errorf("凭据缺少 Access Key / Secret Key")
	}
	c.proxyURL = sdk.ProxyURL(blob.GetProxy())
	return c, nil
}

// GetProfile 账户/套餐信息（含积分账户检测与积分余额）。
func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.AccountProfile{Healthy: false}, nil
	}
	prof := &pb.AccountProfile{
		DisplayName: shared.OrDefault(c.Label, "codearts"),
		Healthy:     true,
		Quota:       map[string]string{},
	}
	obj, err := p.snapRequest(ctx, c, "GET", snapBase+pathPackageInfo, nil, nil)
	if err != nil {
		prof.Healthy = false
		prof.Quota["error"] = err.Error()
		return prof, nil
	}
	data, err := unwrapSnapEnvelope(obj)
	if err != nil {
		prof.Healthy = false
		prof.Quota["error"] = err.Error()
		return prof, nil
	}
	pkg, _ := data["package"].(map[string]interface{})
	if pkg == nil {
		pkg = map[string]interface{}{}
	}
	if isCredit := boolField(pkg, "is_credit_package"); isCredit {
		prof.Quota["billing"] = "credit"
	} else if boolField(pkg, "is_token_package") {
		prof.Quota["billing"] = "token"
	}
	if name := strField(pkg, "package_name_cn"); name != "" {
		prof.DisplayName = name
	} else if name := strField(pkg, "package_name_en"); name != "" {
		prof.DisplayName = name
	}
	// 积分余额：usageTotalPackageCredit 的 package_credit_remain（不累加分类，
	// 分类是总额构成明细，相加会重复计算）
	if metrics, ok := data["metrics"].([]interface{}); ok {
		total, hasTotal := 0.0, false
		var pkgs []string
		for _, raw := range metrics {
			m, _ := raw.(map[string]interface{})
			if m == nil {
				continue
			}
			amount := numField(m, "package_credit_amount")
			remain := numField(m, "package_credit_remain")
			name := strField(m, "name")
			if name == "usageTotalPackageCredit" {
				total, hasTotal = remain, true
				continue
			}
			if amount <= 0 && remain <= 0 {
				continue
			}
			label := metricLabel(name)
			if label != "" {
				pkgs = append(pkgs, fmt.Sprintf("%s %s/%s", label,
					strconv.FormatFloat(remain, 'f', -1, 64),
					strconv.FormatFloat(amount, 'f', -1, 64)))
			}
		}
		if !hasTotal && len(pkgs) > 0 {
			// 总额 metric 缺失时回退分类求和
			for _, raw := range metrics {
				if m, ok := raw.(map[string]interface{}); ok && metricLabel(strField(m, "name")) != "" {
					total += numField(m, "package_credit_remain")
					hasTotal = true
				}
			}
		}
		if hasTotal {
			prof.Quota["credits"] = strconv.FormatFloat(total, 'f', -1, 64)
		}
		if len(pkgs) > 0 {
			prof.Quota["packages"] = fmt.Sprintf("%d", len(pkgs))
			if b, err := json.Marshal(pkgs); err == nil {
				prof.CreditsJson = string(b)
			}
		}
	}
	return prof, nil
}

// Refresh CodeArts AK/SK 无可刷新态：原样返回不报错（凭据失效以对话时 401 反馈）。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	return &pb.RefreshResult{}, nil
}

// metricLabel 积分 metric 名 → 展示名。
func metricLabel(name string) string {
	switch name {
	case "usageTotalPackageCredit":
		return "总积分包"
	case "usageBasicPackageCredit":
		return "基础积分包"
	case "usageOnDemandPackageCredit":
		return "按需积分包"
	case "usageBonusPackageCredit":
		return "赠送积分包"
	}
	return ""
}

// ---------- JSON 取值辅助 ----------

func strField(obj map[string]interface{}, key string) string {
	if obj == nil {
		return ""
	}
	s, _ := obj[key].(string)
	return s
}

func boolField(obj map[string]interface{}, key string) bool {
	if obj == nil {
		return false
	}
	if b, ok := obj[key].(bool); ok {
		return b
	}
	if s, ok := obj[key].(string); ok {
		return s == "true"
	}
	return false
}

// numField 兼容数字与数字字符串两种形态。
func numField(obj map[string]interface{}, key string) float64 {
	if obj == nil {
		return 0
	}
	switch v := obj[key].(type) {
	case float64:
		return v
	case string:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return 0
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
