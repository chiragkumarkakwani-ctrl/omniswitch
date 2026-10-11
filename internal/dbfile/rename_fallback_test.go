package dbfile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMoveLegacySQLiteFallsBackToRename(t *testing.T) {
	for _, names := range [][2]string{{"sentinel.db", "omniswitch.db"}, {"sentinel_cache.db", "omniswitch_cache.db"}} {
		t.Run(names[0], func(t *testing.T) {
			dir := t.TempDir()
			writeLegacyFiles(t, dir, names[0])
			var calls []string
			link := func(oldPath, newPath string) error {
				calls = append(calls, filepath.Base(oldPath))
				if filepath.Base(oldPath) == names[0] {
					for _, suffix := range []string{"-wal", "-shm"} {
						assertFileContents(t, filepath.Join(dir, names[1]+suffix), "original"+suffix)
					}
				}
				return &os.LinkError{Op: "link", Old: oldPath, New: newPath, Err: errors.New("hard links unsupported")}
			}
			if err := moveLegacySQLite(dir, names[0], names[1], link); err != nil {
				t.Fatal(err)
			}
			want := []string{names[0] + "-wal", names[0] + "-shm", names[0]}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("move order = %v, want %v", calls, want)
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				assertFileContents(t, filepath.Join(dir, names[1]+suffix), "original"+suffix)
				if _, err := os.Lstat(filepath.Join(dir, names[0]+suffix)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("legacy file remains: %s err=%v", suffix, err)
				}
			}
		})
	}
}

func TestMoveLegacySQLiteRechecksFallbackDestination(t *testing.T) {
	for _, linkErr := range []error{os.ErrExist, errors.New("hard links unsupported")} {
		t.Run(linkErr.Error(), func(t *testing.T) {
			dir := t.TempDir()
			writeLegacyFiles(t, dir, "sentinel.db")
			link := func(oldPath, newPath string) error {
				if err := os.WriteFile(newPath, []byte("new destination"), 0600); err != nil {
					t.Fatal(err)
				}
				return linkErr
			}
			if err := moveLegacySQLite(dir, "sentinel.db", "omniswitch.db", link); !errors.Is(err, os.ErrExist) {
				t.Fatalf("collision returned %v", err)
			}
			assertFileContents(t, filepath.Join(dir, "omniswitch.db-wal"), "new destination")
			for _, suffix := range []string{"", "-wal", "-shm"} {
				assertFileContents(t, filepath.Join(dir, "sentinel.db"+suffix), "original"+suffix)
			}
		})
	}
}

func TestMoveLegacySQLiteLinkCollisionDoesNotRename(t *testing.T) {
	dir := t.TempDir()
	writeLegacyFiles(t, dir, "sentinel.db")
	if err := moveLegacySQLite(dir, "sentinel.db", "omniswitch.db", func(string, string) error { return os.ErrExist }); !errors.Is(err, os.ErrExist) {
		t.Fatalf("link collision returned %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "omniswitch.db-wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link collision fell back to rename: %v", err)
	}
}

func TestMoveLegacySQLiteRollsBackMixedMoves(t *testing.T) {
	dir := t.TempDir()
	writeLegacyFiles(t, dir, "sentinel.db")
	link := func(oldPath, newPath string) error {
		switch filepath.Base(oldPath) {
		case "sentinel.db-wal":
			return errors.New("hard links unsupported")
		case "sentinel.db-shm":
			return os.Link(oldPath, newPath)
		default:
			if err := os.WriteFile(newPath, []byte("new destination"), 0600); err != nil {
				t.Fatal(err)
			}
			return errors.New("hard links unsupported")
		}
	}
	if err := moveLegacySQLite(dir, "sentinel.db", "omniswitch.db", link); !errors.Is(err, os.ErrExist) {
		t.Fatalf("mixed move collision returned %v", err)
	}
	assertFileContents(t, filepath.Join(dir, "omniswitch.db"), "new destination")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		assertFileContents(t, filepath.Join(dir, "sentinel.db"+suffix), "original"+suffix)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(filepath.Join(dir, "omniswitch.db"+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback left destination %s: %v", suffix, err)
		}
	}
}

func writeLegacyFiles(t *testing.T, dir, name string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(filepath.Join(dir, name+suffix), []byte("original"+suffix), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("file %s contents = %q, want %q, err=%v", path, data, want, err)
	}
}
