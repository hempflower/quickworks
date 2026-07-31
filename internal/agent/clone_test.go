package agent

import (
	"context"
	"testing"
)

func TestCloneRepositoryValidatesInputs(t *testing.T) {
	if err := CloneRepository(context.Background(), "http://github.com/owner/repo", "token", "/workspace"); err == nil {
		t.Fatal("expected non-HTTPS GitHub URL to be rejected")
	}
	if err := CloneRepository(context.Background(), "https://github.com/owner/repo", "", "/workspace"); err == nil {
		t.Fatal("expected missing token to be rejected")
	}
	if err := CloneRepository(context.Background(), "https://github.com/owner/repo", "token", "relative"); err == nil {
		t.Fatal("expected relative workspace to be rejected")
	}
}

func TestSetWorkspaceOwnershipValidatesInput(t *testing.T) {
	if err := SetWorkspaceOwnership("relative", "user", "group"); err == nil {
		t.Fatal("expected relative workspace path to be rejected")
	}
}
