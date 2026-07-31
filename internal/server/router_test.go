package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/evanxiao/quickworks/internal/database/migration"
	"github.com/evanxiao/quickworks/internal/server/auth"
	"github.com/evanxiao/quickworks/internal/server/workspace"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestRouterRegistersChiRoutes(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := migration.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(db, "http://localhost:8082", "client", "secret", []int64{1}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	release, err := NewReleaseAssets(executable)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(db, a, map[string]string{"worker-1": "provisioner-token"}, release, AgentWorkbench{}, TemplateCatalog{
		Default: "incus-vm-v1",
		Names: map[string]bool{
			"incus-vm-v1": true,
		},
		RequiredLabels: map[string][]string{
			"incus-vm-v1": {"home"},
		},
		Limits: map[string]workspace.Limits{
			"incus-vm-v1": {MaxWorkspaces: 1, MaxRunningWorkspaces: 1},
		},
	}, time.Hour)

	for _, path := range []string{"/healthz", "/auth/github"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK && response.Code != http.StatusFound {
			t.Fatalf("GET %s returned %d", path, response.Code)
		}
	}
	for _, path := range []string{"/", "/w/example/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusFound {
			t.Fatalf("GET %s returned %d, want %d", path, response.Code, http.StatusFound)
		}
		if response.Header().Get("Location") == "" {
			t.Fatalf("GET %s did not redirect to login", path)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/workspaces returned %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestExpireLeasesFailsInsteadOfRequeueingBuild(t *testing.T) {
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
	if err := db.Exec("INSERT INTO workspaces(id, owner_id, source_type, desired_state, observed_state, state_key, template_name) VALUES ('calm-blue-harbor', 1, 'empty', 'running', 'starting', 'state', 'incus-vm-v1')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO provisioners(worker_id, labels_json, capacity, active_builds, last_seen_at, updated_at) VALUES ('worker-1', '[]', 1, 1, ?, ?)", time.Now(), time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO workspace_builds(id, workspace_id, sequence, transition, status, claimed_by, lease_expires_at, template_name) VALUES ('expired-build', 'calm-blue-harbor', 1, 'start', 'running', 'worker-1', ?, 'incus-vm-v1')", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	router := &Router{db: db}
	if err := router.expireLeases(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.Raw("SELECT status FROM workspace_builds WHERE id = 'expired-build'").Scan(&status).Error; err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("expired build status = %q, want failed", status)
	}
	var active int
	if err := db.Raw("SELECT active_builds FROM provisioners WHERE worker_id = 'worker-1'").Scan(&active).Error; err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("expired build was not released from worker capacity: %d", active)
	}
}

func TestProvisionerTokenIsBoundToWorkerID(t *testing.T) {
	router := &Router{provisionerTokenHash: map[string]string{"worker-a": hash("token-a")}}
	request := httptest.NewRequest(http.MethodPost, "/api/internal/provisioner/leases", nil)
	request.Header.Set("Authorization", "Bearer token-a")
	if !router.internal(request, "worker-a") {
		t.Fatal("expected matching worker token to authenticate")
	}
	if router.internal(request, "worker-b") {
		t.Fatal("worker token authenticated a different worker ID")
	}
}

func TestClaimMatchingBuildLooksPastUnmatchedQueue(t *testing.T) {
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
	if err := db.Exec("INSERT INTO provisioners(worker_id, labels_json, capacity, active_builds, last_seen_at, updated_at) VALUES ('home-worker', '[\"home\"]', 1, 0, ?, ?)", time.Now(), time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 101; index++ {
		workspaceID := fmt.Sprintf("unmatched-%03d", index)
		buildID := fmt.Sprintf("unmatched-build-%03d", index)
		if err := db.Exec("INSERT INTO workspaces(id, owner_id, source_type, desired_state, observed_state, state_key, template_name) VALUES (?, 1, 'empty', 'running', 'pending', ?, 'remote')", workspaceID, "state-"+workspaceID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO workspace_builds(id, workspace_id, sequence, transition, status, template_name) VALUES (?, ?, 1, 'start', 'queued', 'remote')", buildID, workspaceID).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("INSERT INTO workspaces(id, owner_id, source_type, desired_state, observed_state, state_key, template_name) VALUES ('matching-workspace', 1, 'empty', 'running', 'pending', 'matching-state', 'local')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO workspace_builds(id, workspace_id, sequence, transition, status, template_name) VALUES ('matching-build', 'matching-workspace', 1, 'start', 'queued', 'local')").Error; err != nil {
		t.Fatal(err)
	}
	router := &Router{db: db, templates: TemplateCatalog{RequiredLabels: map[string][]string{"remote": {"remote"}, "local": {"home"}}}}
	var claimed workspace.Build
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := router.claimMatchingBuild(request, "home-worker", []string{"home"}, 1, "lease-1", time.Now().Add(time.Minute), &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.ID != "matching-build" {
		t.Fatalf("claimed %q, want matching build after unmatched queue", claimed.ID)
	}
}

func TestTemplateAvailabilityCountsOnlyHealthyCapacity(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := migration.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO provisioners(worker_id, labels_json, capacity, active_builds, last_seen_at, updated_at) VALUES ('healthy', '[\"home\"]', 3, 1, ?, ?), ('stale', '[\"home\"]', 2, 0, ?, ?)", time.Now(), time.Now(), time.Now().Add(-2*time.Minute), time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	router := &Router{db: db, templates: TemplateCatalog{Default: "local", Names: map[string]bool{"local": true}, RequiredLabels: map[string][]string{"local": {"home"}}}}
	response := httptest.NewRecorder()
	router.templateAvailability(response, httptest.NewRequest(http.MethodGet, "/api/templates", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected template availability status: %d", response.Code)
	}
	var templates []struct {
		Name             string `json:"name"`
		MatchingWorkers  int    `json:"matching_workers"`
		AvailableWorkers int    `json:"available_workers"`
		AvailableSlots   int    `json:"available_slots"`
	}
	if err := json.NewDecoder(response.Body).Decode(&templates); err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 || templates[0].Name != "local" || templates[0].MatchingWorkers != 2 || templates[0].AvailableWorkers != 1 || templates[0].AvailableSlots != 2 {
		t.Fatalf("unexpected template availability: %#v", templates)
	}
}
