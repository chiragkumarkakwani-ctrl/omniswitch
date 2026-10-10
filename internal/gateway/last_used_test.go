package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestAuthMiddlewareLastUsedThrottle(t *testing.T) {
	st := newGatewayTestStore(t)
	ctx := context.Background()
	const rawKey = "test-last-used-key"
	for _, key := range []store.APIKey{
		{ID: "key_one", Name: "one", KeyHash: lastUsedKeyHash(rawKey), CreatedAt: time.Now().UTC(), Enabled: true},
		{ID: "key_two", Name: "two", KeyHash: lastUsedKeyHash("second-key"), CreatedAt: time.Now().UTC(), Enabled: true},
	} {
		if err := st.InsertAPIKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	auth := NewAuthMiddleware(st, true)
	handler := auth.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+rawKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(); rec.Code != http.StatusNoContent {
		t.Fatalf("authentication status = %d: %s", rec.Code, rec.Body.String())
	}
	first, err := st.GetAPIKeyByID(ctx, "key_one")
	if err != nil || first.LastUsedAt == nil {
		t.Fatalf("first request did not set last_used_at: %+v, err=%v", first, err)
	}
	if rec := request(); rec.Code != http.StatusNoContent {
		t.Fatalf("authentication status = %d", rec.Code)
	}
	assertUsed := func(id string, want time.Time) {
		t.Helper()
		key, err := st.GetAPIKeyByID(ctx, id)
		if err != nil || key.LastUsedAt == nil || !key.LastUsedAt.Equal(want) {
			t.Fatalf("%s last use = %v, want %v, err=%v", id, key.LastUsedAt, want, err)
		}
	}
	assertUsed("key_one", *first.LastUsedAt)
	auth.touchAPIKeyLastUsed(ctx, "key_one", first.LastUsedAt.Add(59*time.Second))
	assertUsed("key_one", *first.LastUsedAt)
	boundary := first.LastUsedAt.Add(time.Minute)
	auth.touchAPIKeyLastUsed(ctx, "key_one", boundary)
	assertUsed("key_one", boundary)

	// Throttling one key must not suppress updates to another key.
	auth.touchAPIKeyLastUsed(ctx, "key_two", boundary)
	assertUsed("key_two", boundary)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			auth.touchAPIKeyLastUsed(ctx, "key_one", boundary.Add(time.Duration(i)*time.Second))
		}(i)
	}
	wg.Wait()
	assertUsed("key_one", boundary)
}

func TestAuthMiddlewareRejectedKeyDoesNotTouchLastUsed(t *testing.T) {
	st := newGatewayTestStore(t)
	ctx := context.Background()
	expired := time.Now().UTC().Add(-time.Hour)
	for _, test := range []struct {
		name    string
		enabled bool
		expires *time.Time
	}{
		{name: "disabled", enabled: false},
		{name: "expired", enabled: true, expires: &expired},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := st.InsertAPIKey(ctx, store.APIKey{ID: test.name, Name: test.name, KeyHash: lastUsedKeyHash(test.name), CreatedAt: time.Now().UTC(), Enabled: test.enabled, ExpiresAt: test.expires}); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+test.name)
			rec := httptest.NewRecorder()
			NewAuthMiddleware(st, true).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("rejected key reached handler")
			})).ServeHTTP(rec, req)
			key, err := st.GetAPIKeyByID(ctx, test.name)
			if rec.Code != http.StatusForbidden || err != nil || key.LastUsedAt != nil {
				t.Fatalf("rejected key touched: status=%d, key=%+v, err=%v", rec.Code, key, err)
			}
		})
	}
}

func lastUsedKeyHash(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}
