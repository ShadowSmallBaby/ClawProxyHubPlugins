package newapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared/challenge"
)

func TestChallengeSettings(t *testing.T) {
	for _, raw := range []string{`{}`, `{"challenge_adapter":"disabled","challenge_service_url":12,"challenge_timeout_seconds":"bad"}`} {
		c, err := challengeClientFromSettings([]byte(raw))
		if c != nil || err != nil {
			t.Fatalf("disabled should ignore connection fields: %v", err)
		}
	}
	for _, raw := range []string{`{`, `{"challenge_adapter":"ezsolver"}`, `{"challenge_adapter":123}`, `{"challenge_adapter":"unknown"}`, `{"challenge_adapter":"ezsolver","challenge_service_url":"http://localhost","challenge_timeout_seconds":0}`, `{"challenge_adapter":"ezsolver","challenge_service_url":"http://localhost","challenge_timeout_seconds":5.5}`} {
		_, err := challengeClientFromSettings([]byte(raw))
		var ce *challenge.Error
		if !errors.As(err, &ce) || ce.Code != challenge.InvalidConfig {
			t.Fatalf("wanted invalid_config, got %v", err)
		}
	}
	var schema struct {
		Properties map[string]struct {
			Format string
			Type   string
			OneOf  []struct{ Const string }
		}
	}
	if err := json.Unmarshal([]byte(challengeSettingsSchema()), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["challenge_service_token"].Format != "password" || schema.Properties["challenge_timeout_seconds"].Type != "integer" {
		t.Fatal("incorrect form types")
	}
	opts := schema.Properties["challenge_adapter"].OneOf
	if len(opts) != 3 || opts[0].Const != "disabled" || opts[1].Const != "ezsolver" || opts[2].Const != "solverq" {
		t.Fatal(opts)
	}
}

func TestChallengeSettingsReload(t *testing.T) {
	newService := func(token string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("stale service credentials")
			}
			w.Write([]byte(`{"token":"result"}`))
		}))
		t.Cleanup(s.Close)
		return s
	}
	a, b := newService("first"), newService("second")
	settings := map[string]any{"challenge_adapter": "ezsolver", "challenge_service_url": a.URL, "challenge_service_token": "first"}
	p := &plugin{loadChallengeSettings: func(ctx context.Context) ([]byte, error) { return json.Marshal(settings) }, loadSettings: func(int64) []byte { t.Fatal("must not read instance view"); return nil }}
	for _, s := range []struct{ url, token string }{{a.URL, "first"}, {b.URL, "second"}} {
		settings["challenge_service_url"], settings["challenge_service_token"] = s.url, s.token
		c, err := p.challengeClient(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Solve(context.Background(), challenge.Request{Kind: challenge.TurnstileToken, PageURL: "https://example.com", SiteKey: "key"})
		if err != nil {
			t.Fatal(err)
		}
	}
}
