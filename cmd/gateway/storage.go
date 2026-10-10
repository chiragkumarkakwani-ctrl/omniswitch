package main

import (
	"os"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/dbfile"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

// Serve and both migrate commands share URL precedence and the data directory.
// Migration commands always open with migrateOnStart=false, regardless of the
// configured startup behavior, so status and dry-run cannot change the schema.
func openConfiguredStore(settings runtimeSettings, migrateOnStart bool) (*store.Store, error) {
	storageURL := strings.TrimSpace(os.Getenv("OMNISWITCH_DATABASE_URL"))
	if storageURL == "" {
		storageURL = strings.TrimSpace(settings.storageURL)
	}
	if strings.HasPrefix(storageURL, "postgres://") || strings.HasPrefix(storageURL, "postgresql://") {
		if err := dbfile.MoveLegacyDatabases(settings.dataDir); err != nil {
			return nil, err
		}
		return store.OpenPostgres(storageURL, store.WithMigrateOnStart(migrateOnStart))
	}
	return store.New(settings.dataDir, store.WithMigrateOnStart(migrateOnStart))
}
