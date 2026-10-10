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
// existing destinations. Hard links provide no-clobber moves when supported;
// otherwise rename is used after rechecking that the destination is absent.
// Sidecars move first and the database last, before it can be opened.
func MoveLegacySQLite(dir, legacy, current string) error {
	return moveLegacySQLite(dir, legacy, current, os.Link)
}

func moveLegacySQLite(dir, legacy, current string, link func(string, string) error) error {
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

	type move struct {
		old, new string
		renamed  bool
	}
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
		moves = append(moves, move{old: oldFile, new: newFile})
	}
	for i := range moves {
		file := &moves[i]
		err := link(file.old, file.new)
		if err != nil && !errors.Is(err, os.ErrExist) {
			// SMB/CIFS and other volumes may not support hard links. Rename only
			// after another destination check, never on a link collision.
			err = renameAbsent(file.old, file.new)
			file.renamed = err == nil
		}
		if err != nil {
			// Restore renamed sources and remove only our successful links.
			for _, moved := range moves[:i] {
				var cleanupErr error
				if moved.renamed {
					cleanupErr = renameAbsent(moved.new, moved.old)
				} else {
					cleanupErr = os.Remove(moved.new)
				}
				err = errors.Join(err, cleanupErr)
			}
			return fmt.Errorf("move database file %s to %s: %w", file.old, file.new, err)
		}
	}
	for _, file := range moves {
		if !file.renamed {
			if err := os.Remove(file.old); err != nil {
				return fmt.Errorf("remove moved database file %s: %w", file.old, err)
			}
		}
		log.Printf("Moved database file %s to %s", file.old, file.new)
	}
	return nil
}

func renameAbsent(oldPath, newPath string) error {
	if _, err := os.Lstat(newPath); err == nil {
		return fmt.Errorf("destination %s already exists: %w", newPath, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect destination %s: %w", newPath, err)
	}
	return os.Rename(oldPath, newPath)
}
