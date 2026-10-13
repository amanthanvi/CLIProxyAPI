//go:build linux

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func affinityStoreForTest(t *testing.T) (*FileAffinityStore, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session-affinity.json")
	store, err := NewFileAffinityStore(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

func TestFileAffinityStoreRoundTripAndAtomicFailure(t *testing.T) {
	store, path := affinityStoreForTest(t)
	if _, err := store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	snapshot := AffinitySnapshot{Version: AffinitySnapshotVersion, CreatedAt: time.Now().UTC(), TTL: time.Hour}
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || !loaded.CreatedAt.Equal(snapshot.CreatedAt) {
		t.Fatalf("round trip failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file is not private: %v", err)
	}
	store.rename = func(int, string, string) error { return errors.New("injected failure") }
	snapshot.CreatedAt = snapshot.CreatedAt.Add(time.Second)
	if err := store.Save(snapshot); err == nil {
		t.Fatal("rename failure ignored")
	}
	retained, err := store.Load()
	if err != nil || !retained.CreatedAt.Equal(loaded.CreatedAt) {
		t.Fatal("failed write destroyed last usable checkpoint")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary file not cleaned up")
	}
}

func TestFileAffinityStoreStrictJSONAndByteLimits(t *testing.T) {
	store, path := affinityStoreForTest(t)
	for _, malformed := range []string{"", "null", "{", `{"version":999}`, `{"version":1,"unexpected":"private-value"}`, `{"version":1,"version":1}`, `{"version":1} {}`, `{"version":1} trailing`} {
		if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatalf("accepted malformed checkpoint of length %d", len(malformed))
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxAffinityStateBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("oversized checkpoint accepted")
	}
	if err := store.Save(AffinitySnapshot{Version: 1, Accounts: map[string]string{"id": strings.Repeat("x", maxAffinityStateBytes)}}); err == nil {
		t.Fatal("oversized write accepted")
	}
}

func TestFileAffinityStoreRejectsUnsafePaths(t *testing.T) {
	store, path := affinityStoreForTest(t)
	for _, mode := range []os.FileMode{0o644, 0o660, 0o400} {
		if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatal("public file accepted")
		}
		if err := store.Save(AffinitySnapshot{Version: 1}); err == nil {
			t.Fatal("public file replaced")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("final symlink accepted")
	}
	if err := store.Save(AffinitySnapshot{Version: 1}); err == nil {
		t.Fatal("final symlink replaced")
	}
	linked := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Dir(path), linked); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileAffinityStore(filepath.Join(linked, "file.json"), ""); err == nil {
		t.Fatal("ancestor symlink accepted")
	}
	if _, err := NewFileAffinityStore(path, filepath.Dir(path)); err == nil {
		t.Fatal("path inside auth directory accepted")
	}
	authAlias := filepath.Join(t.TempDir(), "auth-alias")
	if err := os.Symlink(filepath.Dir(path), authAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileAffinityStore(path, authAlias); err == nil {
		t.Fatal("physically inside symlinked auth directory accepted")
	}
	if _, err := NewFileAffinityStore("relative.json", ""); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := NewFileAffinityStore(filepath.Join(t.TempDir(), "missing", "file.json"), ""); err == nil {
		t.Fatal("missing directory created")
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("public parent accepted")
	}
}
