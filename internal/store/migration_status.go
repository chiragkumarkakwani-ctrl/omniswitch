package store

import "context"

// MigrationInfo is the ordered, public description used by migrate up --dry-run.
type MigrationInfo struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
}

// PendingMigrations reads the ledger without creating it or modifying schema.
// It also handles incomplete ledgers without assuming all versions below the
// current maximum were applied.
func (s *Store) PendingMigrations(ctx context.Context) ([]MigrationInfo, error) {
	status, err := s.SchemaStatus(ctx)
	if err != nil {
		return nil, err
	}
	pending := make([]MigrationInfo, 0, status.Pending)
	if status.Pending == 0 {
		return pending, nil
	}
	applied := make(map[int]bool)
	if status.Current != 0 {
		rows, err := s.query(ctx, "SELECT version FROM schema_migrations")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var version int
			if err := rows.Scan(&version); err != nil {
				return nil, err
			}
			applied[version] = true
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	for _, migration := range migrations {
		if !applied[migration.Version] {
			pending = append(pending, MigrationInfo{Version: migration.Version, Name: migration.Name})
		}
	}
	return pending, nil
}
