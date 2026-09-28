// auth.go — 回调登录（两步式）+ ExchangeToken 续期前的凭据组装。
//
// 登录流程：
//  1. 生成 machine_id（hex32）/ device_id（hex32），构建登录 URL（18 参数，
//     ⚠️ 回调参数名必须是 auth_callback_url），起本地回调 server；
//  2. 回调**直接回传 refreshToken**（并存 PKCE 新流程带 code），不能只找 code；
//  3. ExchangeToken(refreshToken) → GetUserInfo → 建档。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// oauthTTL 授权会话有效期：超时自动关停监听并作废。
const oauthTTL = 10 * time.Minute

// Login 双方式：oauth（浏览器回调）/ token_import（粘贴 token）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "oauth":
		return p.loginOAuth(ctx, req)
	case "token_import":
		return p.loginToken(ctx, req)
	}
	return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "unknown auth method: " + req.MethodId}}, nil
}

// loginToken 贴 access_token（或完整 Cloud-IDE-JWT 头），建档前拉用户信息。
func (p *plugin) loginToken(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	token := strings.TrimSpace(req.Form["access_token"])
	token = strings.TrimPrefix(token, "Cloud-IDE-JWT ")
	token = strings.TrimPrefix(token, "Bearer ")
	if token == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "access_token 不能为空"}}, nil
	}
	c := &credential{
		AccessToken:  token,
		RefreshToken: strings.TrimSpace(req.Form["refresh_token"]),
		UID:          strings.TrimSpace(req.Form["uid"]),
		Label:        "trae",
	}
	// 补齐 uid/昵称/脱敏手机号（GetUserInfo 失败不阻塞登录）
	p.getUserInfo(ctx, c)
	if c.UID == "" && c.MachineID == "" {
		// 首次导入：补一份设备指纹（签到设备身份派生用）
		c.MachineID = shared.RandHex(32)
		c.DeviceID = shared.RandHex(32)
	}
	return p.loginDone(ctx, c)
}

// loginOAuth 两步式：第一步起回调 server 返回登录 URL；后续步粘贴回调 URL / 轮询状态。
func (p *plugin) loginOAuth(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		// 第一步：生成设备指纹 + 起本地回调 server
		state := shared.RandHex(16)
		machineID := shared.RandHex(32)
		deviceID := shared.RandHex(32)
		sess := &oauthSession{
			status: "pending", machineID: machineID, deviceID: deviceID,
			expireAt: time.Now().Add(oauthTTL),
		}
		cb := startCallbackServer(state, func(rawQuery url.Values) {
			p.completeOAuth(state, rawQuery) // 回调到达：异步兑换，成功即关停监听
		})
		p.mu.Lock()
		p.oauth = map[string]*oauthSession{state: sess} // 单条即可，覆盖旧的
		p.oauthCB = cb
		p.mu.Unlock()
		time.AfterFunc(oauthTTL, func() { p.expireOAuth(state) })

		loginURL := buildLoginURL(machineID, deviceID, cb.RedirectURI())
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url", Url: loginURL,
			Prompt: map[string]string{"zh": "已打开浏览器授权页，完成登录后此处自动完成", "en": "Browser auth page opened; this step completes automatically after sign-in"},
			State:  []byte(state),
			Wait:   true,
			Fields: callbackURLField(),
		}}, nil
	}

	// 后续步：手动粘贴的回调 URL 优先，其次轮询回调会话状态
	state := string(req.State)
	p.mu.Lock()
	sess := p.oauth[state]
	p.mu.Unlock()
	if sess == nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "state 已失效，请重新发起"}}, nil
	}

	if raw := strings.TrimSpace(req.Form["callback_url"]); raw != "" {
		info := parseCallbackURL(raw)
		if len(info) == 0 {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "回调地址解析失败，请复制浏览器地址栏的完整 URL"}}, nil
		}
		p.finishOAuth(state)
		return p.exchangeCallback(ctx, sess, info)
	}

	// 轮询：pending 继续等 / failed 报错 / done 即刻建档
	p.mu.Lock()
	status, cred, failErr := sess.status, sess.cred, sess.err
	p.mu.Unlock()
	switch status {
	case "done":
		p.finishOAuth(state)
		return p.loginDone(ctx, cred)
	case "failed":
		p.finishOAuth(state)
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: failErr}}, nil
	}
	return &pb.LoginResult{Next: &pb.LoginNextStep{
		Action: "open_url",
		Prompt: map[string]string{"zh": "等待浏览器完成授权...（服务器部署时请把回调地址粘贴到下方提交）", "en": "Waiting for browser authorization... (server deployment: paste the callback URL below and submit)"},
		State:  []byte(state),
		Wait:   true,
		Fields: callbackURLField(),
	}}, nil
}

