package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestMigrateCLIStatusDryRunAndUp(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		name := "sqlite"
		if postgres {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			retentionTestSettings(t)
			dir := t.TempDir()
			t.Setenv("OMNISWITCH_DATA", dir)
			dsn := ""
			if postgres {
				dsn = migrationPostgresDSN(t)
			}
			t.Setenv("OMNISWITCH_DATABASE_URL", dsn)
			t.Setenv("OMNISWITCH_MIGRATE_ON_START", "true")
			initial := migrationSchemaSnapshot(t, postgres, dir, dsn)
			output := runMigrationCLIForTest(t, "status")
			for _, text := range []string{"Driver: " + name, "Current version: 0", "Latest version: 3", "Pending migrations: 3"} {
				if !strings.Contains(output, text) {
					t.Errorf("status missing %q: %s", text, output)
				}
			}
			if after := migrationSchemaSnapshot(t, postgres, dir, dsn); after != initial {
				t.Fatalf("status changed schema: before=%q after=%q", initial, after)
			}
			output = runMigrationCLIForTest(t, "up", "--dry-run")
			for _, text := range []string{"1  baseline", "2  request_usage_and_key_limits", "3  request_and_guardrail_indexes"} {
				if !strings.Contains(output, text) {
					t.Errorf("dry-run missing %q: %s", text, output)
				}
			}
			if after := migrationSchemaSnapshot(t, postgres, dir, dsn); after != initial {
				t.Fatalf("dry-run changed schema: before=%q after=%q", initial, after)
			}
			t.Setenv("OMNISWITCH_MIGRATE_ON_START", "false")
			if output = runMigrationCLIForTest(t, "up"); !strings.Contains(output, "version 3, pending 0") {
				t.Fatalf("explicit up did not migrate: %s", output)
			}
			current := migrationSchemaSnapshot(t, postgres, dir, dsn)
			output = runMigrationCLIForTest(t, "status")
			if !strings.Contains(output, "Current version: 3") || !strings.Contains(output, "Pending migrations: 0") {
				t.Fatalf("post-migration status: %s", output)
			}
			if output = runMigrationCLIForTest(t, "up", "--dry-run"); !strings.Contains(output, "No pending migrations") {
				t.Fatalf("current dry-run: %s", output)
			}
			if output = runMigrationCLIForTest(t, "up"); !strings.Contains(output, "No pending migrations") {
				t.Fatalf("second up was not idempotent: %s", output)
			}
			if after := migrationSchemaSnapshot(t, postgres, dir, dsn); after != current {
				t.Fatal("read-only commands or repeated up changed the current schema")
			}
		})
	}
}

func TestMigrateCLIConfigDataDirectoryAndEnvironmentOverride(t *testing.T) {
	retentionTestSettings(t)
	t.Setenv("OMNISWITCH_DATABASE_URL", "")
	t.Setenv("OMNISWITCH_DATA", "")
	dir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	body := "apiVersion: omniswitch.dev/v1\nkind: GatewayConfig\ngateway:\n  data_dir: " + dir + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNISWITCH_CONFIG", configPath)
	runMigrationCLIForTest(t, "status")
	if _, err := os.Stat(filepath.Join(dir, "omniswitch.db")); err != nil {
		t.Fatalf("CLI ignored configured data directory: %v", err)
	}
	envDir := t.TempDir()
	t.Setenv("OMNISWITCH_DATA", envDir)
	runMigrationCLIForTest(t, "status")
	if _, err := os.Stat(filepath.Join(envDir, "omniswitch.db")); err != nil {
		t.Fatalf("CLI ignored data directory environment override: %v", err)
	}
}

func TestMigrateCLIHelpAndBadArgumentsDoNotOpenStore(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"status", "--help"}, {"up", "--help"}, {"status", "extra"}, {"up", "--unknown"}, {"unknown"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("OMNISWITCH_DATA", dir)
			t.Setenv("OMNISWITCH_CONFIG", filepath.Join(dir, "does-not-exist.yaml"))
			var stdout, stderr bytes.Buffer
			action, code := dispatch(append([]string{"migrate"}, args...), &stdout, &stderr, os.Getenv)
			wantSuccess := args[len(args)-1] == "--help"
			if action != actionExit || (code == 0) != wantSuccess {
				t.Fatalf("args=%v action=%v code=%d stderr=%s", args, action, code, stderr.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "omniswitch.db")); !os.IsNotExist(err) {
				t.Fatalf("help/bad arguments opened store: %v", err)
			}
		})
	}
}

