// upstream.go — HTTP client、mrs-seal-v1（X25519+HKDF+ChaCha20）元数据封密、
// relay 中继客户端（access 刷新、device 票据铸造/缓存、控制面/推理请求）。
package main

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

// hc 凭据对应的 HTTP client（无代理 = 默认直连）。
func (c *credential) hc() *http.Client {
	return sdk.UpstreamClient(c.proxyURL)
}

// sealRelayHeaders 把所有 x-mirasim-*（除 client/enc）JSON 序列化后封密进 x-mirasim-enc。
func sealRelayHeaders(headers http.Header, method, requestPath, sealPubKey string) error {
	recipient, err := decodeSealPubKey(sealPubKey)
	if err != nil {
		return err
	}
	metadata := make(map[string]string)
	sealedNames := make([]string, 0)
	for name := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-mirasim-") || lower == "x-mirasim-client" || lower == "x-mirasim-enc" {
			continue
		}
		value := headers.Get(name)
		if value == "" {
			continue
		}
		metadata[lower] = value
		sealedNames = append(sealedNames, name)
	}
	if len(metadata) == 0 {
		return nil
	}
	plaintext, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode relay metadata: %w", err)
	}
	ephemeralSecret := make([]byte, curve25519.ScalarSize)
	if _, err := io.ReadFull(rand.Reader, ephemeralSecret); err != nil {
		return fmt.Errorf("generate seal key: %w", err)
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("generate seal nonce: %w", err)
	}
	aad := []byte(strings.Join([]string{sealVersion, strings.ToUpper(strings.TrimSpace(method)), requestPath}, "\n"))
	sealed, err := sealPayload(recipient, ephemeralSecret, nonce, plaintext, aad)
	if err != nil {
		return err
	}
	for _, name := range sealedNames {
		headers.Del(name)
	}
	headers.Set("x-mirasim-enc", base64.RawURLEncoding.EncodeToString(sealed))
	return nil
}