// oauthSession 一次授权会话的状态：pending → callback → done（凭据就绪）/ failed。
type oauthSession struct {
	status    string
	cred      *credential
	err       string
	query     url.Values
	machineID string
	deviceID  string
	expireAt  time.Time
}

// callbackURLField 手动粘贴回调 URL 的表单字段（服务器部署用）。
func callbackURLField() []*pb.AuthField {
	return []*pb.AuthField{{
		Name:  "callback_url",
		Label: map[string]string{"zh": "回调地址", "en": "Callback URL"},
		Type:  "text",
	}}
}

// completeOAuth 回调到达：解析 query 写入会话，成功即关停监听（页面已自动关闭）。
func (p *plugin) completeOAuth(state string, q url.Values) {
	p.mu.Lock()
	sess := p.oauth[state]
	p.mu.Unlock()
	if sess == nil {
		return
	}
	p.mu.Lock()
	sess.query = q
	sess.status = "callback"
	p.mu.Unlock()
}

// finishOAuth 完成后关停回调 server 并作废会话。
func (p *plugin) finishOAuth(state string) {
	p.mu.Lock()
	if p.oauthCB != nil {
		p.oauthCB.Close()
		p.oauthCB = nil
	}
	delete(p.oauth, state)
	p.mu.Unlock()
}

// expireOAuth 超时兜底：到点未完成则关停监听并作废会话。
func (p *plugin) expireOAuth(state string) {
	p.mu.Lock()
	sess := p.oauth[state]
	p.mu.Unlock()
	if sess == nil {
		return
	}
	p.mu.Lock()
	if sess.status == "pending" || sess.status == "callback" {
		sess.status, sess.err = "failed", "授权超时，请重试"
	}
	p.mu.Unlock()
}

// buildLoginURL 构建登录 URL（18 参数）。
// ⚠️ 回调参数名必须是 auth_callback_url（不是 redirect_uri）。
func buildLoginURL(machineID, deviceID, callbackURL string) string {
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", pluginVersion)
	q.Set("auth_type", "local")
	q.Set("client_id", clientID)
	q.Set("redirect", "0")
	// login_trace_id：machine_id+device_id 尾 16 位
	joined := machineID + deviceID
	trace := joined
	if len(joined) >= 16 {
		trace = joined[len(joined)-16:]
	}
	q.Set("login_trace_id", trace)
	q.Set("auth_callback_url", callbackURL)
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	q.Set("x_device_brand", "PC")
	q.Set("x_device_type", "PC")
	q.Set("x_os_version", "1.0")
	q.Set("x_app_version", ideVersion)
	q.Set("x_app_type", "stable")
	return consoleHst + "/authorization?" + q.Encode()
}

// parseCallbackURL 解析粘贴的回调 URL（query 参数全量保留）。
func parseCallbackURL(raw string) url.Values {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u.Query()
}

