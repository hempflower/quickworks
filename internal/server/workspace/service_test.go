package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/evanxiao/quickworks/internal/database/migration"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := migration.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO users(github_user_id, github_login) VALUES (?, ?)", 1, "tester").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCreateAndTransition(t *testing.T) {
	s := New(testDB(t))
	ws, first, err := s.Create(context.Background(), 1, "my workspace", "incus-vm-v1")
	if err != nil {
		t.Fatal(err)
	}
	if !idPattern.MatchString(ws.ID) || first.Transition != "start" || first.Status != "queued" {
		t.Fatalf("unexpected workspace/build: %#v %#v", ws, first)
	}
	if _, err := s.Transition(context.Background(), 1, ws.ID, "stop"); err == nil {
		t.Fatal("expected active build transition conflict")
	}
	if err := s.db.Model(&Build{}).Where("id = ?", first.ID).Update("status", "succeeded").Error; err != nil {
		t.Fatal(err)
	}
	second, err := s.Transition(context.Background(), 1, ws.ID, "stop")
	if err != nil {
		t.Fatal(err)
	}
	if second.Sequence != 2 || second.Transition != "stop" {
		t.Fatalf("unexpected follow-up build: %#v", second)
	}
}

func TestCreateIdempotentReturnsOriginalWorkspace(t *testing.T) {
	s := New(testDB(t))
	firstWorkspace, firstBuild, err := s.CreateIdempotent(context.Background(), 1, "request-1", "same", "first", "incus-vm-v1")
	if err != nil {
		t.Fatal(err)
	}
	secondWorkspace, secondBuild, err := s.CreateIdempotent(context.Background(), 1, "request-1", "same", "first", "incus-vm-v1")
	if err != nil {
		t.Fatal(err)
	}
	if firstWorkspace.ID != secondWorkspace.ID || firstBuild.ID != secondBuild.ID {
		t.Fatalf("idempotent request created a second workspace: %s / %s", firstWorkspace.ID, secondWorkspace.ID)
	}
	if _, _, err := s.CreateIdempotent(context.Background(), 1, "request-1", "different", "other", "incus-vm-v1"); err == nil {
		t.Fatal("expected changed idempotency request to fail")
	}
}

func TestCreateIdempotentReplacesExpiredKey(t *testing.T) {
	s := New(testDB(t))
	previous, _, err := s.Create(context.Background(), 1, "old workspace", "incus-vm-v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec(
		"INSERT INTO idempotency_keys(owner_id, key, request_hash, response_workspace_id, expires_at) VALUES (?, ?, ?, ?, ?)",
		1, "expired-request", "old", previous.ID, time.Now().Add(-time.Minute),
	).Error; err != nil {
		t.Fatal(err)
	}

	workspace, _, err := s.CreateIdempotent(context.Background(), 1, "expired-request", "new", "new workspace", "incus-vm-v1")
	if err != nil {
		t.Fatalf("create with expired idempotency key: %v", err)
	}
	if workspace.ID == previous.ID {
		t.Fatalf("workspace reused expired idempotency response: %#v", workspace)
	}
}

func TestBuildsRequireWorkspaceOwnership(t *testing.T) {
	db := testDB(t)
	if err := db.Exec("INSERT INTO users(github_user_id, github_login) VALUES (?, ?)", 2, "other").Error; err != nil {
		t.Fatal(err)
	}
	s := New(db)
	workspace, build, err := s.Create(context.Background(), 1, "owned", "incus-vm-v1")
	if err != nil {
		t.Fatal(err)
	}
	builds, err := s.Builds(context.Background(), 1, workspace.ID)
	if err != nil || len(builds) != 1 || builds[0].ID != build.ID {
		t.Fatalf("unexpected builds: %#v, %v", builds, err)
	}
	if _, err := s.Builds(context.Background(), 2, workspace.ID); err == nil {
		t.Fatal("expected cross-user build list to fail")
	}
	owned, err := s.OwnsBuild(context.Background(), 2, build.ID)
	if err != nil || owned {
		t.Fatalf("unexpected cross-user build ownership: %v, %v", owned, err)
	}
}

