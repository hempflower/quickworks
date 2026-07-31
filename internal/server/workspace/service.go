package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"gorm.io/gorm"
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}-[a-z][a-z0-9]{1,15}-[a-z][a-z0-9]{1,15}$`)

var adjectives = []string{"calm", "bright", "swift", "quiet", "amber", "clear", "lunar", "solid", "kind", "fresh"}
var nouns = []string{"harbor", "meadow", "forest", "river", "summit", "garden", "canyon", "island", "valley", "orbit"}

type Workspace struct {
	ID                 string         `json:"id"`
	OwnerID            int64          `json:"-"`
	DisplayName        *string        `json:"display_name,omitempty"`
	SourceType         string         `json:"source_type"`
	RepositoryID       *int64         `json:"repository_id,omitempty"`
	RepositoryFullName *string        `json:"repository_full_name,omitempty"`
	TemplateName       string         `json:"template_name"`
	DesiredState       string         `json:"desired_state"`
	ObservedState      string         `json:"observed_state"`
	StateKey           string         `json:"-"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	LastAccessAt       time.Time      `json:"last_access_at"`
	DeletedAt          gorm.DeletedAt `json:"-"`
}

func (Workspace) TableName() string {
	return "workspaces"
}

type Build struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	Sequence       int        `json:"sequence"`
	Transition     string     `json:"transition"`
	TemplateName   string     `json:"template_name"`
	Status         string     `json:"status"`
	ClaimedBy      *string    `json:"claimed_by,omitempty"`
	LeaseIDHash    *string    `json:"-"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Error          *string    `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

func (Build) TableName() string {
	return "workspace_builds"
}

type Limits struct {
	MaxWorkspaces        int `json:"max_workspaces"`
	MaxRunningWorkspaces int `json:"max_running_workspaces"`
}

type Resources struct {
	CPUs          int     `json:"cpus"`
	MemoryGiB     int     `json:"memory_gib"`
	DiskGiB       int     `json:"disk_gib"`
	EstimatedCost float64 `json:"estimated_cost"`
}

type Usage struct {
	TemplateName      string `json:"template_name"`
	Workspaces        int64  `json:"workspaces"`
	RunningWorkspaces int64  `json:"running_workspaces"`
	Limits            Limits `json:"limits"`
}

type Repository struct {
	ID       int64
	FullName string
}

type Service struct {
	db        *gorm.DB
	limits    map[string]Limits
	resources map[string]Resources
}

func New(db *gorm.DB) *Service {
	return NewWithLimits(db, nil)
}

func NewWithLimits(db *gorm.DB, limits map[string]Limits) *Service {
	return NewWithPolicies(db, limits, nil)
}

func NewWithPolicies(db *gorm.DB, limits map[string]Limits, resources map[string]Resources) *Service {
	return &Service{db: db, limits: limits, resources: resources}
}

func (s *Service) Create(ctx context.Context, ownerID int64, name, templateName string) (Workspace, Build, error) {
	return s.create(ctx, ownerID, name, templateName, nil)
}

func (s *Service) create(ctx context.Context, ownerID int64, name, templateName string, repository *Repository) (Workspace, Build, error) {
	if len(name) > 120 {
		return Workspace{}, Build{}, errors.New("display name is too long")
	}
	for range 20 {
		var ws Workspace
		var build Build
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var err error
			if err = s.checkCreateQuota(tx, ownerID, templateName); err != nil {
				return err
			}
			ws, build, err = createInTx(tx, ownerID, name, templateName, repository)
			return err
		})
		if err == nil {
			return ws, build, nil
		}
		if !errors.Is(err, gorm.ErrDuplicatedKey) {
			return Workspace{}, Build{}, err
		}
	}
	return Workspace{}, Build{}, errors.New("workspace name space exhausted; retry later")
}

// CreateIdempotent returns the original response when a client repeats a
// creation request with the same key. The database primary key prevents the
// check-then-create race between concurrent browser navigations.
func (s *Service) CreateIdempotent(ctx context.Context, ownerID int64, key, requestHash, name, templateName string) (Workspace, Build, error) {
	return s.CreateRepositoryIdempotent(ctx, ownerID, key, requestHash, name, templateName, nil)
}

