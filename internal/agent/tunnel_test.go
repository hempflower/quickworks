package agent

import "testing"

func TestWebsocketURL(t *testing.T) {
	address, err := websocketURL("https://control.example/base", "agent id")
	if err != nil {
		t.Fatal(err)
	}
	expected := "wss://control.example/base/api/agent/connect?agent_id=agent+id"
	if address != expected {
		t.Fatalf("unexpected tunnel URL: %q", address)
	}
	if _, err := websocketURL("ftp://control.example", "agent"); err == nil {
		t.Fatal("expected unsupported scheme rejection")
	}
}
