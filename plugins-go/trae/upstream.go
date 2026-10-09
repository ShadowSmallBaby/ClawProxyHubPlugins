// upstream.go — TRAE HTTP：API 常量 + 各信道请求头。
package trae

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

const (
	agentHost  = "https://trae-api-cn.mchost.guru"
	ugHost     = "https://api.trae.cn"
	oauthHost  = "https://api.trae.com.cn"
	consoleHst = "https://www.trae.cn"

	pathChat       = "/api/agent/v3/llm_utils_chat"
	pathBatchModel = "/api/ide/v1/batch_get_detail_param"
	pathExchange   = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	pathUserInfo   = "/cloudide/api/v3/trae/GetUserInfo"
	pathChkStatus  = "/trae/api/v2/ug/checkin_credits/status"
	pathChkClaim   = "/trae/api/v2/ug/checkin_credits/claim"
	pathEntUsage   = "/trae/api/v2/pay/ide_user_ent_usage"

	clientID        = "en1oxy7wnw8j9n"
	appID           = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	ideVersion      = "0.1.52"
	ideVersionCode  = "20260811"
	pluginVersion   = "2.3.62834"
	deviceBrand     = "Apple"
	osVersion       = "macOS 15.7.4"
	defaultFunction = "solo_work_lite"
	traeUA          = "Trae/0.1.52"

	defaultChannel = "solo_agent"
)

// soloHeaders 对话/模型信道请求头。
// 版本号是模型可用性准入条件：0.1.52 才能用 glm-5.3 等新模型。
func (p *plugin) soloHeaders(cred *credential, stream bool) map[string]string {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	h := map[string]string{
		"Content-Type":         "application/json",
		"Accept":               accept,
		"User-Agent":           traeUA,
		"Authorization":        "Cloud-IDE-JWT " + cred.AccessToken,
		"X-Cloudide-Token":     cred.AccessToken,
		"X-Ide-Token":          cred.AccessToken,
		"X-Uid":                cred.UID,
		"X-App-Id":             appID,
		"X-App-Version":        "default",
		"X-Ide-Version":        ideVersion,
		"X-Ide-Version-Code":   ideVersionCode,
		"X-App-Version-Code":   ideVersionCode,
		"X-Ide-Version-Type":   "stable",
		"X-Device-Type":        "macos",
		"X-OS-Version":         osVersion,
		"X-Device-Brand":       deviceBrand,
		"Request-Traffic-Type": "prod",
	}
	if cred.MachineID != "" {
		h["X-Machine-Id"] = cred.MachineID
	}
	if cred.DeviceID != "" {
		h["X-Device-Id"] = cred.DeviceID
	}
	return h
}

// checkinHeaders 签到专用完整请求头（约 20 个客户端头）。
// 设备身份基于 user_id 确定性派生（每账号独立稳定，规避设备级限流）。
func (p *plugin) checkinHeaders(cred *credential) map[string]string {
	deviceID := deriveDigits(15, cred.UID, "devid")
	marketUID := deriveUUID(cred.UID, "market")
	sessionID := deriveHex(64, cred.UID, "sess")
	return map[string]string{
		"Content-Type":       "application/json",
		"Accept":             "*/*",
		"Accept-Language":    "zh-CN",
		"User-Agent":         "VSCode 1.107.1 (TRAE SOLO CN)",
		"Authorization":      "Cloud-IDE-JWT " + cred.AccessToken,
		"X-Market-Client-Id": "VSCode 1.107.1",
		"X-Market-User-Id":   marketUID,
		"X-User-Region":      "CN",
		"X-Device-Id":        deviceID,
		"X-Lgw-Req-Sdk-Type": "3",
		"Package-Type":       "stable_cn",
		"X-Lscbd-Aid":        "787976",
		"X-Lscbd-Platform":   "windows",
		"App-Version":        ideVersion,
		"X-Tt-Trace-Id":      "00-" + shared.RandHex(16) + "-01",
		"Vscode-Sessionid":   sessionID,
		"X-Request-Id":       shared.RandUUID(),
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "no-cors",
		"Sec-Fetch-Site":     "none",
	}
}

// oauthHeaders OAuth（ExchangeToken/GetUserInfo）请求头：无签名，仅 UA。
func oauthHeaders() map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"User-Agent":   traeUA,
	}
}

// ---------- 确定性派生 ----------

// seededStream SHA-256 确定性伪随机流：输入 (seed, salt) 永远产生相同输出。
// 算法：SHA256(utf8(salt:seed) ++ counterBE32) 串联。
func seededStream(seed, salt string, n int) []byte {
	prefix := salt + ":" + seed
	out := make([]byte, 0, n)
	var counter uint32
	buf := make([]byte, 4)
	for len(out) < n {
		buf[0] = byte(counter >> 24)
		buf[1] = byte(counter >> 16)
		buf[2] = byte(counter >> 8)
		buf[3] = byte(counter)
		h := sha256.New()
		h.Write([]byte(prefix))
		h.Write(buf)
		out = h.Sum(out)
		counter++
	}
	return out[:n]
}

// deriveDigits 确定性派生 n 位数字字符串（每字节取模 10）。
func deriveDigits(n int, seed, salt string) string {
	bs := seededStream(seed, salt, n)
	s := make([]byte, n)
	for i, b := range bs {
		s[i] = byte('0' + b%10)
	}
	return string(s)
}

// deriveHex 确定性派生 2n 位 hex 字符串。
func deriveHex(n int, seed, salt string) string {
	bs := seededStream(seed, salt, n/2)
	return hex.EncodeToString(bs)
}

// deriveUUID 确定性派生 UUID v4（基于 seed+salt）。
func deriveUUID(seed, salt string) string {
	bs := seededStream(seed, salt, 16)
	bs[6] = (bs[6] & 0x0F) | 0x40
	bs[8] = (bs[8] & 0x3F) | 0x80
	h := hex.EncodeToString(bs)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ---------- JSON helper ----------

// readResp 读响应体（限 4MB）。
func readResp(resp *http.Response) []byte {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return raw
}

// jsonField 从响应 JSON 里取 Result（兼容 Result/result 两种形态）。
func resultField(raw []byte) (map[string]interface{}, bool) {
	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) != nil {
		return nil, false
	}
	if r, ok := obj["Result"].(map[string]interface{}); ok {
		return r, true
	}
	if r, ok := obj["result"].(map[string]interface{}); ok {
		return r, true
	}
	return obj, true
}

// strOf 取字符串字段。
func strOf(obj map[string]interface{}, key string) string {
	if obj == nil {
		return ""
	}
	s, _ := obj[key].(string)
	return s
}

// numOf 取数字字段（兼容数字字符串）。
func numOf(obj map[string]interface{}, key string) float64 {
	if obj == nil {
		return 0
	}
	switch v := obj[key].(type) {
	case float64:
		return v
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			return f
		}
	}
	return 0
}

// postUG Ug 信道 POST（checkin_credits 等）：完整签到头 + 空/JSON body。
// 返回 code===0 时的 body（业务失败返回 nil）。
func (p *plugin) postUG(ctx context.Context, cred *credential, path, body string) map[string]interface{} {
	req, err := httpNewReq(ctx, "POST", ugHost+path, []byte(body))
	if err != nil {
		return nil
	}
	for k, v := range p.checkinHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw := readResp(resp)
	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	if code, ok := obj["code"].(float64); ok && code != 0 {
		return nil
	}
	return obj
}

// hc 凭据对应的 HTTP client。
func (p *plugin) hc(cred *credential) *http.Client {
	return sdk.UpstreamClient(cred.proxyURL)
}
