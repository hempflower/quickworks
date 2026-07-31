package server

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestReleaseAssetsServeConsistentBundle(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	assets, err := NewReleaseAssets(executable)
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	assets.register(router)

	request := httptest.NewRequest(http.MethodGet, "/assets/quickworks-linux-amd64.tar.gz", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected bundle status: %d", response.Code)
	}
	gzipReader, err := gzip.NewReader(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	tarReader := tar.NewReader(gzipReader)
	header, err := tarReader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "quickworks" || header.Mode != 0755 {
		t.Fatalf("unexpected bundle member: %#v", header)
	}
	if _, err := io.Copy(io.Discard, tarReader); err != nil {
		t.Fatal(err)
	}
}
