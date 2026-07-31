package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type EnrollmentResponse struct {
	WorkspaceID          string `json:"workspace_id"`
	BuildID              string `json:"build_id"`
	AgentID              string `json:"agent_id"`
	Session              string `json:"session"`
	WorkbenchVersion     string `json:"workbench_version"`
	WorkbenchBundleURL   string `json:"workbench_bundle_url"`
	WorkbenchBundleSHA   string `json:"workbench_bundle_sha256"`
	WorkbenchEntrypoint  string `json:"workbench_entrypoint"`
	WorkbenchBYOKConfig  string `json:"workbench_byok_config"`
	WorkbenchEnvironment string `json:"workbench_environment"`
	RepositoryURL        string `json:"repository_url"`
	CloneToken           string `json:"clone_token"`
	GitHubToken          string `json:"github_token"`
}

func Enroll(ctx context.Context, controlURL, token string, privateKey ed25519.PrivateKey) (EnrollmentResponse, error) {
	publicKey := privateKey.Public().(ed25519.PublicKey)
	payload, err := json.Marshal(map[string]string{
		"enrollment_token": token,
		"public_key":       base64.RawStdEncoding.EncodeToString(publicKey),
	})
	if err != nil {
		return EnrollmentResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(controlURL, "/")+"/api/agent/enroll", bytes.NewReader(payload))
	if err != nil {
		return EnrollmentResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return EnrollmentResponse{}, fmt.Errorf("agent enrollment request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return EnrollmentResponse{}, fmt.Errorf("agent enrollment rejected: %s", response.Status)
	}
	var registered EnrollmentResponse
	if err := json.NewDecoder(response.Body).Decode(&registered); err != nil {
		return EnrollmentResponse{}, err
	}
	if registered.AgentID == "" || registered.Session == "" || registered.WorkbenchBundleURL == "" || registered.WorkbenchBundleSHA == "" || registered.WorkbenchEntrypoint == "" || registered.GitHubToken == "" {
		return EnrollmentResponse{}, fmt.Errorf("agent enrollment response is incomplete")
	}
	return registered, nil
}

// ReportReady tells the control plane that the locally supervised Workbench
// has passed its health check and can receive proxied browser traffic.
func ReportReady(ctx context.Context, controlURL, agentID, session string) error {
	payload, err := json.Marshal(map[string]string{"agent_id": agentID})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(controlURL, "/")+"/api/agent/ready", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+session)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("report Workbench ready: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("report Workbench ready rejected: %s", response.Status)
	}
	return nil
}
