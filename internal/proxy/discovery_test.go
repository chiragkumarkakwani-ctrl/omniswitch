package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/audit"
	"github.com/omniswitch-dev/omniswitch/internal/policy"
)

func TestDiscoveryFiltersBareNamesAndAuditsOneEvent(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Method == "tools/call" {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"github.delete_repo"},{"name":"search"}]}}`))
	}))
	defer upstream.Close()
	engine, err := policy.NewEngine(
		policy.Rule{Name: "hidden", Expression: `action.name == "list" && tool.name == "github.delete_repo"`},
		policy.Rule{Name: "protected-call", Expression: `action.name == "call" && tool.name == "github.delete_repo" && resource.environment == "production"`},
	)
	if err != nil {
		t.Fatal(err)
	}
	var events bytes.Buffer
	handler, err := NewMultiHandler(engine, audit.NewStdoutLogger(&events), []TargetConfig{{Name: "ops", Upstream: upstream.URL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)

	rec := serveMCP(handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "delete_repo") || !strings.Contains(rec.Body.String(), "ops__search") {
		t.Fatalf("listing = %d/%s, want visible search and hidden destructive tool", rec.Code, rec.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(events.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("audit events = %d, want one target listing", len(lines))
	}
	var event audit.Event
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event.Action != "list" || event.Tool != "ops" || event.Metadata["hidden_tools"] != float64(1) {
		t.Fatalf("listing audit = %+v, want hidden_tools=1", event)
	}

	denied := serveMCP(handler, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ops__github.delete_repo","arguments":{"repo":"payments-prod"}}}`, nil)
	if !strings.Contains(denied.Body.String(), "DENIED") || calls.Load() != 0 {
		t.Fatalf("hidden tool call = %s, upstream calls = %d, want enforced denial", denied.Body.String(), calls.Load())
	}
	allowed := serveMCP(handler, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ops__github.delete_repo","arguments":{"repo":"payments-staging"}}}`, nil)
	if strings.Contains(allowed.Body.String(), "DENIED") || calls.Load() != 1 {
		t.Fatalf("hidden tool call = %s, upstream calls = %d, want allowed call", allowed.Body.String(), calls.Load())
	}
}

func TestDiscoveryCacheReevaluatesPolicyAndExpires(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","inputSchema":{"type":"object"}}]}}`))
	}))
	defer upstream.Close()
	engine, err := policy.NewEngine(policy.Rule{Name: "restricted-list", Expression: `action.name == "list" && agent.role == "viewer"`})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(engine, nil, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for i := 0; i < 2; i++ {
		rec := serveMCP(handler, body, http.Header{"X-Omniswitch-Role": {"member"}})
		if !strings.Contains(rec.Body.String(), "default__search") || strings.Contains(rec.Body.String(), "default__default__") {
			t.Fatalf("cached listing = %s, want unmodified upstream name", rec.Body.String())
		}
	}
	viewer := serveMCP(handler, body, http.Header{"X-Omniswitch-Role": {"viewer"}})
	if strings.Contains(viewer.Body.String(), "search") || requests.Load() != 1 {
		t.Fatalf("viewer listing = %s, requests = %d, want re-filtered cache hit", viewer.Body.String(), requests.Load())
	}
	cache := handler.targets["default"].toolsCache
	cache.mu.Lock()
	if remaining := time.Until(cache.expiresAt); remaining <= 0 || remaining > toolListCacheTTL {
		t.Fatalf("cache TTL = %s, want at most 30s", remaining)
	}
	cache.expiresAt = time.Now().Add(-time.Second)
	cache.mu.Unlock()
	serveMCP(handler, body, nil)
	if requests.Load() != 2 {
		t.Fatalf("requests after expiry = %d, want 2", requests.Load())
	}
	handler.Close()
	serveMCP(handler, body, nil)
	if requests.Load() != 3 {
		t.Fatalf("requests after Close = %d, want invalidated cache", requests.Load())
	}
}

func TestDiscoveryConcurrentCacheAndTargetIsolation(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search"}]}}`))
	}))
	defer upstream.Close()
	engine, _ := policy.NewEngine()
	handler, err := NewMultiHandler(engine, nil, []TargetConfig{{Name: "docs", Upstream: upstream.URL}, {Name: "ops", Upstream: upstream.URL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveMCP(handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, nil)
		}()
	}
	wg.Wait()
	if requests.Load() != 2 {
		t.Fatalf("upstream requests = %d, want one per target", requests.Load())
	}
}

func TestHideDestructiveToolsExampleCompiles(t *testing.T) {
	if _, err := policy.NewEngineFromFiles(filepath.Join("..", "..", "policies", "hide-destructive-tools.yaml")); err != nil {
		t.Fatal(err)
	}
}

func serveMCP(handler *Handler, body string, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if headers != nil {
		req.Header = headers.Clone()
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
