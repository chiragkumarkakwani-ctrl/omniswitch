package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/policy"
)

func TestDiscoveryCachePartitionsVerifiedExchangeSubjects(t *testing.T) {
	var lists, exchanges atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
			exchanges.Add(1)
			_ = r.ParseForm()
			_, _ = fmt.Fprintf(w, `{"access_token":"%s","expires_in":3600}`, r.Form.Get("subject_token"))
			return
		}
		lists.Add(1)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search-%s"}]}}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	defer endpoint.Close()
	handler := exchangeHandler(t, &TokenExchangeConfig{TokenURL: endpoint.URL, ClientID: "client"})
	target := handler.targets["ops"]
	target.upstream, _ = url.Parse(endpoint.URL)
	handler.targets["ops"] = target
	for _, subject := range []string{"alice", "alice", "bob", "bob"} {
		recorder := serveSubjectMCP(handler, subject, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ops__search-"+subject) {
			t.Fatalf("subject listing status/body = %d/%s", recorder.Code, recorder.Body.String())
		}
	}
	if lists.Load() != 2 || exchanges.Load() != 2 {
		t.Fatalf("lists/exchanges = %d/%d, want per-subject cache isolation", lists.Load(), exchanges.Load())
	}
}

func TestOpenAPIToolsFilterAndEnforceArgumentsWithExchange(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
			_, _ = w.Write([]byte(`{"access_token":"delegated","expires_in":3600}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer delegated" {
			t.Errorf("synthetic target did not receive the exchanged token")
		}
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer endpoint.Close()
	spec := filepath.Join(t.TempDir(), "tools.json")
	if err := os.WriteFile(spec, []byte(`{"openapi":"3.0.0","paths":{"/repos/{id}":{"get":{"operationId":"get_repo"},"delete":{"operationId":"delete_repo"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.NewEngine(
		policy.Rule{Name: "hide-delete", Expression: `action.name == "list" && tool.name == "delete_repo"`},
		policy.Rule{Name: "protect-environment", Expression: `action.name == "call" && tool.name == "delete_repo" && resource.environment == "production"`},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewMultiHandler(engine, nil, []TargetConfig{{
		Name: "ops", Upstream: endpoint.URL, OpenAPISpec: spec,
		Headers:       map[string]string{"authorization": "Bearer static"},
		TokenExchange: &TokenExchangeConfig{TokenURL: endpoint.URL, ClientID: "client"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	listing := serveSubjectMCP(handler, "alice", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if listing.Code != http.StatusOK || strings.Contains(listing.Body.String(), "delete_repo") || !strings.Contains(listing.Body.String(), "ops__get_repo") {
		t.Fatalf("synthetic listing = %d/%s", listing.Code, listing.Body.String())
	}
	denied := serveSubjectMCP(handler, "alice", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ops__delete_repo","arguments":{"id":"protected","environment":"production"}}}`)
	if !strings.Contains(denied.Body.String(), "DENIED") || calls.Load() != 0 {
		t.Fatalf("synthetic protected call/body/upstream = %s/%d", denied.Body.String(), calls.Load())
	}
	allowed := serveSubjectMCP(handler, "alice", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ops__delete_repo","arguments":{"id":"public","environment":"staging"}}}`)
	if allowed.Code != http.StatusOK || !strings.Contains(allowed.Body.String(), "ok") || calls.Load() != 1 {
		t.Fatalf("synthetic allowed call = %d/%s/%d", allowed.Code, allowed.Body.String(), calls.Load())
	}
}

func serveSubjectMCP(handler *Handler, subject, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request = request.WithContext(WithSubjectToken(context.Background(), subject))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
