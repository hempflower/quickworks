package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/evanxiao/quickworks/internal/config"
	"github.com/evanxiao/quickworks/internal/provisioner"
	"github.com/evanxiao/quickworks/internal/provisioner/state"
)

func RunProvisioner(ctx context.Context, configPath string) error {
	c, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := c.ValidateProvisioner(); err != nil {
		return err
	}
	token, err := os.ReadFile(c.Provisioner.TokenFile)
	if err != nil {
		return fmt.Errorf("read provisioner token: %w", err)
	}
	return provisioner.New(c, strings.TrimSpace(string(token))).Run(ctx)
}

// RestoreLocalState is an explicit operator operation. It does not run during
// normal worker polling, preventing an accidental restore from racing apply.
func RestoreLocalState(configPath, workspaceID, snapshot string) error {
	c, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := c.ValidateProvisioner(); err != nil {
		return err
	}
	if c.Provisioner.StateBackend == "s3" {
		return errors.New("restore S3 state through the bucket versioning workflow")
	}
	return state.NewLocal(c.Provisioner.StateDir, c.Provisioner.StateBackupDir, c.Provisioner.StateBackupRetention).Restore(workspaceID, snapshot)
}
