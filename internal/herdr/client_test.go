package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// A short temporary directory keeps Unix socket paths below platform limits.
// Cleanup closes every connection and waits for every server goroutine.
func fakeServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr-test-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	var mu sync.Mutex
	connections := make(map[net.Conn]bool)
	var workers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() {
					conn.Close()
					mu.Lock()
					delete(connections, conn)
					mu.Unlock()
				}()
				handle(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-accepted
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
		os.RemoveAll(dir)
	})
	return path
}

func receiveRequest(t *testing.T, conn net.Conn) (testRequest, bool) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var request testRequest
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		t.Errorf("fake server decoding request: %v", err)
		return request, false
	}
	conn.SetReadDeadline(time.Time{})
	if request.ID == "" || request.Method == "" || len(request.Params) == 0 {
		t.Errorf("incomplete request: %+v", request)
		return request, false
	}
	return request, true
}

func sendResult(t *testing.T, conn net.Conn, id string, result any) bool {
	t.Helper()
	if err := json.NewEncoder(conn).Encode(map[string]any{"id": id, "result": result}); err != nil {
		t.Errorf("fake server encoding response: %v", err)
		return false
	}
	return true
}

func nextEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("event channel closed before expected event")
		}
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func streamError(t *testing.T, errs <-chan error) error {
	t.Helper()
	select {
	case err, ok := <-errs:
		if !ok || err == nil {
			t.Fatal("stream closed without reporting its error")
		}
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for stream error")
		return nil
	}
}

