// signer.go — 华为云 SDK-HMAC-SHA256 请求签名。
//
// 契约要点：
//   - x-sdk-date：ISO8601 去连字符（YYYYMMDDTHHMMSSZ）；
//   - GET 无 content-type 头，POST 有；
//   - 额外签名头（如 maas_type: benefit）参与 canonical 计算并列入 SignedHeaders，
//     必须原样随请求发送，否则服务端验签失败；
//   - Agent-Type / X-Language 等运行头在签名**之后**追加（不参与签名）——
//     它们进 canonical request 会被服务端拒绝。
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSha256Hex(key, data []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// signHuawei 签名请求并把签名头写进 req；extraSignedHeaders 参与签名计算。
func signHuawei(req *http.Request, ak, sk, securityToken string, body []byte, extraSignedHeaders map[string]string) {
	rawURL := req.URL.String()
	u, _ := url.Parse(rawURL)
	uri := u.Path
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	query := u.RawQuery
	dateStamp := time.Now().UTC().Format("20060102T150405Z")
	payloadHash := sha256Hex(body)

	headers := map[string]string{
		"host":                  u.Host,
		"x-sdk-date":            dateStamp,
		"x-sdk-content-sha256":  payloadHash,
		"x-security-token":      securityToken,
	}
	if req.Method != http.MethodGet {
		headers["content-type"] = "application/json"
	}
	for k, v := range extraSignedHeaders {
		headers[strings.ToLower(k)] = v
	}

	signed := make([]string, 0, len(headers))
	for k := range headers {
		signed = append(signed, k)
	}
	sort.Strings(signed)

	var line strings.Builder
	for _, k := range signed {
		fmt.Fprintf(&line, "%s:%s\n", k, headers[k])
	}
	canonical := strings.Join([]string{
		req.Method, uri, query,
		strings.TrimSuffix(line.String(), "\n"), "",
		strings.Join(signed, ";"), payloadHash,
	}, "\n")
	canonicalHash := sha256Hex([]byte(canonical))
	stringToSign := "SDK-HMAC-SHA256\n" + dateStamp + "\n" + canonicalHash
	signature := hmacSha256Hex([]byte(sk), []byte(stringToSign))

	for k, v := range headers {
		if k == "host" {
			continue // host 由 http.Client 按连接目标生成
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization",
		fmt.Sprintf("SDK-HMAC-SHA256 Access=%s,SignedHeaders=%s,Signature=%s", ak, strings.Join(signed, ";"), signature))
}
