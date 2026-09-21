package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// fakeHideKeys 模拟站点完全不返回密钥明文（安全验证拦截）。
var fakeHideKeys bool

// fakeSite 模拟 New API：/v1/models（api_key）+ /api/user/self、/api/user/checkin（access_token + New-Api-User）。
func fakeSite(t *testing.T, checkedIn *bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-good" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]string{{"id": "gpt-4o"}, {"id": "claude-3-5-sonnet"}}})
	})
	mgmt := func(w http.ResponseWriter, r *http.Request) bool {
		// 系统访问令牌 / 登录 JWT / 会话 cookie 三选一；会话与 JWT 路径按真实 New API 要求 New-Api-User 与用户一致
		authz := r.Header.Get("Authorization")
		if authz == "Bearer tok-good" {
			return true
		}
		viaSession := authz == "Bearer jwt-good"
		if c, err := r.Cookie("session"); err == nil && c.Value == "sess-good" {
			viaSession = true
		}
		if viaSession {
			switch r.Header.Get("New-Api-User") {
			case "":
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "无权进行此操作，未提供 New-Api-User"})
				return false
			case "42":
				return true
			default:
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "无权进行此操作，与登录用户不匹配，请重新登录"})
				return false
			}
		}
		w.WriteHeader(401)
		return false
	}
	// 账号密码登录：alice → 旧版 session cookie；bob → 新版 JWT（data.access_token + data.user，15 分钟到期）
	logins := 0
	mux.HandleFunc("/api/user/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case body["username"] == "alice" && body["password"] == "pw":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "sess-good"})
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{"id": 42}})
		case body["username"] == "bob" && body["password"] == "pw":
			logins++
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{
				"access_token": "jwt-good", "token_type": "Bearer", "access_expires_at": time.Now().Unix() + 900,
				"user": map[string]interface{}{"id": 42, "username": "alice"},
			}})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "用户名或密码错误"})
		}
	})
	// 签发系统访问令牌：新版（JWT 会话）要求安全验证，拒绝
	mux.HandleFunc("/api/user/token", func(w http.ResponseWriter, r *http.Request) {
		if !mgmt(w, r) {
			return
		}
		if r.Header.Get("Authorization") == "Bearer jwt-good" {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "需要安全验证"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": "tok-good"})
	})
	// API 密钥列表 / 创建（首次为空，创建后有一把，名字取创建时所给）；列表脱敏，明文需按 id 单查（模拟新版站点）
	createdName := ""
	mux.HandleFunc("/api/token/", func(w http.ResponseWriter, r *http.Request) {
		if !mgmt(w, r) {
			return
		}
		if r.URL.Path != "/api/token/" {
			w.WriteHeader(404)
			return
		}
		if r.Method == http.MethodPost {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			createdName, _ = body["name"].(string)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
			return
		}
		items := []map[string]interface{}{{"id": 1, "name": "old", "key": "sk-de****ad", "status": 0}} // 已禁用的旧密钥
		if createdName != "" {
			items = append(items, map[string]interface{}{"id": 2, "name": createdName, "key": "sk-go****od", "status": 1})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{"items": items}})
	})
	// 新版：GET /{id} 仍脱敏，明文只在 POST /{id}/key（fakeHideKeys 时站点不返回任何明文）
	mux.HandleFunc("/api/token/2", func(w http.ResponseWriter, r *http.Request) {
		if !mgmt(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{"id": 2, "key": "sk-go****od", "status": 1}})
	})
	mux.HandleFunc("/api/token/2/key", func(w http.ResponseWriter, r *http.Request) {
		if !mgmt(w, r) || r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if fakeHideKeys {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "需要安全验证"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{"key": "sk-good"}})
	})
	mux.HandleFunc("/api/pricing", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": []map[string]interface{}{
			{"model_name": "gpt-4o", "enable_groups": []string{"default"}}, {"model_name": "deepseek-chat", "enable_groups": []string{"all"}}, {"model_name": "gpt-4o"},
		}})
	})
	mux.HandleFunc("/api/user/self", func(w http.ResponseWriter, r *http.Request) {
		if !mgmt(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{
			"id": 42, "username": "alice", "display_name": "Alice", "quota": 1250000, "used_quota": 250000, "group": "default",
		}})
	})
	mux.HandleFunc("/api/user/checkin", func(w http.ResponseWriter, r *http.Request) {
		if !mgmt(w, r) {
			return
		}
		if r.Header.Get("New-Api-User") != "42" {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "no user id in header"})
			return
		}
		if r.Method == http.MethodPost {
			if *checkedIn {
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "今日已签到"})
				return
			}
			*checkedIn = true
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "签到成功", "data": map[string]interface{}{"quota_awarded": 50000}})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{
			"enabled": true, "stats": map[string]interface{}{"checked_in_today": *checkedIn, "checkin_count": 3, "total_checkins": 10, "total_quota": 500000},
		}})
	})
	return httptest.NewServer(mux)
}

