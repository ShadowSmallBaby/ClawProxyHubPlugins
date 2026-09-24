// ima 上游协议：init_session / qa SSE / refresh 请求构建 + Cookie 工具。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	// upstreamBase ima 上游根地址；pathXxx 为各接口路径。
	upstreamBase    = "https://ima.qq.com"
	pathInitSession = "/cgi-bin/session_logic/init_session"
	pathQA          = "/cgi-bin/assistant/qa"
	pathModels      = "/cgi-bin/model_manage/get_models"
	pathLogin       = "/auth_login/login"
	pathRefresh     = "/auth_login/refresh"
	// defaultUserAgent 上游请求 User-Agent（客户端真实形态；可经设置 user_agent 覆盖）
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 IMA/150.0.7871.5349"
	// defaultWebVersion 上游据 x-ima-cookie 的 WEB-VERSION 判定 Web 客户端形态与模型权限：
	// 实测单此字段即让全部模型（含 glm-5.3 / hy4 等最新）通过；缺失或改用 IMA-IUA（App 形态）
	// 则新模型回 1411「模型失效」。官方 Web 升级后如失效可经设置 web_version 更新。
	defaultWebVersion = "5.13.6"
)

var proxyClients sync.Map // proxyURL → *http.Client

// hc 凭据对应的 HTTP client（SSE 流式不限制总时长）。
func (p *plugin) hc(cred *credential) *http.Client {
	key := ""
	if cred != nil {
		key = cred.proxyURL
	}
	if c, ok := proxyClients.Load(key); ok {
		return c.(*http.Client)
	}
	c := sdk.UpstreamClient(key)
	proxyClients.Store(key, c)
	return c
}

// authHeaders 上游请求头（x-ima-cookie + bkn 签名）。
func (p *plugin) authHeaders(c *credential) map[string]string {
	token := cookieField(c.Cookie, "IMA-TOKEN")
	return map[string]string{
		"from_browser_ima": "1",
		"x-ima-cookie":     p.cookieWithWebVersion(c.Cookie),
		"x-ima-bkn":        strconv.FormatInt(calcBkn(token), 10),
		"referer":          upstreamBase,
		"origin":           upstreamBase,
		"User-Agent":       shared.OrDefault(p.settingStr("user_agent"), defaultUserAgent),
		"Content-Type":     "application/json; charset=utf-8",
	}
}

// cookieWithWebVersion 发请求时动态补 WEB-VERSION（Web 客户端形态标识，缺失新模型回 1411）：
// cookie 已含则原样返回；否则追加，值取设置 web_version，空用内置。
// 放请求侧而非存进账号 cookie：改配置即时生效，已登录账号无需重登。
func (p *plugin) cookieWithWebVersion(cookie string) string {
	if cookieField(cookie, "WEB-VERSION") != "" {
		return cookie
	}
	return cookie + "; WEB-VERSION=" + shared.OrDefault(p.settingStr("web_version"), defaultWebVersion)
}

// imaPost POST ima.qq.com JSON 请求（cred 为 nil 时不带鉴权头）。
// 走 SDK HTTPPost 助手：debug 级统一记录原始请求/原始响应（敏感头打码），可直接复制复现。
func (p *plugin) imaPost(ctx context.Context, cred *credential, client *http.Client, path string, body interface{}, extraAccept string) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	if cred != nil {
		for k, v := range p.authHeaders(cred) {
			headers[k] = v
		}
	} else {
		headers["from_browser_ima"] = "1"
		headers["referer"] = upstreamBase
		headers["origin"] = upstreamBase
		headers["User-Agent"] = defaultUserAgent
		headers["Content-Type"] = "application/json; charset=utf-8"
	}
	if extraAccept != "" {
		headers["Accept"] = extraAccept
	}
	if client == nil {
		if cred != nil {
			client = p.hc(cred)
		} else {
			client = sdk.UpstreamClient("")
		}
	}
	hr, err := p.host.HTTPPost(ctx, sdk.HTTPRequest{
		Method: "POST", URL: upstreamBase + path, Headers: headers, Body: payload,
	}, client)
	if err != nil {
		return nil, err
	}
	// 包装为 http.Response（调用方只读 StatusCode/Body）
	return &http.Response{StatusCode: hr.Status, Header: hr.Header, Body: io.NopCloser(bytes.NewReader(hr.Body))}, nil
}

