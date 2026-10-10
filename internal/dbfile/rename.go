// Package dbfile handles the one-time move of legacy SQLite filenames.
package dbfile

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// MoveLegacyDatabases moves both legacy database filenames in a data directory.
// Missing legacy files are a no-op, including in Postgres-only deployments.
func MoveLegacyDatabases(dir string) error {
	for _, names := range [][2]string{{"sentinel.db", "omniswitch.db"}, {"sentinel_cache.db", "omniswitch_cache.db"}} {
		if err := MoveLegacySQLite(dir, names[0], names[1]); err != nil {
			return err
		}
	}
	return nil
}

// MoveLegacySQLite moves a database and its WAL/SHM sidecars without replacing
// any destination. Same-directory hard links followed by unlinking the old
// names implement a no-clobber move using portable Go filesystem operations.
// All destinations are linked before any source is removed, with the database
// linked last so it is never opened before its sidecars are in place.
func MoveLegacySQLite(dir, legacy, current string) error {
	if legacy == current || filepath.Base(legacy) != legacy || filepath.Base(current) != current || legacy == "." || current == "." {
		return fmt.Errorf("database filenames must be distinct basenames")
	}
	oldPath := filepath.Join(dir, legacy)
	newPath := filepath.Join(dir, current)
	if _, err := os.Lstat(oldPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect legacy database %s: %w", oldPath, err)
	}
	if _, err := os.Lstat(newPath); err == nil {
		log.Printf("WARNING: legacy database %s exists alongside %s; using %s and leaving %s untouched", oldPath, newPath, newPath, oldPath)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect database %s: %w", newPath, err)
	}

	type move struct{ old, new string }
	var moves []move
	for _, suffix := range []string{"-wal", "-shm", ""} {
		oldFile, newFile := oldPath+suffix, newPath+suffix
		if _, err := os.Lstat(oldFile); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect legacy database file %s: %w", oldFile, err)
		}
		if _, err := os.Lstat(newFile); err == nil {
			return fmt.Errorf("refuse to move %s: destination %s already exists", oldFile, newFile)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect database file %s: %w", newFile, err)
		}
		moves = append(moves, move{oldFile, newFile})
	}
	for i, file := range moves {
		if err := os.Link(file.old, file.new); err != nil {
			// Link never overwrites, including if a destination appears after the
			// checks above. Remove only links created by this attempt.
			for _, linked := range moves[:i] {
				if cleanupErr := os.Remove(linked.new); cleanupErr != nil {
					err = errors.Join(err, cleanupErr)
				}
			}
			return fmt.Errorf("move database file %s to %s: %w", file.old, file.new, err)
		}
	}
	for _, file := range moves {
		if err := os.Remove(file.old); err != nil {
			return fmt.Errorf("remove moved database file %s: %w", file.old, err)
		}
		log.Printf("Moved database file %s to %s", file.old, file.new)
	}
	return nil
}
