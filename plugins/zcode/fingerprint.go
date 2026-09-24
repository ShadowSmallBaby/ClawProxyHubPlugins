// Package main — zcode 指纹头：ZCode 桌面客户端 identity 头（3.14.0 g6n 形态，
// LLM 请求平面：无 X-Device-Mid）+ V4 请求签名（Ed25519 + PoW）。
package main

import (
	"bytes"
	"context"
	"crypto/aes"
	aescipher "crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/hkdf"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// ---------- identity 头 ----------

// llmIdentityHeaders ZCode 桌面客户端 LLM 请求平面 identity 头（g6n 形态）。
// 顺序与上游 buildLlmIdentityHeaders 一致；X-Device-Mid 永不携带。
func (p *plugin) llmIdentityHeaders(c *credential) map[string]string {
	appVer := printableOr(p.appVersion(), defaultAppVersion)
	ua := "ZCode/" + appVer
	_ = c
	platform := printableOr(Platform(), runtime.GOOS)
	arch := printableOr(runtime.GOARCH, "")
	release := printableOr(Release(), "")
	return map[string]string{
		"HTTP-Referer":        defaultReferer,
		"User-Agent":          ua,
		"X-ZCode-App-Version": appVer,
		"X-Title":             "Z Code@cli",
		"X-Release-Channel":   "production",
		"X-Client-Language":   clientLanguage(),
		"X-Client-Timezone":   clientTimezone(),
		"X-ZCode-Agent":       "glm",
		"X-Platform":          platform + "-" + arch,
		"X-Os-Category":       osCategory(platform),
		"X-Os-Version":        release,
	}
}

// printableOr 可打印 ASCII 门（对齐上游 fio）：合规返回原值，非合规返回 def。
func printableOr(v, def string) string {
	if v == "" {
		return def
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			return def
		}
	}
	return v
}

// Platform / Release 运行时平台信息（可被设置覆盖的链路此处从简：读真机）。
func Platform() string { return runtime.GOOS }

func Release() string {
	if v, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(v))
	}
	// Windows：读注册表过于复杂，返回空让 X-Os-Version 省略
	return ""
}

// clientLanguage / clientTimezone Intl 形态的兜底值（上游 "unknown" fallback 前置）。
func clientLanguage() string {
	if v := printableOr(osLanguage(), ""); v != "" {
		return v
	}
	return "unknown"
}

func osLanguage() string {
	return strings.Replace(strings.Replace(os.Getenv("LANG"), ".", "-", 1), "_", "-", 1)
}

func clientTimezone() string {
	if name := time.Local.String(); name != "Local" {
		if v := printableOr(name, ""); v != "" {
			return v
		}
	}
	return "unknown"
}