func newTestPlugin(baseURL string) *plugin {
	cfg, _ := json.Marshal(map[string]interface{}{"base_url": baseURL, "quota_per_unit": "500000"})
	return &plugin{loadSettings: func(int64) []byte { return cfg }}
}

func TestLoginAndProfileAndCheckin(t *testing.T) {
	checkedIn := false
	srv := fakeSite(t, &checkedIn)
	defer srv.Close()
	p := newTestPlugin(srv.URL)
	ctx := context.Background()

	// 登录：api_key + access_token，user_id 留空由 /api/user/self 补齐
	res, err := p.Login(ctx, &pb.LoginRequest{MethodId: "api_key", InstanceId: 7,
		Form: map[string]string{"api_key": "sk-good", "access_token": "tok-good"}})
	if err != nil || res.Error != nil {
		t.Fatalf("login failed: %v %v", err, res.GetError())
	}
	var cred credential
	json.Unmarshal(res.Blob, &cred)
	if cred.UserID != 42 || res.Profile.DisplayName != "Alice" {
		t.Fatalf("cred/profile mismatch: %+v %s", cred, res.Profile.DisplayName)
	}
	if res.Profile.Quota["credits"] != "2.50" || res.Profile.Quota["used_credits"] != "0.50" {
		t.Fatalf("quota conversion wrong: %v", res.Profile.Quota)
	}
	if len(res.Profile.Sections) != 1 || res.Profile.Sections[0].Id != "checkin" {
		t.Fatalf("expected checkin section, got %v", res.Profile.Sections)
	}

	// 错误密钥被拒
	bad, _ := p.Login(ctx, &pb.LoginRequest{MethodId: "api_key", InstanceId: 7, Form: map[string]string{"api_key": "sk-bad"}})
	if bad.Error == nil || bad.Error.Code != 401 {
		t.Fatalf("bad key should be 401: %v", bad.Error)
	}

	blob := &pb.CredentialBlob{Blob: res.Blob, InstanceId: 7}

	// 模型目录
	ml, err := p.ListModels(ctx, blob)
	if err != nil || len(ml.Models) != 2 || ml.Models[0].Id != "gpt-4o" {
		t.Fatalf("models: %v %v", err, ml)
	}

	// 签到：首次成功 → 再次为良性「今日已签到」
	r1, err := p.RunTask(ctx, &pb.RunTaskRequest{CapabilityId: "checkin", Credential: blob})
	if err != nil || r1.Summary != "签到成功，额度 +$0.10" {
		t.Fatalf("checkin 1: %v %q", err, r1.GetSummary())
	}
	r2, err := p.RunTask(ctx, &pb.RunTaskRequest{CapabilityId: "checkin", Credential: blob})
	if err != nil || r2.Summary != "今日已签到（上游确认）" {
		t.Fatalf("checkin 2: %v %q", err, r2.GetSummary())
	}

	// 刷新：Blob 为空（凭据不变），签到块反映已签到
	rf, _ := p.Refresh(ctx, blob)
	if rf.Error != nil || len(rf.Blob) != 0 || rf.Profile.Sections[0].Entries[0].Value != "status:已签到" {
		t.Fatalf("refresh: %v", rf)
	}
}