func assertChannelsClosed(t *testing.T, events <-chan Event, errs <-chan error) {
	t.Helper()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected remaining event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event channel did not close")
	}
	select {
	case _, ok := <-errs:
		if ok {
			t.Fatal("unexpected remaining error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("error channel did not close")
	}
}

func TestAgentID(t *testing.T) {
	for _, test := range []struct {
		name  string
		agent Agent
		want  string
	}{
		{"terminal preferred", Agent{TerminalID: "terminal-1", PaneID: "pane-1"}, "terminal-1"},
		{"pane fallback", Agent{PaneID: "terminal-1"}, "pane:terminal-1"},
		{"empty fallback", Agent{}, "pane:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.agent.ID(); got != test.want {
				t.Fatalf("ID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSnapshotWrapper(t *testing.T) {
	want := Snapshot{Agents: []Agent{{
		PaneID: "pane-1", TerminalID: "terminal-1", Status: "working", Focused: true,
		WorkspaceID: "workspace-1", TabID: "tab-1", Name: "build", Agent: "claude",
	}}, FocusedPaneID: "pane-1"}
	path := fakeServer(t, func(conn net.Conn) {
		request, ok := receiveRequest(t, conn)
		if !ok {
			return
		}
		if request.Method != "session.snapshot" || string(request.Params) != "{}" {
			t.Errorf("unexpected snapshot request: %+v", request)
		}
		// The response type is informational and is intentionally not enforced.
		sendResult(t, conn, request.ID, map[string]any{"type": "future_snapshot_type", "snapshot": want})
	})
	got, err := (Client{SocketPath: path}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func TestOrdinaryRPCsUseFreshConnectionsAndFocusPane(t *testing.T) {
	var connections atomic.Int32
	closed := make(chan error, 3)
	path := fakeServer(t, func(conn net.Conn) {
		connections.Add(1)
		request, ok := receiveRequest(t, conn)
		if !ok {
			return
		}
		switch request.Method {
		case "session.snapshot":
			sendResult(t, conn, request.ID, map[string]any{"type": "session_snapshot", "snapshot": Snapshot{Agents: []Agent{}}})
		case "pane.focus":
			var params map[string]string
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Errorf("invalid focus params: %v", err)
			}
			if !reflect.DeepEqual(params, map[string]string{"pane_id": "pane-42"}) {
				t.Errorf("focus params = %v, want only pane_id", params)
			}
			sendResult(t, conn, request.ID, map[string]string{"type": "pane_focused"})
		default:
			t.Errorf("unexpected method %q", request.Method)
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		var extra [1]byte
		_, err := conn.Read(extra[:])
		closed <- err
	})
	client := Client{SocketPath: path}
	for i := 0; i < 2; i++ {
		if _, err := client.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Focus(context.Background(), "pane-42"); err != nil {
		t.Fatal(err)
	}
	if got := connections.Load(); got != 3 {
		t.Fatalf("connections = %d, want 3", got)
	}
	for i := 0; i < 3; i++ {
		select {
		case err := <-closed:
			if !errors.Is(err, io.EOF) {
				t.Errorf("ordinary connection was not closed: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ordinary connection did not close")
		}
	}
}

func TestAPIErrors(t *testing.T) {
	for _, method := range []string{"snapshot", "focus", "subscribe"} {
		t.Run(method, func(t *testing.T) {
			path := fakeServer(t, func(conn net.Conn) {
				request, ok := receiveRequest(t, conn)
				if ok {
					json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "error": APIError{Code: "invalid_target", Message: "invalid target"}})
				}
			})
			client := Client{SocketPath: path}
			var err error
			switch method {
			case "snapshot":
				_, err = client.Snapshot(context.Background())
			case "focus":
				err = client.Focus(context.Background(), "missing")
			case "subscribe":
				var events <-chan Event
				var errs <-chan error
				events, errs, err = client.Subscribe(context.Background())
				if events != nil || errs != nil {
					t.Fatal("failed handshake returned channels")
				}
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != "invalid_target" || apiErr.Message != "invalid target" {
				t.Fatalf("error = %v, want API error invalid_target", err)
			}
		})
	}
}

func TestSnapshotProtocolErrors(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame string
		isErr error
	}{
		{"malformed", "{bad json}\n", nil},
		{"truncated", `{"id":"1","result":`, io.ErrUnexpectedEOF},
		{"disconnect", "", io.EOF},
		{"missing newline", `{"id":"1","result":{"snapshot":{"agents":[]}}}`, io.ErrUnexpectedEOF},
		{"missing snapshot wrapper", "{\"id\":\"1\",\"result\":{\"agents\":[]}}\n", nil},
		{"null snapshot", "{\"id\":\"1\",\"result\":{\"snapshot\":null}}\n", nil},
		{"null result", "{\"id\":\"1\",\"result\":null}\n", nil},
		{"missing agents", "{\"id\":\"1\",\"result\":{\"snapshot\":{}}}\n", nil},
		{"null agents", "{\"id\":\"1\",\"result\":{\"snapshot\":{\"agents\":null}}}\n", nil},
		{"invalid agent", "{\"id\":\"1\",\"result\":{\"snapshot\":{\"agents\":[{\"pane_id\":3}]}}}\n", nil},
		{"wrong id", "{\"id\":\"2\",\"result\":{}}\n", nil},
		{"missing id", "{\"result\":{}}\n", nil},
		{"null id", "{\"id\":null,\"result\":{}}\n", nil},
		{"missing result", "{\"id\":\"1\"}\n", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := fakeServer(t, func(conn net.Conn) {
				if _, ok := receiveRequest(t, conn); ok {
					io.WriteString(conn, test.frame)
				}
			})
			got, err := (Client{SocketPath: path}).Snapshot(context.Background())
			if err == nil {
				t.Fatal("invalid response succeeded")
			}
			if !reflect.DeepEqual(got, Snapshot{}) {
				t.Errorf("failed snapshot returned nonzero value: %+v", got)
			}
			if test.isErr != nil && !errors.Is(err, test.isErr) {
				t.Fatalf("error = %v, want %v", err, test.isErr)
			}
		})
	}
}

func TestSubscribeAcknowledgementThenEvent(t *testing.T) {
	path := fakeServer(t, func(conn net.Conn) {
		request, ok := receiveRequest(t, conn)
		if !ok {
			return
		}
		if request.Method != "events.subscribe" {
			t.Errorf("method = %q, want events.subscribe", request.Method)
		}
		var params struct {
			Subscriptions []map[string]string `json:"subscriptions"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Errorf("invalid subscription params: %v", err)
		}
		want := []map[string]string{
			{"type": "pane.focused"}, {"type": "pane.closed"}, {"type": "pane.moved"},
			{"type": "pane.agent_detected"}, {"type": "pane.exited"},
		}
		if !reflect.DeepEqual(params.Subscriptions, want) {
			t.Errorf("subscriptions = %v, want %v", params.Subscriptions, want)
		}
		// Coalescing ack and event exercises buffered bytes left by the handshake.
		fmt.Fprintf(conn, "{\"id\":%q,\"result\":{\"type\":\"subscription_started\"}}\n{\"event\":\"pane.focused\",\"data\":{\"pane_id\":\"pane-1\",\"workspace_id\":\"ws-1\"}}\n", request.ID)
		io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errs, err := (Client{SocketPath: path}).Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cap(events) != 512 {
		t.Fatalf("event capacity = %d, want 512", cap(events))
	}
	event := nextEvent(t, events)
	if event.Event != "pane.focused" || string(event.Data) != `{"pane_id":"pane-1","workspace_id":"ws-1"}` {
		t.Fatalf("event = %+v", event)
	}
	cancel()
	if err := streamError(t, errs); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	assertChannelsClosed(t, events, errs)
}

func TestSubscribeWaitsForAcknowledgement(t *testing.T) {
	requestReceived := make(chan struct{})
	allowAck := make(chan struct{})
	// Release a blocked handler even if the test fails before sending the ack.
	var release sync.Once
	path := fakeServer(t, func(conn net.Conn) {
		request, ok := receiveRequest(t, conn)
		if !ok {
			return
		}
		close(requestReceived)
		<-allowAck
		sendResult(t, conn, request.ID, map[string]string{"type": "subscription_started"})
		io.Copy(io.Discard, conn)
	})
	t.Cleanup(func() { release.Do(func() { close(allowAck) }) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		events <-chan Event
		errs   <-chan error
		err    error
	}
	returned := make(chan result, 1)
	go func() {
		events, errs, err := (Client{SocketPath: path}).Subscribe(ctx)
		returned <- result{events, errs, err}
	}()
	select {
	case <-requestReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription request was not sent")
	}
	select {
	case got := <-returned:
		t.Fatalf("Subscribe returned before acknowledgement: %v", got.err)
	default:
	}
	release.Do(func() { close(allowAck) })
	select {
	case got := <-returned:
		if got.err != nil {
			t.Fatal(got.err)
		}
		cancel()
		if err := streamError(t, got.errs); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		assertChannelsClosed(t, got.events, got.errs)
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after acknowledgement")
	}
}

func TestSubscriptionSendsNothingElseAndHasNoReadTimeout(t *testing.T) {
	path := fakeServer(t, func(conn net.Conn) {
		request, ok := receiveRequest(t, conn)
		if !ok || !sendResult(t, conn, request.ID, map[string]string{"type": "subscription_started"}) {
			return
		}
		// The quiet period exceeds the client's handshake timeout. Any ping or
		// other post-subscription request fails this test.
		conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		var extra [1]byte
		n, err := conn.Read(extra[:])
		var timeout net.Error
		if n != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Errorf("subscription connection sent data or closed early: n=%d err=%v", n, err)
			return
		}
		conn.SetReadDeadline(time.Time{})
		io.WriteString(conn, "{\"event\":\"pane.exited\",\"data\":{\"pane_id\":\"p\"}}\n")
		io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errs, err := (Client{SocketPath: path, Timeout: 100 * time.Millisecond}).Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := nextEvent(t, events); got.Event != "pane.exited" {
		t.Fatalf("event = %+v", got)
	}
	cancel()
	if err := streamError(t, errs); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertChannelsClosed(t, events, errs)
}

func TestSubscriptionWrappedEvents(t *testing.T) {
	path := fakeServer(t, func(conn net.Conn) {
		request, ok := receiveRequest(t, conn)
		if !ok || !sendResult(t, conn, request.ID, map[string]string{"type": "subscription_started"}) {
			return
		}
		io.WriteString(conn, "{\"event\":{\"event\":\"pane.closed\",\"data\":{\"n\":1}}}\n")
		io.WriteString(conn, "{\"result\":{\"event\":\"pane.moved\",\"data\":{\"n\":2}}}\n")
		io.WriteString(conn, "{\"result\":{\"event\":{\"event\":\"pane.agent_detected\",\"data\":{\"n\":3}}}}\n")
	})
	events, errs, err := (Client{SocketPath: path}).Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"pane.closed", "pane.moved", "pane.agent_detected"} {
		got := nextEvent(t, events)
		if got.Event != name || string(got.Data) != fmt.Sprintf(`{"n":%d}`, i+1) {
			t.Fatalf("event %d = %+v", i, got)
		}
	}
	if err := streamError(t, errs); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	assertChannelsClosed(t, events, errs)
}

func TestSubscriptionPreservesOrderWithoutDropping(t *testing.T) {
	const count = 1800
	path := fakeServer(t, func(conn net.Conn) {
		request, ok := receiveRequest(t, conn)
		if !ok || !sendResult(t, conn, request.ID, map[string]string{"type": "subscription_started"}) {
			return
		}
		var frames bytes.Buffer
		for i := 0; i < count; i++ {
			fmt.Fprintf(&frames, "{\"event\":\"pane.agent_status_changed\",\"data\":{\"n\":%d}}\n", i)
		}
		if _, err := conn.Write(frames.Bytes()); err != nil {
			t.Errorf("send event burst: %v", err)
		}
	})
	events, errs, err := (Client{SocketPath: path}).Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Let the reader fill its bounded queue before consuming the burst.
	<-time.After(20 * time.Millisecond)
	for i := 0; i < count; i++ {
		got := nextEvent(t, events)
		if got.Event != "pane.agent_status_changed" || string(got.Data) != fmt.Sprintf(`{"n":%d}`, i) {
			t.Fatalf("event %d = %+v", i, got)
		}
	}
	if err := streamError(t, errs); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	assertChannelsClosed(t, events, errs)
}

func TestSubscriptionStreamErrors(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame string
		isErr error
	}{
		{"disconnect", "", io.EOF},
		{"malformed", "{not json}\n", nil},
		{"truncated", `{"event":"pane.focused","data":`, io.ErrUnexpectedEOF},
		{"missing newline", `{"event":"pane.focused","data":{}}`, io.ErrUnexpectedEOF},
		{"missing event", "{\"data\":{}}\n", nil},
		{"missing data", "{\"event\":\"pane.closed\"}\n", nil},
		{"bad envelope", "{\"event\":{\"wrong\":true}}\n", nil},
		{"API error", "{\"error\":{\"code\":\"stream_failed\",\"message\":\"stream failed\"}}\n", nil},
		{"oversized", strings.Repeat("x", maxFrameBytes+1) + "\n", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := fakeServer(t, func(conn net.Conn) {
				request, ok := receiveRequest(t, conn)
				if ok && sendResult(t, conn, request.ID, map[string]string{"type": "subscription_started"}) {
					// Oversized frames may make the client close before the write ends.
					io.WriteString(conn, test.frame)
				}
			})
			events, errs, err := (Client{SocketPath: path}).Subscribe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := streamError(t, errs)
			if test.isErr != nil && !errors.Is(got, test.isErr) {
				t.Fatalf("stream error = %v, want %v", got, test.isErr)
			}
			if test.name == "API error" {
				var apiErr *APIError
				if !errors.As(got, &apiErr) || apiErr.Code != "stream_failed" {
					t.Fatalf("stream error = %v, want API error", got)
				}
			}
			assertChannelsClosed(t, events, errs)
		})
	}
}

func TestSubscribeHandshakeFailureClosesConnection(t *testing.T) {
	for _, frame := range []string{
		"{\"id\":\"1\",\"result\":{\"type\":\"wrong_type\"}}\n",
		"{\"id\":\"1\",\"result\":null}\n",
		"{not json}\n",
	} {
		t.Run(frame, func(t *testing.T) {
			closed := make(chan error, 1)
			path := fakeServer(t, func(conn net.Conn) {
				if _, ok := receiveRequest(t, conn); !ok {
					return
				}
				io.WriteString(conn, frame)
				conn.SetReadDeadline(time.Now().Add(time.Second))
				var b [1]byte
				_, err := conn.Read(b[:])
				closed <- err
			})
			events, errs, err := (Client{SocketPath: path}).Subscribe(context.Background())
			if err == nil || events != nil || errs != nil {
				t.Fatalf("invalid handshake: events=%v errs=%v err=%v", events, errs, err)
			}
			select {
			case err := <-closed:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("failed handshake did not close socket: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("failed handshake socket was not closed")
			}
		})
	}
}

func TestTimeouts(t *testing.T) {
	for _, method := range []string{"snapshot", "subscribe"} {
		t.Run(method, func(t *testing.T) {
			path := fakeServer(t, func(conn net.Conn) {
				if _, ok := receiveRequest(t, conn); ok {
					io.Copy(io.Discard, conn) // Never acknowledge; wait for client close.
				}
			})
			client := Client{SocketPath: path, Timeout: 40 * time.Millisecond}
			var err error
			if method == "snapshot" {
				_, err = client.Snapshot(context.Background())
			} else {
				var events <-chan Event
				var errs <-chan error
				events, errs, err = client.Subscribe(context.Background())
				if events != nil || errs != nil {
					t.Fatal("timed out handshake returned channels")
				}
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout error = %v, want deadline exceeded", err)
			}
		})
	}
	if got := (Client{}).timeout(); got != 3*time.Second {
		t.Fatalf("default timeout = %v", got)
	}
}

func TestContextCancellationDuringRequest(t *testing.T) {
	for _, method := range []string{"focus", "subscribe"} {
		t.Run(method, func(t *testing.T) {
			received := make(chan struct{})
			path := fakeServer(t, func(conn net.Conn) {
				if _, ok := receiveRequest(t, conn); ok {
					close(received)
					io.Copy(io.Discard, conn)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan error, 1)
			go func() {
				client := Client{SocketPath: path, Timeout: time.Minute}
				if method == "focus" {
					returned <- client.Focus(ctx, "p")
				} else {
					_, _, err := client.Subscribe(ctx)
					returned <- err
				}
			}()
			select {
			case <-received:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not reach server")
			}
			cancel()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("request cancellation error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not unblock request")
			}
		})
	}
}

func TestCancellationWithFullEventBuffer(t *testing.T) {
	path := fakeServer(t, func(conn net.Conn) {
		request, ok := receiveRequest(t, conn)
		if !ok || !sendResult(t, conn, request.ID, map[string]string{"type": "subscription_started"}) {
			return
		}
		var frames bytes.Buffer
		for i := 0; i < 900; i++ {
			fmt.Fprintf(&frames, "{\"event\":\"pane.focused\",\"data\":%d}\n", i)
		}
		conn.Write(frames.Bytes())
		io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errs, err := (Client{SocketPath: path}).Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for a full queue without consuming it. A timer bounds this test.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for len(events) != cap(events) {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("event buffer did not fill")
		}
	}
	cancel()
	if err := streamError(t, errs); !errors.Is(err, context.Canceled) {
		t.Fatalf("full-buffer cancellation error = %v", err)
	}
	for range events {
		// Cancellation closes the channel even with unconsumed queued events.
	}
	assertChannelsClosed(t, events, errs)
}

func TestConnectionFailures(t *testing.T) {
	client := Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	if _, err := client.Snapshot(context.Background()); err == nil {
		t.Fatal("missing socket snapshot succeeded")
	}
	if err := client.Focus(context.Background(), "p"); err == nil {
		t.Fatal("missing socket focus succeeded")
	}
	if events, errs, err := client.Subscribe(context.Background()); err == nil || events != nil || errs != nil {
		t.Fatalf("missing socket subscription = %v, %v, %v", events, errs, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-canceled request = %v", err)
	}
}

func TestFrameLimit(t *testing.T) {
	for _, size := range []int{maxFrameBytes, maxFrameBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			scanner := newFrameScanner(strings.NewReader(strings.Repeat(" ", size) + "\n"))
			frame, err := readFrame(scanner)
			if size == maxFrameBytes {
				if err != nil || len(frame) != size {
					t.Fatalf("maximum frame: len=%d err=%v", len(frame), err)
				}
			} else if err == nil {
				t.Fatal("oversized frame accepted")
			}
		})
	}
}
