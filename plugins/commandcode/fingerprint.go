// 设备指纹：形态与哈希逐字对齐官方 CLI 1.53.1。
// 信号值不读宿主真机，而是按 apiKey 确定性伪造——同一 key 永远同一台设备
// （重启 / 多实例 / 停用恢复后上游都应看到同一设备，换指纹本身是可疑信号）。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// FP_SALT CLI 的根盐（buildMachineFingerprint 常量 sb）。哈希阶段固定用它；
// 用户配的 fingerprintSalt 只影响「伪造出哪台机器」，不参与 hashSignal。
const fpSalt = "command-code:device-fingerprint:v1"

// deviceProfile 设备档案：指纹 / config.environment / config.workingDir /
// x-project-slug / lifecycle.os 共用同一份，避免自相矛盾，也不泄漏宿主真实信息。
type deviceProfile struct {
	platform   string
	arch       string
	osRelease  string
	projectDir string
}

func defaultDeviceProfile(projectDir string) deviceProfile {
	if projectDir == "" {
		projectDir = `C:\Users\dev\projects\app`
	}
	return deviceProfile{platform: "win32", arch: "x64", osRelease: "10.0.22631", projectDir: projectDir}
}

// CPU 型号与核心数对应表（仅 Windows x64）。
var fpCPUs = []struct {
	model string
	cores int
}{
	{"12th Gen Intel(R) Core(TM) i7-12650H", 10},
	{"12th Gen Intel(R) Core(TM) i5-12400F", 6},
	{"12th Gen Intel(R) Core(TM) i9-12900K", 16},
	{"13th Gen Intel(R) Core(TM) i7-13700K", 16},
	{"13th Gen Intel(R) Core(TM) i5-13600K", 14},
	{"13th Gen Intel(R) Core(TM) i9-13900K", 24},
	{"Intel(R) Core(TM) Ultra 7 155H", 16},
	{"Intel(R) Core(TM) Ultra 9 285H", 16},
	{"Intel(R) Core(TM) i9-14900K", 24},
	{"Intel(R) Core(TM) i7-14700K", 20},
	{"AMD Ryzen 7 7800X3D", 8},
	{"AMD Ryzen 9 7950X", 16},
	{"AMD Ryzen 5 7600", 6},
	{"AMD Ryzen 9 7900X", 12},
	{"AMD Ryzen 7 5800X3D", 8},
}

var (
	fpMems        = []int{8, 16, 24, 32, 48, 64}
	fpTZs         = []string{"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Toronto", "Europe/London", "Europe/Berlin", "Europe/Paris", "Europe/Moscow", "Asia/Shanghai", "Asia/Tokyo", "Asia/Singapore", "Asia/Seoul", "Asia/Hong_Kong", "Australia/Sydney", "Pacific/Auckland"}
	fpMacCounts   = []int{2, 3, 4, 5}
	fpOSUsers     = []string{"dev", "user", "admin", "coder", "engineer", "work"}
	fpMailDomains = []string{"gmail.com", "outlook.com", "qq.com", "163.com"}
)

// fpDigest 伪造信号派生源：sha256(fingerprintSalt \0 apiKey \0 field)。
// salt 只影响挑出哪台机器；哈希阶段用固定 fpSalt。
func fpDigest(salt, apiKey, field string) []byte {
	h := sha256.Sum256([]byte(salt + "\x00" + apiKey + "\x00" + field))
	return h[:]
}

// fpPickIndex 从候选池确定性挑一项：打分取最大。
// 用打分而非取模——往池里加候选只影响「新候选恰好胜出」的 key，不会全体换设备。
func fpPickIndex(salt, apiKey, field string, n int, labelOf func(int) string) int {
	bestIdx := 0
	var bestScore []byte
	for i := 0; i < n; i++ {
		score := fpDigest(salt, apiKey, field+"\x00"+labelOf(i))
		if bestScore == nil || bytes.Compare(score, bestScore) > 0 {
			bestScore, bestIdx = score, i
		}
	}
	return bestIdx
}

// fingerprintHash CLI 的 hashSignal：sha256(fpSalt \0 value.toLowerCase())；空值返回空串（JSON 里省略）。
func fingerprintHash(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	h := sha256.Sum256([]byte(fpSalt + "\x00" + strings.ToLower(v)))
	return hex.EncodeToString(h[:])
}

// fingerprint /alpha/fingerprint/record 的请求体形态。
type fingerprint struct {
	Thumbmark  string                `json:"thumbmark"`
	Components fingerprintComponents `json:"components"`
}