func TestSiteRequiresBaseURL(t *testing.T) {
	p := &plugin{loadSettings: func(int64) []byte { return []byte(`{}`) }}
	if _, err := p.site(1); err == nil {
		t.Fatal("expected error when base_url missing")
	}
}

func TestLoginWithoutManagementToken(t *testing.T) {
	checkedIn := false
	srv := fakeSite(t, &checkedIn)
	defer srv.Close()
	p := newTestPlugin(srv.URL)
	res, err := p.Login(context.Background(), &pb.LoginRequest{MethodId: "api_key", Form: map[string]string{"api_key": "sk-good"}})
	if err != nil || res.Error != nil {
		t.Fatalf("login: %v %v", err, res.GetError())
	}
	// 仅 API 密钥无法自动签到：摘要 + 站内提醒（与站点签到同形态）
	r, _ := p.RunTask(context.Background(), &pb.RunTaskRequest{CapabilityId: "checkin", Credential: &pb.CredentialBlob{Blob: res.Blob}})
	if !strings.Contains(r.Summary, "手动签到") || r.Notification == nil {
		t.Fatalf("expected manual reminder, got %q %v", r.Summary, r.Notification)
	}
}

// 站点不返回密钥明文：账号照常建档但 healthy=false；刷新取得后 healthy=true 并通知。
func TestLoginPasswordWithoutPlainKey(t *testing.T) {
	fakeHideKeys = true
	defer func() { fakeHideKeys = false }()
	checkedIn := false
	srv := fakeSite(t, &checkedIn)
	defer srv.Close()
	p := newTestPlugin(srv.URL)
	ctx := context.Background()
	res, err := p.Login(ctx, &pb.LoginRequest{MethodId: "password", InstanceId: 1, Form: map[string]string{"username": "alice", "password": "pw"}})
	if err != nil || res.Error != nil {
		t.Fatalf("login should succeed without key: %v %v", err, res.GetError())
	}
	var cred credential
	json.Unmarshal(res.Blob, &cred)
	if cred.APIKey != "" || res.Profile.Healthy || res.Profile.Quota["api_key"] == "" {
		t.Fatalf("expected unhealthy account without key: %+v %v", cred, res.Profile)
	}
	blob := &pb.CredentialBlob{Blob: res.Blob, InstanceId: 1}
	// 无密钥时模型目录回退 /api/pricing（去重）
	ml, err := p.ListModels(ctx, blob)
	if err != nil || len(ml.Models) != 2 || ml.Models[1].Id != "deepseek-chat" {
		t.Fatalf("models should fall back to pricing: %v %v", err, ml)
	}
	fakeHideKeys = false
	rf, _ := p.Refresh(ctx, blob)
	var healed credential
	json.Unmarshal(rf.Blob, &healed)
	if rf.Error != nil || !rf.Profile.Healthy || healed.APIKey != "sk-good" || rf.Notification == nil {
		t.Fatalf("refresh should acquire key: %v %+v %v", rf.Error, healed, rf.Notification)
	}
}

