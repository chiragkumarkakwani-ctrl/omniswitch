package dbfile

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveLegacySQLiteAndSidecars(t *testing.T) {
	for _, names := range [][2]string{{"sentinel.db", "omniswitch.db"}, {"sentinel_cache.db", "omniswitch_cache.db"}} {
		t.Run(names[0], func(t *testing.T) {
			dir := t.TempDir()
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.WriteFile(filepath.Join(dir, names[0]+suffix), []byte("original"+suffix), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := MoveLegacySQLite(dir, names[0], names[1]); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if _, err := os.Stat(filepath.Join(dir, names[0]+suffix)); !os.IsNotExist(err) {
					t.Fatalf("old file remains: %s%s err=%v", names[0], suffix, err)
				}
				data, err := os.ReadFile(filepath.Join(dir, names[1]+suffix))
				if err != nil || string(data) != "original"+suffix {
					t.Fatalf("moved file lost data: %s%s data=%q err=%v", names[1], suffix, data, err)
				}
				if !strings.Contains(output.String(), names[0]+suffix+" to ") {
					t.Errorf("move not logged for %s%s: %s", names[0], suffix, output.String())
				}
			}
			if err := MoveLegacySQLite(dir, names[0], names[1]); err != nil {
				t.Fatalf("second move should be a no-op: %v", err)
			}
		})
	}
}

func TestMoveLegacySQLiteNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for _, name := range []string{"sentinel.db", "omniswitch.db"} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.WriteFile(filepath.Join(dir, name+suffix), []byte(name+suffix), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := MoveLegacySQLite(dir, "sentinel.db", "omniswitch.db"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sentinel.db", "omniswitch.db"} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			data, err := os.ReadFile(filepath.Join(dir, name+suffix))
			if err != nil || string(data) != name+suffix {
				t.Fatalf("file overwritten or moved: %s%s data=%q err=%v", name, suffix, data, err)
			}
		}
	}
	if !strings.Contains(output.String(), "WARNING") || !strings.Contains(output.String(), "sentinel.db") {
		t.Fatalf("legacy collision warning missing: %s", output.String())
	}
}

func TestMoveLegacySQLiteRefusesSidecarCollision(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"sentinel.db", "sentinel.db-wal", "omniswitch.db-wal"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := MoveLegacySQLite(dir, "sentinel.db", "omniswitch.db"); err == nil {
		t.Fatal("sidecar collision was accepted")
	}
	for _, name := range []string{"sentinel.db", "sentinel.db-wal", "omniswitch.db-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != name {
			t.Fatalf("collision modified %s: %q, err=%v", name, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "omniswitch.db")); !os.IsNotExist(err) {
		t.Fatalf("collision created a destination database: %v", err)
	}
}

func TestMoveLegacySQLiteRejectsPaths(t *testing.T) {
	for _, names := range [][2]string{{"../sentinel.db", "omniswitch.db"}, {"sentinel.db", "../omniswitch.db"}, {"same.db", "same.db"}, {".", "other.db"}} {
		if err := MoveLegacySQLite(t.TempDir(), names[0], names[1]); err == nil {
			t.Fatalf("invalid filenames accepted: %v", names)
		}
	}
}
