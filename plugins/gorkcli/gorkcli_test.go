package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"alice@x.ai": "al***@x.ai",
		"ab@x.ai":    "ab***@x.ai",
		"a@x.ai":     "a***@x.ai",
		"nope":       "nope",
		"":           "",
	}
	for in, want := range cases {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeJWT(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"sub": "u-1", "email": "bob@x.ai", "exp": 123.0})
	token := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	claims := decodeJWT(token)
	if firstStr(claims, "email") != "bob@x.ai" || firstStr(claims, "sub") != "u-1" {
		t.Fatalf("decodeJWT claims mismatch: %+v", claims)
	}
	if decodeJWT("garbage")["sub"] != nil {
		t.Error("decodeJWT should tolerate malformed token")
	}
}

func TestApplyAuthJSONAliases(t *testing.T) {
	c := &credential{}
	err := c.applyAuthJSON(`{"refreshToken":"rt-1","accessToken":"at-1","userId":"u-9","client_id":"grok-cli"}`)
	if err != nil {
		t.Fatalf("applyAuthJSON: %v", err)
	}
	if c.RefreshToken != "rt-1" || c.AccessToken != "at-1" || c.UserID != "u-9" || c.ClientID != "grok-cli" {
		t.Fatalf("alias mapping failed: %+v", c)
	}
	if err := (&credential{}).applyAuthJSON(`{"email":"x@x.ai"}`); err == nil {
		t.Error("expected error when refresh_token absent")
	}
}

func TestExpiringSoon(t *testing.T) {
	if !(&credential{}).expiringSoon() {
		t.Error("empty access token should expire soon")
	}
	if (&credential{AccessToken: "a"}).expiringSoon() {
		t.Error("unknown expiry (0) should not be treated as expiring")
	}
	past := &credential{AccessToken: "a", ExpiresAt: float64(time.Now().Add(-time.Minute).Unix())}
	if !past.expiringSoon() {
		t.Error("past expiry should be expiring")
	}
	future := &credential{AccessToken: "a", ExpiresAt: float64(time.Now().Add(time.Hour).Unix())}
	if future.expiringSoon() {
		t.Error("far future expiry should not be expiring")
	}
}

func TestCredFrom(t *testing.T) {
	blob, _ := json.Marshal(&credential{RefreshToken: "rt"})
	if _, err := credFrom(&pb.CredentialBlob{Blob: blob}); err != nil {
		t.Fatalf("credFrom valid: %v", err)
	}
	if _, err := credFrom(&pb.CredentialBlob{Blob: []byte(`{}`)}); err == nil {
		t.Error("credFrom should reject credential without any token")
	}
	if c, _ := credFrom(&pb.CredentialBlob{Blob: blob, Proxy: &pb.ProxyConfig{Host: "127.0.0.1", Port: 1080}}); c.proxyURL != "http://127.0.0.1:1080" {
		t.Errorf("proxy url = %q", c.proxyURL)
	}
}

func TestClientIDDefault(t *testing.T) {
	if (&credential{}).clientID() != defaultClientID {
		t.Error("empty client id should fall back to default")
	}
	if (&credential{ClientID: "custom"}).clientID() != "custom" {
		t.Error("explicit client id should win")
	}
}