func TestCreateRespectsTemplateQuota(t *testing.T) {
	limits := map[string]Limits{
		"incus-vm-v1": {MaxWorkspaces: 1, MaxRunningWorkspaces: 1},
	}
	s := NewWithLimits(testDB(t), limits)
	if _, _, err := s.Create(context.Background(), 1, "first", "incus-vm-v1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(context.Background(), 1, "second", "incus-vm-v1"); err == nil {
		t.Fatal("expected workspace quota rejection")
	}
}

func TestCreateRepositoryWorkspaceStoresCanonicalRepository(t *testing.T) {
	s := New(testDB(t))
	repository := &Repository{ID: 123, FullName: "owner/repository"}
	workspace, _, err := s.CreateRepositoryIdempotent(context.Background(), 1, "repository-request", "hash", "repository workspace", "incus-vm-v1", repository)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.SourceType != "github" || workspace.RepositoryID == nil || *workspace.RepositoryID != 123 || workspace.RepositoryFullName == nil || *workspace.RepositoryFullName != "owner/repository" {
		t.Fatalf("repository metadata was not persisted: %#v", workspace)
	}
}

func TestStopIdleQueuesStopBuild(t *testing.T) {
	s := New(testDB(t))
	if err := s.db.Exec("INSERT INTO workspaces(id, owner_id, source_type, desired_state, observed_state, state_key, template_name, last_access_at) VALUES ('calm-blue-harbor', 1, 'empty', 'running', 'running', 'state', 'incus-vm-v1', ?)", time.Now().UTC().Add(-9*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.StopIdle(context.Background(), time.Now().UTC().Add(-8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var build Build
	if err := s.db.Where("workspace_id = ?", "calm-blue-harbor").First(&build).Error; err != nil {
		t.Fatal(err)
	}
	if build.Transition != "stop" || build.Status != "queued" {
		t.Fatalf("unexpected idle stop build: %#v", build)
	}
}

func TestNewWorkspaceIsNotIdleInNonUTCTimeZone(t *testing.T) {
	originalLocal := time.Local
	t.Setenv("TZ", "Asia/Shanghai")
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	time.Local = location
	t.Cleanup(func() { time.Local = originalLocal })

	s := New(testDB(t))
	workspace, _, err := s.Create(context.Background(), 1, "new workspace", "incus-vm-v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&Build{}).Where("workspace_id = ?", workspace.ID).Update("status", "succeeded").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&Workspace{}).Where("id = ?", workspace.ID).Update("observed_state", "running").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.StopIdle(context.Background(), time.Now().UTC().Add(-8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var builds []Build
	if err := s.db.Where("workspace_id = ?", workspace.ID).Order("sequence").Find(&builds).Error; err != nil {
		t.Fatal(err)
	}
	if len(builds) != 1 || builds[0].Transition != "start" {
		t.Fatalf("new workspace was incorrectly considered idle: %#v", builds)
	}
}

func TestCreateRespectsResourceQuota(t *testing.T) {
	limits := map[string]Limits{
		"incus-vm-v1": {MaxWorkspaces: 10, MaxRunningWorkspaces: 10},
	}
	resources := map[string]Resources{
		"incus-vm-v1": {CPUs: 2, MemoryGiB: 4, DiskGiB: 20},
	}
	s := NewWithPolicies(testDB(t), limits, resources)
	if _, _, err := s.Create(context.Background(), 1, "first", "incus-vm-v1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(context.Background(), 1, "second", "incus-vm-v1"); err != nil {
		t.Fatalf("count-only quota should not reject by resources: %v", err)
	}
}

func TestUsageIncludesConfiguredTemplateWithoutWorkspace(t *testing.T) {
	s := NewWithPolicies(testDB(t), map[string]Limits{"incus-vm-v1": {MaxWorkspaces: 2, MaxRunningWorkspaces: 1}}, map[string]Resources{"incus-vm-v1": {CPUs: 2, MemoryGiB: 4, DiskGiB: 20}})
	usage, err := s.Usage(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].TemplateName != "incus-vm-v1" || usage[0].Workspaces != 0 {
		t.Fatalf("unexpected empty template usage: %#v", usage)
	}
}
