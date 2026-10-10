package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyDatabaseMovePreservesUncheckpointedWALData(t *testing.T) {
	fixtureDir := t.TempDir()
	fixture, err := New(fixtureDir, WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	seedBaseline(t, fixture)

	// Copy a consistent, idle WAL-mode fixture while its connection remains
	// open. The main file alone does not contain the uncheckpointed legacy rows.
	dir := t.TempDir()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(fixtureDir, "omniswitch.db"+suffix))
		if err != nil || len(data) == 0 {
			t.Fatalf("legacy WAL fixture %q missing: %v", suffix, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sentinel.db"+suffix), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cacheSnapshot := []byte("legacy cache snapshot is moved without opening it")
	if err := os.WriteFile(filepath.Join(dir, "sentinel_cache.db"), cacheSnapshot, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, "sentinel.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("old database/sidecar remains after first open: %q err=%v", suffix, err)
		}
	}
	logs, _, err := st.ListLogs(context.Background(), LogFilter{Since: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Limit: 10})
	if err != nil || len(logs) != 1 || logs[0].ID != "legacy_request" || logs[0].RequestBody != "original prompt" || logs[0].ResponseBody != "original output" || logs[0].Cost != 1.25 {
		t.Fatalf("legacy data lost after WAL move/migration: %+v, err=%v", logs, err)
	}
	key, err := st.GetAPIKeyByID(context.Background(), "legacy_key")
	if err != nil || key.Name != "legacy" || key.MonthlyTokenBudget != 100 {
		t.Fatalf("legacy key lost: %+v, err=%v", key, err)
	}
	status, err := st.SchemaStatus(context.Background())
	if err != nil || status.Pending != 0 || status.Current != status.Latest {
		t.Fatalf("moved legacy schema not upgraded: %+v, err=%v", status, err)
	}
	cacheData, err := os.ReadFile(filepath.Join(dir, "omniswitch_cache.db"))
	if err != nil || !bytes.Equal(cacheData, cacheSnapshot) {
		t.Fatalf("first store open did not move the cache: %q, err=%v", cacheData, err)
	}
}

func TestNewDatabaseWinsWithoutChangingLegacyDatabase(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAPIKey(context.Background(), APIKey{ID: "new", Name: "new", KeyHash: "new_hash", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("legacy database must not be opened or overwritten")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(filepath.Join(dir, "sentinel.db"+suffix), legacy, 0600); err != nil {
			t.Fatal(err)
		}
	}
	st, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if key, err := st.GetAPIKeyByID(context.Background(), "new"); err != nil || key.Name != "new" {
		t.Fatalf("new database was not selected: %+v, err=%v", key, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(dir, "sentinel.db"+suffix))
		if err != nil || !bytes.Equal(data, legacy) {
			t.Fatalf("legacy database/sidecar changed: %q, err=%v", suffix, err)
		}
	}
}
