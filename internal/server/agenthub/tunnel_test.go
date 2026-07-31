package agenthub

import (
	"context"
	"testing"
	"time"

	protocol "github.com/evanxiao/quickworks/internal/protocol/agent"
)

func TestTunnelRequestResponse(t *testing.T) {
	tunnels := NewTunnels()
	connection := tunnels.Connect("agent-1")
	done := make(chan struct{})
	go func() {
		request := <-connection.Outbound
		connection.Deliver(protocol.Frame{Type: protocol.Response, ID: request.ID, Status: 200, Body: []byte("ok")})
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := tunnels.Request(ctx, "agent-1", protocol.Frame{Type: protocol.Request, ID: "request-1"})
	if err != nil || response.Status != 200 || string(response.Body) != "ok" {
		t.Fatalf("unexpected tunnel response: %#v, %v", response, err)
	}
	<-done
}

func TestTunnelDisconnectUnblocksRequest(t *testing.T) {
	tunnels := NewTunnels()
	connection := tunnels.Connect("agent-1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := tunnels.Request(ctx, "agent-1", protocol.Frame{Type: protocol.Request, ID: "request-1"})
		result <- err
	}()
	<-connection.Outbound
	tunnels.Disconnect("agent-1", connection)
	if err := <-result; err == nil {
		t.Fatal("expected disconnected request to fail")
	}
}

func TestTunnelWebSocketStream(t *testing.T) {
	tunnels := NewTunnels()
	connection := tunnels.Connect("agent-1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	opened := make(chan struct{})
	go func() {
		frame := <-connection.Outbound
		if frame.Type != protocol.WebSocketOpen || frame.ID != "socket-1" {
			t.Errorf("unexpected open frame: %#v", frame)
			return
		}
		connection.Deliver(protocol.Frame{Type: protocol.WebSocketOpened, ID: frame.ID, Status: 101})
		close(opened)
	}()
	returnedConnection, inbound, err := tunnels.OpenWebSocket(ctx, "agent-1", protocol.Frame{Type: protocol.WebSocketOpen, ID: "socket-1"})
	if err != nil || returnedConnection != connection {
		t.Fatalf("unexpected WebSocket open result: %v", err)
	}
	<-opened
	connection.Deliver(protocol.Frame{Type: protocol.WebSocketMessage, ID: "socket-1", MessageType: "text", Body: []byte("hello")})
	message := <-inbound
	if message.Type != protocol.WebSocketMessage || string(message.Body) != "hello" {
		t.Fatalf("unexpected upstream message: %#v", message)
	}
	if err := returnedConnection.Send(protocol.Frame{Type: protocol.WebSocketMessage, ID: "socket-1", Body: []byte("back")}); err != nil {
		t.Fatal(err)
	}
	if frame := <-connection.Outbound; frame.Type != protocol.WebSocketMessage || string(frame.Body) != "back" {
		t.Fatalf("unexpected downstream frame: %#v", frame)
	}
	returnedConnection.CloseWebSocket("socket-1", protocol.Frame{Type: protocol.WebSocketClose, ID: "socket-1"})
	if frame := <-connection.Outbound; frame.Type != protocol.WebSocketClose {
		t.Fatalf("unexpected close frame: %#v", frame)
	}
	if _, ok := <-inbound; ok {
		t.Fatal("WebSocket inbound stream remained open")
	}
}
