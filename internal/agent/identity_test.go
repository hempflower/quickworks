package agent

import (
	"crypto/ed25519"
	"os"
	"testing"
)

func TestLoadOrCreateIdentityIsPersistent(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatal("identity changed between loads")
	}
	info, err := os.Stat(dir + "/" + identityFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || len(first) != ed25519.PrivateKeySize {
		t.Fatalf("unexpected identity mode or size: %v %d", info.Mode(), len(first))
	}
}

func TestLoadOrCreateIdentityRejectsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir+"/"+identityFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Fatal("expected loose identity permissions to be rejected")
	}
}
