package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"os"

	"github.com/evanxiao/quickworks/assets"
)

type getRouter interface {
	Get(pattern string, handler http.HandlerFunc)
}

type ReleaseAssets struct {
	bootstrap []byte
	bundle    []byte
	checksum  string
}

func NewReleaseAssets(executable string) (*ReleaseAssets, error) {
	bootstrap, err := fs.ReadFile(assets.Files, "workspace-bootstrap.sh")
	if err != nil {
		return nil, fmt.Errorf("read workspace bootstrap: %w", err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return nil, fmt.Errorf("read quickworks executable: %w", err)
	}
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "quickworks", Mode: 0755, Size: int64(len(binary))}); err != nil {
		return nil, err
	}
	if _, err := tarWriter.Write(binary); err != nil {
		return nil, err
	}
	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, err
	}
	bundle := archive.Bytes()
	sum := sha256.Sum256(bundle)
	return &ReleaseAssets{bootstrap: bootstrap, bundle: bundle, checksum: hex.EncodeToString(sum[:])}, nil
}

func (a *ReleaseAssets) register(router getRouter) {
	router.Get("/assets/workspace-bootstrap.sh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		_, _ = w.Write(a.bootstrap)
	})
	router.Get("/assets/quickworks-linux-amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", fmt.Sprint(len(a.bundle)))
		_, _ = w.Write(a.bundle)
	})
	router.Get("/assets/quickworks-linux-amd64.tar.gz.sha256", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(a.checksum + "  quickworks-linux-amd64.tar.gz\n"))
	})
}
