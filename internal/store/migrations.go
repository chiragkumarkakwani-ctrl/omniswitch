package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// OpenOption configures a store connection. Existing callers migrate by default.
type OpenOption func(*openOptions)

type openOptions struct {
	migrateOnStart bool
}

// WithMigrateOnStart disables automatic migrations when enabled is false.
// A store opened this way can still report its schema status and run Migrate.
func WithMigrateOnStart(enabled bool) OpenOption {
	return func(options *openOptions) { options.migrateOnStart = enabled }
}

func (s *Store) initialize(options []OpenOption) error {
	settings := openOptions{migrateOnStart: true}
	for _, option := range options {
		option(&settings)
	}
	if settings.migrateOnStart {
		return s.Migrate(context.Background())
	}
	return s.Ping(context.Background())
}

// SchemaStatus describes the applied schema and the migrations still required.
type SchemaStatus struct {
	Current int `json:"current"`
	Latest  int `json:"latest"`
	Pending int `json:"pending_migrations"`
}

// Driver returns the storage driver without exposing its connection string.
func (s *Store) Driver() string {
	if s.pg {
		return "postgres"
	}
	return "sqlite"
}

// SchemaStatus reads migration state without creating or altering any tables.
// An unversioned v0.1.0 database reports current version zero until adopted.
func (s *Store) SchemaStatus(ctx context.Context) (SchemaStatus, error) {
	status := SchemaStatus{Latest: migrations[len(migrations)-1].Version, Pending: len(migrations)}
	if s == nil || s.db == nil {
		return status, fmt.Errorf("store is not initialized")
	}
	var exists bool
	query := "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations')"
	if s.pg {
		query = "SELECT to_regclass('schema_migrations') IS NOT NULL"
	}
	if err := s.queryRow(ctx, query).Scan(&exists); err != nil {
		return status, err
	}
	if !exists {
		return status, nil
	}
	rows, err := s.query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return status, err
	}
	defer rows.Close()
	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return status, err
		}
		applied[version] = true
		if version > status.Current {
			status.Current = version
		}
	}
	for _, migration := range migrations {
		if applied[migration.Version] {
			status.Pending--
		}
	}
	return status, rows.Err()
}

type migrationColumn struct {
	Table, Name, SQLite, Postgres string
}

type migration struct {
	Version          int
	Name             string
	SQLite, Postgres []string
	Columns          []migrationColumn
	After            []string
}

