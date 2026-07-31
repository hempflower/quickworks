package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	agentArchivePath     = "/assets/quickworks-linux-amd64.tar.gz"
	agentChecksumPath    = agentArchivePath + ".sha256"
	agentUpdateInterval  = 15 * time.Minute
	maxAgentArchiveBytes = 128 << 20
)

// MaintainSelfUpdate makes the workspace agent responsible for its own
// updates. A successful replacement invokes restart so systemd starts the new
// binary through the existing Restart=always service policy.
func MaintainSelfUpdate(ctx context.Context, controlURL, stateDir, executable string, restart func()) {
	check := func() bool {
		updated, err := UpdateSelf(ctx, controlURL, stateDir, executable)
		if err != nil {
			log.Printf("quickworks agent update check failed: %v", err)
			return false
		}
		if updated {
			log.Printf("quickworks agent updated; restarting")
			restart()
			return true
		}
		return false
	}
	if check() {
		return
	}
	ticker := time.NewTicker(agentUpdateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if check() {
				return
			}
		}
	}
}

// UpdateSelf installs the current control-plane agent binary when its archive
// checksum differs from the last checked release. It returns true only after
// atomically replacing the executable.
func UpdateSelf(ctx context.Context, controlURL, stateDir, executable string) (bool, error) {
	base, err := url.Parse(controlURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return false, errors.New("agent control URL is invalid")
	}
	if !filepath.IsAbs(executable) {
		return false, errors.New("agent executable path must be absolute")
	}
	checksumURL := strings.TrimRight(controlURL, "/") + agentChecksumPath
	checksum, err := fetchChecksum(ctx, checksumURL)
	if err != nil {
		return false, err
	}
	marker := filepath.Join(stateDir, "agent", "bundle.sha256")
	if previous, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(previous)) == checksum {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read agent update marker: %w", err)
	}

	binary, err := downloadAgentBinary(ctx, strings.TrimRight(controlURL, "/")+agentArchivePath, checksum)
	if err != nil {
		return false, err
	}
	current, err := fileSHA256(executable)
	if err != nil {
		return false, fmt.Errorf("hash current agent executable: %w", err)
	}
	candidate := sha256.Sum256(binary)
	if current == hex.EncodeToString(candidate[:]) {
		return false, writeUpdateMarker(marker, checksum)
	}
	if err := replaceExecutable(executable, binary); err != nil {
		return false, err
	}
	if err := writeUpdateMarker(marker, checksum); err != nil {
		// The replacement already succeeded. Restarting is more important than
		// retaining this download-avoidance marker; the next agent will retry it.
		log.Printf("record agent update marker: %v", err)
	}
	return true, nil
}

func fetchChecksum(ctx context.Context, rawURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("download agent checksum: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download agent checksum: %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return "", errors.New("agent checksum is invalid")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", errors.New("agent checksum is invalid")
	}
	return strings.ToLower(fields[0]), nil
}

func downloadAgentBinary(ctx context.Context, rawURL, expectedSHA string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download agent archive: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download agent archive: %s", response.Status)
	}
	if response.ContentLength > maxAgentArchiveBytes {
		return nil, errors.New("agent archive exceeds maximum size")
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, maxAgentArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if len(archive) > maxAgentArchiveBytes {
		return nil, errors.New("agent archive exceeds maximum size")
	}
	actual := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), expectedSHA) {
		return nil, errors.New("agent archive SHA-256 does not match")
	}
	return extractAgentBinary(archive)
}

func extractAgentBinary(archive []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open agent archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	header, err := tarReader.Next()
	if err != nil {
		return nil, errors.New("agent archive is empty")
	}
	if header.Name != "quickworks" || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Size < 1 || header.Size > maxAgentArchiveBytes {
		return nil, errors.New("agent archive has invalid layout")
	}
	binary, err := io.ReadAll(io.LimitReader(tarReader, header.Size+1))
	if err != nil || int64(len(binary)) != header.Size {
		return nil, errors.New("read agent binary from archive")
	}
	if _, err := tarReader.Next(); !errors.Is(err, io.EOF) {
		return nil, errors.New("agent archive has unexpected entries")
	}
	return binary, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func replaceExecutable(path string, binary []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect agent executable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("agent executable is not a regular file")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".quickworks-agent-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(binary); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0755); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace agent executable: %w", err)
	}
	return nil
}

func writeUpdateMarker(path, checksum string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".bundle-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.WriteString(temporary, checksum+"\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
