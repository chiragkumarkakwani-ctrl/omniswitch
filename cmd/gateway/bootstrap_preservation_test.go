package main

import (
	"bytes"
	"context"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestBootstrapPreservesAdministrativeState(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	stamp := time.Now().UTC().Truncate(time.Second)
	expires, used := stamp.Add(time.Hour), stamp.Add(-time.Minute)
	key := store.APIKey{ID: "bootstrap-admin", Name: "administrator-chosen name", KeyHash: sha256Hex("old-secret"), KeyPrefix: "original-display", CreatedAt: stamp.Add(-time.Hour), ExpiresAt: &expires, LastUsedAt: &used, Enabled: false, Role: "admin", WorkspaceID: "original-workspace", RateLimit: 17, BudgetUSD: 10, SpendUSD: 8, MonthlyCostBudget: 11, MonthlyTokenBudget: 12, RequestsPerMinute: 13, TokensPerMinute: 14, USDPerDay: 15, Metadata: map[string]string{"keep": "value"}}
	if err := st.InsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetAPIKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&warnings)
	t.Cleanup(func() { log.SetOutput(writer) })
	settings := runtimeSettings{authEnabled: true, bootstrapAPIKey: "old-secret", bootstrapRole: "owner", bootstrapWorkspaceID: "new-workspace"}
	for i := 0; i < 2; i++ {
		if err := ensureBootstrapKey(ctx, st, settings); err != nil {
			t.Fatal(err)
		}
	}
	after, err := st.GetAPIKeyByID(ctx, key.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("restart overwrote state: before=%+v after=%+v err=%v", before, after, err)
	}
	if !strings.Contains(warnings.String(), "disabled") || !strings.Contains(warnings.String(), "personal administrator key") || strings.Contains(warnings.String(), "old-secret") {
		t.Fatalf("disabled bootstrap warning unsafe/missing: %s", warnings.String())
	}
	settings.bootstrapAPIKey = "rotated-secret"
	if err := ensureBootstrapKey(ctx, st, settings); err != nil {
		t.Fatal(err)
	}
	before.KeyHash = sha256Hex("rotated-secret")
	after, err = st.GetAPIKeyByID(ctx, key.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rotation changed more than hash: before=%+v after=%+v err=%v", before, after, err)
	}
	if strings.Contains(warnings.String(), "rotated-secret") {
		t.Fatal("bootstrap secret logged")
	}
}

func TestBootstrapConcurrentStarts(t *testing.T) {
	st := newTestStore(t)
	settings := runtimeSettings{authEnabled: true, bootstrapAPIKey: "concurrent-secret", bootstrapRole: "admin", bootstrapWorkspaceID: "workspace"}
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- ensureBootstrapKey(context.Background(), st, settings) }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	keys, err := st.ListAPIKeys(context.Background())
	if err != nil || len(keys) != 1 || !keys[0].Enabled || keys[0].Role != "admin" || keys[0].WorkspaceID != "workspace" {
		t.Fatalf("concurrent startup keys = %+v/%v", keys, err)
	}
	stored, err := st.GetAPIKeyByHash(context.Background(), sha256Hex(settings.bootstrapAPIKey))
	if err != nil || stored.ID != "bootstrap-admin" {
		t.Fatalf("concurrent hash lookup: %v", err)
	}
}
