// 浏览器 OAuth 登录：本地回调端口自动接住 JoyCode 登录页回吐的 pt_key。
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	// joycodeLoginURL JoyCode 官方登录页：authPort 指向本地回调端口，authKey 防串号。
	joycodeLoginURL   = "https://joycode.jd.com/login/?ideAppName=JoyCode&fromIde=ide&redirect=0&authPort=%s&authKey=%s"
	oauthCallbackPath = "/api/oauth-callback"
	oauthTTL          = 5 * time.Minute
	manualPort        = "34891" // 监听失败时的占位端口（仅走手动粘贴兜底）
)

// oauthSession 一次授权会话：pending → done（凭据就绪）/ failed。
type oauthSession struct {
	status string
	cred   *credential
	err    string
}

// callbackServer 本地回环回调：JoyCode 登录页授权后回调这里，直接取走 pt_key。
type callbackServer struct {
	listener net.Listener
	server   *http.Server
	state    string
	onCred   func(ptKey, tenant, loginType string)
}

func startCallbackServer(state string, onCred func(ptKey, tenant, loginType string)) *callbackServer {
	cb := &callbackServer{state: state, onCred: onCred}
	mux := http.NewServeMux()
	mux.HandleFunc(oauthCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ptKey := q.Get("pt_key")
		if ak := q.Get("authKey"); ak != "" && ak != cb.state { // authKey 非空则校验（登录页原样回传）
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(autoClosePage("登录状态校验失败，请回到授权窗口重试。")))
			return
		}
		if ptKey == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(autoClosePage("未获取到登录凭据，请重试。")))
			return
		}
		if cb.onCred != nil {
			cb.onCred(ptKey, q.Get("tenant"), q.Get("login_type"))
		}
		w.Write([]byte(autoClosePage("登录成功，正在完成授权…")))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return cb // 无监听能力时退化为粘回调路径
	}
	cb.listener = ln
	cb.server = &http.Server{Handler: mux}
	go cb.server.Serve(ln)
	return cb
}

// Port 回调 server 端口（监听失败返回占位端口，走手动粘贴兜底）。
func (c *callbackServer) Port() string {
	if c.listener == nil {
		return manualPort
	}
	return strconv.Itoa(c.listener.Addr().(*net.TCPAddr).Port)
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

// loginOAuth 浏览器授权：发起时才监听本地回调端口（懒加载），
// JoyCode 登录页完成后回调吐 pt_key，插件侧 validate 补 userId 建档；
// 服务器部署时回调不可达，用户把授权后地址栏完整 URL 粘贴到 callback_url 提交。
func (p *plugin) loginOAuth(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		// 第一步：起本地回调 server + 生成登录链接
		state := shared.RandHex(16)
		cb := startCallbackServer(state, func(ptKey, tenant, loginType string) {
			go p.completeOAuth(state, ptKey, tenant, loginType) // 回调到达：异步校验，成功即关停监听
		})
		p.mu.Lock()
		p.oauth = map[string]*oauthSession{state: {status: "pending"}} // 单条即可，覆盖旧的
		p.oauthCB = cb
		p.mu.Unlock()
		time.AfterFunc(oauthTTL, func() { p.expireOAuth(state) }) // 超时兜底

		loginURL := fmt.Sprintf(joycodeLoginURL, url.QueryEscape(cb.Port()), url.QueryEscape(state))
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url", Url: loginURL,
			Prompt: map[string]string{"zh": "已打开浏览器授权页，完成 JD 登录后此处自动完成", "en": "Browser auth page opened; this step completes automatically after JD sign-in"},
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
		ptKey, tenant, loginType := parseCallbackURL(raw)
		if ptKey == "" {
			return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "回调地址中缺少 pt_key，请复制浏览器地址栏的完整 URL"}}, nil
		}
		p.finishOAuth(state)
		cred, err := p.credFromPtKey(ctx, ptKey, tenant, loginType)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: err.Error()}}, nil
		}
		return loginDone(cred), nil
	}

	// 轮询：pending 继续等 / failed 报错 / done 即刻建档
	p.mu.Lock()
	status, cred, failErr := sess.status, sess.cred, sess.err
	p.mu.Unlock()
	switch status {
	case "done":
		p.finishOAuth(state)
		return loginDone(cred), nil
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

// completeOAuth 回调到达：用 pt_key 校验补 userId 写入会话，成功即关停监听。
func (p *plugin) completeOAuth(state, ptKey, tenant, loginType string) {
	cred, err := p.credFromPtKey(context.Background(), ptKey, tenant, loginType)
	p.mu.Lock()
	if sess, ok := p.oauth[state]; ok {
		if err != nil {
			sess.status, sess.err = "failed", err.Error()
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

// credFromPtKey pt_key + 回调附带的 tenant/loginType → 校验补 userId 的完整凭据。
func (p *plugin) credFromPtKey(ctx context.Context, ptKey, tenant, loginType string) (*credential, error) {
	c := &credential{PtKey: ptKey, Tenant: tenant, LoginType: loginType}
	if err := p.validate(ctx, c); err != nil {
		return nil, fmt.Errorf("校验失败: %w", err)
	}
	return c, nil
}

// parseCallbackURL 从粘贴的回调 URL 提取 pt_key 与 tenant/login_type。
func parseCallbackURL(raw string) (ptKey, tenant, loginType string) {
	if u, err := url.Parse(raw); err == nil {
		q := u.Query()
		return q.Get("pt_key"), q.Get("tenant"), q.Get("login_type")
	}
	return "", "", ""
}

// callbackURLField 手动粘贴回调地址的输入框（服务器部署时本地回调不可达）。
func callbackURLField() []*pb.AuthField {
	return []*pb.AuthField{{
		Name:        "callback_url",
		Label:       map[string]string{"zh": "回调地址", "en": "Callback URL"},
		Type:        "textarea",
		Placeholder: "授权后浏览器地址栏的完整 URL（http://127.0.0.1:…/api/oauth-callback?pt_key=…）",
	}}
}
