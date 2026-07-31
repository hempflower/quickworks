package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/evanxiao/quickworks/internal/config"
	"github.com/evanxiao/quickworks/internal/database"
	"github.com/evanxiao/quickworks/internal/database/migration"
	"github.com/evanxiao/quickworks/internal/server"
	"github.com/evanxiao/quickworks/internal/server/auth"
	"github.com/evanxiao/quickworks/internal/server/workspace"
)

func RunServer(ctx context.Context, configPath string) error {
	c, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := c.ValidateServer(); err != nil {
		return err
	}
	secret, err := os.ReadFile(c.Auth.GitHub.ClientSecretFile)
	if err != nil {
		return fmt.Errorf("read GitHub client secret: %w", err)
	}
	masterKey, err := os.ReadFile(c.Server.SecretKeyFile)
	if err != nil {
		return fmt.Errorf("read server secret key: %w", err)
	}
	provisionerTokens := make(map[string]string, len(c.Provisioners))
	for _, provisioner := range c.Provisioners {
		token, err := os.ReadFile(provisioner.TokenFile)
		if err != nil {
			return fmt.Errorf("read provisioner token for %s: %w", provisioner.WorkerID, err)
		}
		if strings.TrimSpace(string(token)) == "" {
			return fmt.Errorf("provisioner token for %s is empty", provisioner.WorkerID)
		}
		provisionerTokens[provisioner.WorkerID] = strings.TrimSpace(string(token))
	}
	db, err := database.Open(c.Database.Path)
	if err != nil {
		return err
	}
	if err := migration.Apply(ctx, db); err != nil {
		return err
	}
	a, err := auth.New(db, c.Server.PublicURL, c.Auth.GitHub.ClientID, strings.TrimSpace(string(secret)), c.Auth.GitHub.AllowedUserIDs, masterKey)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find server executable: %w", err)
	}
	release, err := server.NewReleaseAssets(executable)
	if err != nil {
		return err
	}
	byok, err := os.ReadFile(c.Workbench.BYOKConfigFile)
	if err != nil {
		return fmt.Errorf("read Workbench BYOK config: %w", err)
	}
	environment, err := os.ReadFile(c.Workbench.EnvFile)
	if err != nil {
		return fmt.Errorf("read Workbench environment: %w", err)
	}
	workbench := server.AgentWorkbench{
		Version:      c.Workbench.Version,
		BundleURL:    c.Workbench.BundleURL,
		BundleSHA256: c.Workbench.BundleSHA256,
		Entrypoint:   c.Workbench.Entrypoint,
		BYOKConfig:   byok,
		Environment:  environment,
	}
	templates := server.TemplateCatalog{Default: c.Templates.Default, Names: make(map[string]bool), RequiredLabels: make(map[string][]string), Limits: make(map[string]workspace.Limits), Resources: make(map[string]workspace.Resources)}
	for _, template := range c.Templates.Items {
		templates.Names[template.Name] = true
		templates.RequiredLabels[template.Name] = template.RequiredLabels
		limit := c.Quotas.Default
		if override, ok := c.Quotas.Templates[template.Name]; ok {
			limit = override
		}
		templates.Limits[template.Name] = workspace.Limits{MaxWorkspaces: limit.MaxWorkspaces, MaxRunningWorkspaces: limit.MaxRunningWorkspaces}
		templates.Resources[template.Name] = workspace.Resources{CPUs: template.Resources.CPUs, MemoryGiB: template.Resources.MemoryGiB, DiskGiB: template.Resources.DiskGiB, EstimatedCost: template.Resources.EstimatedCost}
	}
	router := server.New(db, a, provisionerTokens, release, workbench, templates, c.Lifecycle.AutoStopAfter)
	go router.Reconcile(ctx)
	srv := http.Server{Addr: c.Server.Listen, Handler: router}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	err = srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
