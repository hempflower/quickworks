// Package config loads the administrator-owned Quickworks configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		PublicURL     string `yaml:"public_url"`
		Listen        string `yaml:"listen"`
		SecretKeyFile string `yaml:"secret_key_file"`
	} `yaml:"server"`
	Provisioners []ControlProvisioner `yaml:"provisioners"`
	Templates    struct {
		Default string     `yaml:"default"`
		Items   []Template `yaml:"items"`
	} `yaml:"templates"`
	Quotas struct {
		Default   QuotaLimit            `yaml:"default"`
		Templates map[string]QuotaLimit `yaml:"templates"`
	} `yaml:"quotas"`
	Workbench struct {
		Version        string `yaml:"version"`
		BundleURL      string `yaml:"bundle_url"`
		BundleSHA256   string `yaml:"bundle_sha256"`
		Entrypoint     string `yaml:"entrypoint"`
		BYOKConfigFile string `yaml:"byok_config_file"`
		EnvFile        string `yaml:"env_file"`
	} `yaml:"workbench"`
	Auth struct {
		GitHub struct {
			ClientID         string  `yaml:"client_id"`
			ClientSecretFile string  `yaml:"client_secret_file"`
			AllowedUserIDs   []int64 `yaml:"allowed_user_ids"`
		} `yaml:"github"`
	} `yaml:"auth"`
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`
	Provisioner struct {
		ControlURL           string            `yaml:"control_url"`
		WorkerID             string            `yaml:"worker_id"`
		TokenFile            string            `yaml:"token_file"`
		Binary               string            `yaml:"binary"`
		Labels               []string          `yaml:"labels"`
		Capacity             int               `yaml:"capacity"`
		TemplateDirs         map[string]string `yaml:"template_dirs"`
		StateDir             string            `yaml:"state_dir"`
		StateBackend         string            `yaml:"state_backend"`
		StateS3Bucket        string            `yaml:"state_s3_bucket"`
		StateS3Region        string            `yaml:"state_s3_region"`
		StateS3Endpoint      string            `yaml:"state_s3_endpoint"`
		StateS3KeyPrefix     string            `yaml:"state_s3_key_prefix"`
		StateBackupDir       string            `yaml:"state_backup_dir"`
		StateBackupRetention int               `yaml:"state_backup_retention"`
	} `yaml:"provisioner"`
	Lifecycle struct {
		AutoStopAfter time.Duration `yaml:"auto_stop_after"`
	} `yaml:"lifecycle"`
}

type Template struct {
	Name           string    `yaml:"name"`
	RequiredLabels []string  `yaml:"required_labels"`
	Resources      Resources `yaml:"resources"`
}

type Resources struct {
	CPUs          int     `yaml:"cpus"`
	MemoryGiB     int     `yaml:"memory_gib"`
	DiskGiB       int     `yaml:"disk_gib"`
	EstimatedCost float64 `yaml:"estimated_cost"`
}

type ControlProvisioner struct {
	WorkerID  string `yaml:"worker_id"`
	TokenFile string `yaml:"token_file"`
}

type QuotaLimit struct {
	MaxWorkspaces        int `yaml:"max_workspaces"`
	MaxRunningWorkspaces int `yaml:"max_running_workspaces"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	d := yaml.NewDecoder(strings.NewReader(string(data)))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	return c, nil
}