func runMigrationCLIForTest(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	action, code := dispatch(append([]string{"migrate"}, args...), &stdout, &stderr, os.Getenv)
	if action != actionExit || code != 0 {
		t.Fatalf("migrate %v action=%v code=%d stderr=%s", args, action, code, stderr.String())
	}
	return stdout.String()
}

func migrationSchemaSnapshot(t *testing.T, postgres bool, dir, dsn string) string {
	t.Helper()
	var db *sql.DB
	var err error
	query := "SELECT name || ':' || COALESCE(sql, '') FROM sqlite_master ORDER BY name"
	if postgres {
		db, err = sql.Open("pgx", dsn)
		query = "SELECT table_name || ':' || column_name || ':' || data_type FROM information_schema.columns WHERE table_schema = current_schema() ORDER BY table_name, ordinal_position"
	} else {
		db, err = sql.Open("sqlite", filepath.Join(dir, "omniswitch.db"))
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			t.Fatal(err)
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(result, "\n")
}

func migrationPostgresDSN(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("OMNISWITCH_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("OMNISWITCH_TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "osw_migrate_cli_" + strings.ToLower(rand.Text())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema)); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", schema)); err != nil {
			t.Errorf("remove isolated migration test schema: %v", err)
		}
		_ = admin.Close()
	})
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	values := parsed.Query()
	values.Set("search_path", schema)
	parsed.RawQuery = values.Encode()
	return parsed.String()
}

func TestConfiguredStoreDatabaseURLPrecedenceAndLegacyMove(t *testing.T) {
	dsn := migrationPostgresDSN(t)
	t.Setenv("OMNISWITCH_DATABASE_URL", dsn)
	dir := t.TempDir()
	for _, name := range []string{"sentinel.db", "sentinel_cache.db"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	settings := runtimeSettings{dataDir: dir, storageURL: "postgres://127.0.0.1:18119/never_selected"}
	st, err := openConfiguredStore(settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if status, err := st.SchemaStatus(context.Background()); err != nil || status.Current != 0 || st.Driver() != "postgres" {
		t.Fatalf("environment URL did not take precedence: %+v err=%v driver=%s", status, err, st.Driver())
	}
	for old, current := range map[string]string{"sentinel.db": "omniswitch.db", "sentinel_cache.db": "omniswitch_cache.db"} {
		data, err := os.ReadFile(filepath.Join(dir, current))
		if err != nil || string(data) != old {
			t.Fatalf("Postgres startup did not preserve/move %s: %q err=%v", old, data, err)
		}
	}
}

func TestMigrateCLIReportsSettingsAndStoreErrors(t *testing.T) {
	retentionTestSettings(t)
	t.Setenv("OMNISWITCH_DATABASE_URL", "")
	dir := t.TempDir()
	t.Setenv("OMNISWITCH_DATA", dir)
	t.Setenv("OMNISWITCH_RETENTION_REQUEST_LOGS", "invalid")
	var stdout, stderr bytes.Buffer
	if _, code := dispatch([]string{"migrate", "status"}, &stdout, &stderr, os.Getenv); code == 0 || !strings.Contains(stderr.String(), "load gateway settings") {
		t.Fatalf("invalid settings not reported: code=%d stderr=%s", code, stderr.String())
	}
	t.Setenv("OMNISWITCH_RETENTION_REQUEST_LOGS", "")
	badDir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(badDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNISWITCH_DATA", badDir)
	stdout.Reset()
	stderr.Reset()
	if _, code := dispatch([]string{"migrate", "up"}, &stdout, &stderr, os.Getenv); code == 0 || !strings.Contains(stderr.String(), "open migration store") {
		t.Fatalf("store error not reported: code=%d stderr=%s", code, stderr.String())
	}
	if _, err := store.New(badDir); err == nil {
		t.Fatal("invalid store fixture unexpectedly opened")
	}
}
