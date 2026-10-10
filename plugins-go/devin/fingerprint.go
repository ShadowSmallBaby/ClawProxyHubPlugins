// 设备指纹：F 信号值不读宿主真机，而是按 token 确定性伪造（366 hex）——
// 同一 token 永远同一台设备（换指纹本身是可疑信号）。
package devin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// fingerprintFromToken token → 确定性 366 hex 指纹（哈希级联填充）。
func fingerprintFromToken(token string) string {
	seed := []byte("devin-plugin:device-fingerprint:v1\x00" + token)
	var out strings.Builder
	for len(out.String()) < 366*2 {
		h := sha256.Sum256(seed)
		out.WriteString(hex.EncodeToString(h[:]))
		seed = h[:]
	}
	return out.String()[:366*2]
}