func (s *Service) CreateRepositoryIdempotent(ctx context.Context, ownerID int64, key, requestHash, name, templateName string, repository *Repository) (Workspace, Build, error) {
	if key == "" {
		return s.create(ctx, ownerID, name, templateName, repository)
	}
	if len(key) > 200 {
		return Workspace{}, Build{}, errors.New("idempotency key is too long")
	}
	for range 20 {
		var ws Workspace
		var build Build
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			now := time.Now()
			if err := tx.Exec("DELETE FROM idempotency_keys WHERE owner_id = ? AND key = ? AND expires_at <= ?", ownerID, key, now).Error; err != nil {
				return err
			}
			var existing struct {
				WorkspaceID string `gorm:"column:response_workspace_id"`
				RequestHash string `gorm:"column:request_hash"`
			}
			if err := tx.Raw("SELECT response_workspace_id, request_hash FROM idempotency_keys WHERE owner_id = ? AND key = ? AND expires_at > ?", ownerID, key, now).Scan(&existing).Error; err != nil {
				return err
			}
			if existing.WorkspaceID != "" {
				if existing.RequestHash != requestHash {
					return errors.New("idempotency key was already used with a different request")
				}
				if err := tx.Where("id = ? AND owner_id = ?", existing.WorkspaceID, ownerID).First(&ws).Error; err != nil {
					return err
				}
				return tx.Where("workspace_id = ?", ws.ID).Order("sequence ASC").First(&build).Error
			}
			var err error
			if err = s.checkCreateQuota(tx, ownerID, templateName); err != nil {
				return err
			}
			ws, build, err = createInTx(tx, ownerID, name, templateName, repository)
			if err != nil {
				return err
			}
			return tx.Exec("INSERT INTO idempotency_keys(owner_id, key, request_hash, response_workspace_id, expires_at) VALUES (?, ?, ?, ?, ?)", ownerID, key, requestHash, ws.ID, now.Add(10*time.Minute)).Error
		})
		if err == nil {
			return ws, build, nil
		}
		if !errors.Is(err, gorm.ErrDuplicatedKey) {
			return Workspace{}, Build{}, err
		}
	}
	return Workspace{}, Build{}, errors.New("idempotency retry limit exceeded")
}

func createInTx(tx *gorm.DB, ownerID int64, name, templateName string, repository *Repository) (Workspace, Build, error) {
	id, err := petName()
	if err != nil {
		return Workspace{}, Build{}, err
	}
	ws := Workspace{ID: id, OwnerID: ownerID, SourceType: "empty", TemplateName: templateName, DesiredState: "running", ObservedState: "pending", StateKey: "workspaces/" + id + "/terraform.tfstate"}
	if repository != nil {
		if repository.ID < 1 || repository.FullName == "" {
			return Workspace{}, Build{}, errors.New("repository metadata is invalid")
		}
		ws.SourceType = "github"
		ws.RepositoryID = &repository.ID
		ws.RepositoryFullName = &repository.FullName
	}
	if name != "" {
		ws.DisplayName = &name
	}
	if err := tx.Create(&ws).Error; err != nil {
		return Workspace{}, Build{}, err
	}
	build := Build{ID: randomID(), WorkspaceID: id, Sequence: 1, Transition: "start", TemplateName: templateName, Status: "queued"}
	if err := tx.Create(&build).Error; err != nil {
		return Workspace{}, Build{}, err
	}
	return ws, build, nil
}

func (s *Service) List(ctx context.Context, ownerID int64) ([]Workspace, error) {
	var out []Workspace
	return out, s.db.WithContext(ctx).Where("owner_id = ?", ownerID).Order("created_at DESC").Find(&out).Error
}
func (s *Service) Get(ctx context.Context, ownerID int64, id string) (Workspace, error) {
	if !idPattern.MatchString(id) {
		return Workspace{}, gorm.ErrRecordNotFound
	}
	var ws Workspace
	return ws, s.db.WithContext(ctx).Where("id = ? AND owner_id = ?", id, ownerID).First(&ws).Error
}

func (s *Service) Builds(ctx context.Context, ownerID int64, workspaceID string) ([]Build, error) {
	if _, err := s.Get(ctx, ownerID, workspaceID); err != nil {
		return nil, err
	}
	var builds []Build
	err := s.db.WithContext(ctx).Where("workspace_id = ?", workspaceID).Order("sequence DESC").Find(&builds).Error
	return builds, err
}

func (s *Service) OwnsBuild(ctx context.Context, ownerID int64, buildID string) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Raw("SELECT COUNT(*) FROM workspace_builds b JOIN workspaces w ON w.id = b.workspace_id WHERE b.id = ? AND w.owner_id = ?", buildID, ownerID).Scan(&count).Error
	return count == 1, err
}

