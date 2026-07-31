package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	protocol "github.com/evanxiao/quickworks/internal/protocol/agent"
	"github.com/hashicorp/yamux"
	"nhooyr.io/websocket"
)

// MaintainTunnel establishes the authenticated outbound WebSocket and keeps
// the agent presence fresh. Stream multiplexing is intentionally layered above
// these authenticated heartbeat frames.
func MaintainTunnel(ctx context.Context, controlURL, agentID, session string) error {
	delay := time.Second
	for {
		err := maintainTCPTunnelOnce(ctx, controlURL, agentID, session)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay < time.Minute {
			delay *= 2
		}
	}
}

func maintainTCPTunnelOnce(ctx context.Context, controlURL, agentID, session string) error {
	tunnelURL, err := websocketURL(controlURL, agentID)
	if err != nil {
		return err
	}
	connection, _, err := websocket.Dial(ctx, tunnelURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + session}}})
	if err != nil {
		return err
	}
	defer connection.Close(websocket.StatusNormalClosure, "agent shutdown")
	mux, err := yamux.Client(websocket.NetConn(ctx, connection, websocket.MessageBinary), nil)
	if err != nil {
		return err
	}
	defer mux.Close()
	for {
		stream, err := mux.AcceptStreamWithContext(ctx)
		if err != nil {
			return err
		}
		go func() {
			defer stream.Close()
			upstream, err := net.Dial("tcp", "127.0.0.1:3000")
			if err != nil {
				return
			}
			defer upstream.Close()
			go io.Copy(upstream, stream)
			_, _ = io.Copy(stream, upstream)
		}()
	}
}

func maintainTunnelOnce(ctx context.Context, controlURL, agentID, session string) error {
	tunnelURL, err := websocketURL(controlURL, agentID)
	if err != nil {
		return err
	}
	connection, _, err := websocket.Dial(ctx, tunnelURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + session}}})
	if err != nil {
		return fmt.Errorf("connect agent tunnel: %w", err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "agent shutdown")
	connection.SetReadLimit(protocol.MaxFrameSize)
	tunnelContext, cancelTunnel := context.WithCancel(ctx)
	defer cancelTunnel()
	var writeMu sync.Mutex
	writeFrame := func(frame protocol.Frame) error {
		payload, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return connection.Write(tunnelContext, websocket.MessageText, payload)
	}
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	tickerDone := make(chan struct{})
	defer close(tickerDone)
	go func() {
		for {
			select {
			case <-tickerDone:
				return
			case <-ticker.C:
				if writeFrame(protocol.Frame{Type: protocol.Heartbeat}) != nil {
					cancelTunnel()
					return
				}
			}
		}
	}()
	if err := writeFrame(protocol.Frame{Type: protocol.Heartbeat}); err != nil {
		return err
	}
	streams := make(map[string]*websocket.Conn)
	webSocketBodies := make(map[string][]byte)
	var streamsMu sync.Mutex
	stream := func(id string) *websocket.Conn {
		streamsMu.Lock()
		defer streamsMu.Unlock()
		return streams[id]
	}
	removeStream := func(id string) *websocket.Conn {
		streamsMu.Lock()
		defer streamsMu.Unlock()
		upstream := streams[id]
		delete(streams, id)
		return upstream
	}
	defer func() {
		streamsMu.Lock()
		defer streamsMu.Unlock()
		for _, upstream := range streams {
			_ = upstream.Close(websocket.StatusGoingAway, "agent tunnel closed")
		}
	}()
	for {
		_, payload, err := connection.Read(tunnelContext)
		if err != nil {
			return err
		}
		var frame protocol.Frame
		if err := json.Unmarshal(payload, &frame); err != nil {
			return err
		}
		switch frame.Type {
		case protocol.HeartbeatAck:
			continue
		case protocol.Request:
			if frame.ID == "" {
				return fmt.Errorf("agent request ID is missing")
			}
			go forwardHTTPRequest(tunnelContext, frame, writeFrame)
		case protocol.WebSocketOpen:
			if frame.ID == "" {
				return fmt.Errorf("agent WebSocket ID is missing")
			}
			go forwardWebSocket(ctx, frame, writeFrame, func(upstream *websocket.Conn) bool {
				streamsMu.Lock()
				defer streamsMu.Unlock()
				if _, exists := streams[frame.ID]; exists {
					return false
				}
				streams[frame.ID] = upstream
				return true
			}, removeStream)
		case protocol.WebSocketMessage:
			upstream := stream(frame.ID)
			if upstream == nil {
				continue
			}
			body := append(webSocketBodies[frame.ID], frame.Body...)
			if frame.More {
				webSocketBodies[frame.ID] = body
				continue
			}
			delete(webSocketBodies, frame.ID)
			messageType := websocket.MessageText
			if frame.MessageType == "binary" {
				messageType = websocket.MessageBinary
			}
			if err := upstream.Write(ctx, messageType, body); err != nil {
				_ = writeFrame(protocol.Frame{Type: protocol.WebSocketClose, ID: frame.ID, CloseCode: int(websocket.CloseStatus(err)), CloseReason: "workbench WebSocket write failed"})
				if closed := removeStream(frame.ID); closed != nil {
					_ = closed.Close(websocket.StatusInternalError, "workbench WebSocket write failed")
				}
			}
		case protocol.WebSocketClose:
			if upstream := removeStream(frame.ID); upstream != nil {
				code := websocket.StatusCode(frame.CloseCode)
				if code == 0 {
					code = websocket.StatusNormalClosure
				}
				_ = upstream.Close(code, frame.CloseReason)
			}
		default:
			return fmt.Errorf("unexpected agent tunnel frame %q", frame.Type)
		}
	}
}

