// upstream：CC 上游协议收敛（header / 预请求 / session）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

var proxyClients sync.Map // proxyURL → *http.Client

// ccConfig 组请求体/头时的运行配置（从插件设置解析）。
type ccConfig struct {
	profile                deviceProfile
	fingerprintSalt        string
	cliMode                string // 信封 mode（默认 agent）
	cliSessionMode         string // lifecycle metadata mode（默认 interactive）
	zdr                    bool
	emptySystemPlaceholder bool
}

// hc 凭据对应的 HTTP client（流式对话整体不设超时，长回复合法）。
func (p *plugin) hc(cred *credential) *http.Client {
	key := ""
	if cred != nil {
		key = cred.proxyURL
	}
	if c, ok := proxyClients.Load(key); ok {
		return c.(*http.Client)
	}
	c := shared.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
}

// ---------- 上游请求头 ----------

// initHeaders 预请求（fingerprint / lifecycle）头。
func (p *plugin) initHeaders(cfg ccConfig, cred *credential) map[string]string {
	h := map[string]string{
		"Content-Type":           "application/json",
		"x-cli-environment":      "production",
		"Authorization":          "Bearer " + cred.Key,
		"x-command-code-version": ccProtocolVersion,
	}
	if cfg.zdr {
		h["x-cmd-zdr"] = "1"
	}
	return h
}

// chatHeaders 对话请求头（与 CLI buildCommandAuthHeaders 对齐：User-Agent 固定 "cli"，无 x-co-flag）。
func chatHeaders(cfg ccConfig, cred *credential, sess string) map[string]string {
	h := map[string]string{
		"Content-Type":           "application/json",
		"User-Agent":             "cli",
		"x-command-code-version": ccProtocolVersion,
		"x-cli-environment":      "production",
		"x-project-slug":         slugifyProjectPath(cfg.profile.projectDir),
		"x-taste-learning":       "false",
		"x-session-id":           sess,
		"Authorization":          "Bearer " + cred.Key,
		"traceparent":            traceparent(),
	}
	if cfg.zdr {
		h["x-cmd-zdr"] = "1"
	}
	return h
}

// postJSON POST CC 上游 JSON（小请求：fingerprint/lifecycle），返回 body 与状态码。
func (p *plugin) postJSON(ctx context.Context, cred *credential, path string, headers map[string]string, body any) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", p.apiBase()+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// ---------- session / 指纹预请求 ----------

// sessionID per-key session：12h + 1h 抖动，到期换新。
func (p *plugin) sessionID(apiKey string) string {
	now := time.Now()
	if v, ok := p.sessions.Load(apiKey); ok {
		e := v.(*sessionEntry)
		if now.Before(e.expiresAt) {
			return e.id
		}
	}
	e := &sessionEntry{id: shared.RandUUID(), expiresAt: now.Add(sessionDuration + randDur(sessionJitter))}
	p.sessions.Store(apiKey, e)
	return e.id
}

// ensureInitialized 首次 + 每 8h+2h 抖动，并发发指纹与 lifecycle 预请求。失败只告警不阻断。
// 指纹来自 blob 持久化（无则按当前设置生成）。
func (p *plugin) ensureInitialized(ctx context.Context, cred *credential, cfg ccConfig) {
	// 节流时间戳存 sessions 同一 Map（init:: 前缀 key），避免再开一张永不清的 Map
	throttleKey := "init::" + cred.Key
	now := time.Now()
	if v, ok := p.sessions.Load(throttleKey); ok {
		if e, ok := v.(*sessionEntry); ok && now.Before(e.expiresAt) {
			return
		}
	}
	stamp := &sessionEntry{id: "", expiresAt: now.Add(initRefresh + randDur(initJitter))}
	p.sessions.Store(throttleKey, stamp)

	fp := p.fingerprintFor(cred)
	header := p.initHeaders(cfg, cred)
	fpBody, _ := json.Marshal(fp)
	lifeBody, _ := json.Marshal(map[string]interface{}{
		"eventType": "cli_session_exists",
		"metadata": map[string]interface{}{
			"sessionId": "sess_" + shared.RandHex(8), "cliVersion": ccProtocolVersion,
			"mode": shared.OrDefault(cfg.cliSessionMode, "interactive"),
			"os":   cfg.profile.platform + "-" + cfg.profile.arch,
		},
	})

	var wg sync.WaitGroup
	wg.Add(2)
	fire := func(path string, body []byte) {
		defer wg.Done()
		_, status, err := p.postJSON(ctx, cred, path, header, json.RawMessage(body))
		if err != nil {
			p.host.Log("warn", "commandcode init "+path+" error: "+err.Error())
			return
		}
		if status/100 != 2 {
			p.host.Log("warn", fmt.Sprintf("commandcode init %s HTTP %d", path, status))
		}
	}
	go fire("/alpha/fingerprint/record", fpBody)
	go fire("/alpha/lifecycle-events", lifeBody)
	wg.Wait()
}
