package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/policy"
)

func TestGetExchangeTokenRFC8693CacheExpiryAndSubjectIsolation(t *testing.T) {
	var requests atomic.Int32
	forms := make(chan url.Values, 8)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected exchange method or content type")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		forms <- r.PostForm
		_, _ = fmt.Fprintf(w, `{"access_token":"downstream-%s","token_type":"Bearer","expires_in":3600}`, r.Form.Get("subject_token"))
	}))
	defer endpoint.Close()
	handler := exchangeHandler(t, &TokenExchangeConfig{
		TokenURL: endpoint.URL, ClientID: "client", ClientSecret: "secret", SubjectTokenType: "subject-type",
		RequestedTokenType: "requested-type", Scope: "read write", Audience: "mcp", Resource: "https://mcp.example.com",
		ActorToken: "actor", ActorTokenType: "actor-type",
	})
	target := handler.targets["ops"]
	ctx := WithSubjectToken(context.Background(), "alice")
	for i := 0; i < 2; i++ {
		token, err := handler.getExchangeToken(ctx, target)
		if err != nil || token != "downstream-alice" {
			t.Fatalf("exchange token/error = %q/%v", token, err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("exchange requests = %d, want cache hit", requests.Load())
	}
	form := <-forms
	for field, want := range map[string]string{
		"grant_type": "urn:ietf:params:oauth:grant-type:token-exchange", "subject_token": "alice",
		"subject_token_type": "subject-type", "requested_token_type": "requested-type",
		"client_id": "client", "client_secret": "secret", "scope": "read write", "audience": "mcp",
		"resource": "https://mcp.example.com", "actor_token": "actor", "actor_token_type": "actor-type",
	} {
		if form.Get(field) != want {
			t.Errorf("form[%s] = %q, want %q", field, form.Get(field), want)
		}
	}
	token, err := handler.getExchangeToken(WithSubjectToken(context.Background(), "bob"), target)
	if err != nil || token != "downstream-bob" || requests.Load() != 2 {
		t.Fatalf("subject-isolated token/error/requests = %q/%v/%d", token, err, requests.Load())
	}
	key := sha256.Sum256([]byte("alice"))
	target.tokenCache.mu.Lock()
	entry := target.tokenCache.entries[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	target.tokenCache.entries[key] = entry
	target.tokenCache.mu.Unlock()
	if _, err := handler.getExchangeToken(ctx, target); err != nil || requests.Load() != 3 {
		t.Fatalf("expired exchange error/requests = %v/%d", err, requests.Load())
	}
	handler.Close()
	if _, err := handler.getExchangeToken(ctx, target); err != nil || requests.Load() != 4 {
		t.Fatalf("Close invalidation error/requests = %v/%d", err, requests.Load())
	}
	other := exchangeHandler(t, handler.targets["ops"].tokenExchange)
	if _, err := other.getExchangeToken(ctx, other.targets["ops"]); err != nil || requests.Load() != 5 {
		t.Fatalf("handler isolation error/requests = %v/%d", err, requests.Load())
	}
}

func TestGetExchangeTokenDefaultsAndConcurrentCache(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = r.ParseForm()
		for _, field := range []string{"subject_token_type", "requested_token_type", "actor_token_type"} {
			if r.Form.Get(field) != accessTokenType {
				t.Errorf("%s = %q, want default token type", field, r.Form.Get(field))
			}
		}
		if r.Form.Has("client_secret") {
			t.Error("public client should omit client_secret")
		}
		_, _ = w.Write([]byte(`{"access_token":"token","token_type":"bearer","expires_in":1}`))
	}))
	defer endpoint.Close()
	handler := exchangeHandler(t, &TokenExchangeConfig{TokenURL: endpoint.URL, ClientID: "client", ActorToken: "actor"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if token, err := handler.getExchangeToken(WithSubjectToken(context.Background(), "alice"), handler.targets["ops"]); err != nil || token != "token" {
				t.Errorf("concurrent exchange = %q/%v", token, err)
			}
		}()
	}
	wg.Wait()
	if requests.Load() != 1 {
		t.Fatalf("exchange requests = %d, want a cached short-lived token", requests.Load())
	}
}

