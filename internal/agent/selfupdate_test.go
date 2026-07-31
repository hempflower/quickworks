package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateSelfReplacesExecutableOnce(t *testing.T) {
	archive := agentArchive(t, []byte("new-agent"))
	checksum := sha256.Sum256(archive)
	server := agentReleaseServer(t, archive, hex.EncodeToString(checksum[:]))
	defer server.Close()

	executable := filepath.Join(t.TempDir(), "quickworks")
	if err := os.WriteFile(executable, []byte("old-agent"), 0755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	updated, err := UpdateSelf(context.Background(), server.URL, stateDir, executable)
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("expected agent update")
	}
	data, err := os.ReadFile(executable)
	if err != nil || string(data) != "new-agent" {
		t.Fatalf("unexpected executable after update: %q, %v", data, err)
	}
	updated, err = UpdateSelf(context.Background(), server.URL, stateDir, executable)
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("agent updated despite unchanged release")
	}
}

func TestUpdateSelfRejectsChecksumMismatch(t *testing.T) {
	archive := agentArchive(t, []byte("new-agent"))
	server := agentReleaseServer(t, archive, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	defer server.Close()

	executable := filepath.Join(t.TempDir(), "quickworks")
	if err := os.WriteFile(executable, []byte("old-agent"), 0755); err != nil {
		t.Fatal(err)
	}
	updated, err := UpdateSelf(context.Background(), server.URL, t.TempDir(), executable)
	if err == nil || updated {
		t.Fatalf("expected checksum failure, updated=%v err=%v", updated, err)
	}
	data, err := os.ReadFile(executable)
	if err != nil || string(data) != "old-agent" {
		t.Fatalf("executable changed after failed update: %q, %v", data, err)
	}
}

func agentReleaseServer(t *testing.T, archive []byte, checksum string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(agentArchivePath, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc(agentChecksumPath, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(checksum + "  quickworks-linux-amd64.tar.gz\n"))
	})
	return httptest.NewServer(mux)
}

func agentArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "quickworks", Mode: 0755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