// The v0.1.0 baseline statements and column definitions remain idempotent and
// unchanged. Adopting version one never rewrites existing data.
var baselineStatements = []string{
	`CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY,
			timestamp TEXT NOT NULL,
			trace_id TEXT,
			session_id TEXT,
			provider TEXT,
			model TEXT,
			api_key_id TEXT,
			status TEXT,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			latency_ms REAL DEFAULT 0,
			cost REAL DEFAULT 0,
			request_body TEXT,
			response_body TEXT,
			decision TEXT,
			decision_reason TEXT,
			error_message TEXT,
			cached INTEGER DEFAULT 0
		)`,
	`CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			key_hash TEXT UNIQUE NOT NULL,
			key_prefix TEXT NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT,
			rate_limit INTEGER DEFAULT 60,
			budget_usd REAL DEFAULT 0,
			spend_usd REAL DEFAULT 0,
			monthly_cost_budget REAL DEFAULT 0,
			monthly_token_budget INTEGER DEFAULT 0,
			enabled INTEGER DEFAULT 1,
			workspace_id TEXT,
			role TEXT,
			metadata TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS prompts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			version INTEGER DEFAULT 1,
			template TEXT NOT NULL,
			variables TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			metadata TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS guardrail_events (
			id TEXT PRIMARY KEY,
			request_id TEXT,
			timestamp TEXT NOT NULL,
			guardrail_type TEXT,
			action TEXT,
			matched INTEGER DEFAULT 0,
			details TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS semantic_cache (
			id TEXT PRIMARY KEY,
			cache_key TEXT,
			created_at TEXT NOT NULL,
			expires_at TEXT,
			model TEXT NOT NULL,
			prompt_text TEXT NOT NULL,
			vector_json TEXT NOT NULL,
			response_json TEXT NOT NULL
		)`,
	`CREATE TABLE IF NOT EXISTS shadow_requests (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL,
			trace_id TEXT,
			timestamp TEXT NOT NULL,
			primary_provider TEXT,
			shadow_provider TEXT,
			model TEXT,
			latency_ms REAL DEFAULT 0,
			cost REAL DEFAULT 0,
			status TEXT,
			error_message TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS feedback (
			id TEXT PRIMARY KEY,
			request_id TEXT,
			trace_id TEXT,
			timestamp TEXT NOT NULL,
			score INTEGER DEFAULT 0,
			rating TEXT,
			comment TEXT,
			user_id TEXT,
			metadata TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS organizations (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			created_at TEXT NOT NULL,
			metadata TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS workspaces (
			id TEXT PRIMARY KEY,
			organization_id TEXT NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT NOT NULL,
			metadata TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			email TEXT UNIQUE NOT NULL,
			name TEXT,
			created_at TEXT NOT NULL,
			metadata TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS workspace_members (
			workspace_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			role TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (workspace_id, user_id)
		)`,
	`CREATE TABLE IF NOT EXISTS virtual_keys (
			id TEXT PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			provider_type TEXT NOT NULL,
			provider_name TEXT,
			base_url TEXT,
			encrypted_key TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			enabled INTEGER DEFAULT 1,
			metadata TEXT
		)`,
	`CREATE TABLE IF NOT EXISTS gateway_configs (
			id TEXT PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			description TEXT,
			format TEXT NOT NULL DEFAULT 'yaml',
			body TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			enabled INTEGER DEFAULT 1
		)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON request_logs(timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_trace ON request_logs(trace_id)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_session ON request_logs(session_id)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_api_key ON request_logs(api_key_id)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_provider ON request_logs(provider)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_status ON request_logs(status)`,
	`CREATE INDEX IF NOT EXISTS idx_guardrail_request ON guardrail_events(request_id)`,
	`CREATE INDEX IF NOT EXISTS idx_cache_model ON semantic_cache(model)`,
	`CREATE INDEX IF NOT EXISTS idx_shadow_request ON shadow_requests(request_id)`,
	`CREATE INDEX IF NOT EXISTS idx_feedback_request ON feedback(request_id)`,
	`CREATE INDEX IF NOT EXISTS idx_feedback_trace ON feedback(trace_id)`,
	`CREATE INDEX IF NOT EXISTS idx_workspaces_org ON workspaces(organization_id)`,
	`CREATE INDEX IF NOT EXISTS idx_workspace_members_user ON workspace_members(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_virtual_keys_name ON virtual_keys(name)`,
	`CREATE INDEX IF NOT EXISTS idx_gateway_configs_name ON gateway_configs(name)`,
}

var migrations = []migration{
	{
		Version: 1,
		Name:    "baseline",
		SQLite:  baselineStatements,
		Columns: []migrationColumn{
			{Table: "request_logs", Name: "trace_id", SQLite: "TEXT"},
			{Table: "request_logs", Name: "session_id", SQLite: "TEXT"},
			{Table: "request_logs", Name: "metadata", SQLite: "TEXT"},
			{Table: "api_keys", Name: "monthly_cost_budget", SQLite: "REAL DEFAULT 0"},
			{Table: "api_keys", Name: "monthly_token_budget", SQLite: "INTEGER DEFAULT 0"},
			{Table: "api_keys", Name: "budget_usd", SQLite: "REAL DEFAULT 0"},
			{Table: "api_keys", Name: "spend_usd", SQLite: "REAL DEFAULT 0"},
			{Table: "api_keys", Name: "workspace_id", SQLite: "TEXT"},
			{Table: "api_keys", Name: "role", SQLite: "TEXT"},
			{Table: "semantic_cache", Name: "cache_key", SQLite: "TEXT"},
			{Table: "semantic_cache", Name: "expires_at", SQLite: "TEXT"},
		},
		After: []string{
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_cache_key ON semantic_cache(cache_key) WHERE cache_key IS NOT NULL AND cache_key != ''",
		},
	},
	{
		Version: 2,
		Name:    "request_usage_and_key_limits",
		Columns: []migrationColumn{
			{Table: "request_logs", Name: "endpoint", SQLite: "TEXT NOT NULL DEFAULT ''"},
			{Table: "request_logs", Name: "cost_source", SQLite: "TEXT NOT NULL DEFAULT 'none'"},
			{Table: "request_logs", Name: "cached_tokens", SQLite: "INTEGER NOT NULL DEFAULT 0"},
			{Table: "request_logs", Name: "reasoning_tokens", SQLite: "INTEGER NOT NULL DEFAULT 0"},
			{Table: "api_keys", Name: "requests_per_minute", SQLite: "INTEGER NOT NULL DEFAULT 0"},
			{Table: "api_keys", Name: "tokens_per_minute", SQLite: "INTEGER NOT NULL DEFAULT 0"},
			{Table: "api_keys", Name: "usd_per_day", SQLite: "REAL NOT NULL DEFAULT 0", Postgres: "DOUBLE PRECISION NOT NULL DEFAULT 0"},
			{Table: "api_keys", Name: "last_used_at", SQLite: "TEXT"},
		},
	},
	{
		Version: 3,
		Name:    "request_and_guardrail_indexes",
		SQLite: []string{
			// Timestamp and guardrail request_id indexes already exist in version one.
			`CREATE INDEX IF NOT EXISTS idx_logs_api_key_timestamp ON request_logs(api_key_id, timestamp)`,
			`CREATE INDEX IF NOT EXISTS idx_logs_provider_timestamp ON request_logs(provider, timestamp)`,
			`CREATE INDEX IF NOT EXISTS idx_logs_decision ON request_logs(decision)`,
			`CREATE INDEX IF NOT EXISTS idx_guardrail_timestamp ON guardrail_events(timestamp)`,
		},
	},
	{
		Version: 4,
		Name:    "retention_indexes",
		// These statements are shared by both dialects and applied in one
		// transaction. Partial indexes exclude cleared payloads, immortal cache
		// entries, and the other independently retained request-log class.
		SQLite: []string{
			`CREATE INDEX IF NOT EXISTS idx_shadow_timestamp ON shadow_requests(timestamp)`,
			`CREATE INDEX IF NOT EXISTS idx_logs_payload_timestamp ON request_logs(timestamp)
				WHERE COALESCE(request_body, '') != '' OR COALESCE(response_body, '') != ''`,
			`CREATE INDEX IF NOT EXISTS idx_logs_retention_timestamp ON request_logs(timestamp)
				WHERE COALESCE(status, '') != 'limited' AND COALESCE(decision, '') != 'DENY'`,
			`CREATE INDEX IF NOT EXISTS idx_logs_limited_timestamp ON request_logs(timestamp)
				WHERE status = 'limited' OR decision = 'DENY'`,
			`CREATE INDEX IF NOT EXISTS idx_cache_expires_at ON semantic_cache(expires_at) WHERE expires_at > ''`,
		},
	},
}