// 账号密码：alice（旧版 cookie）拿到系统令牌；bob（新版 JWT）签发被拒 → 保存密码靠会话，过期自动重登。
func TestLoginPasswordBootstrap(t *testing.T) {
	checkedIn := false
	srv := fakeSite(t, &checkedIn)
	defer srv.Close()
	p := newTestPlugin(srv.URL)
	ctx := context.Background()
	creds := map[string]credential{}
	for _, user := range []string{"alice", "bob"} {
		res, err := p.Login(ctx, &pb.LoginRequest{MethodId: "password", InstanceId: 1,
			Form: map[string]string{"username": user, "password": "pw"}})
		if err != nil || res.Error != nil {
			t.Fatalf("login %s: %v %v", user, err, res.GetError())
		}
		var cred credential
		json.Unmarshal(res.Blob, &cred)
		if cred.APIKey != "sk-good" || cred.UserID != 42 {
			t.Fatalf("bootstrap %s wrong: %+v", user, cred)
		}
		if res.Profile.DisplayName != "Alice" || res.Profile.Quota["credits"] != "2.50" {
			t.Fatalf("profile %s: %v", user, res.Profile)
		}
		creds[user] = cred
	}
	if creds["alice"].AccessToken != "tok-good" {
		t.Fatalf("alice should hold system token: %+v", creds["alice"])
	}
	bob := creds["bob"]
	if bob.AccessToken != "" || bob.Password != "pw" || bob.SessionJWT != "jwt-good" || bob.SessionExp == 0 {
		t.Fatalf("bob should fall back to password+jwt: %+v", bob)
	}

	// JWT 过期 → 签到前自动重登，Refresh 回传刷新后的 Blob
	bob.SessionExp = time.Now().Unix() - 1
	blob, _ := json.Marshal(bob)
	r1, err := p.RunTask(ctx, &pb.RunTaskRequest{CapabilityId: "checkin", Credential: &pb.CredentialBlob{Blob: blob, InstanceId: 1}})
	if err != nil || r1.Summary != "签到成功，额度 +$0.10" {
		t.Fatalf("bob checkin: %v %q", err, r1.GetSummary())
	}
	rf, _ := p.Refresh(ctx, &pb.CredentialBlob{Blob: blob, InstanceId: 1})
	var renewed credential
	json.Unmarshal(rf.Blob, &renewed)
	if rf.Error != nil || renewed.SessionExp <= time.Now().Unix() {
		t.Fatalf("refresh should renew session: %v %+v", rf.Error, renewed)
	}

	bad, _ := p.Login(ctx, &pb.LoginRequest{MethodId: "password", Form: map[string]string{"username": "alice", "password": "x"}})
	if bad.Error == nil || bad.Error.Code != 401 {
		t.Fatalf("bad password should be 401: %v", bad.Error)
	}

	// api_key 失效 → Refresh 从站点令牌列表重选可用密钥并回传 Blob
	alice := creds["alice"]
	alice.APIKey = "sk-dead"
	blob, _ = json.Marshal(alice)
	rf, _ = p.Refresh(ctx, &pb.CredentialBlob{Blob: blob, InstanceId: 1})
	var healed credential
	json.Unmarshal(rf.Blob, &healed)
	if rf.Error != nil || healed.APIKey != "sk-good" {
		t.Fatalf("refresh should heal api_key: %v %+v", rf.Error, healed)
	}
	if rf.Notification == nil || !strings.Contains(rf.Notification.Content, "同步模型") {
		t.Fatalf("key change should notify: %v", rf.Notification)
	}
}

// 指定密钥名：站点没有同名密钥时以该名新建；指定到已禁用的密钥则不偷换别的——建档但 healthy=false 并说明原因。
func TestLoginPasswordTokenName(t *testing.T) {
	checkedIn := false
	srv := fakeSite(t, &checkedIn)
	defer srv.Close()
	p := newTestPlugin(srv.URL)
	ctx := context.Background()
	res, err := p.Login(ctx, &pb.LoginRequest{MethodId: "password", InstanceId: 1,
		Form: map[string]string{"username": "alice", "password": "pw", "token_name": "mykey"}})
	if err != nil || res.Error != nil {
		t.Fatalf("login: %v %v", err, res.GetError())
	}
	var cred credential
	json.Unmarshal(res.Blob, &cred)
	if cred.APIKey != "sk-good" || cred.TokenName != "mykey" {
		t.Fatalf("token_name bootstrap wrong: %+v", cred)
	}
	dead, _ := p.Login(ctx, &pb.LoginRequest{MethodId: "password", InstanceId: 1,
		Form: map[string]string{"username": "alice", "password": "pw", "token_name": "old"}})
	if dead.Error != nil || dead.Profile.Healthy || !strings.Contains(dead.Profile.Quota["api_key"], "old") {
		t.Fatalf("disabled named token should yield unhealthy account: %v %v", dead.Error, dead.Profile)
	}
}