func osCategory(platform string) string {
	switch platform {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// ---------- V4 请求签名 ----------

const (
	kdfSalt          = "WD_CLIENT_SIGN_KDF_SALT"
	kdfInfoHMAC      = "getSignKey_hmac"
	kdfInfoEd25519   = "ed25519_priv"
	handshakeMethod  = "get_sign_key"
	handshakePath    = "/api/paas/c1f3a7e2/v2/client"
	signingAppID     = "zcode"
	powBits          = 8
	nonceBytes       = 16
	powNonceBytes    = 12
	gatePath         = "/api/v1/agent/configs"
	gateTTL          = time.Hour
	gateFailCool     = time.Minute
	verifySigInvalid = "VERIFY_SIGNATURE_INVALID"
	verifyKeyExpired = "VERIFY_APIKEY_EXPIRED"
)

// signerState (origin, credential) 维度的签名状态。
type signerState struct {
	mu          sync.Mutex
	gateEnabled bool
	gateOKAt    time.Time
	gateNegAt   time.Time
	bypass      bool
	privKey     ed25519.PrivateKey
	failCount   int
}

// signer V4 签名管理器。
type signer struct {
	origin string
	p      *plugin
	mu     sync.Mutex
	states map[string]*signerState
}

func newSigner(p *plugin, origin string) *signer {
	return &signer{origin: strings.TrimRight(origin, "/"), p: p, states: map[string]*signerState{}}
}

func (s *signer) stateFor(cred string) *signerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.origin + "\n" + cred
	if st, ok := s.states[key]; ok {
		return st
	}
	st := &signerState{}
	s.states[key] = st
	return st
}

// parseSigningCredential {apiKeyId}.{apiKeySecret} 双密钥解析；非双密钥返回 false。
func parseSigningCredential(cred string) (id, secret string, ok bool) {
	dot := strings.Index(cred, ".")
	if dot <= 0 || dot != strings.LastIndex(cred, ".") {
		return "", "", false
	}
	id, secret = cred[:dot], cred[dot+1:]
	if strings.TrimSpace(id) == "" || strings.TrimSpace(secret) == "" {
		return "", "", false
	}
	return id, secret, true
}

// isUnsignedPath start-plan / off-peak 网关路径永不签名。
func isUnsignedPath(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	path := strings.TrimRight(u.Path, "/")
	switch path {
	case "/api/v1/zcode-plan/anthropic/v1/messages",
		"/api/v1/zcode-plan/chat/completions",
		"/api/v1/off-peak/anthropic/v1/messages":
		return true
	}
	return false
}

// signHeaders 给上游请求头追加 V4 签名；签名不适用时原样返回。
// 返回 signed=false 表示未签名（fail-open：门关 / 握手失败 / bypass / 非双密钥）。
func (s *signer) signHeaders(ctx *signCtx) (map[string]string, bool) {
	u, err := url.Parse(ctx.url)
	if err != nil || u.Scheme != "https" || isUnsignedPath(ctx.url) {
		return ctx.headers, false
	}
	st := s.stateFor(ctx.cred)
	st.mu.Lock()
	bypass, gateOK, gateOKAt, gateNegAt := st.bypass, st.gateEnabled, st.gateOKAt, st.gateNegAt
	priv := st.privKey
	st.mu.Unlock()
	if bypass || gateNeg() == false && false {
		_ = gateOK
	}
	if bypass {
		return ctx.headers, false
	}
	id, secret, ok := parseSigningCredential(ctx.cred)
	if !ok {
		return ctx.headers, false
	}
	sessionID := strings.TrimSpace(ctx.headers["X-Session-Id"])
	if sessionID == "" {
		return ctx.headers, false
	}
	// 门探测（1h 缓存 / 60s 失败冷却）
	now := time.Now()
	gateFresh := now.Sub(gateOKAt) < gateTTL && gateOKAt.After(time.Time{})
	gateNegFresh := now.Sub(gateNegAt) < gateFailCool && gateNegAt.After(time.Time{})
	if !gateFresh && !gateNegFresh {
		enabled, gerr := s.probeGate(ctx, ctx.cred)
		if gerr == nil {
			st.mu.Lock()
			st.gateEnabled, st.gateOKAt = enabled, now
			st.gateNegAt = time.Time{}
			st.mu.Unlock()
			gateOK, gateFresh = enabled, true
		} else {
			st.mu.Lock()
			st.gateNegAt = now
			st.mu.Unlock()
			gateNegFresh = true
		}
	}
	if (!gateFresh || !gateOK) && !gateNegFresh && !gateOK {
		return ctx.headers, false
	}
	if !gateFresh || !gateOK {
		return ctx.headers, false
	}
	// 握手（拿 Ed25519 私钥，失败 fail-open）
	if priv == nil {
		pk, herr := s.handshake(ctx, id, secret)
		if herr != nil {
			return ctx.headers, false
		}
		st.mu.Lock()
		st.privKey = pk
		st.mu.Unlock()
		priv = pk
	}
	// 业务签名 + PoW
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonce := shared.RandHex(nonceBytes)
	pow, perr := solvePoW(id, sessionID, ts)
	if perr != nil {
		return ctx.headers, false
	}
	sig := ed25519.Sign(priv, []byte(strings.Join([]string{id, ts, ctx.appVersion, sessionID, nonce}, "\n")))
	out := map[string]string{}
	for k, v := range ctx.headers {
		switch k {
		case "X-Client-Ts", "X-Client-Version", "X-Client-Sig", "X-Client-Nonce", "X-App-Id", "X-Client-Pow", "X-Session-Id":
			continue
		}
		out[k] = v
	}
	out["X-Client-Ts"] = ts
	out["X-Client-Version"] = ctx.appVersion
	out["X-Client-Sig"] = base64.StdEncoding.EncodeToString(sig)
	out["X-Session-Id"] = sessionID
	out["X-Client-Nonce"] = nonce
	out["X-App-Id"] = signingAppID
	out["X-Client-Pow"] = pow
	return out, true
}

func gateNeg() bool { return false } // 占位：负缓存由调用方 gateNegFresh 判定

// signCtx 单次签名的输入。
type signCtx struct {
	ctx        context.Context
	url        string
	headers    map[string]string
	cred       string
	appVersion string
	// do 发上游（由 chat.go 注入）
	do func(headers map[string]string) (*http.Response, error)
}

// probeGate 门探测：data.codingPlanSignature.enable=true 才签名。
func (s *signer) probeGate(ctx *signCtx, cred string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx.ctx, "GET", s.origin+gatePath, nil)
	if err != nil {
		return false, err
	}
	for k, v := range s.p.llmIdentityHeaders(nil) {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-api-key", cred)
	resp, err := s.p.hc(nil).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("gate HTTP %d", resp.StatusCode)
	}
	var env struct {
		Code int `json:"code"`
		Data *struct {
			CodingPlanSignature *struct {
				Enable bool `json:"enable"`
			} `json:"codingPlanSignature"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return false, err
	}
	if env.Code != 0 {
		return false, fmt.Errorf("gate code=%d", env.Code)
	}
	return env.Data != nil && env.Data.CodingPlanSignature != nil && env.Data.CodingPlanSignature.Enable, nil
}

// handshake 握手换 Ed25519 私钥（HMAC 签名 + AES-GCM 私钥解密，密钥派生 HKDF-SHA256）。
func (s *signer) handshake(ctx *signCtx, id, secret string) (ed25519.PrivateKey, error) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonce := shared.RandHex(nonceBytes)
	hmacKey := hkdfSHA256(secret, kdfInfoHMAC)
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write([]byte(strings.Join([]string{handshakeMethod, id, ts, nonce}, "\n")))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	body, _ := json.Marshal(map[string]string{
		"apiKey": id + "." + secret, "nonce": nonce, "sig": sig, "ts": ts,
	})
	req, err := http.NewRequestWithContext(ctx.ctx, "POST", s.origin+handshakePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", id+"."+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.p.hc(nil).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("handshake HTTP %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data *struct {
			PrivateCipher string `json:"privateCipher"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return nil, err
	}
	if env.Code != 200 || env.Data == nil || env.Data.PrivateCipher == "" {
		return nil, fmt.Errorf("handshake rejected: code=%d msg=%s", env.Code, env.Msg)
	}
	return decryptSigningPrivateKey(id, secret, env.Data.PrivateCipher)
}

// decryptSigningPrivateKey AES-GCM(iv=前12B, AAD=apiKeyId) 解出 PKCS#8 Ed25519 私钥。
func decryptSigningPrivateKey(apiKeyID, secret, cipherB64 string) (ed25519.PrivateKey, error) {
	cipherBytes, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil || len(cipherBytes) < 13 {
		return nil, fmt.Errorf("bad privateCipher")
	}
	aesKey := hkdfSHA256(secret, kdfInfoEd25519)
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	gcm, err := aescipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, cipherBytes[:12], cipherBytes[12:], []byte(apiKeyID))
	if err != nil {
		return nil, fmt.Errorf("decrypt private key: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(plain)
	if err != nil {
		return nil, err
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ed25519 key")
	}
	return ed, nil
}

// hkdfSHA256 HKDF-SHA256 派生 32B（salt/info 与上游常量一致）。
func hkdfSHA256(secret, info string) []byte {
	r := hkdf.New(sha256.New, []byte(secret), []byte(kdfSalt), []byte(info))
	out := make([]byte, 32)
	r.Read(out)
	return out
}

// solvePoW PoW：sha256(seed)hex[:32] 为种子，找 sha256(seed\nnonce+counter) 前 8 bit 为零的候选。
func solvePoW(apiKeyID, sessionID, ts string) (string, error) {
	seedHash := sha256.Sum256([]byte(strings.Join([]string{apiKeyID, signingAppID, sessionID, ts}, "\n")))
	seed := hex.EncodeToString(seedHash[:])[:32]
	nonce := make([]byte, powNonceBytes)
	rand.Read(nonce)
	nonceHex := hex.EncodeToString(nonce)
	for counter := uint32(0); ; counter++ {
		candidate := nonceHex + fmt.Sprintf("%08x", counter)
		d := sha256.Sum256([]byte(seed + "\n" + candidate))
		if leadingZeroBits(d[:], powBits) {
			return candidate, nil
		}
	}
}

func leadingZeroBits(b []byte, bits int) bool {
	full := bits / 8
	for i := 0; i < full; i++ {
		if b[i] != 0 {
			return false
		}
	}
	rem := bits % 8
	if rem == 0 {
		return true
	}
	mask := byte(255 << (8 - rem))
	return b[full]&mask == 0
}