// Migrate applies each pending version atomically. PostgreSQL holds a session
// advisory lock on one dedicated connection for the entire run, including
// creation of schema_migrations. SQLite serializes migrations with its writer.
func (s *Store) Migrate(ctx context.Context) (err error) {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is not initialized")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if s.pg {
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext('omniswitch_schema'))"); err != nil {
			return fmt.Errorf("lock schema migrations: %w", err)
		}
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, unlockErr := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock(hashtext('omniswitch_schema'))")
			if unlockErr != nil {
				// Do not return a still-locked session to the connection pool.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				err = errors.Join(err, fmt.Errorf("unlock schema migrations: %w", unlockErr))
			}
		}()
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema migrations: %w", err)
	}
	for _, migration := range migrations {
		if err := s.applyMigration(ctx, conn, migration); err != nil {
			return fmt.Errorf("migration %d (%s): %w", migration.Version, migration.Name, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, conn *sql.Conn, migration migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if !s.pg {
		// Acquire SQLite's writer before checking the version so another opener
		// cannot read the same pending version and race its ALTER TABLE calls.
		if _, err := tx.ExecContext(ctx, "UPDATE schema_migrations SET version = version WHERE 0"); err != nil {
			return err
		}
	}
	query := "SELECT COUNT(*) FROM schema_migrations WHERE version = ?"
	if s.pg {
		query = rewriteSQL(query)
	}
	var applied int
	if err := tx.QueryRowContext(ctx, query, migration.Version).Scan(&applied); err != nil {
		return err
	}
	if applied > 0 {
		return nil
	}
	statements := migration.SQLite
	if s.pg && migration.Postgres != nil {
		statements = migration.Postgres
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, column := range migration.Columns {
		if err := s.ensureColumn(ctx, tx, column); err != nil {
			return err
		}
	}
	for _, statement := range migration.After {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	query = "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)"
	if s.pg {
		query = rewriteSQL(query)
	}
	if _, err := tx.ExecContext(ctx, query, migration.Version, migration.Name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ensureColumn(ctx context.Context, tx *sql.Tx, column migrationColumn) error {
	exists, err := s.columnExists(ctx, tx, column.Table, column.Name)
	if err != nil || exists {
		return err
	}
	definition := column.SQLite
	if s.pg && column.Postgres != "" {
		definition = column.Postgres
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", column.Table, column.Name, definition)); err != nil {
		return fmt.Errorf("add column %s.%s: %w", column.Table, column.Name, err)
	}
	return nil
}

func (s *Store) columnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	if s.pg {
		var exists bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2
		)`, table, column).Scan(&exists)
		return exists, err
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