func (s *Service) Touch(ctx context.Context, ownerID int64, id string) error {
	result := s.db.WithContext(ctx).Exec("UPDATE workspaces SET last_access_at = ?, updated_at = ? WHERE id = ? AND owner_id = ? AND deleted_at IS NULL", time.Now(), time.Now(), id, ownerID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (s *Service) StopIdle(ctx context.Context, before time.Time) error {
	var candidates []struct {
		ID      string
		OwnerID int64
	}
	if err := s.db.WithContext(ctx).Raw("SELECT id, owner_id FROM workspaces WHERE desired_state = 'running' AND observed_state IN ('running', 'degraded') AND deleted_at IS NULL AND last_access_at < ? ORDER BY last_access_at LIMIT 100", before).Scan(&candidates).Error; err != nil {
		return err
	}
	for _, workspace := range candidates {
		if _, err := s.Transition(ctx, workspace.OwnerID, workspace.ID, "stop"); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			if err.Error() == "workspace already has an active build" {
				continue
			}
			return err
		}
	}
	return nil
}

func (s *Service) Transition(ctx context.Context, ownerID int64, id, transition string) (Build, error) {
	if transition != "start" && transition != "stop" && transition != "delete" {
		return Build{}, errors.New("invalid transition")
	}
	var b Build
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ws Workspace
		if err := tx.Where("id = ? AND owner_id = ?", id, ownerID).First(&ws).Error; err != nil {
			return err
		}
		if ws.DeletedAt.Valid {
			return errors.New("workspace is deleted")
		}
		if transition == "start" {
			if err := s.checkStartQuota(tx, ownerID, ws.ID, ws.TemplateName); err != nil {
				return err
			}
		}
		desired, observed := "running", "starting"
		if transition == "stop" {
			desired, observed = "stopped", "stopping"
		}
		if transition == "delete" {
			desired, observed = "deleted", "deleting"
		}
		var active int64
		if err := tx.Model(&Build{}).Where("workspace_id = ? AND status IN ('queued','running')", id).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return errors.New("workspace already has an active build")
		}
		if err := tx.Model(&Workspace{}).Where("id = ?", id).Updates(map[string]any{"desired_state": desired, "observed_state": observed, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		var sequence int
		tx.Raw("SELECT COALESCE(MAX(sequence), 0) + 1 FROM workspace_builds WHERE workspace_id = ?", id).Scan(&sequence)
		b = Build{ID: randomID(), WorkspaceID: id, Sequence: sequence, Transition: transition, TemplateName: ws.TemplateName, Status: "queued"}
		return tx.Create(&b).Error
	})
	return b, err
}

func (s *Service) Usage(ctx context.Context, ownerID int64) ([]Usage, error) {
	var rows []struct {
		TemplateName string
		Workspaces   int64
		Running      int64
	}
	err := s.db.WithContext(ctx).Raw(`SELECT template_name, COUNT(*) AS workspaces, SUM(CASE WHEN desired_state = 'running' THEN 1 ELSE 0 END) AS running FROM workspaces WHERE owner_id = ? AND deleted_at IS NULL GROUP BY template_name`, ownerID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	byTemplate := make(map[string]struct {
		Workspaces int64
		Running    int64
	}, len(rows))
	for _, row := range rows {
		byTemplate[row.TemplateName] = struct {
			Workspaces int64
			Running    int64
		}{Workspaces: row.Workspaces, Running: row.Running}
	}
	names := make([]string, 0, len(s.limits))
	for name := range s.limits {
		names = append(names, name)
	}
	for name := range byTemplate {
		if _, known := s.limits[name]; !known {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := make([]Usage, 0, len(names))
	for _, name := range names {
		row := byTemplate[name]
		result = append(result, Usage{TemplateName: name, Workspaces: row.Workspaces, RunningWorkspaces: row.Running, Limits: s.limit(name)})
	}
	return result, nil
}

func (s *Service) checkCreateQuota(tx *gorm.DB, ownerID int64, templateName string) error {
	limit := s.limit(templateName)
	var count int64
	if err := tx.Model(&Workspace{}).Where("owner_id = ? AND template_name = ? AND deleted_at IS NULL", ownerID, templateName).Count(&count).Error; err != nil {
		return err
	}
	if limit.MaxWorkspaces > 0 && count >= int64(limit.MaxWorkspaces) {
		return errors.New("workspace quota exceeded")
	}
	var running int64
	if err := tx.Model(&Workspace{}).Where("owner_id = ? AND template_name = ? AND desired_state = 'running' AND deleted_at IS NULL", ownerID, templateName).Count(&running).Error; err != nil {
		return err
	}
	if limit.MaxRunningWorkspaces > 0 && running >= int64(limit.MaxRunningWorkspaces) {
		return errors.New("running workspace quota exceeded")
	}
	return nil
}

func (s *Service) checkStartQuota(tx *gorm.DB, ownerID int64, workspaceID, templateName string) error {
	limit := s.limit(templateName)
	if limit.MaxRunningWorkspaces == 0 {
		return nil
	}
	var running int64
	if err := tx.Model(&Workspace{}).Where("owner_id = ? AND template_name = ? AND id <> ? AND desired_state = 'running' AND deleted_at IS NULL", ownerID, templateName, workspaceID).Count(&running).Error; err != nil {
		return err
	}
	if running >= int64(limit.MaxRunningWorkspaces) {
		return errors.New("running workspace quota exceeded")
	}
	return nil
}

func (s *Service) limit(templateName string) Limits       { return s.limits[templateName] }
func (s *Service) resource(templateName string) Resources { return s.resources[templateName] }

func petName() (string, error) {
	a, err := randomIndex(len(adjectives))
	if err != nil {
		return "", err
	}
	b, err := randomIndex(len(adjectives))
	if err != nil {
		return "", err
	}
	n, err := randomIndex(len(nouns))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%s", adjectives[a], adjectives[b], nouns[n]), nil
}
func randomIndex(n int) (int, error) {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int(b[0]) % n, nil
}
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func RandomID() string {
	return randomID()
}