func TestGetExchangeTokenErrorsDoNotExposeCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"rejected", 400, `{"error":"invalid_grant","error_description":"secret alice actor"}`, "HTTP 400"},
		{"invalid JSON", 200, `secret alice actor`, "invalid token exchange response"},
		{"missing token", 200, `{}`, "no access token"},
		{"wrong token type", 200, `{"access_token":"secret","token_type":"MAC"}`, "unsupported token type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer endpoint.Close()
			handler := exchangeHandler(t, &TokenExchangeConfig{TokenURL: endpoint.URL, ClientID: "client", ClientSecret: "secret", ActorToken: "actor"})
			for i := 0; i < 2; i++ {
				_, err := handler.getExchangeToken(WithSubjectToken(context.Background(), "alice"), handler.targets["ops"])
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("exchange error = %v, want %s", err, tc.want)
				}
				for _, secret := range []string{"secret", "alice", "actor"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("exchange error exposed credential %q", secret)
					}
				}
			}
			if requests.Load() != 2 {
				t.Fatal("exchange errors must not be cached")
			}
		})
	}
}

func TestGetExchangeTokenRequiresVerifiedSubjectAndHonorsCancellation(t *testing.T) {
	handler := exchangeHandler(t, &TokenExchangeConfig{TokenURL: "https://auth.example.com/token", ClientID: "client"})
	if _, err := handler.getExchangeToken(context.Background(), handler.targets["ops"]); err == nil || !strings.Contains(err.Error(), "authenticated OIDC") {
		t.Fatalf("missing verified subject = %v, want fail-closed exchange", err)
	}
	ctx, cancel := context.WithCancel(WithSubjectToken(context.Background(), "alice"))
	cancel()
	if _, err := handler.getExchangeToken(ctx, handler.targets["ops"]); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled exchange = %v, want context.Canceled", err)
	}
	if token, err := handler.getExchangeToken(context.Background(), target{}); err != nil || token != "" {
		t.Fatalf("unconfigured exchange = %q/%v", token, err)
	}
	handler.targets["ops"].tokenExchange.TokenURL = "://secret alice actor"
	if _, err := handler.getExchangeToken(WithSubjectToken(context.Background(), "alice"), handler.targets["ops"]); err == nil || err.Error() != "invalid token exchange endpoint" {
		t.Fatalf("invalid endpoint error = %v, want sanitized error", err)
	}
}

func TestGetExchangeTokenDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
	}))
	defer destination.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()
	handler := exchangeHandler(t, &TokenExchangeConfig{TokenURL: endpoint.URL, ClientID: "client", ClientSecret: "secret"})
	_, err := handler.getExchangeToken(WithSubjectToken(context.Background(), "alice"), handler.targets["ops"])
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") || leaked.Load() != 0 {
		t.Fatalf("redirect exchange error/leaked requests = %v/%d", err, leaked.Load())
	}
}

func TestGetExchangeTokenUnknownLifetimeIsNotCached(t *testing.T) {
	for _, expires := range []int{0, -1} {
		var requests atomic.Int32
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			_, _ = fmt.Fprintf(w, `{"issued_token":"token","expires_in":%d}`, expires)
		}))
		handler := exchangeHandler(t, &TokenExchangeConfig{TokenURL: endpoint.URL, ClientID: "client"})
		for i := 0; i < 2; i++ {
			if token, err := handler.getExchangeToken(WithSubjectToken(context.Background(), "alice"), handler.targets["ops"]); err != nil || token != "token" {
				t.Fatalf("unknown-lifetime exchange = %q/%v", token, err)
			}
		}
		endpoint.Close()
		if requests.Load() != 2 {
			t.Fatalf("unknown-lifetime exchange requests = %d, want no caching", requests.Load())
		}
	}
}

func TestTargetRequestUsesExchangedToken(t *testing.T) {
	var upstreamCalls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
			_, _ = w.Write([]byte(`{"access_token":"downstream","expires_in":3600}`))
			return
		}
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer downstream" {
			t.Errorf("target authorization = %q, want exchanged token", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer endpoint.Close()
	handler := exchangeHandler(t, &TokenExchangeConfig{TokenURL: endpoint.URL, ClientID: "client"})
	target := handler.targets["ops"]
	target.upstream, _ = url.Parse(endpoint.URL)
	target.headers = map[string]string{"Authorization": "Bearer static"}
	headers := http.Header{"Authorization": {"Bearer gateway-token"}, "X-Omniswitch-Auth-Method": {"oidc"}}
	if _, err := handler.targetRequest(context.Background(), target, []byte(`{}`), headers); err == nil {
		t.Fatal("spoofed identity headers must not become a verified subject token")
	}
	resp, err := handler.targetRequest(WithSubjectToken(context.Background(), "alice"), target, []byte(`{}`), headers)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if upstreamCalls.Load() != 1 {
		t.Fatalf("target calls = %d, want one verified exchange", upstreamCalls.Load())
	}
}

func exchangeHandler(t *testing.T, config *TokenExchangeConfig) *Handler {
	t.Helper()
	engine, err := policy.NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewMultiHandler(engine, nil, []TargetConfig{{Name: "ops", Upstream: "https://mcp.example.com/mcp", TokenExchange: config}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	return handler
}
