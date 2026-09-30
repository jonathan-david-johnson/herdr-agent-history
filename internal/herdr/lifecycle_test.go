package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestLifecycleSnakeCaseNormalization(t *testing.T) {
	for _, pair := range [][2]string{
		{"pane_focused", "pane.focused"},
		{"pane_agent_detected", "pane.agent_detected"},
		{"pane_agent_status_changed", "pane.agent_status_changed"},
		{"pane.focused", "pane.focused"},
		{"pane.agent_status_changed", "pane.agent_status_changed"},
		{"workspace_focused", "workspace.focused"},
		{"unknown_event", "unknown_event"},
	} {
		frame, _ := json.Marshal(map[string]any{"event": pair[0], "data": map[string]string{"type": "pane_focused", "pane_id": "w7:pE"}})
		event, err := decodeEvent(frame)
		if err != nil || event.Event != pair[1] {
			t.Fatalf("%s: %v %v", pair[0], event, err)
		}
	}
}

// Optional integration probe: reads live state and subscribes/cancels, but never
// focuses a pane, sends input, changes configuration, or acknowledges completion.
func TestReadOnlyLiveHerdr(t *testing.T) {
	path := os.Getenv("HERDR_HISTORY_LIVE_SOCKET")
	if path == "" {
		t.Skip("set HERDR_HISTORY_LIVE_SOCKET for read-only integration probe")
	}
	client := Client{SocketPath: path}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live snapshot: %d agents, focused pane %s", len(snapshot.Agents), snapshot.FocusedPaneID)
	ctx, cancel := context.WithCancel(context.Background())
	events, failures, err := client.Subscribe(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	for range events {
	}
	if err := streamError(t, failures); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
