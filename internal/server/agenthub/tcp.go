package agenthub

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/hashicorp/yamux"
)

// TCPHub stores agent-initiated multiplexed TCP sessions. Each virtual stream
// is a net.Conn suitable for http.Transport.DialContext.
type TCPHub struct {
	mu       sync.Mutex
	sessions map[string]*yamux.Session
}

func NewTCPHub() *TCPHub {
	return &TCPHub{sessions: make(map[string]*yamux.Session)}
}

func (h *TCPHub) Connect(agentID string, session *yamux.Session) {
	h.mu.Lock()
	previous := h.sessions[agentID]
	h.sessions[agentID] = session
	h.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
}

func (h *TCPHub) Disconnect(agentID string, session *yamux.Session) {
	h.mu.Lock()
	if h.sessions[agentID] == session {
		delete(h.sessions, agentID)
	}
	h.mu.Unlock()
	_ = session.Close()
}

func (h *TCPHub) DialContext(ctx context.Context, agentID string) (net.Conn, error) {
	h.mu.Lock()
	session := h.sessions[agentID]
	h.mu.Unlock()
	if session == nil {
		return nil, errors.New("workspace agent TCP tunnel is unavailable")
	}
	stream, err := session.OpenStream()
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	return stream, nil
}
