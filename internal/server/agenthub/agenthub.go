// Package agenthub implements development workspace-agent enrollment.
package agenthub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

type Hub struct{ db *gorm.DB }

type Enrollment struct {
	AgentID string `json:"agent_id"`
	Token   string `json:"enrollment_token"`
}

type Registration struct {
	WorkspaceID string `json:"workspace_id"`
	BuildID     string `json:"build_id"`
	AgentID     string `json:"agent_id"`
	Session     string `json:"session"`
}

func New(db *gorm.DB) *Hub { return &Hub{db: db} }

func (h *Hub) CreateEnrollment(ctx context.Context, workspaceID, buildID string) (Enrollment, error) {
	token, err := secret()
	if err != nil {
		return Enrollment{}, err
	}
	enrollment := Enrollment{AgentID: randomID(), Token: token}
	err = h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("INSERT INTO workspace_agents(id, workspace_id, build_id, public_key, status) VALUES (?, ?, ?, ?, 'starting')", enrollment.AgentID, workspaceID, buildID, []byte{}).Error; err != nil {
			return err
		}
		return tx.Exec("INSERT INTO agent_enrollments(id_hash, workspace_id, build_id, agent_id, expires_at) VALUES (?, ?, ?, ?, ?)", hash(token), workspaceID, buildID, enrollment.AgentID, time.Now().AddDate(1, 0, 0)).Error
	})
	return enrollment, err
}

func (h *Hub) Register(ctx context.Context, token string, publicKey []byte) (Registration, error) {
	if len(publicKey) != 32 {
		return Registration{}, errors.New("agent public key must be Ed25519")
	}
	var result Registration
	session, err := secret()
	if err != nil {
		return result, err
	}
	err = h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var enrollment struct {
			WorkspaceID string
			BuildID     string
			AgentID     string
		}
		if err := tx.Raw("SELECT workspace_id, build_id, agent_id FROM agent_enrollments WHERE id_hash = ? AND expires_at > ?", hash(token), time.Now()).Scan(&enrollment).Error; err != nil {
			return err
		}
		if enrollment.AgentID == "" {
			return errors.New("agent enrollment is invalid or expired")
		}
		expires := time.Now().Add(15 * time.Minute)
		if err := tx.Exec("UPDATE workspace_agents SET public_key = ?, status = 'starting', session_hash = ?, session_expires_at = ?, last_heartbeat_at = ?, connected_at = ? WHERE id = ?", publicKey, hash(session), expires, time.Now(), time.Now(), enrollment.AgentID).Error; err != nil {
			return err
		}
		result = Registration{WorkspaceID: enrollment.WorkspaceID, BuildID: enrollment.BuildID, AgentID: enrollment.AgentID, Session: session}
		return nil
	})
	return result, err
}

// AuthenticateSession verifies the short-lived credential used to establish an
// outbound WebSocket. It deliberately requires the agent ID as well as the
// opaque session, preventing a session from being used for another workspace.
func (h *Hub) AuthenticateSession(ctx context.Context, agentID, session string) error {
	if agentID == "" || session == "" {
		return errors.New("agent session is missing")
	}
	var count int64
	err := h.db.WithContext(ctx).Raw("SELECT COUNT(*) FROM workspace_agents WHERE id = ? AND session_hash = ? AND session_expires_at > ? AND status IN ('starting', 'ready', 'degraded')", agentID, hash(session), time.Now()).Scan(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("agent session is invalid or expired")
	}
	return nil
}

func (h *Hub) Heartbeat(ctx context.Context, agentID string) error {
	return h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var agent struct {
			WorkspaceID string
			Status      string
		}
		if err := tx.Raw("SELECT workspace_id, status FROM workspace_agents WHERE id = ?", agentID).Scan(&agent).Error; err != nil {
			return err
		}
		if agent.WorkspaceID == "" {
			return errors.New("agent no longer exists")
		}
		now := time.Now()
		result := tx.Exec("UPDATE workspace_agents SET last_heartbeat_at = ?, session_expires_at = ? WHERE id = ?", now, now.Add(15*time.Minute), agentID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("agent no longer exists")
		}
		if agent.Status == "starting" {
			return nil
		}
		if err := tx.Exec("UPDATE workspace_agents SET status = 'ready' WHERE id = ?", agentID).Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE workspaces SET observed_state = 'running', updated_at = ? WHERE id = ? AND desired_state = 'running' AND observed_state = 'degraded'", now, agent.WorkspaceID).Error
	})
}

// MarkReady records that the agent has started a healthy local Workbench.
func (h *Hub) MarkReady(ctx context.Context, agentID string) error {
	now := time.Now()
	return h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var agent struct{ WorkspaceID string }
		result := tx.Raw("UPDATE workspace_agents SET status = 'ready', last_heartbeat_at = ?, session_expires_at = ? WHERE id = ? RETURNING workspace_id", now, now.Add(15*time.Minute), agentID).Scan(&agent)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 || agent.WorkspaceID == "" {
			return errors.New("agent no longer exists")
		}
		return tx.Exec("UPDATE workspaces SET observed_state = 'running', updated_at = ? WHERE id = ? AND desired_state = 'running' AND observed_state IN ('starting', 'degraded')", now, agent.WorkspaceID).Error
	})
}

// MarkStale marks agents and otherwise-running workspaces degraded when no
// heartbeat arrived within the supplied deadline. Resource reconciliation is
// deliberately separate: a lost tunnel is not evidence that the VM vanished.
func (h *Hub) MarkStale(ctx context.Context, deadline time.Time) error {
	return h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var workspaceIDs []string
		if err := tx.Raw("SELECT workspace_id FROM workspace_agents WHERE status = 'ready' AND last_heartbeat_at < ?", deadline).Scan(&workspaceIDs).Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE workspace_agents SET status = 'degraded' WHERE status = 'ready' AND last_heartbeat_at < ?", deadline).Error; err != nil {
			return err
		}
		for _, workspaceID := range workspaceIDs {
			if err := tx.Exec("UPDATE workspaces SET observed_state = 'degraded', updated_at = ? WHERE id = ? AND desired_state = 'running' AND observed_state = 'running'", time.Now(), workspaceID).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (h *Hub) ActiveAgent(ctx context.Context, workspaceID string) (string, error) {
	var agentID string
	err := h.db.WithContext(ctx).Raw("SELECT id FROM workspace_agents WHERE workspace_id = ? AND status = 'ready' AND last_heartbeat_at > ? ORDER BY connected_at DESC LIMIT 1", workspaceID, time.Now().Add(-45*time.Second)).Scan(&agentID).Error
	if err != nil {
		return "", err
	}
	if agentID == "" {
		return "", errors.New("no ready agent")
	}
	return agentID, nil
}

func secret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate agent secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
func randomID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes)
}
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
