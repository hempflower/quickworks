package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanxiao/quickworks/internal/agent"
)

// RunAgent establishes the one-time agent identity. The enrollment protocol is
// deliberately separate from the future long-lived tunnel implementation.
func RunAgent(ctx context.Context) error {
	controlURL := os.Getenv("QUICKWORKS_AGENT_CONTROL_URL")
	enrollmentToken := os.Getenv("QUICKWORKS_AGENT_ENROLLMENT_TOKEN")
	stateDir := os.Getenv("QUICKWORKS_AGENT_STATE_DIR")
	if controlURL == "" || enrollmentToken == "" || stateDir == "" {
		return errors.New("agent requires QUICKWORKS_AGENT_CONTROL_URL and QUICKWORKS_AGENT_ENROLLMENT_TOKEN")
	}
	identity, err := agent.LoadOrCreateIdentity(stateDir)
	if err != nil {
		return err
	}
	registered, err := agent.Enroll(ctx, controlURL, enrollmentToken, identity)
	if err != nil {
		return err
	}
	workspaceDir := environmentOrDefault("QUICKWORKS_WORKSPACE_DIR", "/workspace")
	workbenchUser := environmentOrDefault("QUICKWORKS_WORKBENCH_USER", "workspace")
	workbenchGroup := environmentOrDefault("QUICKWORKS_WORKBENCH_GROUP", "workspace")
	if registered.RepositoryURL != "" {
		if err := agent.CloneRepository(ctx, registered.RepositoryURL, registered.CloneToken, workspaceDir); err != nil {
			return fmt.Errorf("clone workspace repository: %w", err)
		}
	}
	if err := agent.SetWorkspaceOwnership(workspaceDir, workbenchUser, workbenchGroup); err != nil {
		return err
	}
	if err := agent.ConfigureGitHubCredential(ctx, registered.GitHubToken, workbenchUser); err != nil {
		return fmt.Errorf("configure GitHub credential: %w", err)
	}
	configurationDir := "/etc/quickworks/workbench"
	if err := os.MkdirAll(configurationDir, 0700); err != nil {
		return fmt.Errorf("create workbench configuration directory: %w", err)
	}
	byokPath := filepath.Join(configurationDir, "byok.json")
	envPath := filepath.Join(configurationDir, "workbench.env")
	if err := os.WriteFile(byokPath, []byte(registered.WorkbenchBYOKConfig), 0600); err != nil {
		return fmt.Errorf("write Workbench BYOK configuration: %w", err)
	}
	if err := os.WriteFile(envPath, []byte(registered.WorkbenchEnvironment), 0600); err != nil {
		return fmt.Errorf("write Workbench environment: %w", err)
	}
	entrypoint, err := agent.InstallBundle(ctx, stateDir, agent.Bundle{
		URL:        registered.WorkbenchBundleURL,
		SHA256:     registered.WorkbenchBundleSHA,
		Entrypoint: registered.WorkbenchEntrypoint,
	})
	if err != nil {
		return err
	}
	go func() {
		_ = agent.MaintainTunnel(ctx, controlURL, registered.AgentID, registered.Session)
	}()
	return agent.WorkbenchConfig{
		Entrypoint:     entrypoint,
		EnvFile:        envPath,
		BYOKConfigFile: byokPath,
		WorkspaceDir:   workspaceDir,
		User:           workbenchUser,
		Group:          workbenchGroup,
		BasePath:       "/w/" + registered.WorkspaceID + "/",
		HealthURL:      environmentOrDefault("QUICKWORKS_AGENT_HEALTH_URL", "http://127.0.0.1:3000/healthz"),
	}.RunWithRestart(ctx, func() error {
		return agent.ReportReady(ctx, controlURL, registered.AgentID, registered.Session)
	}, func(error) {})
}

func environmentOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
