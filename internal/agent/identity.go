// Package agent contains the workspace-side security and Workbench runtime.
package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const identityFile = "identity.ed25519"

// LoadOrCreateIdentity maintains the agent's persistent Ed25519 identity. The
// state directory must be outside the workspace writable by repository code.
func LoadOrCreateIdentity(stateDir string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("create agent state directory: %w", err)
	}
	path := filepath.Join(stateDir, identityFile)
	data, err := os.ReadFile(path)
	if err == nil {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("stat agent identity: %w", statErr)
		}
		if info.Mode().Perm() != 0600 {
			return nil, errors.New("agent identity permissions must be 0600")
		}
		if len(data) != ed25519.PrivateKeySize {
			return nil, errors.New("agent identity has invalid length")
		}
		return ed25519.PrivateKey(data), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read agent identity: %w", err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate agent identity: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return LoadOrCreateIdentity(stateDir)
	}
	if err != nil {
		return nil, fmt.Errorf("create agent identity: %w", err)
	}
	if _, err := file.Write(privateKey); err != nil {
		file.Close()
		return nil, fmt.Errorf("write agent identity: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close agent identity: %w", err)
	}
	return privateKey, nil
}
