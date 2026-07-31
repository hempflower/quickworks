package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type WorkbenchConfig struct {
	Entrypoint     string
	EnvFile        string
	BYOKConfigFile string
	WorkspaceDir   string
	User           string
	Group          string
	BasePath       string
	HealthURL      string
}

// Command returns the only supported Workbench invocation. Workbench is bound
// to loopback; the control plane remains the sole browser-facing endpoint.
func (c WorkbenchConfig) Command(ctx context.Context) (*exec.Cmd, error) {
	if !filepath.IsAbs(c.Entrypoint) || !filepath.IsAbs(c.EnvFile) || !filepath.IsAbs(c.BYOKConfigFile) || !filepath.IsAbs(c.WorkspaceDir) {
		return nil, errors.New("workbench paths must be absolute")
	}
	if c.User == "" || c.Group == "" {
		return nil, errors.New("workbench user and group are required")
	}
	values, err := readDotenv(c.EnvFile)
	if err != nil {
		return nil, err
	}
	account, err := user.Lookup(c.User)
	if err != nil {
		return nil, fmt.Errorf("lookup workbench user: %w", err)
	}
	group, err := user.LookupGroup(c.Group)
	if err != nil {
		return nil, fmt.Errorf("lookup workbench group: %w", err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse workbench uid: %w", err)
	}
	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse workbench gid: %w", err)
	}
	if err := os.Chown(c.BYOKConfigFile, int(uid), int(gid)); err != nil {
		return nil, fmt.Errorf("set workbench BYOK configuration owner: %w", err)
	}
	if err := os.Chmod(c.BYOKConfigFile, 0600); err != nil {
		return nil, fmt.Errorf("set workbench BYOK configuration permissions: %w", err)
	}
	if err := os.Chown(filepath.Dir(c.BYOKConfigFile), 0, int(gid)); err != nil {
		return nil, fmt.Errorf("set workbench configuration directory owner: %w", err)
	}
	if err := os.Chmod(filepath.Dir(c.BYOKConfigFile), 0710); err != nil {
		return nil, fmt.Errorf("set workbench configuration directory permissions: %w", err)
	}
	stateDir, err := workbenchStateDir(account.HomeDir)
	if err != nil {
		return nil, err
	}
	if err := writeWorkbenchSettings(stateDir, int(uid), int(gid)); err != nil {
		return nil, err
	}
	basePath := c.BasePath
	if basePath == "" {
		basePath = "/"
	}
	if !strings.HasPrefix(basePath, "/") {
		return nil, errors.New("workbench base path must start with a slash")
	}
	arguments := []string{
		"--auth", "none",
		"--bind-addr", "127.0.0.1:3000",
		"--disable-update-check",
		"--agents-byok-config", c.BYOKConfigFile,
		"--user-data-dir", filepath.Join(stateDir, "user-data"),
		"--extensions-dir", filepath.Join(stateDir, "extensions"),
		c.WorkspaceDir,
	}
	values["HOME"] = account.HomeDir
	values["USER"] = c.User
	values["LOGNAME"] = c.User
	command := exec.CommandContext(ctx, c.Entrypoint, arguments...)
	command.Dir = c.WorkspaceDir
	command.Env = mergeEnvironment(
		os.Environ(),
		values,
		"WORKBENCH_BASE_PATH="+basePath,
		"VSCODE_AGENT_HOST_BYOK_MODELS_ENABLED=true",
	)
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
		Setpgid:    true,
	}
	return command, nil
}

// writeWorkbenchSettings makes the administrator-provided DeepSeek model the
// default for both agent work and internal utility requests. Without this, VS
// Code defaults utility requests to GitHub Copilot, which is unavailable in a
// BYOK-only workbench.
func workbenchStateDir(homeDir string) (string, error) {
	if !filepath.IsAbs(homeDir) {
		return "", errors.New("workbench user home directory must be absolute")
	}
	return filepath.Join(homeDir, ".quickworks"), nil
}

func writeWorkbenchSettings(stateDir string, uid, gid int) error {
	stateDirs := []string{
		stateDir,
		filepath.Join(stateDir, "user-data"),
		filepath.Join(stateDir, "user-data", "User"),
	}
	for _, directory := range stateDirs {
		if err := os.MkdirAll(directory, 0750); err != nil {
			return fmt.Errorf("create workbench state directory: %w", err)
		}
		if err := os.Chown(directory, uid, gid); err != nil {
			return fmt.Errorf("set workbench state directory owner: %w", err)
		}
		if err := os.Chmod(directory, 0750); err != nil {
			return fmt.Errorf("set workbench state directory permissions: %w", err)
		}
	}
	settingsDir := stateDirs[len(stateDirs)-1]

	path := filepath.Join(settingsDir, "settings.json")
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse workbench settings: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read workbench settings: %w", err)
	}
	settings["chat.byokUtilityModelDefault"] = "mainAgent"
	settings["chat.utilityModel"] = "deepseek/deepseek-v4-flash"
	settings["chat.utilitySmallModel"] = "deepseek/deepseek-v4-flash"
	settings["chat.agentHost.enabled"] = true
	settings["chat.agentHost.byokModels.enabled"] = true

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workbench settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write workbench settings: %w", err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("set workbench settings owner: %w", err)
	}
	return nil
}

// RunWithRestart keeps a failed Workbench separate from the agent process.
// The caller controls shutdown through ctx; rapid failures back off to avoid a
// systemd/internal-supervisor restart loop.
func (c WorkbenchConfig) RunWithRestart(ctx context.Context, ready func() error, report func(error)) error {
	delay := time.Second
	for {
		command, err := c.Command(ctx)
		if err == nil {
			err = command.Start()
		}
		if err == nil {
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			err = c.waitForHealth(ctx, exited)
			if err == nil {
				err = ready()
			}
			if err == nil {
				err = <-exited
			} else {
				_ = command.Process.Kill()
				<-exited
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		report(err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay < time.Minute {
			delay *= 2
		}
	}
}

func (c WorkbenchConfig) waitForHealth(ctx context.Context, exited <-chan error) error {
	healthURL := c.HealthURL
	if healthURL == "" {
		healthURL = "http://127.0.0.1:3000/healthz"
	}
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if requestErr == nil {
				response.Body.Close()
				if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
					return nil
				}
			}
		}
		select {
		case err := <-exited:
			return fmt.Errorf("Workbench exited before becoming healthy: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out waiting for Workbench health check")
		case <-ticker.C:
		}
	}
}

func readDotenv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workbench environment: %w", err)
	}
	return ParseDotenv(string(data))
}

func mergeEnvironment(base []string, values map[string]string, additions ...string) []string {
	result := make([]string, 0, len(base)+len(values)+len(additions))
	seen := make(map[string]bool)
	for _, item := range append(base, additions...) {
		key, _, ok := strings.Cut(item, "=")
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, item)
	}
	for key, value := range values {
		if seen[key] {
			for index, item := range result {
				if strings.HasPrefix(item, key+"=") {
					result[index] = key + "=" + value
				}
			}
			continue
		}
		seen[key] = true
		result = append(result, key+"="+value)
	}
	return result
}