type fingerprintComponents struct {
	MachineIDHash    string   `json:"machineIdHash,omitempty"`
	MacHashes        []string `json:"macHashes"`
	OSUserHash       string   `json:"osUserHash,omitempty"`
	HostnameHash     string   `json:"hostnameHash,omitempty"`
	GitEmailHash     string   `json:"gitEmailHash,omitempty"`
	Platform         string   `json:"platform"`
	Arch             string   `json:"arch"`
	OSRelease        string   `json:"osRelease"`
	CPUModel         string   `json:"cpuModel"`
	CPUCount         int      `json:"cpuCount"`
	MemGiB           int      `json:"memGiB"`
	IsContainer      bool     `json:"isContainer"`
	Timezone         string   `json:"timezone"`
	Runtime          string   `json:"runtime"`
	CollectorVersion int      `json:"collectorVersion"`
}

// generateFingerprint 按 apiKey 派生一台逼真设备。salt 空时全体沿用默认设备分布。
func generateFingerprint(apiKey, salt string, prof deviceProfile) fingerprint {
	cpuEntry := fpCPUs[fpPickIndex(salt, apiKey, "cpu", len(fpCPUs), func(i int) string {
		return fmt.Sprintf("%s|%d", fpCPUs[i].model, fpCPUs[i].cores)
	})]
	memGiB := fpMems[fpPickIndex(salt, apiKey, "mem", len(fpMems), func(i int) string { return fmt.Sprintf("%d", fpMems[i]) })]
	tz := fpTZs[fpPickIndex(salt, apiKey, "timezone", len(fpTZs), func(i int) string { return fpTZs[i] })]
	macCount := fpMacCounts[fpPickIndex(salt, apiKey, "macCount", len(fpMacCounts), func(i int) string { return fmt.Sprintf("%d", fpMacCounts[i]) })]
	osUser := fpOSUsers[fpPickIndex(salt, apiKey, "osUser", len(fpOSUsers), func(i int) string { return fpOSUsers[i] })]
	mailDomain := fpMailDomains[fpPickIndex(salt, apiKey, "mailDomain", len(fpMailDomains), func(i int) string { return fpMailDomains[i] })]

	hexOf := func(field string, n int) string { return hex.EncodeToString(fpDigest(salt, apiKey, field)[:n]) }

	// Windows MachineGuid 形状：8-4-4-4-12
	mid := hexOf("machineId", 16)
	machineID := fmt.Sprintf("%s-%s-%s-%s-%s", mid[0:8], mid[8:12], mid[12:16], mid[16:20], mid[20:32])

	macs := make([]string, 0, macCount)
	for i := 0; i < macCount; i++ {
		b := fpDigest(salt, apiKey, fmt.Sprintf("mac%d", i))[:6]
		parts := make([]string, 6)
		for j, x := range b {
			parts[j] = fmt.Sprintf("%02x", x)
		}
		macs = append(macs, strings.Join(parts, ":"))
	}
	sort.Strings(macs) // CLI 对 MAC 去重后排序

	hostname := "DESKTOP-" + strings.ToUpper(hexOf("hostname", 4))
	gitEmail := fmt.Sprintf("%s.%s@%s", osUser, hexOf("gitEmail", 3), mailDomain)

	var macHashes []string
	for _, m := range macs {
		if h := fingerprintHash(m); h != "" {
			macHashes = append(macHashes, h)
		}
	}
	if macHashes == nil {
		macHashes = []string{}
	}

	// CLI 的 thumbmark：主盐 + "\0machine\0" + join([machineId, macs.join(",")])
	// （machineId 非空时不再拼 hostname/cpuModel）。
	var thumbSeed []string
	if t := strings.TrimSpace(machineID); t != "" {
		thumbSeed = append(thumbSeed, t)
	}
	if joined := strings.Join(macs, ","); joined != "" {
		thumbSeed = append(thumbSeed, joined)
	}
	if strings.TrimSpace(machineID) == "" {
		if hostname != "" {
			thumbSeed = append(thumbSeed, hostname)
		}
		if cpuEntry.model != "" {
			thumbSeed = append(thumbSeed, cpuEntry.model)
		}
	}
	seed := strings.Join(thumbSeed, "|")
	if seed == "" {
		seed = "unknown"
	}
	tm := sha256.Sum256([]byte(fpSalt + "\x00machine\x00" + seed))

	return fingerprint{
		Thumbmark: hex.EncodeToString(tm[:]),
		Components: fingerprintComponents{
			MachineIDHash:    fingerprintHash(machineID),
			MacHashes:        macHashes,
			OSUserHash:       fingerprintHash(osUser),
			HostnameHash:     fingerprintHash(hostname),
			GitEmailHash:     fingerprintHash(gitEmail),
			Platform:         prof.platform,
			Arch:             prof.arch,
			OSRelease:        prof.osRelease,
			CPUModel:         cpuEntry.model,
			CPUCount:         cpuEntry.cores,
			MemGiB:           memGiB,
			IsContainer:      false,
			Timezone:         tz,
			Runtime:          "cli",
			CollectorVersion: 1,
		},
	}
}

// slugifyProjectPath CLI 的 slug 规则：对完整工作目录 slugify，空则 "root"。
// slug 与 config.workingDir 同源（真机里 slug = slugify(workingDir)）。
func slugifyProjectPath(p string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(p) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "root"
	}
	return s
}
