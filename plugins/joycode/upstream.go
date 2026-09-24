// color gateway 签名、端点路由、公共头与 JSON 请求。
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// colorSign 构造 color gateway 的 query 串与 HMAC-SHA256 签名。
func colorSign(functionID string) (query, sign string) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(colorHMACKey))
	mac.Write([]byte(colorGatewayAppID + "&" + functionID + "&" + ts))
	sign = hex.EncodeToString(mac.Sum(nil))
	query = "appid=" + colorGatewayAppID + "&functionId=" + functionID + "&t=" + ts
	return query, sign
}

// endpointURL 端点 key（chat/models/userInfo）→ 最终请求 URL。
// 有 colorBaseUrl → gateway 模式（带签名）；否则 direct v2（masterBaseUrl/默认）。
func (c *credential) endpointURL(key string) string {
	ep := colorEndpoints[key]
	colorBase := shared.OrDefault(c.ColorBaseURL, defaultColorBaseURL)
	if colorBase != "" {
		if u, err := url.Parse(colorBase); err == nil && u.Host != "" {
			basePath := strings.TrimRight(u.Path, "/")
			query, sign := colorSign(ep.functionID)
			return u.Scheme + "://" + u.Host + basePath + colorGatewayPath + "?" + query + "&sign=" + sign
		}
	}
	base := shared.OrDefault(c.MasterBaseURL, defaultBaseURL)
	return strings.TrimRight(base, "/") + ep.v2Path
}

// headers 请求公共头。
func (c *credential) headers() map[string]string {
	return map[string]string{
		"Content-Type":    "application/json; charset=UTF-8",
		"source-type":     "joycoder-ide",
		"ptKey":           c.PtKey,
		"loginType":       shared.OrDefault(c.LoginType, "N_PIN_PC"),
		"User-Agent":      userAgent,
		"Accept":          "*/*",
		"Accept-Encoding": "identity", // 不压缩，避免 gzip 缓冲打断流式
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
	}
}

// prepareBody 注入 JoyCode 请求体公共字段。
func (c *credential) prepareBody(body map[string]interface{}) map[string]interface{} {
	if body == nil {
		body = map[string]interface{}{}
	}
	body["tenant"] = shared.OrDefault(c.Tenant, "JOYCODE")
	body["orgFullName"] = c.OrgFullName
	body["userId"] = c.UserID
	body["client"] = "JoyCode"
	body["clientVersion"] = clientVersion
	body["language"] = "UNKNOWN"
	return body
}

// postJSON 普通 JSON POST（userInfo / modelList），解出 map 响应。
func (p *plugin) postJSON(ctx context.Context, c *credential, key string, extra map[string]interface{}) (map[string]interface{}, error) {
	raw, _ := json.Marshal(c.prepareBody(extra))
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpointURL(key), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers() {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data := readBody(resp)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(data), 300))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("invalid JSON response: %s", shared.Truncate(string(data), 300))
	}
	return out, nil
}

// readBody 读响应体（按需 gzip 解压）。
func readBody(resp *http.Response) []byte {
	var r io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		if gz, err := gzip.NewReader(resp.Body); err == nil {
			defer gz.Close()
			r = gz
		}
	}
	data, _ := io.ReadAll(io.LimitReader(r, 8<<20))
	return data
}
