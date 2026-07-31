package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

const (
	cookieName     = "quickworks_session"
	csrfCookieName = "quickworks_csrf"
	jwtIssuer      = "quickworks"
	jwtAudience    = "quickworks-browser"
	returnCookie   = "quickworks_oauth_return"
)

var githubNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,98}[A-Za-z0-9])?$`)

type User struct {
	ID           int64  `gorm:"column:id"`
	GitHubUserID int64  `gorm:"column:github_user_id"`
	GitHubLogin  string `gorm:"column:github_login"`
	AvatarURL    string `gorm:"column:avatar_url"`
}

func (User) TableName() string {
	return "users"
}

type Manager struct {
	db      *gorm.DB
	oauth   oauth2.Config
	allowed map[int64]bool
	secure  bool
	box     cipher.AEAD
	jwtKey  []byte
}

type browserClaims struct {
	UserID int64 `json:"uid"`
	jwt.RegisteredClaims
}

type Repository struct {
	ID            int64
	FullName      string
	CloneURL      string
	DefaultBranch string
	Private       bool
}

type CloneCredential struct {
	RepositoryURL string
	Token         string
}

func New(db *gorm.DB, publicURL, clientID, clientSecret string, allowed []int64, masterKey []byte) (*Manager, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return nil, err
	}
	derived, err := hkdf.Key(sha256.New, masterKey, nil, "quickworks/github-oauth/v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive GitHub credential key: %w", err)
	}
	jwtKey, err := hkdf.Key(sha256.New, masterKey, nil, "quickworks/browser-session/jwt-hs256/v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive browser session signing key: %w", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	box, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	m := &Manager{db: db, oauth: oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: oauth2.Endpoint{AuthURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token"}, RedirectURL: strings.TrimRight(publicURL, "/") + "/auth/github/callback", Scopes: []string{"read:user", "repo"}}, allowed: map[int64]bool{}, secure: u.Scheme == "https", box: box, jwtKey: jwtKey}
	for _, id := range allowed {
		m.allowed[id] = true
	}
	return m, nil
}
func (m *Manager) Begin(w http.ResponseWriter, r *http.Request) {
	state := random()
	verifier := oauth2.GenerateVerifier()
	returnTo := safeReturnTo(r.URL.Query().Get("return_to"))
	http.SetCookie(w, &http.Cookie{Name: "quickworks_oauth_state", Value: state, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.SetCookie(w, &http.Cookie{Name: "quickworks_oauth_verifier", Value: verifier, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.SetCookie(w, &http.Cookie{Name: returnCookie, Value: returnTo, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, m.oauth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}
func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) {
	returnTo := "/"
	if cookie, err := r.Cookie(returnCookie); err == nil {
		returnTo = safeReturnTo(cookie.Value)
	}
	defer m.clearOAuthCookies(w)
	c, err := r.Cookie("quickworks_oauth_state")
	if err != nil || r.URL.Query().Get("state") == "" || c.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid OAuth state", 400)
		return
	}
	verifier, err := r.Cookie("quickworks_oauth_verifier")
	if err != nil {
		http.Error(w, "missing OAuth verifier", http.StatusBadRequest)
		return
	}
	token, err := m.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier.Value))
	if err != nil {
		http.Error(w, "GitHub token exchange failed", 502)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		http.Error(w, "GitHub user lookup failed", 502)
		return
	}
	defer resp.Body.Close()
	var gh struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	if json.NewDecoder(resp.Body).Decode(&gh) != nil || !m.allowed[gh.ID] {
		http.Error(w, "GitHub user is not authorized", 403)
		return
	}
	var u User
	result := m.db.WithContext(r.Context()).Where(User{GitHubUserID: gh.ID}).Assign(User{GitHubLogin: gh.Login, AvatarURL: gh.AvatarURL}).FirstOrCreate(&u)
	if result.Error != nil {
		http.Error(w, "save user", 500)
		return
	}
	if err := m.storeCredential(r.Context(), u.ID, token.AccessToken); err != nil {
		http.Error(w, "store GitHub credential", http.StatusInternalServerError)
		return
	}
	if err := m.issue(w, u.ID); err != nil {
		http.Error(w, "issue browser session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

func (m *Manager) storeCredential(ctx context.Context, userID int64, token string) error {
	nonce, ciphertext, err := m.encryptToken(userID, token)
	if err != nil {
		return err
	}
	return m.db.WithContext(ctx).Exec("INSERT INTO github_credentials(user_id, ciphertext, nonce, scopes, updated_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(user_id) DO UPDATE SET ciphertext = excluded.ciphertext, nonce = excluded.nonce, scopes = excluded.scopes, updated_at = excluded.updated_at", userID, ciphertext, nonce, "read:user repo", time.Now()).Error
}

func (m *Manager) encryptToken(userID int64, token string) ([]byte, []byte, error) {
	nonce := make([]byte, m.box.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ciphertext := m.box.Seal(nil, nonce, []byte(token), []byte(fmt.Sprint(userID)))
	return nonce, ciphertext, nil
}

// GitHubToken decrypts a credential only for the short operation that needs it.
// Callers must not log, persist, or place the returned token in an argument.
func (m *Manager) GitHubToken(ctx context.Context, userID int64) (string, error) {
	var credential struct {
		Ciphertext []byte
		Nonce      []byte
	}
	if err := m.db.WithContext(ctx).Raw("SELECT ciphertext, nonce FROM github_credentials WHERE user_id = ?", userID).Scan(&credential).Error; err != nil {
		return "", err
	}
	if len(credential.Nonce) != m.box.NonceSize() {
		return "", errors.New("GitHub credential is missing")
	}
	plaintext, err := m.box.Open(nil, credential.Nonce, credential.Ciphertext, []byte(fmt.Sprint(userID)))
	if err != nil {
		return "", errors.New("GitHub credential cannot be decrypted")
	}
	return string(plaintext), nil
}

// Repository resolves a repository with the current user's OAuth credential.
// The returned clone URL is accepted only when GitHub identifies it as its
// canonical HTTPS URL, preventing a request from supplying an arbitrary host.
func (m *Manager) Repository(ctx context.Context, userID int64, owner, name string) (Repository, error) {
	if !githubName(owner) || !githubName(name) {
		return Repository{}, errors.New("invalid GitHub repository name")
	}
	token, err := m.GitHubToken(ctx, userID)
	if err != nil {
		return Repository{}, err
	}
	endpoint := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Repository{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return Repository{}, fmt.Errorf("query GitHub repository: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		_ = m.db.WithContext(ctx).Exec("DELETE FROM github_credentials WHERE user_id = ?", userID).Error
		return Repository{}, errors.New("GitHub authorization expired; sign in again")
	}
	if response.StatusCode == http.StatusNotFound {
		return Repository{}, errors.New("GitHub repository was not found or is not accessible")
	}
	if response.StatusCode != http.StatusOK {
		return Repository{}, fmt.Errorf("GitHub repository lookup failed: %s", response.Status)
	}
	var result struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return Repository{}, errors.New("invalid GitHub repository response")
	}
	cloneURL, err := url.Parse(result.CloneURL)
	if err != nil || cloneURL.Scheme != "https" || cloneURL.Host != "github.com" || result.ID < 1 || result.FullName == "" || result.DefaultBranch == "" {
		return Repository{}, errors.New("GitHub returned invalid repository metadata")
	}
	return Repository{ID: result.ID, FullName: result.FullName, CloneURL: result.CloneURL, DefaultBranch: result.DefaultBranch, Private: result.Private}, nil
}

func (m *Manager) CreateCloneCredential(ctx context.Context, workspaceID, buildID string, userID int64, repositoryURL string) error {
	token, err := m.GitHubToken(ctx, userID)
	if err != nil {
		return err
	}
	nonce, ciphertext, err := m.encryptToken(userID, token)
	if err != nil {
		return err
	}
	return m.db.WithContext(ctx).Exec("INSERT INTO clone_credentials(id, workspace_id, build_id, user_id, repository_url, ciphertext, nonce, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(build_id) DO NOTHING", random(), workspaceID, buildID, userID, repositoryURL, ciphertext, nonce, time.Now().Add(15*time.Minute)).Error
}

// ConsumeCloneCredential returns a credential only once, after the matching
// agent enrolled. It deletes the encrypted record before returning plaintext.
func (m *Manager) ConsumeCloneCredential(ctx context.Context, workspaceID, buildID string) (CloneCredential, error) {
	var record struct {
		UserID        int64
		RepositoryURL string
		Ciphertext    []byte
		Nonce         []byte
	}
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT user_id, repository_url, ciphertext, nonce FROM clone_credentials WHERE workspace_id = ? AND build_id = ? AND consumed_at IS NULL AND expires_at > ?", workspaceID, buildID, time.Now()).Scan(&record).Error; err != nil {
			return err
		}
		if record.RepositoryURL == "" {
			return gorm.ErrRecordNotFound
		}
		deleted := tx.Exec("DELETE FROM clone_credentials WHERE workspace_id = ? AND build_id = ? AND consumed_at IS NULL", workspaceID, buildID)
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CloneCredential{}, nil
	}
	if err != nil {
		return CloneCredential{}, err
	}
	if len(record.Nonce) != m.box.NonceSize() {
		return CloneCredential{}, errors.New("clone credential is invalid")
	}
	plaintext, err := m.box.Open(nil, record.Nonce, record.Ciphertext, []byte(fmt.Sprint(record.UserID)))
	if err != nil {
		return CloneCredential{}, errors.New("clone credential cannot be decrypted")
	}
	return CloneCredential{RepositoryURL: record.RepositoryURL, Token: string(plaintext)}, nil
}
func (m *Manager) Current(r *http.Request) (User, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return User{}, err
	}
	claims := &browserClaims{}
	token, err := jwt.ParseWithClaims(c.Value, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected browser session signing method")
		}
		return m.jwtKey, nil
	}, jwt.WithAudience(jwtAudience), jwt.WithIssuer(jwtIssuer), jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil || !token.Valid || claims.UserID < 1 {
		return User{}, errors.New("browser session is invalid")
	}
	var u User
	if err := m.db.WithContext(r.Context()).First(&u, claims.UserID).Error; err != nil {
		return User{}, err
	}
	if !m.allowed[u.GitHubUserID] {
		return User{}, errors.New("GitHub user is not authorized")
	}
	return u, nil
}
func (m *Manager) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := m.Current(r)
		if err != nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if isUnsafeMethod(r.Method) && !m.validCSRF(r) {
			http.Error(w, "CSRF token is missing or invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

// RequirePage redirects unauthenticated browser navigations to GitHub while
// keeping API routes on Require, which correctly returns a machine-readable
// 401 instead of HTML.
func (m *Manager) RequirePage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := m.Current(r)
		if err != nil {
			http.Redirect(w, r, "/auth/github?return_to="+url.QueryEscape(safeReturnTo(r.URL.RequestURI())), http.StatusFound)
			return
		}
		if isUnsafeMethod(r.Method) && !m.validCSRF(r) {
			http.Error(w, "CSRF token is missing or invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

// RequireWorkbench authenticates a browser workspace proxy request without
// applying control-plane CSRF rules. The proxied application owns its POST
// semantics; workspace ownership is checked by the router before proxying.
func (m *Manager) RequireWorkbench(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := m.Current(r)
		if err != nil {
			http.Redirect(w, r, "/auth/github?return_to="+url.QueryEscape(safeReturnTo(r.URL.RequestURI())), http.StatusFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

type userKey struct{}

func UserFromContext(ctx context.Context) (User, error) {
	u, ok := ctx.Value(userKey{}).(User)
	if !ok {
		return User{}, errors.New("missing authenticated user")
	}
	return u, nil
}
func (m *Manager) issue(w http.ResponseWriter, userID int64) error {
	now := time.Now()
	claims := browserClaims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{jwtAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    jwtIssuer,
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
			ID:        random(),
		},
	}
	v, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.jwtKey)
	if err != nil {
		return fmt.Errorf("sign browser session: %w", err)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: v, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: random(), Path: "/", Secure: m.secure, SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	return nil
}

func (m *Manager) clearOAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{"quickworks_oauth_state", "quickworks_oauth_verifier", returnCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	}
}

func safeReturnTo(value string) string {
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, "\\") {
		return "/"
	}
	return value
}

func githubName(value string) bool {
	return githubNamePattern.MatchString(value)
}

func (m *Manager) CSRF(r *http.Request) string {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (m *Manager) validCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		if err := r.ParseForm(); err != nil {
			return false
		}
		token = r.Form.Get("csrf_token")
	}
	return subtleEqual(cookie.Value, token)
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func subtleEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var result byte
	for index := range left {
		result |= left[index] ^ right[index]
	}
	return result == 0
}
func random() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}
