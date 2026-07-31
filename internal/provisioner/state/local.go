// Package state owns workspace Terraform state locations and backups.
package state

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store permits the provisioner executor to use local state today and a
// remote backend implementation later without changing build orchestration.
type Store interface {
	Path(workspaceID string) (string, error)
	BackendArgs(workspaceID string) ([]string, error)
	Backup(workspaceID string) (string, error)
	Restore(workspaceID, snapshot string) error
}

type Local struct {
	root      string
	backupDir string
	retention int
}

func NewLocal(root, backupDir string, retention int) *Local {
	if backupDir == "" {
		backupDir = filepath.Join(root, "backups")
	}
	if retention == 0 {
		retention = 14
	}
	return &Local{root: root, backupDir: backupDir, retention: retention}
}

func (s *Local) Path(workspaceID string) (string, error) {
	if !validWorkspaceID(workspaceID) {
		return "", fmt.Errorf("invalid workspace ID for state path")
	}
	directory := filepath.Join(s.root, workspaceID)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", fmt.Errorf("create workspace state directory: %w", err)
	}
	return filepath.Join(directory, "terraform.tfstate"), nil
}

func (s *Local) BackendArgs(workspaceID string) ([]string, error) {
	path, err := s.Path(workspaceID)
	if err != nil {
		return nil, err
	}
	return []string{"-backend-config=path=" + path}, nil
}

func (s *Local) Backup(workspaceID string) (string, error) {
	statePath, err := s.Path(workspaceID)
	if err != nil {
		return "", err
	}
	source, err := os.Open(statePath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open Terraform state: %w", err)
	}
	defer source.Close()
	directory := filepath.Join(s.backupDir, workspaceID)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + ".tfstate"
	temporary, err := os.CreateTemp(directory, ".backup-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, source); err != nil {
		temporary.Close()
		return "", fmt.Errorf("copy Terraform state backup: %w", err)
	}
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	if err := s.prune(directory); err != nil {
		return "", err
	}
	return path, nil
}

// Restore atomically replaces a local state file with one retained snapshot.
func (s *Local) Restore(workspaceID, snapshot string) error {
	statePath, err := s.Path(workspaceID)
	if err != nil {
		return err
	}
	backupRoot := filepath.Join(s.backupDir, workspaceID)
	if filepath.Base(snapshot) != snapshot || !strings.HasSuffix(snapshot, ".tfstate") {
		return errors.New("state snapshot name is invalid")
	}
	source, err := os.Open(filepath.Join(backupRoot, snapshot))
	if err != nil {
		return fmt.Errorf("open state snapshot: %w", err)
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(statePath), ".restore-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, source); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, statePath)
}

func (s *Local) prune(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tfstate") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for len(names) > s.retention {
		if err := os.Remove(filepath.Join(directory, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

func validWorkspaceID(value string) bool {
	return value != "" && filepath.Base(value) == value && !strings.Contains(value, string(filepath.Separator))
}
