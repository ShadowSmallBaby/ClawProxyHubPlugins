// auth.go — 登录（凭据文件 / 浏览器 OAuth）、本地回调服务器、登录 URL 构造。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "auth_file":
		cred, err := parseCred([]byte(req.Form["content"]))
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
		}
		return loginDone(cred)

	case "oauth":
		return p.loginOAuth(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "unknown auth method: "+req.MethodId)
}

// loginOAuth 浏览器授权：发起时才监听本地回调端口（懒加载），
// 回调到达即在插件侧完成兑换并立即关停监听；超时自动作废。
// 服务器部署时回调地址不可达，用户把授权后地址栏的完整 URL 粘贴到 callback_url 提交。
func (p *plugin) loginOAuth(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		// 第一步：起本地回调 server + 生成登录链接
		state := shared.RandHex(16)
		cb := startCallbackServer(state, func(code string) {
			go p.completeOAuth(state, code) // 回调到达：异步兑换，成功即关停监听
		})
		p.mu.Lock()
		p.oauth = map[string]*oauthSession{state: {status: "pending"}} // 单条即可，覆盖旧的
		p.oauthCB = cb
		p.mu.Unlock()
		// 超时兜底：到点未完成则关停监听并作废会话
		time.AfterFunc(oauthTTL, func() { p.expireOAuth(state) })

		loginBase, err := resolveLoginURL(ctx)
		if err != nil {
			p.finishOAuth(state)
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}, nil
		}
		loginURL := composeLoginURL(loginBase, state, cb.RedirectURI())
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
		cbState, code := parseCallbackURL(raw)
		if code == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "回调地址中缺少 code 参数，请复制浏览器地址栏的完整 URL"}}, nil
		}
		if cbState != "" && cbState != state {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "回调地址 state 不匹配，请重新发起授权"}}, nil
		}
		p.finishOAuth(state)
		cred, fail := p.exchangeCred(ctx, code)
		if fail != nil {
			return fail, nil
		}
		return loginDone(cred)
	}

	// 轮询：pending 继续等 / failed 报错 / done 即刻建档
	p.mu.Lock()
	status, cred, failErr := sess.status, sess.cred, sess.err
	p.mu.Unlock()
	switch status {
	case "done":
		p.finishOAuth(state)
		return loginDone(cred)
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

// oauthSession 一次授权会话的状态：pending → done（凭据就绪）/ failed。
type oauthSession struct {
	status string
	cred   *credential
	err    string
}

// oauthTTL 授权会话有效期：超时自动关停监听并作废。
const oauthTTL = 5 * time.Minute

// completeOAuth 回调到达：兑换凭据写入会话，成功即关停监听（页面已自动关闭）。
func (p *plugin) completeOAuth(state, code string) {
	cred, fail := p.exchangeCred(context.Background(), code)
	p.mu.Lock()
	sess, ok := p.oauth[state]
	if ok {
		if fail != nil {
			sess.status, sess.err = "failed", fail.Error.GetMessage()
		} else {
			sess.status, sess.cred = "done", cred
		}
	}
	cb := p.oauthCB
	p.oauthCB = nil
	p.mu.Unlock()
	if cb != nil {
		cb.Close()
	}
}

// expireOAuth 超时兜底：关停监听并作废未完成的会话（已被新一轮覆盖的不动）。
func (p *plugin) expireOAuth(state string) {
	p.mu.Lock()
	sess, ok := p.oauth[state]
	if !ok || sess.status != "pending" {
		p.mu.Unlock()
		return
	}
	sess.status, sess.err = "failed", "授权超时（5 分钟未完成），请重新发起"
	cb := p.oauthCB
	p.oauthCB = nil
	p.mu.Unlock()
	if cb != nil {
		cb.Close()
	}
}

// callbackURLField 手动粘贴回调地址的输入框（服务器部署时本地回调不可达）。
func callbackURLField() []*pb.AuthField {
	return []*pb.AuthField{{
		Name:        "callback_url",
		Label:       map[string]string{"zh": "回调地址", "en": "Callback URL"},
		Type:        "textarea",
		Placeholder: "授权后浏览器地址栏的完整 URL（http://127.0.0.1:…/auth/callback?code=…&state=…）",
	}}
}

// finishOAuth 清理进行中的授权会话与回调 server。
func (p *plugin) finishOAuth(state string) {
	p.mu.Lock()
	delete(p.oauth, state)
	cb := p.oauthCB
	p.oauthCB = nil
	p.mu.Unlock()
	if cb != nil {
		cb.Close()
	}
}

// exchangeCred 用授权码换凭据；fail 非 nil 表示兑换失败（已含错误信息）。
func (p *plugin) exchangeCred(ctx context.Context, code string) (*credential, *pb.LoginResult) {
	cred := &credential{InstallationUUID: shared.RandUUID()}
	exchangeBody := withKeyfrom(map[string]interface{}{"authCode": code}, cred, p.clientVersion())
	resp, err := postJSON(ctx, nil, serverBase+pathExchange, map[string]string{"Content-Type": "application/json"}, exchangeBody)
	if err != nil {
		return nil, &pb.LoginResult{Error: &pb.Error{Code: 502, Message: err.Error()}}
	}
	data, err := envelope(resp)
	if err != nil {
		return nil, &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}
	}
	var exchanged struct {
		AccessToken  string          `json:"accessToken"`
		RefreshToken string          `json:"refreshToken"`
		ExpiresAt    float64         `json:"expiresAt"`
		User         json.RawMessage `json:"user"`
	}
	if err := json.Unmarshal(data, &exchanged); err != nil || exchanged.AccessToken == "" {
		return nil, &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "exchange 响应缺少 accessToken"}}
	}
	cred.AccessToken, cred.RefreshToken, cred.ExpiresAt, cred.User =
		exchanged.AccessToken, exchanged.RefreshToken, exchanged.ExpiresAt, exchanged.User
	return cred, nil
}

