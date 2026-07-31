package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evanxiao/quickworks/internal/database/migration"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestGitHubCredentialIsEncryptedAndBoundToUser(t *testing.T) {
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
	manager, err := New(db, "http://localhost:8082", "client", "secret", []int64{1}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.storeCredential(ctx, 1, "gho_secret_token"); err != nil {
		t.Fatal(err)
	}
	var stored struct{ Ciphertext []byte }
	if err := db.Raw("SELECT ciphertext FROM github_credentials WHERE user_id = 1").Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Ciphertext, []byte("gho_secret_token")) {
		t.Fatal("credential was stored in plaintext")
	}
	token, err := manager.GitHubToken(ctx, 1)
	if err != nil || token != "gho_secret_token" {
		t.Fatalf("unexpected decrypted token: %q, %v", token, err)
	}
	if _, err := manager.GitHubToken(ctx, 2); err == nil {
		t.Fatal("expected missing credential to fail")
	}
}

func TestBrowserSessionUsesSignedJWTAndChecksAllowlist(t *testing.T) {
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
	manager, err := New(db, "http://localhost:8082", "client", "secret", []int64{1}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := manager.issue(recorder, 1); err != nil {
		t.Fatal(err)
	}
	response := recorder.Result()
	t.Cleanup(func() { _ = response.Body.Close() })
	var session *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == cookieName {
			session = cookie
		}
	}
	if session == nil || len(session.Value) < 40 || !session.HttpOnly {
		t.Fatal("expected an HttpOnly JWT session cookie")
	}
	request := httptest.NewRequest(http.MethodGet, "http://localhost:8082/", nil)
	request.AddCookie(session)
	user, err := manager.Current(request)
	if err != nil || user.ID != 1 {
		t.Fatalf("unexpected authenticated user: %+v, %v", user, err)
	}
	tampered := *session
	tampered.Value += "x"
	request = httptest.NewRequest(http.MethodGet, "http://localhost:8082/", nil)
	request.AddCookie(&tampered)
	if _, err := manager.Current(request); err == nil {
		t.Fatal("expected tampered JWT to be rejected")
	}
	delete(manager.allowed, 1)
	request = httptest.NewRequest(http.MethodGet, "http://localhost:8082/", nil)
	request.AddCookie(session)
	if _, err := manager.Current(request); err == nil {
		t.Fatal("expected removed allowlist user to be rejected")
	}
}

func TestRequirePageRedirectsOnlyToSafeLocalReturnPath(t *testing.T) {
	manager := &Manager{}
	next := manager.RequirePage(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	next.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://quickworks.test/w/demo/?q=1", nil))
	if response.Code != http.StatusFound {
		t.Fatalf("unexpected redirect status: %d", response.Code)
	}
	if response.Header().Get("Location") != "/auth/github?return_to=%2Fw%2Fdemo%2F%3Fq%3D1" {
		t.Fatalf("unexpected login location: %q", response.Header().Get("Location"))
	}
	if got := safeReturnTo("https://attacker.example"); got != "/" {
		t.Fatalf("unsafe return path accepted: %q", got)
	}
}

func TestCloneCredentialIsEncryptedAndConsumedOnce(t *testing.T) {
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
	if err := db.Exec("INSERT INTO workspaces(id, owner_id, source_type, desired_state, observed_state, state_key, template_name) VALUES ('calm-blue-harbor', 1, 'github', 'running', 'pending', 'state', 'incus-vm-v1')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO workspace_builds(id, workspace_id, sequence, transition, status, template_name) VALUES ('build-1', 'calm-blue-harbor', 1, 'start', 'queued', 'incus-vm-v1')").Error; err != nil {
		t.Fatal(err)
	}
	manager, err := New(db, "http://localhost:8082", "client", "secret", []int64{1}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.storeCredential(ctx, 1, "gho_long_lived_token"); err != nil {
		t.Fatal(err)
	}
	if err := manager.CreateCloneCredential(ctx, "calm-blue-harbor", "build-1", 1, "https://github.com/owner/repository.git"); err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Ciphertext []byte
	}
	if err := db.Raw("SELECT ciphertext FROM clone_credentials WHERE build_id = 'build-1'").Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Ciphertext, []byte("gho_long_lived_token")) {
		t.Fatal("clone credential was stored in plaintext")
	}
	credential, err := manager.ConsumeCloneCredential(ctx, "calm-blue-harbor", "build-1")
	if err != nil {
		t.Fatal(err)
	}
	if credential.RepositoryURL != "https://github.com/owner/repository.git" || credential.Token != "gho_long_lived_token" {
		t.Fatalf("unexpected clone credential: %#v", credential)
	}
	again, err := manager.ConsumeCloneCredential(ctx, "calm-blue-harbor", "build-1")
	if err != nil || again.Token != "" || again.RepositoryURL != "" {
		t.Fatalf("clone credential was replayed: %#v, %v", again, err)
	}
}
