package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// CloneRepository performs the initial sparse-ish clone without putting the
// OAuth token into an URL, command argument, permanent environment, or log.
func CloneRepository(ctx context.Context, repositoryURL, token, workspaceDir string) error {
	if !strings.HasPrefix(repositoryURL, "https://github.com/") {
		return errors.New("repository URL must be a GitHub HTTPS URL")
	}
	if token == "" {
		return errors.New("GitHub clone token is missing")
	}
	if workspaceDir == "" || !filepath.IsAbs(workspaceDir) {
		return errors.New("workspace directory must be absolute")
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(workspaceDir, 0750); err != nil {
		return fmt.Errorf("create workspace directory: %w", err)
	}
	empty, err := os.ReadDir(workspaceDir)
	if err != nil {
		return err
	}
	if len(empty) != 0 {
		return errors.New("workspace directory is not empty")
	}
	askpass, err := os.CreateTemp("", "quickworks-git-askpass-")
	if err != nil {
		return err
	}
	path := askpass.Name()
	defer os.Remove(path)
	if _, err := askpass.WriteString("#!/bin/sh\ncase \"$1\" in\n  *Username*) printf '%s\\n' x-access-token ;;\n  *) printf '%s\\n' \"$QUICKWORKS_GIT_TOKEN\" ;;\nesac\n"); err != nil {
		askpass.Close()
		return err
	}
	if err := askpass.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "git", "clone", "--filter=blob:none", repositoryURL, workspaceDir)
	command.Env = append(os.Environ(), "GIT_ASKPASS="+path, "GIT_TERMINAL_PROMPT=0", "QUICKWORKS_GIT_TOKEN="+token)
	output, err := command.CombinedOutput()
	if err != nil {
		_ = output
		return errors.New("git clone failed")
	}
	return nil
}

// ConfigureGitHubCredential enables Git's persistent credential store for the
// workspace user and records the OAuth token after a successful initial clone.
// The token is deliberately supplied to git on stdin rather than in a command
// argument or environment variable.
func ConfigureGitHubCredential(ctx context.Context, token, username string) error {
	if token == "" || username == "" {
		return errors.New("GitHub credential input is invalid")
	}
	account, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("look up Git user: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return fmt.Errorf("parse Git UID: %w", err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return fmt.Errorf("parse Git GID: %w", err)
	}
	environment := append(os.Environ(), "HOME="+account.HomeDir, "USER="+username, "LOGNAME="+username)
	for _, args := range [][]string{{"config", "--global", "credential.helper", "store"}, {"config", "--global", "credential.useHttpPath", "true"}} {
		command := exec.CommandContext(ctx, "git", args...)
		command.Env = environment
		if err := command.Run(); err != nil {
			return fmt.Errorf("configure Git credential helper: %w", err)
		}
	}
	command := exec.CommandContext(ctx, "git", "credential", "approve")
	command.Env = environment
	command.Stdin = strings.NewReader("protocol=https\nhost=github.com\nusername=x-access-token\npassword=" + token + "\n\n")
	if err := command.Run(); err != nil {
		return fmt.Errorf("store GitHub credential: %w", err)
	}
	for _, name := range []string{".gitconfig", ".git-credentials"} {
		path := filepath.Join(account.HomeDir, name)
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("set Git credential owner: %w", err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			return fmt.Errorf("set Git credential permissions: %w", err)
		}
	}
	return nil
}

// SetWorkspaceOwnership transfers cloned content to the unprivileged
// workbench account. Lchown deliberately does not follow repository symlinks.
func SetWorkspaceOwnership(workspaceDir, username, groupName string) error {
	if !filepath.IsAbs(workspaceDir) || username == "" || groupName == "" {
		return errors.New("workspace ownership input is invalid")
	}
	account, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("look up workbench user: %w", err)
	}
	group, err := user.LookupGroup(groupName)
	if err != nil {
		return fmt.Errorf("look up workbench group: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return fmt.Errorf("parse workbench UID: %w", err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return fmt.Errorf("parse workbench GID: %w", err)
	}
	return filepath.WalkDir(workspaceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("set workspace ownership for %s: %w", path, err)
		}
		return nil
	})
}
