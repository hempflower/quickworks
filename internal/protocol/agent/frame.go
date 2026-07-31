// Package agent defines the control-plane/agent WebSocket frame protocol.
package agent

const (
	MaxFrameSize    = 128 << 10
	MaxFramePayload = 64 << 10
)

type Frame struct {
	Type        string              `json:"type"`
	ID          string              `json:"id,omitempty"`
	Method      string              `json:"method,omitempty"`
	Path        string              `json:"path,omitempty"`
	Headers     map[string][]string `json:"headers,omitempty"`
	Body        []byte              `json:"body,omitempty"`
	More        bool                `json:"more,omitempty"`
	Status      int                 `json:"status,omitempty"`
	Error       string              `json:"error,omitempty"`
	MessageType string              `json:"message_type,omitempty"`
	CloseCode   int                 `json:"close_code,omitempty"`
	CloseReason string              `json:"close_reason,omitempty"`
}

const (
	Heartbeat        = "heartbeat"
	HeartbeatAck     = "heartbeat_ack"
	Request          = "request"
	Response         = "response"
	WebSocketOpen    = "websocket_open"
	WebSocketOpened  = "websocket_opened"
	WebSocketMessage = "websocket_message"
	WebSocketClose   = "websocket_close"
)
