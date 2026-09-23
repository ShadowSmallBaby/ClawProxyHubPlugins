// ima 微信扫码登录：qrconnect 拉二维码 + 长轮询状态 + code 换 ima 凭证（照 ima2api qrCreateSession/qrPollSession）。
// 全链路插件侧 HTTP 完成，不依赖浏览器跨源。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const (
	// wxAppid ima 网页版扫码用 appid
	wxAppid = "wx0d63f5de059f1d52"
	// wxRedirect 微信白名单内唯一允许的 redirect
	wxRedirect = "https://ima.qq.com/login"
	// 微信 OAuth 扫码端点
	wxQRConnectURL = "https://open.weixin.qq.com/connect/qrconnect"        // 拉二维码页（取 uuid）
	wxQRCodeURL    = "https://open.weixin.qq.com/connect/qrcode/"          // + uuid 取二维码图片
	wxQRPollURL    = "https://long.open.weixin.qq.com/connect/l/qrconnect" // 长轮询扫码状态
)

// wxGet 简单 GET（微信 qrconnect 链路，返回原始字节）。
func wxGet(ctx context.Context, rawURL string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// qrCreate 建 wx 扫码会话：qrconnect 页取 uuid，二维码 JPEG 转 data URL。
// 返回 (uuid, 二维码 data URL)。
func (p *plugin) qrCreate(ctx context.Context) (string, string, error) {
	qs := url.Values{
		"appid":         {wxAppid},
		"scope":         {"snsapi_login"},
		"redirect_uri":  {wxRedirect},
		"state":         {"cph"},
		"login_type":    {"jssdk"},
		"self_redirect": {"true"},
	}
	buf, err := wxGet(ctx, wxQRConnectURL+"?"+qs.Encode(), 20*time.Second)
	if err != nil {
		return "", "", err
	}
	html := string(buf)
	// uuid 从长轮询链接取，兜底二维码路径
	var uuidRe = regexp.MustCompile(`/connect/l/qrconnect\?uuid=([0-9A-Za-z_\-]+)`)
	m := uuidRe.FindStringSubmatch(html)
	if m == nil {
		var qrcodeRe = regexp.MustCompile(`connect/qrcode/([0-9A-Za-z_\-]{10,})`)
		m = qrcodeRe.FindStringSubmatch(html)
	}
	if m == nil {
		return "", "", fmt.Errorf("未能从 qrconnect 页面解析出 uuid")
	}
	uuid := m[1]

	img, err := wxGet(ctx, wxQRCodeURL+uuid, 20*time.Second)
	if err != nil || len(img) == 0 {
		return "", "", fmt.Errorf("二维码图片获取失败: %v", err)
	}
	mime := "image/png"
	if len(img) > 1 && img[0] == 0xFF && img[1] == 0xD8 {
		mime = "image/jpeg"
	}
	return uuid, "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img), nil
}

// qrPoll 长轮询扫码状态。返回 (wx_code, state, error)；
// code 非空 = 已确认。单次长轮询上限 25s（微信侧挂住直到状态变化）。
func (p *plugin) qrPoll(ctx context.Context, uuid string) (string, string, error) {
	rawURL := wxQRPollURL + "?uuid=" + url.QueryEscape(uuid)
	buf, err := wxGet(ctx, rawURL, 30*time.Second)
	if err != nil {
		// 超时/网络抖动 = 还没扫码，继续等
		return "", "waiting", nil
	}
	txt := string(buf)
	mErr := regexp.MustCompile(`wx_errcode\s*=\s*(\d+)`).FindStringSubmatch(txt)
	mCode := regexp.MustCompile(`wx_code\s*=\s*['"]([^'"]*)['"]`).FindStringSubmatch(txt)
	errcode := 0
	if mErr != nil {
		fmt.Sscanf(mErr[1], "%d", &errcode)
	}
	switch errcode {
	case 405:
		if mCode == nil || mCode[1] == "" {
			return "", "error", fmt.Errorf("微信返回已确认但没带 code")
		}
		return mCode[1], "confirmed", nil
	case 404:
		return "", "scanned", nil
	case 403:
		return "", "canceled", fmt.Errorf("用户取消了扫码")
	case 402:
		return "", "expired", fmt.Errorf("二维码已过期，请重新发起")
	default:
		return "", "waiting", nil
	}
}

// wxLogin 用微信 code 换 ima 凭证（POST /auth_login/login），组装 x-ima-cookie。
// client_info 对齐客户端抓包（platform=5 Windows 客户端 + guid/qimei36/version）：
// platform=1（网页版）拿到的 token 模型权限表旧，对话回 1411「模型失效」。
// 响应字段名是 snake_case（id_type/token_type，照客户端实测响应）。
func (p *plugin) wxLogin(ctx context.Context, code string) (*credential, error) {
	resp, err := p.imaPost(ctx, nil, nil, pathLogin, map[string]interface{}{
		"client_info": map[string]interface{}{
			"platform": 5,
			"guid":     shared.RandHex(8),
			"version":  "2.6.11.5160",
		},
		"account_type": 2,
		"code":         code,
	}, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := ioReadAll(resp.Body)
	var out struct {
		Code         int64       `json:"code"`
		Msg          string      `json:"msg"`
		Token        string      `json:"token"`
		RefreshToken string      `json:"refresh_token"`
		UserID       string      `json:"user_id"`
		IDType       json.Number `json:"id_type"`    // 实测回字符串 "2"，Number 兼容字符串/数字
		TokenType    json.Number `json:"token_type"` // 实测回数字 14
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil, fmt.Errorf("login 响应异常")
	}
	if out.Code != 0 || out.Token == "" {
		return nil, fmt.Errorf("code=%d msg=%s", out.Code, out.Msg)
	}
	if out.UserID == "" {
		return nil, fmt.Errorf("ima 未返回 userId，无法组 Cookie")
	}
	idType, _ := out.IDType.Int64()
	tokenType, _ := out.TokenType.Int64()
	// 组 cookie：字段名对齐客户端，类型值取登录响应（实测 2/14），缺失兜底。
	// IMA-IUA 不存进账号 cookie，改由发请求时动态补（见 cookieWithIUA），改配置即时生效。
	cookie := fmt.Sprintf("IMA-UID=%s; IMA-TOKEN=%s; IMA-REFRESH-TOKEN=%s; UID-TYPE=%d; TOKEN-TYPE=%d; PLATFORM=H5",
		out.UserID, out.Token, out.RefreshToken, orDefaultInt(idType, 2), orDefaultInt(tokenType, 14))
	return &credential{Cookie: cookie, RefreshToken: out.RefreshToken}, nil
}

// Login 登录入口分发：qr（微信扫码两步）/ cookie（粘贴导入）。
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "qr":
		return p.loginQR(ctx, req)
	case "cookie":
		return p.loginCookie(ctx, req)
	}
	return nil, fmt.Errorf("unknown auth method: %s", req.MethodId)
}

