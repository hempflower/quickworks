package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalBackupCopiesStateAndPrunesOldSnapshots(t *testing.T) {
	root := t.TempDir()
	store := NewLocal(root, filepath.Join(root, "snapshots"), 2)
	path, err := store.Path("calm-blue-harbor")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := store.Backup("calm-blue-harbor")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(first); err != nil || string(data) != "first" {
		t.Fatalf("unexpected first backup: %q, %v", data, err)
	}
	if err := os.WriteFile(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Backup("calm-blue-harbor"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("third"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Backup("calm-blue-harbor"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "snapshots", "calm-blue-harbor"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("backup retention kept %d snapshots, want 2", len(entries))
	}
}

func TestLocalStateRejectsUnsafeWorkspaceID(t *testing.T) {
	store := NewLocal(t.TempDir(), "", 1)
	if _, err := store.Path("../escape"); err == nil {
		t.Fatal("unsafe workspace ID was accepted")
	}
}

func TestLocalRestoreReplacesStateFromSnapshot(t *testing.T) {
	root := t.TempDir()
	store := NewLocal(root, filepath.Join(root, "snapshots"), 2)
	path, err := store.Path("calm-blue-harbor")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("known-good"), 0600); err != nil {
		t.Fatal(err)
	}
	backup, err := store.Backup("calm-blue-harbor")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bad-state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore("calm-blue-harbor", filepath.Base(backup)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "known-good" {
		t.Fatalf("unexpected restored state: %q, %v", data, err)
	}
}