// initSession POST /cgi-bin/session_logic/init_session 建会话；IMA 上限 20 轮。
func (p *plugin) initSession(ctx context.Context, c *credential, title string) (string, error) {
	if title == "" {
		title = "新对话"
	}
	resp, err := p.imaPost(ctx, c, nil, pathInitSession, map[string]interface{}{
		"env_info":   map[string]interface{}{"interact_type": 2, "robot_type": 10000},
		"name":       truncateString(title, 50),
		"msgs_limit": 20,
	}, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := ioReadAll(resp.Body)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", errAuth(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	var out struct {
		Code      int64  `json:"code"`
		Msg       string `json:"msg"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", fmt.Errorf("init_session 响应异常: %s", truncateString(string(raw), 200))
	}
	if out.Code != 0 {
		if out.Code == 41 || out.Code == 600001 {
			return "", errAuth(fmt.Sprintf("code=%d msg=%s", out.Code, out.Msg))
		}
		return "", fmt.Errorf("init_session failed: code=%d msg=%s", out.Code, out.Msg)
	}
	if out.SessionID == "" {
		return "", fmt.Errorf("init_session 响应缺 session_id")
	}
	return out.SessionID, nil
}

// probeSession init_session 探测校验 Cookie（登录/刷新/档案共用）。
func (p *plugin) probeSession(ctx context.Context, c *credential) error {
	_, err := p.initSession(ctx, c, "probe")
	return err
}

// blockParser 行级 SSE → 块级回调适配器（空行分块，event/data 行合并）。
// 回调 error 记入 err 字段并终止后续回调（对齐旧 HTTPStream 透传语义）。
type blockParser struct {
	h      *sdk.Host
	method string
	url    string
	on     func(event, data string) error

	event string
	datas []string
	err   error
}

func (b *blockParser) feed(line string) {
	if b.err != nil {
		return
	}
	switch {
	case strings.HasPrefix(line, "event:"):
		b.event = strings.TrimSpace(line[6:])
	case strings.HasPrefix(line, "data:"):
		b.datas = append(b.datas, strings.TrimSpace(line[5:]))
	case line == "":
		b.flush()
	}
}

func (b *blockParser) flush() {
	if b.err != nil || (b.event == "" && len(b.datas) == 0) {
		return
	}
	e := b.on(b.event, strings.Join(b.datas, "\n"))
	b.event = ""
	b.datas = nil
	if e != nil {
		b.err = e
		b.h.LogFields("debug", "cph-http 流结束(回调中断): "+b.method+" "+b.url, map[string]string{"action": "http", "detail": e.Error()})
	}
}

func (b *blockParser) Feed(line string) { b.feed(line) }
func (b *blockParser) Finish()          { b.flush() }
func (b *blockParser) FinishWithError(code int32, msg string) {
	if b.err == nil {
		b.err = fmt.Errorf("upstream %d: %s", code, msg)
	}
}

// qaStream POST /cgi-bin/assistant/qa SSE 流逐事件回调（event, data）。
// 走 host.StreamSSE（统一日志）；行级流经 blockParser 转回块级回调。
// 返回 HTTP 层错误；业务事件经 onEvent 处理，返回 error 表示终止流。
func (p *plugin) qaStream(ctx context.Context, c *credential, sessionID, question string, modelType int64, modelUpID string,
	onEvent func(event, data string) error) error {

	payload, _ := json.Marshal(map[string]interface{}{
		"session_id":    sessionID,
		"robot_type":    10000,
		"question":      question,
		"question_type": 3, // 对齐官方客户端抓包（2 = 旧值，glm 等模型会直接回 COMPLETED 无输出）
		"client_id":     shared.RandUUID(),
		"model_info": map[string]interface{}{
			"model_type":         modelType,
			"model_id":           modelUpID,
			"enable_enhancement": true, // 官方客户端固定带；缺失时部分模型无输出
		},
		"history_info": map[string]interface{}{"type": 0}, // 对齐抓包
		"client_tools": []interface{}{},
	})
	headers := p.authHeaders(c)
	headers["Accept"] = "text/event-stream"
	bp := &blockParser{h: p.host, method: "POST", url: upstreamBase + pathQA, on: onEvent}
	hr, err := p.host.StreamSSE(ctx, sdk.HTTPRequest{
		Method: "POST", URL: upstreamBase + pathQA, Headers: headers, Body: payload,
	}, p.hc(c), bp)
	if err != nil {
		return err
	}
	if hr.Status != 200 {
		return fmt.Errorf("HTTP %d: %s", hr.Status, truncateString(string(hr.Body), 300))
	}
	if bp.err != nil {
		return bp.err
	}
	return nil
}

// refreshToken 用 refresh_token 换新 IMA-TOKEN，原地更新 c.Cookie，返回有效期秒数。
func (p *plugin) refreshToken(ctx context.Context, c *credential) (int64, error) {
	uid := cookieField(c.Cookie, "IMA-UID")
	if uid == "" {
		return 0, fmt.Errorf("cookie 中缺少 IMA-UID")
	}
	resp, err := p.imaPost(ctx, c, nil, pathRefresh, map[string]interface{}{
		"user_id":         uid,
		"refresh_token":   c.RefreshToken,
		"token_type":      14,
		"registration_id": "",
	}, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := ioReadAll(resp.Body)
	var out struct {
		Code           int64       `json:"code"`
		Msg            string      `json:"msg"`
		Token          string      `json:"token"`
		TokenValidTime json.Number `json:"token_valid_time"` // 上游实测回字符串 "7200"，Number 兼容双形态
	}
	if json.Unmarshal(raw, &out) != nil {
		return 0, fmt.Errorf("refresh 响应异常: %s", truncateString(string(raw), 200))
	}
	if out.Code != 0 || out.Token == "" {
		return 0, fmt.Errorf("code=%d msg=%s", out.Code, out.Msg)
	}
	c.Cookie = replaceCookieToken(c.Cookie, out.Token)
	valid, _ := out.TokenValidTime.Int64()
	return orDefaultInt(valid, 7200), nil
}

// replaceCookieToken 替换 cookie 中的 IMA-TOKEN。
func replaceCookieToken(cookie, newToken string) string {
	if strings.Contains(cookie, "IMA-TOKEN=") {
		return regexp.MustCompile(`IMA-TOKEN=[^;]*`).ReplaceAllString(cookie, "IMA-TOKEN="+newToken)
	}
	return cookie + "; IMA-TOKEN=" + newToken
}

// ---------- Cookie / 签名工具 ----------

// calcBkn x-ima-bkn 签名（DJB hash 变体）。
func calcBkn(token string) int64 {
	h := int64(5381)
	for _, ch := range token {
		h += (h << 5) + int64(ch)
	}
	return h & 0x7FFFFFFF
}

// cookieField 取 cookie 串中的指定字段。
func cookieField(cookie, key string) string {
	if cookie == "" {
		return ""
	}
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, key+"=") {
			return part[len(key)+1:]
		}
	}
	return ""
}

// errAuth 鉴权失效类错误（core 侧提示重新登录）。
func errAuth(msg string) error {
	return fmt.Errorf("登录已失效: %s", msg)
}

// ioReadAll 便捷读取。
func ioReadAll(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, 4<<20))
	// 去 UTF-8 BOM（ima 服务端部分接口回包带 BOM，json.Unmarshal 直接报错）
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	return b, err
}

// truncateString 按字符数截断（保护多字节中文）。
func truncateString(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func orDefaultInt(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// mapAuthErr 判断错误是否属于鉴权失效（core 映射 401 换账号）。
func mapAuthErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "登录已失效") || strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403")
}
