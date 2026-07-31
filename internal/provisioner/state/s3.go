package state

import (
	"fmt"
	"strings"
)

// S3 delegates state locking and versioning to OpenTofu's native S3 backend.
// Cloud credentials are supplied through the provisioner process environment
// (for example instance roles or AWS_* variables), never YAML or state args.
type S3 struct {
	bucket   string
	region   string
	endpoint string
	prefix   string
}

func NewS3(bucket, region, endpoint, prefix string) *S3 {
	return &S3{bucket: bucket, region: region, endpoint: endpoint, prefix: strings.Trim(prefix, "/")}
}

func (s *S3) Path(workspaceID string) (string, error) {
	if !validWorkspaceID(workspaceID) {
		return "", fmt.Errorf("invalid workspace ID for state key")
	}
	key := "workspaces/" + workspaceID + "/terraform.tfstate"
	if s.prefix != "" {
		key = s.prefix + "/" + key
	}
	return key, nil
}

func (s *S3) BackendArgs(workspaceID string) ([]string, error) {
	key, err := s.Path(workspaceID)
	if err != nil {
		return nil, err
	}
	args := []string{"-backend-config=bucket=" + s.bucket, "-backend-config=key=" + key, "-backend-config=region=" + s.region, "-backend-config=use_lockfile=true"}
	if s.endpoint != "" {
		args = append(args, "-backend-config=endpoints.s3="+s.endpoint)
	}
	return args, nil
}

func (s *S3) Backup(string) (string, error) {
	// Enable bucket versioning and lifecycle retention for remote backups.
	return "", nil
}

func (s *S3) Restore(string, string) error {
	return fmt.Errorf("restore remote state through the S3 bucket versioning workflow")
}