// parseCallbackURL 从粘贴的回调 URL 提取 state 与 code（解析失败时裸扫 code 兜底）。
func parseCallbackURL(raw string) (state, code string) {
	if u, err := url.Parse(raw); err == nil {
		q := u.Query()
		return q.Get("state"), q.Get("code")
	}
	return "", extractCode(raw)
}

// callbackServer 本地回环回调：浏览器授权后上游跳转到这里，自动取走 code。
// 页面在通知 onCode 后自动关闭（脚本 window.close，手动打开的标签兜底提示）。
type callbackServer struct {
	listener net.Listener
	server   *http.Server
	state    string
	onCode   func(code string)
}

func startCallbackServer(state string, onCode func(code string)) *callbackServer {
	cb := &callbackServer{state: state, onCode: onCode}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != cb.state || q.Get("code") == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(autoClosePage("登录状态校验失败，请回到授权窗口重试。")))
			return
		}
		if cb.onCode != nil {
			cb.onCode(q.Get("code"))
		}
		w.Write([]byte(autoClosePage("登录成功，正在完成授权…")))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return &callbackServer{state: state, onCode: onCode} // 无回调能力时退化为粘回调路径
	}
	cb.listener = ln
	cb.server = &http.Server{Handler: mux}
	go cb.server.Serve(ln)
	return cb
}

// autoClosePage 授权结果页：短暂展示后自动关闭（脚本开的窗口可关，手动开的兜底文案）。
func autoClosePage(msg string) string {
	return "<html><body style=\"font-family:sans-serif;text-align:center;padding-top:80px\">" +
		"<h2>" + msg + "</h2>" +
		"<p style=\"color:#888\">本页面将自动关闭，若未关闭可手动关闭。</p>" +
		"<script>setTimeout(function(){window.close();},800)</script>" +
		"</body></html>"
}

// RedirectURI 回调地址（监听失败返回手工粘贴用的固定端口形态）。
func (c *callbackServer) RedirectURI() string {
	if c.listener == nil {
		return manualCallback + "?return_to=none"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/auth/callback", c.listener.Addr().(*net.TCPAddr).Port)
}

func (c *callbackServer) Close() {
	if c.server != nil {
		c.server.Close()
	}
}

func loginDone(cred *credential) (*pb.LoginResult, error) {
	blob, _ := json.Marshal(cred)
	name := credentialName(cred)
	return &pb.LoginResult{
		Blob: blob,
		Profile: &pb.AccountProfile{
			DisplayName: name, Healthy: true, Quota: map[string]string{},
		},
	}, nil
}

func credentialName(cred *credential) string {
	var user struct {
		Nickname string `json:"nickname"`
		UserName string `json:"userName"`
		Email    string `json:"email"`
		UserID   string `json:"userId"`
	}
	_ = json.Unmarshal(cred.User, &user)
	for _, v := range []string{user.Nickname, user.UserName, user.Email, user.UserID} {
		if v != "" {
			return v
		}
	}
	return "lobsterai-account"
}

// ---------- 登录 URL 构造 ----------

// resolveLoginURL 配置下发接口取真实登录页，失败回退 Portal。
// 响应形状：{data: {value: "<url>"}}。
func resolveLoginURL(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", overmindLogin, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := sdk.UpstreamClient("").Do(req)
	if err == nil {
		defer resp.Body.Close()
		var body struct {
			Data struct {
				Value string `json:"value"`
			} `json:"data"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil && strings.TrimSpace(body.Data.Value) != "" {
			return strings.TrimSpace(body.Data.Value), nil
		}
	}
	return portalLoginURL, nil
}

// composeLoginURL 拼登录 URL；redirectURI 为本地回调地址（auto 模式为真实回调）。
func composeLoginURL(loginBase, state, redirectURI string) string {
	// return_to 指回登录页自身（hash 路由参数拼在 fragment 上）
	returnTo := appendHashParams(loginBase, map[string]string{"source": "electron", "electronLogin": "success"})
	redirectURI = appendQueryParams(redirectURI, map[string]string{"return_to": returnTo})
	return appendHashParams(loginBase, map[string]string{
		"source": "electron", "redirect_uri": redirectURI, "state": state,
	})
}

// appendQueryParams 普通 URL 的 query 参数追加。
// 纯字符串拼接：u.String() 会对 RawQuery 再 escape，导致双重编码。
func appendQueryParams(base string, params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + q.Encode()
}

// appendHashParams hash 路由页面的参数要落在 fragment 的 query 上。
// 纯字符串拼接，避免 u.String() 对 fragment 的二次 escape。
func appendHashParams(base string, params map[string]string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if u.Fragment == "" {
		return appendQueryParams(base, params)
	}
	fragPath, fq, _ := strings.Cut(u.Fragment, "?")
	values, _ := url.ParseQuery(fq)
	for k, v := range params {
		values.Set(k, v)
	}
	prefix := base[:strings.Index(base, "#")]
	return prefix + "#" + fragPath + "?" + values.Encode()
}

func extractCode(callbackURL string) string {
	if i := strings.Index(callbackURL, "code="); i >= 0 {
		rest := callbackURL[i+len("code="):]
		for _, sep := range []string{"&", "\"", " ", "'"} {
			if j := strings.Index(rest, sep); j >= 0 {
				rest = rest[:j]
			}
		}
		return rest
	}
	return ""
}