func forwardWebSocket(ctx context.Context, frame protocol.Frame, writeFrame func(protocol.Frame) error, add func(*websocket.Conn) bool, remove func(string) *websocket.Conn) {
	upstreamURL, err := upstreamWebSocketURL(frame.Path, frame.Headers)
	if err != nil {
		_ = writeFrame(protocol.Frame{Type: protocol.WebSocketOpened, ID: frame.ID, Error: "invalid workbench WebSocket request"})
		return
	}
	upstream, _, err := websocket.Dial(ctx, upstreamURL, &websocket.DialOptions{HTTPHeader: http.Header(frame.Headers)})
	if err != nil {
		_ = writeFrame(protocol.Frame{Type: protocol.WebSocketOpened, ID: frame.ID, Error: "workbench WebSocket upstream unavailable"})
		return
	}
	if !add(upstream) {
		_ = upstream.Close(websocket.StatusPolicyViolation, "duplicate WebSocket ID")
		_ = writeFrame(protocol.Frame{Type: protocol.WebSocketOpened, ID: frame.ID, Error: "duplicate workbench WebSocket"})
		return
	}
	if err := writeFrame(protocol.Frame{Type: protocol.WebSocketOpened, ID: frame.ID, Status: http.StatusSwitchingProtocols}); err != nil {
		if closed := remove(frame.ID); closed != nil {
			_ = closed.Close(websocket.StatusGoingAway, "control plane disconnected")
		}
		return
	}
	defer func() {
		if closed := remove(frame.ID); closed != nil {
			_ = closed.Close(websocket.StatusNormalClosure, "proxy closed")
		}
	}()
	for {
		messageType, body, err := upstream.Read(ctx)
		if err != nil {
			code := websocket.CloseStatus(err)
			if code < 0 {
				code = websocket.StatusInternalError
			}
			_ = writeFrame(protocol.Frame{Type: protocol.WebSocketClose, ID: frame.ID, CloseCode: int(code), CloseReason: "workbench WebSocket closed"})
			return
		}
		kind := "text"
		if messageType == websocket.MessageBinary {
			kind = "binary"
		}
		if err := writeWebSocketFrames(frame.ID, kind, body, writeFrame); err != nil {
			return
		}
	}
}

func forwardHTTPRequest(ctx context.Context, frame protocol.Frame, writeFrame func(protocol.Frame) error) {
	responseFrame := protocol.Frame{Type: protocol.Response, ID: frame.ID}
	path, err := upstreamPath(frame.Path, frame.Headers)
	if err != nil {
		responseFrame.Error = "invalid workbench request path"
		_ = writeFrame(responseFrame)
		return
	}
	request, err := http.NewRequestWithContext(ctx, frame.Method, "http://127.0.0.1:3000"+path, bytes.NewReader(frame.Body))
	if err != nil {
		responseFrame.Error = "invalid upstream request"
		_ = writeFrame(responseFrame)
		return
	}
	request.Header = http.Header(frame.Headers)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		responseFrame.Error = "workbench upstream unavailable"
		_ = writeFrame(responseFrame)
		return
	}
	defer response.Body.Close()
	responseFrame.Status = response.StatusCode
	responseFrame.Headers = map[string][]string(response.Header)
	buffer := make([]byte, protocol.MaxFramePayload)
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			responseFrame.Body = append(responseFrame.Body[:0], buffer[:count]...)
			responseFrame.More = true
			if readErr == io.EOF {
				responseFrame.More = false
			}
			if writeFrame(responseFrame) != nil {
				return
			}
			responseFrame.Headers = nil
		}
		if readErr == io.EOF {
			if count == 0 {
				responseFrame.More = false
				_ = writeFrame(responseFrame)
			}
			return
		}
		if readErr != nil {
			responseFrame.Body = nil
			responseFrame.More = false
			responseFrame.Error = "read workbench upstream response"
			_ = writeFrame(responseFrame)
			return
		}
	}
}

func writeWebSocketFrames(id, messageType string, body []byte, writeFrame func(protocol.Frame) error) error {
	if len(body) == 0 {
		return writeFrame(protocol.Frame{Type: protocol.WebSocketMessage, ID: id, MessageType: messageType})
	}
	for len(body) > 0 {
		count := min(len(body), protocol.MaxFramePayload)
		frame := protocol.Frame{Type: protocol.WebSocketMessage, ID: id, MessageType: messageType, Body: body[:count], More: len(body) > count}
		if err := writeFrame(frame); err != nil {
			return err
		}
		body = body[count:]
	}
	return nil
}

func websocketURL(controlURL, agentID string) (string, error) {
	parsed, err := url.Parse(controlURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported control URL scheme")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/agent/connect"
	query := parsed.Query()
	query.Set("agent_id", agentID)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func upstreamWebSocketURL(requestPath string, headers http.Header) (string, error) {
	path, err := upstreamPath(requestPath, headers)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return "", errors.New("invalid upstream WebSocket path")
	}
	parsed.Scheme = "ws"
	parsed.Host = "127.0.0.1:3000"
	return parsed.String(), nil
}

func upstreamPath(requestPath string, headers http.Header) (string, error) {
	parsed, err := url.Parse(requestPath)
	if err != nil || !strings.HasPrefix(parsed.Path, "/") {
		return "", errors.New("invalid workbench request path")
	}
	prefix := strings.TrimRight(headers.Get("X-Forwarded-Prefix"), "/")
	if prefix == "" {
		return parsed.RequestURI(), nil
	}
	if !strings.HasPrefix(parsed.Path, prefix) {
		return "", errors.New("workbench request is outside its forwarded prefix")
	}
	path := strings.TrimPrefix(parsed.Path, prefix)
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "", errors.New("invalid workbench request path")
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.RequestURI(), nil
}
