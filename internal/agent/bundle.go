package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxBundleBytes = 2 << 30

type Bundle struct {
	URL        string
	SHA256     string
	Entrypoint string
}

// InstallBundle verifies, extracts, and atomically exposes a Workbench bundle.
// It never reuses a previous version when the requested bundle is unavailable.
func InstallBundle(ctx context.Context, stateDir string, bundle Bundle) (string, error) {
	if !strings.HasPrefix(bundle.URL, "https://") {
		return "", errors.New("workbench bundle URL must use HTTPS")
	}
	if len(bundle.SHA256) != sha256.Size*2 {
		return "", errors.New("workbench bundle SHA-256 is invalid")
	}
	if !relativeArchivePath(bundle.Entrypoint) {
		return "", errors.New("workbench entrypoint is not a safe relative path")
	}
	versions := filepath.Join(stateDir, "workbench", "versions")
	if err := os.MkdirAll(versions, 0755); err != nil {
		return "", fmt.Errorf("create bundle versions directory: %w", err)
	}
	if err := os.Chmod(versions, 0755); err != nil {
		return "", fmt.Errorf("set bundle versions permissions: %w", err)
	}
	target := filepath.Join(versions, bundle.SHA256)
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		return activateBundle(stateDir, bundle.SHA256, bundle.Entrypoint)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	file, err := downloadBundle(ctx, bundle)
	if err != nil {
		return "", err
	}
	defer file.Close()
	defer os.Remove(file.Name())
	temporary, err := os.MkdirTemp(versions, ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	if err := extractBundle(file, temporary, bundle.Entrypoint); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("install bundle: %w", err)
		}
	}
	return activateBundle(stateDir, bundle.SHA256, bundle.Entrypoint)
}

func downloadBundle(ctx context.Context, bundle Bundle) (*os.File, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, bundle.URL, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download workbench bundle: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download workbench bundle: %s", response.Status)
	}
	if response.ContentLength > maxBundleBytes {
		return nil, errors.New("workbench bundle exceeds maximum size")
	}
	file, err := os.CreateTemp("", "quickworks-workbench-*.tar.gz")
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	bytesCopied, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxBundleBytes+1))
	if err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	if bytesCopied > maxBundleBytes {
		file.Close()
		os.Remove(file.Name())
		return nil, errors.New("workbench bundle exceeds maximum size")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, bundle.SHA256) {
		file.Close()
		os.Remove(file.Name())
		return nil, errors.New("workbench bundle SHA-256 does not match")
	}
	return file, nil
}

func extractBundle(file *os.File, destination, entrypoint string) error {
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open workbench archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	root := ""
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name, first, ok := archivePath(header.Name, header.Typeflag == tar.TypeDir)
		if !ok {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		if root == "" {
			root = first
		}
		if first != root {
			return errors.New("workbench archive must have exactly one top-level directory")
		}
		path := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := safeParent(path, destination); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode) & 0777
			output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, tarReader)
			closeErr := output.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) || !symlinkStaysInRoot(path, header.Linkname, destination, root) {
				return fmt.Errorf("unsafe archive symlink %q", header.Name)
			}
			if err := safeParent(path, destination); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type for %q", header.Name)
		}
	}
	if root == "" || !relativeArchivePath(filepath.Join(root, entrypoint)) {
		return errors.New("workbench archive is empty or entrypoint is invalid")
	}
	entry := filepath.Join(destination, root, filepath.FromSlash(entrypoint))
	if info, err := os.Stat(entry); err != nil || info.IsDir() {
		return errors.New("workbench archive does not contain its entrypoint")
	}
	return makeReadOnly(destination)
}

func activateBundle(stateDir, sha, entrypoint string) (string, error) {
	versions := filepath.Join(stateDir, "workbench", "versions")
	target := filepath.Join(versions, sha)
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return "", errors.New("installed workbench version has invalid layout")
	}
	entry := filepath.Join(target, entries[0].Name(), filepath.FromSlash(entrypoint))
	if info, err := os.Stat(entry); err != nil || info.IsDir() {
		return "", errors.New("installed workbench entrypoint is missing")
	}
	link := filepath.Join(stateDir, "workbench", "current")
	temporary := link + ".new"
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Symlink(filepath.Join("versions", sha), temporary); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, link); err != nil {
		return "", err
	}
	return entry, nil
}

func archivePath(name string, isDirectory bool) (string, string, bool) {
	if isDirectory {
		name = strings.TrimSuffix(name, "/")
	}
	if strings.HasPrefix(name, "/") || !relativeArchivePath(name) {
		return "", "", false
	}
	parts := strings.Split(name, "/")
	if len(parts) == 1 && isDirectory {
		return name, parts[0], true
	}
	if len(parts) < 2 {
		return "", "", false
	}
	return name, parts[0], true
}

func symlinkStaysInRoot(path, target, destination, root string) bool {
	resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
	allowed := filepath.Join(destination, root)
	relative, err := filepath.Rel(allowed, resolved)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func relativeArchivePath(path string) bool {
	path = filepath.ToSlash(path)
	if path == "" || strings.HasPrefix(path, "/") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func safeParent(path, root string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	for current := parent; current != root; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive entry has a symlink parent")
		}
	}
	return nil
}

func makeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		mode := os.FileMode(0444)
		if entry.IsDir() {
			mode = 0555
		} else if info, err := entry.Info(); err != nil {
			return err
		} else {
			if info.Mode().Perm()&0111 != 0 {
				mode = 0555
			}
		}
		return os.Chmod(path, mode)
	})
}
