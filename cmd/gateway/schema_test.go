package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestReadinessSchemaPending(t *testing.T) {
	st, err := store.New(t.TempDir(), store.WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := newHealthState(st, provider.NewRegistry(), true)
	rec := httptest.NewRecorder()
	h.readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body struct {
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Current int    `json:"current"`
		Latest  int    `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable || body.Reason != "schema_pending" || body.Current != 0 || body.Latest <= body.Current {
		t.Fatalf("pending readiness = %d: %s", rec.Code, rec.Body.String())
	}
	live := httptest.NewRecorder()
	h.liveness(live, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("pending migrations failed liveness: %d", live.Code)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("migrated readiness = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrapKeyDeferredForPendingSchema(t *testing.T) {
	st, err := store.New(t.TempDir(), store.WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settings := runtimeSettings{authEnabled: true, bootstrapAPIKey: "test-bootstrap-key"}
	if err := ensureBootstrapKey(context.Background(), st, settings); err != nil {
		t.Fatalf("pending schema should not prevent startup: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ensureBootstrapKey(context.Background(), st, settings); err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListAPIKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("bootstrap after migrations = %+v, err=%v", keys, err)
	}
}

func TestMigrateOnStartSettings(t *testing.T) {
	clearGatewayEnv(t)
	settings, err := loadRuntimeSettings()
	if err != nil || !settings.migrateOnStart {
		t.Fatalf("migration default = %v, err=%v", settings.migrateOnStart, err)
	}
	config := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(config, []byte("gateway:\n  migrate_on_start: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNISWITCH_CONFIG", config)
	settings, err = loadRuntimeSettings()
	if err != nil || settings.migrateOnStart {
		t.Fatalf("migration config = %v, err=%v", settings.migrateOnStart, err)
	}
	t.Setenv("OMNISWITCH_MIGRATE_ON_START", "true")
	settings, err = loadRuntimeSettings()
	if err != nil || !settings.migrateOnStart {
		t.Fatalf("migration env override = %v, err=%v", settings.migrateOnStart, err)
	}
	t.Setenv("OMNISWITCH_MIGRATE_ON_START", "false")
	settings, err = loadRuntimeSettings()
	if err != nil || settings.migrateOnStart {
		t.Fatalf("migration env override = %v, err=%v", settings.migrateOnStart, err)
	}
}

func TestConfigStorageSummary(t *testing.T) {
	st, err := store.New(t.TempDir(), store.WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mux := http.NewServeMux()
	registerInfoRoutes(mux, runtimeSettings{storageURL: "postgres://private-user:private-secret@private-host/db"}, provider.NewRegistry(), st)
	getSummary := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		var body struct {
			Storage map[string]any `json:"storage"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("config response = %d: %s", rec.Code, rec.Body.String())
		}
		if len(body.Storage) != 3 {
			t.Fatalf("storage summary must contain only public schema state: %+v", body.Storage)
		}
		return body.Storage
	}
	pending := getSummary()
	if pending["driver"] != "sqlite" || pending["schema_version"] != float64(0) || pending["pending_migrations"].(float64) <= 0 {
		t.Fatalf("pending storage summary = %+v", pending)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := getSummary()
	status, err := st.SchemaStatus(context.Background())
	if err != nil || current["schema_version"] != float64(status.Latest) || current["pending_migrations"] != float64(0) {
		t.Fatalf("current storage summary = %+v, err=%v", current, err)
	}
}
