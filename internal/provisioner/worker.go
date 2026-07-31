// Package provisioner executes one administrator-provided OpenTofu template.
package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/evanxiao/quickworks/internal/config"
	"github.com/evanxiao/quickworks/internal/provisioner/state"
)

type Worker struct {
	config config.Config
	client *http.Client
	token  string
	state  state.Store
}

type build struct {
	ID           string `json:"id"`
	WorkspaceID  string `json:"workspace_id"`
	Transition   string `json:"transition"`
	TemplateName string `json:"template_name"`
}

type lease struct {
	Build   build  `json:"build"`
	LeaseID string `json:"lease_id"`
	Agent   struct {
		AgentID string `json:"agent_id"`
		Token   string `json:"enrollment_token"`
	} `json:"agent"`
}

func New(c config.Config, token string) *Worker {
	store := state.Store(state.NewLocal(c.Provisioner.StateDir, c.Provisioner.StateBackupDir, c.Provisioner.StateBackupRetention))
	if c.Provisioner.StateBackend == "s3" {
		store = state.NewS3(c.Provisioner.StateS3Bucket, c.Provisioner.StateS3Region, c.Provisioner.StateS3Endpoint, c.Provisioner.StateS3KeyPrefix)
	}
	return &Worker{config: c, token: token, client: &http.Client{Timeout: 30 * time.Second}, state: store}
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := w.runOnce(ctx); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(3 * time.Second):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (w *Worker) runOnce(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{
		"worker_id": w.config.Provisioner.WorkerID,
		"labels":    w.config.Provisioner.Labels,
		"capacity":  w.config.Provisioner.Capacity,
	})
	request, err := w.request(ctx, http.MethodPost, "/api/internal/provisioner/leases", body)
	if err != nil {
		return err
	}
	response, err := w.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("claim build: %s", response.Status)
	}
	var l lease
	if err := json.NewDecoder(response.Body).Decode(&l); err != nil {
		return err
	}
	w.log(ctx, l, "info", "build claimed")
	executionContext, cancel := context.WithCancel(ctx)
	leaseDone := make(chan struct{})
	go w.maintainLease(executionContext, l, leaseDone)
	err = w.execute(executionContext, l)
	cancel()
	<-leaseDone
	if err != nil {
		w.log(ctx, l, "error", err.Error())
	}
	return w.complete(ctx, l, err)
}

func (w *Worker) maintainLease(ctx context.Context, l lease, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.renew(ctx, l); err != nil {
				w.log(ctx, l, "warning", "build lease renewal failed")
			}
		}
	}
}

func (w *Worker) execute(ctx context.Context, l lease) error {
	dir, err := os.MkdirTemp("", "quickworks-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	templateDir := w.config.Provisioner.TemplateDirs[l.Build.TemplateName]
	if templateDir == "" {
		return fmt.Errorf("worker does not have template %q", l.Build.TemplateName)
	}
	if err := copyTemplate(templateDir, dir); err != nil {
		return err
	}
	backendArgs, err := w.state.BackendArgs(l.Build.WorkspaceID)
	if err != nil {
		return err
	}
	startupScript := "#!/bin/sh\nexit 0\n"
	if l.Build.Transition == "start" {
		startupScript = fmt.Sprintf(`#!/bin/sh
set -eu
export QUICKWORKS_AGENT_CONTROL_URL=%q
export QUICKWORKS_AGENT_ID=%q
export QUICKWORKS_AGENT_ENROLLMENT_TOKEN=%q
curl --fail --silent --show-error --location "$QUICKWORKS_AGENT_CONTROL_URL/assets/workspace-bootstrap.sh" --output /usr/local/bin/quickworks-bootstrap
chmod 0700 /usr/local/bin/quickworks-bootstrap
exec /usr/local/bin/quickworks-bootstrap
`, w.config.Provisioner.ControlURL, l.Agent.AgentID, l.Agent.Token)
	}
	vars, err := json.Marshal(map[string]string{
		"workspace_id":   l.Build.WorkspaceID,
		"transition":     l.Build.Transition,
		"startup_script": startupScript,
	})
	if err != nil {
		return err
	}
	varsPath := filepath.Join(dir, "quickworks.auto.tfvars.json")
	if err := os.WriteFile(varsPath, vars, 0600); err != nil {
		return err
	}
	defer os.Remove(varsPath)
	commands := [][]string{
		append([]string{"init", "-input=false"}, backendArgs...),
		{"plan", "-input=false", "-lock-timeout=30s", "-out=plan.bin"},
		{"apply", "-input=false", "plan.bin"},
	}
	for _, args := range commands {
		if err := w.command(ctx, l, dir, args...); err != nil {
			return err
		}
	}
	if backup, err := w.state.Backup(l.Build.WorkspaceID); err != nil {
		return fmt.Errorf("backup Terraform state: %w", err)
	} else if backup != "" {
		w.log(ctx, l, "info", "Terraform state backup created")
	}
	return nil
}

func (w *Worker) command(ctx context.Context, l lease, dir string, args ...string) error {
	command := exec.CommandContext(ctx, w.config.Provisioner.Binary, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(dir, ".terraform"))
	output, err := command.CombinedOutput()
	if len(output) > 0 {
		w.log(ctx, l, "info", string(output))
	}
	if err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	return nil
}

func (w *Worker) complete(ctx context.Context, l lease, runErr error) error {
	payload := map[string]string{"worker_id": w.config.Provisioner.WorkerID, "lease_id": l.LeaseID}
	if runErr != nil {
		payload["error"] = runErr.Error()
	}
	return w.post(ctx, "/api/internal/provisioner/leases/"+l.Build.ID+"/complete", payload)
}

func (w *Worker) renew(ctx context.Context, l lease) error {
	return w.post(ctx, "/api/internal/provisioner/leases/"+l.Build.ID+"/renew", map[string]string{
		"worker_id": w.config.Provisioner.WorkerID,
		"lease_id":  l.LeaseID,
	})
}

func (w *Worker) log(ctx context.Context, l lease, level, message string) {
	_ = w.post(ctx, "/api/internal/provisioner/leases/"+l.Build.ID+"/logs", map[string]string{"worker_id": w.config.Provisioner.WorkerID, "lease_id": l.LeaseID, "level": level, "message": w.redact(message, l)})
}

func (w *Worker) redact(message string, l lease) string {
	for _, secret := range []string{w.token, l.LeaseID, l.Agent.Token} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

func (w *Worker) post(ctx context.Context, path string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := w.request(ctx, http.MethodPost, path, data)
	if err != nil {
		return err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return nil
}

func (w *Worker) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.config.Provisioner.ControlURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func copyTemplate(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == ".terraform" || entry.Name() == "terraform.tfstate" {
			continue
		}
		in, err := os.Open(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(destination, entry.Name()), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
