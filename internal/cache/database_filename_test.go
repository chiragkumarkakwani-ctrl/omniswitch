package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestSQLiteCacheLegacyFilenamePreservesEntries(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	original, err := NewSQLiteCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.SetContext(ctx, "fixture", &provider.ChatResponse{ID: "original-cache-response", Model: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "omniswitch_cache.db"), filepath.Join(dir, "sentinel_cache.db")); err != nil {
		t.Fatal(err)
	}
	cache, err := NewSQLiteCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	response, found, err := cache.GetContext(ctx, "fixture")
	if err != nil || !found || response == nil || response.ID != "original-cache-response" || response.Model != "fixture" {
		t.Fatalf("legacy cache entry lost after move: %+v, err=%v", response, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sentinel_cache.db")); !os.IsNotExist(err) {
		t.Fatalf("old cache database still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "omniswitch_cache.db")); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteCacheNewFilenameWins(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewSQLiteCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.SetContext(context.Background(), "fixture", &provider.ChatResponse{ID: "new-cache-response"}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := "old cache must remain untouched"
	if err := os.WriteFile(filepath.Join(dir, "sentinel_cache.db"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	cache, err = NewSQLiteCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	response, found, err := cache.GetContext(context.Background(), "fixture")
	if err != nil || !found || response == nil || response.ID != "new-cache-response" {
		t.Fatalf("new cache database was not selected: %+v err=%v", response, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sentinel_cache.db"))
	if err != nil || string(data) != legacy {
		t.Fatalf("legacy cache overwritten: %q err=%v", data, err)
	}
}
