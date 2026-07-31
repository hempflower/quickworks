package agent

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractBundleAcceptsSingleRootAndRelativeSymlink(t *testing.T) {
	archive := makeArchive(t, []tar.Header{
		{Name: "workbench/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "workbench/bin/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "workbench/bin/server", Typeflag: tar.TypeReg, Mode: 0755, Size: 2},
		{Name: "workbench/bin/link", Typeflag: tar.TypeSymlink, Linkname: "server"},
	}, []string{"", "", "ok", ""})
	destination := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(destination, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.Type()&os.ModeSymlink == 0 {
				_ = os.Chmod(path, 0755)
			}
			return nil
		})
	})
	if err := extractBundle(archive, destination, "bin/server"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "workbench", "bin", "server"))
	if err != nil || info.Mode().Perm()&0222 != 0 || info.Mode().Perm()&0111 != 0111 {
		t.Fatalf("bundle was not installed read-only: %v, %v", info, err)
	}
}

func TestExtractBundleRejectsEscapingSymlink(t *testing.T) {
	archive := makeArchive(t, []tar.Header{
		{Name: "workbench/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "workbench/bin/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "workbench/bin/server", Typeflag: tar.TypeReg, Mode: 0755, Size: 2},
		{Name: "workbench/bin/escape", Typeflag: tar.TypeSymlink, Linkname: "../../../etc/passwd"},
	}, []string{"", "", "ok", ""})
	if err := extractBundle(archive, t.TempDir(), "bin/server"); err == nil {
		t.Fatal("expected escaping symlink rejection")
	}
}

func makeArchive(t *testing.T, headers []tar.Header, bodies []string) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "bundle-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for index := range headers {
		if err := tarWriter.WriteHeader(&headers[index]); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(bodies[index])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	return file
}