// exchangeCallback 回调解出的凭据建档。
// 分支 1：query.refreshToken → ExchangeToken（轮换）；分支 2：userJwt.Token 兜底。
func (p *plugin) exchangeCallback(ctx context.Context, sess *oauthSession, q url.Values) (*pb.LoginResult, error) {
	c := &credential{
		MachineID: sess.machineID,
		DeviceID:  sess.deviceID,
		Label:     "trae",
	}
	refresh := q.Get("refreshToken")
	if refresh == "" {
		if jwt, ok := q["userJwt"]; ok && len(jwt) > 0 {
			// userJwt 是 JSON 字符串（含 Token/RefreshToken）
			var uj struct {
				Token        string `json:"Token"`
				RefreshToken string `json:"RefreshToken"`
			}
			if json.Unmarshal([]byte(jwt[0]), &uj) == nil {
				if uj.RefreshToken != "" {
					refresh = uj.RefreshToken
				} else {
					c.AccessToken = uj.Token
				}
			}
		}
	}
	if refresh != "" {
		// ── 分支 1：ExchangeToken（access + refreshToken 轮换）──
		body, _ := json.Marshal(map[string]interface{}{
			"ClientID": clientID, "RefreshToken": refresh, "ClientSecret": "-", "UserID": "",
		})
		req, err := httpNewReq(ctx, "POST", oauthHost+pathExchange, body)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
		}
		for k, v := range oauthHeaders() {
			req.Header.Set(k, v)
		}
		resp, err := p.hc(c).Do(req)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "ExchangeToken 失败：" + err.Error()}}, nil
		}
		defer resp.Body.Close()
		raw := readResp(resp)
		if resp.StatusCode != 200 {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: fmt.Sprintf(
				"TRAE ExchangeToken 失败（HTTP %d）：%s", resp.StatusCode, shared.Truncate(string(raw), 200))}}, nil
		}
		result, ok := resultField(raw)
		if !ok {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "TRAE ExchangeToken 响应解析失败"}}, nil
		}
		token := strOf(result, "Token")
		if token == "" {
			token = strOf(result, "accessToken")
		}
		if token == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "TRAE ExchangeToken 响应缺少 Token"}}, nil
		}
		c.AccessToken = token
		c.RefreshToken = strOf(result, "RefreshToken")
		if exp := numOf(result, "TokenExpireAt"); exp > 1e12 {
			c.ExpiresAt = fmt.Sprintf("%.0f", exp)
		} else if exp > 0 {
			c.ExpiresAt = fmt.Sprintf("%.0f", exp*1000)
		}
	} else if c.AccessToken == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "回调载荷缺少 refreshToken / userJwt，请重新发起授权"}}, nil
	}

	// GetUserInfo 补齐信息；失败不阻塞（登录已成功）
	p.getUserInfo(ctx, c)
	return p.loginDone(ctx, c)
}

// loginDone 登录完成：凭据 + 档案。
func (p *plugin) loginDone(ctx context.Context, c *credential) (*pb.LoginResult, error) {
	blob, _ := json.Marshal(c)
	prof, _ := p.GetProfile(ctx, &pb.CredentialBlob{Blob: blob})
	return &pb.LoginResult{Blob: blob, Profile: prof}, nil
}

// ---------- 本地回调 server（照 lobsterai 同款形态）----------

// callbackServer 本地回调监听：TRAE 授权页把 auth_callback_url 指向本地址。
type callbackServer struct {
	listener net.Listener
	server   *http.Server
	state    string
	onQuery  func(url.Values)
}

// startCallbackServer 起本地回调监听；TRAE 回调直接回传 refreshToken（query 全量）。
func startCallbackServer(state string, onQuery func(url.Values)) *callbackServer {
	cb := &callbackServer{state: state, onQuery: onQuery}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if cb.onQuery != nil {
			cb.onQuery(r.URL.Query())
		}
		w.Write([]byte(autoClosePage("登录成功，正在完成授权…")))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return &callbackServer{state: state, onQuery: onQuery} // 无回调能力时退化为粘回调路径
	}
	cb.listener = ln
	cb.server = &http.Server{Handler: mux}
	go cb.server.Serve(ln)
	return cb
}

// RedirectURI 回调地址（监听失败返回手工粘贴用的固定端口形态）。
func (c *callbackServer) RedirectURI() string {
	if c.listener == nil {
		return "http://127.0.0.1:53682/auth/callback?return_to=none"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/auth/callback", c.listener.Addr().(*net.TCPAddr).Port)
}

func (c *callbackServer) Close() {
	if c.server != nil {
		c.server.Close()
	}
}

// autoClosePage 授权结果页：短暂展示后自动关闭。
func autoClosePage(msg string) string {
	return "<html><body style=\"font-family:sans-serif;text-align:center;padding-top:80px\">" +
		"<h2>" + msg + "</h2>" +
		"<p style=\"color:#888\">本页面将自动关闭，若未关闭可手动关闭。</p>" +
		"<script>setTimeout(function(){window.close();},800)</script>" +
		"</body></html>"
}

var _ = sync.Mutex{}
