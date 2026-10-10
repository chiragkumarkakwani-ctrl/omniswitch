package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/policy"
	mcpproxy "github.com/omniswitch-dev/omniswitch/internal/proxy"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestMCPTokenExchangeReceivesVerifiedOIDCSubjectNotGatewayKeys(t *testing.T) {
	var exchanges, calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
			exchanges.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("subject_token") != "verified-jwt" {
				t.Error("exchange did not receive the verified OIDC subject token")
			}
			_, _ = w.Write([]byte(`{"access_token":"delegated","expires_in":3600}`))
			return
		}
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer delegated" {
			t.Error("MCP upstream did not receive the exchanged bearer token")
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	}))
	defer upstream.Close()
	engine, _ := policy.NewEngine()
	proxy, err := mcpproxy.NewMultiHandler(engine, nil, []mcpproxy.TargetConfig{{
		Name: "ops", Upstream: upstream.URL,
		TokenExchange: &mcpproxy.TokenExchangeConfig{TokenURL: upstream.URL, ClientID: "client"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	st := newGatewayTestStore(t)
	hash := sha256.Sum256([]byte("gateway-key"))
	if err := st.InsertAPIKey(context.Background(), store.APIKey{ID: "key", Name: "key", KeyHash: hex.EncodeToString(hash[:]), KeyPrefix: "test", Enabled: true, CreatedAt: time.Now(), Role: "member"}); err != nil {
		t.Fatal(err)
	}
	auth := NewAuthMiddleware(st, true)
	auth.SetTokenAuthenticator(tokenAuthenticatorFunc(func(ctx context.Context, token string) (Identity, error) {
		if token != "verified-jwt" {
			return Identity{}, fmt.Errorf("invalid token")
		}
		return Identity{AuthMethod: "oidc", Subject: "oidc-subject", Role: "member"}, nil
	}))
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ops__search","arguments":{}}}`
	for _, tc := range []struct {
		name    string
		token   string
		enabled bool
		want    int
	}{
		{"verified OIDC", "verified-jwt", true, http.StatusOK},
		{"gateway API key", "gateway-key", true, http.StatusBadGateway},
		{"spoofed header without auth", "spoofed-jwt", false, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+tc.token)
			request.Header.Set("X-Omniswitch-Auth-Method", "oidc")
			recorder := httptest.NewRecorder()
			if tc.enabled {
				auth.Wrap(proxy).ServeHTTP(recorder, request)
			} else {
				NewAuthMiddleware(st, false).Wrap(proxy).ServeHTTP(recorder, request)
			}
			if recorder.Code != tc.want {
				t.Fatalf("status/body = %d/%s, want %d", recorder.Code, recorder.Body.String(), tc.want)
			}
		})
	}
	if exchanges.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("exchanges/upstream calls = %d/%d, want verified OIDC exchange and call", exchanges.Load(), calls.Load())
	}
}