// decodeSealPubKey 收件人公钥（4 种 base64 编码尝试，须解为 32 字节）。
func decodeSealPubKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		encoded = defaultSealPubKey
	}
	var key []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err = enc.DecodeString(encoded); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("seal public key not valid base64")
	}
	if len(key) != curve25519.PointSize {
		return nil, fmt.Errorf("seal public key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

// sealPayload X25519 ECDH + HKDF-SHA256 + ChaCha20-Poly1305；打包 epk||nonce||ct。
func sealPayload(recipient, ephemeralSecret, nonce, plaintext, aad []byte) ([]byte, error) {
	ephemeralPublic, err := curve25519.X25519(ephemeralSecret, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("derive ephemeral public key: %w", err)
	}
	shared, err := curve25519.X25519(ephemeralSecret, recipient)
	if err != nil {
		return nil, fmt.Errorf("derive shared key: %w", err)
	}
	key, err := hkdf.Key(sha256.New, shared, ephemeralPublic, sealVersion, chacha20poly1305.KeySize)
	if err != nil {
		return nil, fmt.Errorf("derive seal key: %w", err)
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("init seal: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	packed := make([]byte, 0, len(ephemeralPublic)+len(nonce)+len(ciphertext))
	packed = append(packed, ephemeralPublic...)
	packed = append(packed, nonce...)
	packed = append(packed, ciphertext...)
	return packed, nil
}

// ticketEntry 进程级票据缓存项（按 deviceID 缓存，跨请求复用）。
type ticketEntry struct {
	ticket      string
	expiresAt   time.Time
	unmintUntil time.Time // 命中 404/501 后回退 access token 的静默期
}

var ticketCache sync.Map // deviceID → *ticketEntry
var ticketMu sync.Map    // deviceID → *sync.Mutex

func ticketLock(deviceID string) *sync.Mutex {
	m, _ := ticketMu.LoadOrStore(deviceID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// relayClient 一次调用的中继上下文。
type relayClient struct {
	cred          *credential
	relayURL      string
	adminURL      string
	clientVersion string
	sealPubKey    string
	collectOff    bool
	http          *http.Client
}

func (p *plugin) relay(cred *credential, s map[string]string) *relayClient {
	return &relayClient{
		cred:          cred,
		relayURL:      p.relayURL(s),
		adminURL:      p.adminURL(s),
		clientVersion: p.clientVersion(s),
		sealPubKey:    strings.TrimSpace(s["seal_pubkey"]),
		collectOff:    collectOff(s),
		http:          cred.hc(),
	}
}

// ensureAccess access token 就绪：进入 30s stale lead 或缺失即用 refresh token 刷新。
func (rc *relayClient) ensureAccess(ctx context.Context) error {
	c := rc.cred
	if c.AccessToken != "" && (c.ExpiresAt == 0 || time.Now().UnixMilli() < c.ExpiresAt-int64(accessStaleLead/time.Millisecond)) {
		return nil
	}
	return rc.refreshAccess(ctx)
}

// refreshAccess POST {admin}/auth/refresh，用私有 client（body 含长期 refresh token，不入日志）。
func (rc *relayClient) refreshAccess(ctx context.Context) error {
	c := rc.cred
	if c.RefreshToken == "" {
		return fmt.Errorf("refresh token 缺失，请重新登录")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": c.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, "POST", rc.adminURL+"/auth/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rc.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw := shared.ReadLimited(resp.Body, 1<<20)
	if resp.StatusCode == 401 {
		return fmt.Errorf("refresh token 已失效，请重新登录")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("refresh failed: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.AccessToken == "" {
		return fmt.Errorf("refresh 响应缺少 access_token")
	}
	c.AccessToken = payload.AccessToken
	if payload.RefreshToken != "" {
		c.RefreshToken = payload.RefreshToken
	}
	c.ExpiresAt = resolveAccessExpiry(payload.AccessToken, payload.ExpiresIn)
	c.deriveIdentity()
	return nil
}

// resolveAccessExpiry expires_in（秒）> JWT exp > 30 分钟兜底（unix 毫秒）。
func resolveAccessExpiry(accessToken string, expiresIn int64) int64 {
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	}
	if claims := decodeJWTClaims(accessToken); claims != nil {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			return int64(exp) * 1000
		}
	}
	return time.Now().Add(30 * time.Minute).UnixMilli()
}

// relayCredential 中继授权 credential：票据优先，铸造失败/无路由回退 access token。
func (rc *relayClient) relayCredential(ctx context.Context) (string, error) {
	c := rc.cred
	deviceID := c.deviceID
	lock := ticketLock(deviceID)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	if v, ok := ticketCache.Load(deviceID); ok {
		e := v.(*ticketEntry)
		if e.ticket != "" && now.Before(e.expiresAt.Add(-ticketRefreshLead)) {
			return e.ticket, nil
		}
		if now.Before(e.unmintUntil) {
			return c.AccessToken, nil // 静默期内回退 access token
		}
	}
	ticket, unmint, err := rc.mintTicket(ctx)
	if err != nil {
		if c.AccessToken != "" {
			return c.AccessToken, nil // 铸造失败仍尝试用 access token
		}
		return "", err
	}
	entry := &ticketEntry{}
	if unmint > 0 {
		entry.unmintUntil = now.Add(unmint)
		ticketCache.Store(deviceID, entry)
		return c.AccessToken, nil
	}
	entry.ticket = ticket.value
	entry.expiresAt = ticket.expiresAt
	ticketCache.Store(deviceID, entry)
	return ticket.value, nil
}

type mintedTicket struct {
	value     string
	expiresAt time.Time
}

// mintTicket POST /v1/device/session 铸造票据（access token 签名 + Bearer）。
// unmint>0 表示 relay 无票据路由（404=1min / 501=15min），应回退 access token。
func (rc *relayClient) mintTicket(ctx context.Context) (mintedTicket, time.Duration, error) {
	c := rc.cred
	body, _ := json.Marshal(struct {
		PublicKey string `json:"publicKey"`
		DeviceID  string `json:"deviceId"`
	}{PublicKey: c.publicB64, DeviceID: c.deviceID})

	headers, err := c.signHeaders(signingInput{
		method: "POST", path: sessionPath, clientVersion: rc.clientVersion,
		credential: c.AccessToken, body: body,
	})
	if err != nil {
		return mintedTicket{}, 0, err
	}
	headers.Set("Authorization", "Bearer "+c.AccessToken)
	headers.Set("Content-Type", "application/json")

	req, err := http.NewRequestWithContext(ctx, "POST", rc.relayURL+sessionPath, bytes.NewReader(body))
	if err != nil {
		return mintedTicket{}, 0, err
	}
	req.Header = headers
	resp, err := rc.http.Do(req)
	if err != nil {
		return mintedTicket{}, 0, fmt.Errorf("mint device ticket: %w", err)
	}
	defer resp.Body.Close()
	raw := shared.ReadLimited(resp.Body, 1<<20)
	if resp.StatusCode == 404 {
		return mintedTicket{}, time.Minute, nil
	}
	if resp.StatusCode == 501 {
		return mintedTicket{}, 15 * time.Minute, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mintedTicket{}, 0, fmt.Errorf("mint device ticket: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Ticket    string   `json:"ticket"`
		ExpiresIn *float64 `json:"expiresIn"`
		ExpiresAt *float64 `json:"expiresAt"`
	}
	if json.Unmarshal(raw, &payload) != nil || strings.TrimSpace(payload.Ticket) == "" {
		return mintedTicket{}, 0, fmt.Errorf("device ticket response missing ticket")
	}
	now := time.Now()
	exp := now.Add(ticketDefaultTTL)
	if payload.ExpiresIn != nil && *payload.ExpiresIn > 0 {
		exp = now.Add(time.Duration(*payload.ExpiresIn) * time.Second)
	} else if payload.ExpiresAt != nil && *payload.ExpiresAt > 0 {
		exp = time.Unix(int64(*payload.ExpiresAt), 0)
	}
	return mintedTicket{value: strings.TrimSpace(payload.Ticket), expiresAt: exp}, 0, nil
}

// controlDo 控制面请求（/v1/models、/v1/limits、/v1/model-roster）：空 metadata 签名，不封密。
func (rc *relayClient) controlDo(ctx context.Context, method, path string) ([]byte, int, error) {
	if err := rc.ensureAccess(ctx); err != nil {
		return nil, 0, err
	}
	cred, err := rc.relayCredential(ctx)
	if err != nil {
		return nil, 0, err
	}
	headers, err := rc.cred.signHeaders(signingInput{
		method: method, path: path, clientVersion: rc.clientVersion, credential: cred,
	})
	if err != nil {
		return nil, 0, err
	}
	headers.Set("Authorization", "Bearer "+cred)
	req, err := http.NewRequestWithContext(ctx, method, rc.relayURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header = headers
	resp, err := rc.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	return shared.ReadLimited(resp.Body, maxRespBody), resp.StatusCode, nil
}

// relayDo 中继推理请求：带 relay metadata + 封密，返回未关闭的响应（调用方读流）。
func (rc *relayClient) relayDo(ctx context.Context, path string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	if err := rc.ensureAccess(ctx); err != nil {
		return nil, err
	}
	cred, err := rc.relayCredential(ctx)
	if err != nil {
		return nil, err
	}
	metadata := rc.relayMetadata(ctx, path)
	headers, err := rc.cred.signHeaders(signingInput{
		method: "POST", path: path, clientVersion: rc.clientVersion,
		credential: cred, metadata: metadata, body: body,
	})
	if err != nil {
		return nil, err
	}
	if err := sealRelayHeaders(headers, "POST", path, rc.sealPubKey); err != nil {
		return nil, err
	}
	headers.Set("Authorization", "Bearer "+cred)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	for k, v := range extraHeaders {
		headers.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", rc.relayURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers
	return rc.http.Do(req)
}

// relayMetadata session/agent/call/account/locale/collect（后续封进 x-mirasim-enc）。
func (rc *relayClient) relayMetadata(ctx context.Context, path string) map[string]string {
	c := rc.cred
	session := "mirasim_" + shared.RandUUID()
	if c.AccountID != "" {
		// 账号维度稳定 session（对齐官方客户端的会话关联）
		session = "mirasim_" + sha256Hex([]byte(c.AccountID + "\x00" + shared.RandUUID()))[:32]
	}
	metadata := map[string]string{
		"x-mirasim-session": session,
		"x-mirasim-agent":   relayAgent(path),
		"x-mirasim-call":    shared.RandUUID(),
	}
	if v := safeHeaderValue(c.relayAcct); v != "" {
		metadata["x-mirasim-account"] = v
	}
	if rc.collectOff {
		metadata["x-mirasim-collect"] = "off"
	}
	return metadata
}

// relayAgent /v1/responses 或 /v1/alpha/search 前缀 → codex，否则 claude。
func relayAgent(path string) string {
	if strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/v1/alpha/search") {
		return "codex"
	}
	return "claude"
}

// safeHeaderValue 合法 header 值（剔除控制字符）。
func safeHeaderValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, "\r\n\x00") {
		return ""
	}
	return v
}