func (c Config) ValidateServer() error {
	if _, err := url.ParseRequestURI(c.Server.PublicURL); err != nil || c.Server.PublicURL == "" {
		return errors.New("server.public_url must be an absolute URL")
	}
	if c.Server.Listen == "" || c.Database.Path == "" {
		return errors.New("server.listen and database.path are required")
	}
	if c.Templates.Default == "" || len(c.Templates.Items) == 0 {
		return errors.New("templates.default and templates.items are required")
	}
	seenTemplates := make(map[string]bool)
	for _, template := range c.Templates.Items {
		if template.Name == "" || seenTemplates[template.Name] {
			return errors.New("template names must be non-empty and unique")
		}
		if template.Resources.CPUs < 1 || template.Resources.MemoryGiB < 1 || template.Resources.DiskGiB < 1 || template.Resources.EstimatedCost < 0 {
			return errors.New("template resources must use positive CPU, memory, disk and non-negative estimated cost")
		}
		seenTemplates[template.Name] = true
	}
	if !seenTemplates[c.Templates.Default] {
		return errors.New("templates.default must name a configured template")
	}
	if !validQuota(c.Quotas.Default, true) {
		return errors.New("quotas.default workspace limits must be positive")
	}
	for name, limit := range c.Quotas.Templates {
		if !seenTemplates[name] || !validQuota(limit, true) {
			return errors.New("template quotas must have configured template names and positive limits")
		}
	}
	if c.Workbench.Version == "" || c.Workbench.BundleURL == "" || len(c.Workbench.BundleSHA256) != 64 || c.Workbench.Entrypoint == "" {
		return errors.New("complete workbench bundle metadata is required")
	}
	if u, err := url.Parse(c.Workbench.BundleURL); err != nil || u.Scheme != "https" {
		return errors.New("workbench.bundle_url must use HTTPS")
	}
	for _, p := range []string{c.Workbench.BYOKConfigFile, c.Workbench.EnvFile, c.Auth.GitHub.ClientSecretFile, c.Server.SecretKeyFile} {
		if p == "" {
			return errors.New("required secret or workbench file is missing")
		}
	}
	if len(c.Provisioners) == 0 {
		return errors.New("at least one provisioner credential is required")
	}
	seenWorkers := make(map[string]bool)
	for _, provisioner := range c.Provisioners {
		if provisioner.WorkerID == "" || provisioner.TokenFile == "" || seenWorkers[provisioner.WorkerID] {
			return errors.New("provisioners require unique worker_id values and token_file values")
		}
		seenWorkers[provisioner.WorkerID] = true
	}
	if c.Auth.GitHub.ClientID == "" || len(c.Auth.GitHub.AllowedUserIDs) == 0 {
		return errors.New("GitHub client_id and allowed_user_ids are required")
	}
	if c.Lifecycle.AutoStopAfter <= 0 {
		return errors.New("lifecycle.auto_stop_after must be positive")
	}
	return nil
}

func validQuota(limit QuotaLimit, requireWorkspaceLimits bool) bool {
	if requireWorkspaceLimits && (limit.MaxWorkspaces < 1 || limit.MaxRunningWorkspaces < 1) {
		return false
	}
	return true
}

func (c Config) ValidateProvisioner() error {
	if c.Provisioner.ControlURL == "" || c.Provisioner.WorkerID == "" || c.Provisioner.TokenFile == "" || c.Provisioner.Binary == "" || len(c.Provisioner.TemplateDirs) == 0 {
		return errors.New("complete provisioner configuration is required")
	}
	if c.Provisioner.StateBackend == "" {
		c.Provisioner.StateBackend = "local"
	}
	switch c.Provisioner.StateBackend {
	case "local":
		if c.Provisioner.StateDir == "" {
			return errors.New("provisioner.state_dir is required for local state")
		}
	case "s3":
		if c.Provisioner.StateS3Bucket == "" || c.Provisioner.StateS3Region == "" {
			return errors.New("provisioner.state_s3_bucket and state_s3_region are required for s3 state")
		}
	default:
		return errors.New("provisioner.state_backend must be local or s3")
	}
	for name, directory := range c.Provisioner.TemplateDirs {
		if name == "" || directory == "" {
			return errors.New("provisioner.template_dirs must use non-empty names and directories")
		}
		if info, err := os.Stat(filepath.Join(directory, "main.tf")); err != nil || info.IsDir() {
			return fmt.Errorf("provisioner.template_dirs[%s] must contain main.tf", name)
		}
	}
	if !validLabels(c.Provisioner.Labels) {
		return errors.New("provisioner.labels must use unique lowercase labels")
	}
	if c.Provisioner.Capacity < 1 || c.Provisioner.Capacity > 100 {
		return errors.New("provisioner.capacity must be between 1 and 100")
	}
	if c.Provisioner.StateBackupRetention < 0 {
		return errors.New("provisioner.state_backup_retention must not be negative")
	}
	return nil
}

func validLabels(labels []string) bool {
	seen := make(map[string]bool)
	for _, label := range labels {
		if len(label) == 0 || len(label) > 32 || label[0] < 'a' || label[0] > 'z' || seen[label] {
			return false
		}
		for _, character := range label[1:] {
			if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
				return false
			}
		}
		seen[label] = true
	}
	return true
}
