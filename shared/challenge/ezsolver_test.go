package challenge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) (Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c, err := NewClient(Config{Adapter: "ezsolver", BaseURL: s.URL + "/prefix/", AccessToken: "service-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func validRequest() Request {
	return Request{Kind: TurnstileToken, PageURL: "https://example.com/checkin", SiteKey: "site-key"}
}

func requireCode(t *testing.T, err error, code Code) {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

func TestSolveProtocol(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/prefix/solve" || r.Header.Get("Authorization") != "Bearer service-secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("incorrect request method/path/headers")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 3 || body["sitekey"] != "site-key" || body["siteurl"] != validRequest().PageURL || body["timeout"] != float64(90) {
			t.Errorf("incorrect protocol body: %v", body)
		}
		fmt.Fprintf(w, `{"token":"token-%d","elapsed":0.1}`, calls.Load())
	})
	for i := 1; i <= 2; i++ {
		result, err := c.Solve(context.Background(), validRequest())
		if err != nil || result.Kind != TurnstileToken || result.Token != fmt.Sprintf("token-%d", i) || result.ExpiresAt != nil {
			t.Fatalf("unexpected result: %+v %v", result, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("tokens were cached")
	}
}

func TestValidationBeforeHTTP(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unsupported request reached service") })
	cases := []struct {
		name   string
		change func(*Request)
		code   Code
	}{
		{"kind", func(r *Request) { r.Kind = ClearanceCookie }, UnsupportedKind},
		{"action", func(r *Request) { r.Action = "login" }, UnsupportedParameter},
		{"cdata", func(r *Request) { r.CData = "secret" }, UnsupportedParameter},
		{"proxy", func(r *Request) { r.TargetProxy = "http://user:secret@proxy" }, UnsupportedParameter},
		{"ua", func(r *Request) { r.UserAgent = "browser" }, UnsupportedParameter},
		{"cookies", func(r *Request) { r.Cookies = []*http.Cookie{{Name: "session", Value: "secret"}} }, UnsupportedParameter},
		{"sitekey", func(r *Request) { r.SiteKey = " " }, InvalidRequest},
		{"page", func(r *Request) { r.PageURL = "file:///tmp" }, InvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRequest()
			tc.change(&r)
			_, err := c.Solve(context.Background(), r)
			requireCode(t, err, tc.code)
		})
	}
}

func TestConfigValidation(t *testing.T) {
	for _, raw := range []string{"", "ftp://example.com", "http:///path", "http://user:secret@example.com", "http://example.com?key=secret", "http://example.com?", "http://example.com#", "http://example.com:bad"} {
		_, err := NewClient(Config{Adapter: "ezsolver", BaseURL: raw})
		requireCode(t, err, InvalidConfig)
	}
	for _, timeout := range []time.Duration{-1, 4 * time.Second, 301 * time.Second} {
		_, err := NewClient(Config{Adapter: "ezsolver", BaseURL: "http://127.0.0.1:8191", Timeout: timeout})
		requireCode(t, err, InvalidConfig)
	}
	_, err := NewClient(Config{Adapter: "disabled", BaseURL: "http://localhost"})
	requireCode(t, err, InvalidConfig)
	_, err = NewClient(Config{Adapter: "ezsolver", BaseURL: "http://localhost", AccessToken: "secret\r\nx: bad"})
	requireCode(t, err, InvalidConfig)
	for _, raw := range []string{"http://localhost:8191", "http://192.168.0.1/prefix", "https://example.com/a%20b/"} {
		if _, err := NewClient(Config{Adapter: "ezsolver", BaseURL: raw}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResponses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   Code
	}{
		{"auth", 401, `{"error":"service-secret"}`, ServiceAuthFailed},
		{"forbidden", 403, "service-secret", ServiceAuthFailed},
		{"limited", 429, "service-secret", RateLimited},
		{"solve", 500, `{"error":"service-secret"}`, SolveFailed},
		{"unavailable", 503, "service-secret", ServiceUnavailable},
		{"empty", 200, `{"token":" "}`, InvalidResponse},
		{"missing", 200, `{}`, InvalidResponse},
		{"null", 200, `null`, InvalidResponse},
		{"wrong_type", 200, `{"token":123}`, InvalidResponse},
		{"conflicting", 200, `{"token":"secret","error":"failure"}`, InvalidResponse},
		{"json", 200, `<html>service-secret</html>`, InvalidResponse},
		{"trailing", 200, `{"token":"secret"}{}`, InvalidResponse},
		{"oversize", 200, strings.Repeat("x", maxResponseBytes+1), InvalidResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := c.Solve(context.Background(), validRequest())
			requireCode(t, err, tc.code)
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("response leaked")
			}
			if calls.Load() != 1 {
				t.Fatal("request retried")
			}
		})
	}
}

func TestRedirectDoesNotLeak(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	_, err := c.Solve(context.Background(), validRequest())
	requireCode(t, err, ServiceUnavailable)
}

func TestContextCancellationAndDeadline(t *testing.T) {
	for _, cancelEarly := range []bool{true, false} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body struct {
					Timeout int `json:"timeout"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.Timeout > 1 {
					t.Error("task deadline not forwarded")
				}
				close(started)
				<-release
			})
			defer close(release)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if cancelEarly {
				go func() { <-started; cancel() }()
			}
			_, err := c.Solve(ctx, validRequest())
			if cancelEarly {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else {
				requireCode(t, err, Timeout)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("unexpected retry")
			}
		})
	}
}

func TestMetadataIsolation(t *testing.T) {
	a := Adapters()
	if len(a) != 2 || !a[0].Supports(TurnstileToken) || a[0].Supports(ClearanceCookie) || len(a[0].OptionalParameters) != 0 {
		t.Fatal(a)
	}
	a[0].Name["zh"] = "changed"
	a[0].Kinds[0] = ClearanceCookie
	if Adapters()[0].Name["zh"] == "changed" || !Adapters()[0].Supports(TurnstileToken) {
		t.Fatal("metadata shares mutable state")
	}
}