// loginQR 微信扫码两步：State 空 = 建扫码会话（二维码 data URL）；非空 = 长轮询，confirmed 换凭证建档。
func (p *plugin) loginQR(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	if len(req.State) == 0 {
		uuid, qrURL, err := p.qrCreate(ctx)
		if err != nil {
			return &pb.LoginResult{Error: &pb.Error{Code: 502, Message: "创建扫码会话失败：" + err.Error()}}, nil
		}
		state, _ := json.Marshal(map[string]string{"uuid": uuid})
		return &pb.LoginResult{Next: &pb.LoginNextStep{
			Action: "open_url", Url: qrURL,
			Prompt: map[string]string{
				"zh": "已弹出微信登录二维码，请用微信扫一扫并在手机上确认；确认后自动完成登录",
				"en": "WeChat QR code opened; scan and confirm on your phone to finish login",
			},
			State: state, Wait: true,
		}}, nil
	}
	var s struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(req.State, &s) != nil || s.UUID == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: "扫码会话已失效，请重新发起"}}, nil
	}
	code, state, err := p.qrPoll(ctx, s.UUID)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	if code == "" {
		// waiting / scanned：保持原会话继续轮询（二维码还在）
		stateBytes, _ := json.Marshal(map[string]string{"uuid": s.UUID})
		prompt := map[string]string{"zh": "等待扫码中…", "en": "Waiting for scan…"}
		if state == "scanned" {
			prompt = map[string]string{"zh": "已扫码，请在手机上确认", "en": "Scanned; confirm on your phone"}
		}
		return &pb.LoginResult{Next: &pb.LoginNextStep{Action: "open_url", Prompt: prompt, State: stateBytes, Wait: true}}, nil
	}
	c, err := p.wxLogin(ctx, code)
	if err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "扫码登录失败：" + err.Error()}}, nil
	}
	if err := p.probeSession(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "登录校验失败：" + err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{Blob: blob, Profile: &pb.AccountProfile{DisplayName: credentialName(c), Healthy: true, Quota: map[string]string{}}}, nil
}

// loginCookie 粘贴导入：raw cookie 头或 JSON（cookie/refresh_token 字段）均可，init_session 探测校验。
func (p *plugin) loginCookie(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	cookie, rt := sniffCredential(req.Form["content"])
	if cookie == "" || cookieField(cookie, "IMA-TOKEN") == "" {
		return &pb.LoginResult{Error: &pb.Error{Code: 400,
			Message: "未能提取到有效 Cookie，需含 IMA-TOKEN（请复制 x-ima-cookie 完整值）"}}, nil
	}
	c := &credential{Cookie: cookie, RefreshToken: rt}
	if err := p.probeSession(ctx, c); err != nil {
		return &pb.LoginResult{Error: &pb.Error{Code: 401, Message: "Cookie 校验失败：" + err.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.LoginResult{Blob: blob, Profile: &pb.AccountProfile{DisplayName: credentialName(c), Healthy: true, Quota: map[string]string{}}}, nil
}

// sniffCredential 提取 Cookie：JSON 包裹优先，否则整段当 cookie 头；refresh_token 从 IMA-REFRESH-TOKEN 提取。
func sniffCredential(input string) (cookie, refreshToken string) {
	s := strings.TrimSpace(input)
	if strings.HasPrefix(s, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(s), &m) == nil {
			for _, k := range []string{"cookie", "x-ima-cookie", "content"} {
				if v := strings.TrimSpace(m[k]); v != "" {
					s = v
					break
				}
			}
			if v := strings.TrimSpace(m["refresh_token"]); v != "" {
				refreshToken = v
			}
		}
	}
	cookie = strings.Trim(s, "\"'")
	if refreshToken == "" {
		refreshToken = cookieField(cookie, "IMA-REFRESH-TOKEN")
	}
	return cookie, refreshToken
}

// credentialName 账号展示名（自定义名优先，否则 ima-<UID>）。
func credentialName(c *credential) string {
	if c.Name != "" {
		return c.Name
	}
	return "ima-" + shared.OrDefault(cookieField(c.Cookie, "IMA-UID"), "account")
}
