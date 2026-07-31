package state

import (
	"reflect"
	"testing"
)

func TestS3BackendArgsUseWorkspaceScopedKeyAndLockfile(t *testing.T) {
	store := NewS3("quickworks-state", "cn-hangzhou", "https://s3.example.test", "production")
	args, err := store.BackendArgs("calm-blue-harbor")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-backend-config=bucket=quickworks-state",
		"-backend-config=key=production/workspaces/calm-blue-harbor/terraform.tfstate",
		"-backend-config=region=cn-hangzhou",
		"-backend-config=use_lockfile=true",
		"-backend-config=endpoints.s3=https://s3.example.test",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected S3 backend args: %#v", args)
	}
}

func TestS3StateRejectsUnsafeWorkspaceID(t *testing.T) {
	store := NewS3("bucket", "region", "", "")
	if _, err := store.BackendArgs("../../escape"); err == nil {
		t.Fatal("unsafe workspace ID was accepted")
	}
}
