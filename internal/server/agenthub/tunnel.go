package agenthub

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/evanxiao/quickworks/internal/protocol/agent"
)

type Tunnels struct {
	mu          sync.Mutex
	connections map[string]*Connection
}

type Connection struct {
	Outbound chan agent.Frame
	mu       sync.Mutex
	pending  map[string]chan agent.Frame
	streams  map[string]chan agent.Frame
}

const responseQueueCapacity = 256

func NewTunnels() *Tunnels { return &Tunnels{connections: make(map[string]*Connection)} }

func (t *Tunnels) Connect(agentID string) *Connection {
	connection := &Connection{Outbound: make(chan agent.Frame, 32), pending: make(map[string]chan agent.Frame), streams: make(map[string]chan agent.Frame)}
	t.mu.Lock()
	previous := t.connections[agentID]
	t.connections[agentID] = connection
	t.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	return connection
}

func (t *Tunnels) Disconnect(agentID string, connection *Connection) {
	t.mu.Lock()
	if t.connections[agentID] == connection {
		delete(t.connections, agentID)
	}
	t.mu.Unlock()
	connection.Close()
}

func (t *Tunnels) Request(ctx context.Context, agentID string, frame agent.Frame) (agent.Frame, error) {
	t.mu.Lock()
	connection := t.connections[agentID]
	t.mu.Unlock()
	if connection == nil {
		return agent.Frame{}, errors.New("workspace agent is offline")
	}
	return connection.Request(ctx, frame)
}

func (t *Tunnels) OpenWebSocket(ctx context.Context, agentID string, frame agent.Frame) (*Connection, <-chan agent.Frame, error) {
	t.mu.Lock()
	connection := t.connections[agentID]
	t.mu.Unlock()
	if connection == nil {
		return nil, nil, errors.New("workspace agent is offline")
	}
	stream, err := connection.OpenWebSocket(ctx, frame)
	if err != nil {
		return nil, nil, err
	}
	return connection, stream, nil
}

func (c *Connection) Request(ctx context.Context, frame agent.Frame) (agent.Frame, error) {
	response := make(chan agent.Frame, responseQueueCapacity)
	c.mu.Lock()
	if c.pending == nil {
		c.mu.Unlock()
		return agent.Frame{}, errors.New("workspace agent disconnected")
	}
	c.pending[frame.ID] = response
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, frame.ID)
		c.mu.Unlock()
	}()
	select {
	case c.Outbound <- frame:
	case <-ctx.Done():
		return agent.Frame{}, ctx.Err()
	}
	var result agent.Frame
	for {
		select {
		case received, ok := <-response:
			if !ok {
				return agent.Frame{}, errors.New("workspace agent disconnected")
			}
			if result.Type == "" {
				result = received
			} else {
				result.Body = append(result.Body, received.Body...)
				if received.Error != "" {
					result.Error = received.Error
				}
			}
			if !received.More {
				return result, nil
			}
		case <-ctx.Done():
			return agent.Frame{}, ctx.Err()
		}
	}
}

func (c *Connection) Deliver(frame agent.Frame) {
	if frame.Type == agent.WebSocketMessage || frame.Type == agent.WebSocketClose {
		c.mu.Lock()
		stream := c.streams[frame.ID]
		c.mu.Unlock()
		if stream != nil {
			select {
			case stream <- frame:
			default:
			}
		}
		return
	}
	c.mu.Lock()
	response := c.pending[frame.ID]
	c.mu.Unlock()
	if response != nil {
		select {
		case response <- frame:
		case <-time.After(time.Second):
		}
	}
}

// OpenWebSocket opens an upstream browser WebSocket through the agent tunnel.
// The returned channel receives frames from the upstream workbench.
func (c *Connection) OpenWebSocket(ctx context.Context, frame agent.Frame) (<-chan agent.Frame, error) {
	response := make(chan agent.Frame, 1)
	stream := make(chan agent.Frame, 32)
	c.mu.Lock()
	if c.pending == nil {
		c.mu.Unlock()
		return nil, errors.New("workspace agent disconnected")
	}
	c.pending[frame.ID] = response
	c.streams[frame.ID] = stream
	c.mu.Unlock()
	cleanup := func() {
		c.mu.Lock()
		delete(c.pending, frame.ID)
		current, ok := c.streams[frame.ID]
		delete(c.streams, frame.ID)
		if ok && current == stream {
			close(stream)
		}
		c.mu.Unlock()
	}
	select {
	case c.Outbound <- frame:
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	}
	select {
	case received, ok := <-response:
		c.mu.Lock()
		delete(c.pending, frame.ID)
		c.mu.Unlock()
		if !ok || received.Type != agent.WebSocketOpened || received.Error != "" {
			cleanup()
			if received.Error != "" {
				return nil, errors.New(received.Error)
			}
			return nil, errors.New("workspace WebSocket open failed")
		}
		return stream, nil
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	}
}

func (c *Connection) Send(frame agent.Frame) error {
	c.mu.Lock()
	open := c.pending != nil
	c.mu.Unlock()
	if !open {
		return errors.New("workspace agent disconnected")
	}
	select {
	case c.Outbound <- frame:
		return nil
	default:
		return errors.New("workspace agent tunnel is congested")
	}
}

func (c *Connection) CloseWebSocket(id string, frame agent.Frame) {
	c.mu.Lock()
	stream := c.streams[id]
	_, open := c.streams[id]
	delete(c.streams, id)
	c.mu.Unlock()
	if open && stream != nil {
		close(stream)
	}
	_ = c.Send(frame)
}

func (c *Connection) Close() {
	c.mu.Lock()
	if c.pending == nil {
		c.mu.Unlock()
		return
	}
	for _, response := range c.pending {
		close(response)
	}
	for _, stream := range c.streams {
		close(stream)
	}
	c.pending = nil
	c.streams = nil
	c.mu.Unlock()
}