// 凭据文件：JSON 形态（带 user_id）可用；仅 Cookie 头原文缺 user_id 时给出明确提示。
func TestLoginCredFile(t *testing.T) {
	checkedIn := false
	srv := fakeSite(t, &checkedIn)
	defer srv.Close()
	p := newTestPlugin(srv.URL)
	res, err := p.Login(context.Background(), &pb.LoginRequest{MethodId: "cred_file", Form: map[string]string{"content": `{"session":"sess-good","user_id":42}`}})
	if err != nil || res.Error != nil {
		t.Fatalf("cred_file json: %v %v", err, res.GetError())
	}
	var cred credential
	json.Unmarshal(res.Blob, &cred)
	if cred.APIKey != "sk-good" || cred.UserID != 42 {
		t.Fatalf("cred_file json → %+v", cred)
	}
	noUID, _ := p.Login(context.Background(), &pb.LoginRequest{MethodId: "cred_file", Form: map[string]string{"content": `_ga=1; session=sess-good; other=x`}})
	if noUID.Error == nil || !strings.Contains(noUID.Error.Message, "user_id") {
		t.Fatalf("cookie-only cred_file should hint user_id: %v", noUID.Error)
	}
	if _, err := parseCredFile(`{"foo":1}`); err == nil {
		t.Fatal("missing session must error")
	}
}

// modePlugin 指定实例签到类型的测试插件。
func modePlugin(baseURL, mode, checkinURL string) *plugin {
	cfg, _ := json.Marshal(map[string]interface{}{
		"base_url": baseURL, "instance_name": "主站", "checkin_mode": mode, "checkin_url": checkinURL,
	})
	return &plugin{loadSettings: func(int64) []byte { return cfg }}
}

func TestCheckinModes(t *testing.T) {
	checkedIn := false
	srv := fakeSite(t, &checkedIn)
	defer srv.Close()
	ctx := context.Background()
	cred, _ := json.Marshal(credential{APIKey: "sk-good", AccessToken: "tok-good", UserID: 42})
	blob := &pb.CredentialBlob{AccountId: "9", Blob: cred, InstanceId: 1}

	// none：不声明能力，任务直接跳过
	p := modePlugin(srv.URL, "none", "")
	caps, _ := p.ListTaskCapabilities(ctx, &pb.TaskCapabilitiesRequest{InstanceId: 1})
	if len(caps.Capabilities) != 0 {
		t.Fatalf("none mode must hide checkin capability: %v", caps)
	}
	caps, _ = p.ListTaskCapabilities(ctx, &pb.TaskCapabilitiesRequest{})
	if len(caps.Capabilities) != 1 {
		t.Fatalf("instance-less query must list full capabilities: %v", caps)
	}
	if r, _ := p.RunTask(ctx, &pb.RunTaskRequest{CapabilityId: "checkin", Credential: blob}); !strings.Contains(r.Summary, "无签到") {
		t.Fatalf("none summary: %q", r.Summary)
	}

	// site：摘要带地址 + 通知
	p = modePlugin(srv.URL, "site", "https://checkin.example.com")
	r, err := p.RunTask(ctx, &pb.RunTaskRequest{CapabilityId: "checkin", Credential: blob})
	if err != nil || !strings.Contains(r.Summary, "https://checkin.example.com") || r.Notification == nil ||
		r.Notification.Title != "New API · 主站 该签到了" || !strings.Contains(r.Notification.Content, "https://checkin.example.com") {
		t.Fatalf("site mode: %v %+v", err, r)
	}

	// refresh：两次 /api/user/self，额度无变化 → 已刷新
	p = modePlugin(srv.URL, "refresh", "")
	r, err = p.RunTask(ctx, &pb.RunTaskRequest{CapabilityId: "checkin", Credential: blob})
	if err != nil || !strings.Contains(r.Summary, "已刷新") {
		t.Fatalf("refresh mode: %v %+v", err, r)
	}

	// login：无密码的凭据 → 明确失败原因
	p = modePlugin(srv.URL, "login", "")
	r, err = p.RunTask(ctx, &pb.RunTaskRequest{CapabilityId: "checkin", Credential: blob})
	if err != nil || r.Error == nil || !strings.Contains(r.Error.Message, "授权方式") {
		t.Fatalf("login mode without password: %v %+v", err, r)
	}
}
