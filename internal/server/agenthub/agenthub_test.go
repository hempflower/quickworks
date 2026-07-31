package agenthub

import (
	"context"
	"testing"
	"time"

	"github.com/evanxiao/quickworks/internal/database/migration"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestEnrollmentIsSingleUse(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := migration.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO users(id, github_user_id, github_login) VALUES (1, 1, 'tester')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO workspaces(id, owner_id, source_type, desired_state, observed_state, state_key) VALUES ('calm-blue-harbor', 1, 'empty', 'running', 'starting', 'workspaces/calm-blue-harbor/terraform.tfstate')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO workspace_builds(id, workspace_id, sequence, transition, status) VALUES ('build-1', 'calm-blue-harbor', 1, 'start', 'running')").Error; err != nil {
		t.Fatal(err)
	}
	hub := New(db)
	enrollment, err := hub.CreateEnrollment(ctx, "calm-blue-harbor", "build-1")
	if err != nil {
		t.Fatal(err)
	}
	registration, err := hub.Register(ctx, enrollment.Token, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if registration.AgentID != enrollment.AgentID || registration.Session == "" {
		t.Fatalf("unexpected registration: %#v", registration)
	}
	if _, err := hub.ActiveAgent(ctx, "calm-blue-harbor"); err == nil {
		t.Fatal("registered but unhealthy agent was considered ready")
	}
	if err := hub.MarkReady(ctx, enrollment.AgentID); err != nil {
		t.Fatalf("mark agent ready: %v", err)
	}
	if active, err := hub.ActiveAgent(ctx, "calm-blue-harbor"); err != nil || active != enrollment.AgentID {
		t.Fatalf("healthy agent was not active: %q, %v", active, err)
	}
	if _, err := hub.Register(ctx, enrollment.Token, make([]byte, 32)); err != nil {
		t.Fatalf("expected persistent enrollment to allow agent restart: %v", err)
	}
}

func TestMarkStaleAndHeartbeatRecoverWorkspace(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := migration.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO users(id, github_user_id, github_login) VALUES (1, 1, 'tester')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO workspaces(id, owner_id, source_type, desired_state, observed_state, state_key) VALUES ('calm-blue-harbor', 1, 'empty', 'running', 'running', 'state')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO workspace_builds(id, workspace_id, sequence, transition, status) VALUES ('build-1', 'calm-blue-harbor', 1, 'start', 'succeeded')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO workspace_agents(id, workspace_id, build_id, public_key, status, last_heartbeat_at) VALUES ('agent-1', 'calm-blue-harbor', 'build-1', ?, 'ready', ?)", make([]byte, 32), time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	hub := New(db)
	if err := hub.MarkStale(ctx, time.Now().Add(-45*time.Second)); err != nil {
		t.Fatal(err)
	}
	var observed string
	if err := db.Raw("SELECT observed_state FROM workspaces WHERE id = 'calm-blue-harbor'").Scan(&observed).Error; err != nil {
		t.Fatal(err)
	}
	if observed != "degraded" {
		t.Fatalf("workspace state = %q, want degraded", observed)
	}
	if err := hub.Heartbeat(ctx, "agent-1"); err != nil {
		t.Fatal(err)
	}
	var sessionExpiresAt time.Time
	if err := db.Raw("SELECT session_expires_at FROM workspace_agents WHERE id = 'agent-1'").Scan(&sessionExpiresAt).Error; err != nil {
		t.Fatal(err)
	}
	if !sessionExpiresAt.After(time.Now().Add(14 * time.Minute)) {
		t.Fatalf("heartbeat did not renew agent session: %v", sessionExpiresAt)
	}
	if err := db.Raw("SELECT observed_state FROM workspaces WHERE id = 'calm-blue-harbor'").Scan(&observed).Error; err != nil {
		t.Fatal(err)
	}
	if observed != "running" {
		t.Fatalf("workspace state = %q, want running", observed)
	}
}
