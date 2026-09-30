// Package herdr implements the newline-delimited JSON API on Herdr's Unix socket.
package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	defaultTimeout = 3 * time.Second
	maxFrameBytes  = 8 * 1024 * 1024
	requestID      = "1"
)

type Agent struct {
	PaneID         string `json:"pane_id"`
	TerminalID     string `json:"terminal_id"`
	Status         string `json:"agent_status"`
	StateChangeSeq uint64 `json:"state_change_seq"`
	Focused        bool   `json:"focused"`
	WorkspaceID    string `json:"workspace_id"`
	TabID          string `json:"tab_id"`
	Name           string `json:"name"`
	Agent          string `json:"agent"`
}

func (a Agent) ID() string {
	if a.TerminalID != "" {
		return a.TerminalID
	}
	return "pane:" + a.PaneID
}

type Snapshot struct {
	Agents        []Agent `json:"agents"`
	FocusedPaneID string  `json:"focused_pane_id"`
}

type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type Client struct {
	SocketPath string
	Timeout    time.Duration
}

// APIError is an error returned by Herdr, rather than by the socket transport.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr API error %s: %s", e.Code, e.Message)
}

type response struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
}

func (c Client) Snapshot(ctx context.Context) (Snapshot, error) {
	result, err := c.call(ctx, "session.snapshot", struct{}{})
	if err != nil {
		return Snapshot{}, err
	}
	var wrapper struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil {
		return Snapshot{}, fmt.Errorf("session.snapshot: decode result: %w", err)
	}
	if !isObject(wrapper.Snapshot) {
		return Snapshot{}, errors.New("session.snapshot: missing or invalid snapshot object")
	}
	var fields struct {
		Agents json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(wrapper.Snapshot, &fields); err != nil {
		return Snapshot{}, fmt.Errorf("session.snapshot: decode snapshot: %w", err)
	}
	if raw := bytes.TrimSpace(fields.Agents); len(raw) == 0 || raw[0] != '[' {
		return Snapshot{}, errors.New("session.snapshot: missing or invalid agents array")
	}
	var snapshot Snapshot
	if err := json.Unmarshal(wrapper.Snapshot, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("session.snapshot: decode snapshot: %w", err)
	}
	return snapshot, nil
}

func (c Client) Focus(ctx context.Context, paneID string) error {
	_, err := c.call(ctx, "pane.focus", struct {
		PaneID string `json:"pane_id"`
	}{PaneID: paneID})
	return err
}

func (c Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

func (c Client) connect(ctx context.Context) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to Herdr: %w", contextError(ctx, err))
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return nil, fmt.Errorf("set Herdr deadline: %w", err)
		}
	}
	return conn, nil
}

func (c Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	conn, err := c.connect(callCtx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(callCtx, func() { conn.Close() })
	defer stop()
	if err := writeRequest(conn, method, params); err != nil {
		return nil, fmt.Errorf("%s: send request: %w", method, contextError(callCtx, err))
	}
	result, err := readResponse(newFrameScanner(conn))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", method, contextError(callCtx, err))
	}
	if err := callCtx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return result, nil
}

func writeRequest(conn net.Conn, method string, params any) error {
	return json.NewEncoder(conn).Encode(struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{ID: requestID, Method: method, Params: params})
}

func readResponse(scanner *bufio.Scanner) (json.RawMessage, error) {
	frame, err := readFrame(scanner)
	if err != nil {
		return nil, err
	}
	var reply response
	if err := json.Unmarshal(frame, &reply); err != nil {
		return nil, fmt.Errorf("invalid response JSON: %w", err)
	}
	var id string
	if len(reply.ID) == 0 || bytes.Equal(bytes.TrimSpace(reply.ID), []byte("null")) {
		return nil, errors.New("response is missing its request ID")
	}
	if err := json.Unmarshal(reply.ID, &id); err != nil || id != requestID {
		return nil, fmt.Errorf("unexpected response ID %s", reply.ID)
	}
	if reply.Error != nil {
		return nil, reply.Error
	}
	if !isObject(reply.Result) {
		return nil, errors.New("response is missing a valid result object and error")
	}
	return reply.Result, nil
}

// Subscribe completes the acknowledgement handshake before returning. The event
// stream has no read timeout; cancellation closes the socket and both channels.
// Events are delivered in order, with backpressure rather than dropping frames.
func (c Client) Subscribe(ctx context.Context) (<-chan Event, <-chan error, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	conn, err := c.connect(handshakeCtx)
	if err != nil {
		return nil, nil, err
	}
	stopHandshake := context.AfterFunc(handshakeCtx, func() { conn.Close() })
	ready := false
	defer func() {
		stopHandshake()
		if !ready {
			conn.Close()
		}
	}()

	// pane.agent_status_changed requires a pane_id and cannot be subscribed
	// globally. Action snapshots refresh agent status instead.
	types := []string{"pane.focused", "pane.closed", "pane.moved", "pane.agent_detected", "pane.exited"}
	type subscription struct {
		Type string `json:"type"`
	}
	params := struct {
		Subscriptions []subscription `json:"subscriptions"`
	}{Subscriptions: make([]subscription, 0, len(types))}
	for _, eventType := range types {
		params.Subscriptions = append(params.Subscriptions, subscription{Type: eventType})
	}
	if err := writeRequest(conn, "events.subscribe", params); err != nil {
		return nil, nil, fmt.Errorf("events.subscribe: send request: %w", contextError(handshakeCtx, err))
	}
	scanner := newFrameScanner(conn)
	result, err := readResponse(scanner)
	if err != nil {
		return nil, nil, fmt.Errorf("events.subscribe: read acknowledgement: %w", contextError(handshakeCtx, err))
	}
	var ack struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(result, &ack); err != nil {
		return nil, nil, fmt.Errorf("events.subscribe: invalid acknowledgement: %w", err)
	}
	if ack.Type != "subscription_started" {
		return nil, nil, fmt.Errorf("events.subscribe: unexpected acknowledgement type %q", ack.Type)
	}
	if err := handshakeCtx.Err(); err != nil {
		return nil, nil, fmt.Errorf("events.subscribe: %w", err)
	}
	if !stopHandshake() {
		return nil, nil, fmt.Errorf("events.subscribe: handshake expired: %w", context.DeadlineExceeded)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, nil, fmt.Errorf("events.subscribe: clear handshake deadline: %w", err)
	}
	stopStream := context.AfterFunc(ctx, func() { conn.Close() })
	events := make(chan Event, 512)
	errs := make(chan error, 1)
	ready = true
	go func() {
		defer close(events)
		defer close(errs)
		defer conn.Close()
		defer stopStream()
		for {
			frame, err := readFrame(scanner)
			if err != nil {
				errs <- fmt.Errorf("Herdr event stream: %w", contextError(ctx, err))
				return
			}
			event, err := decodeEvent(frame)
			if err != nil {
				errs <- fmt.Errorf("Herdr event stream: %w", err)
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
	}()
	return events, errs, nil
}

func decodeEvent(frame []byte) (Event, error) {
	// Accept direct events and nested event/result envelopes, while bounding
	// nesting and rejecting frames that cannot be identified as events.
	for depth := 0; depth < 8; depth++ {
		if !isObject(frame) {
			return Event{}, errors.New("invalid event envelope: expected an object")
		}
		var envelope struct {
			Event  json.RawMessage `json:"event"`
			Data   json.RawMessage `json:"data"`
			Result json.RawMessage `json:"result"`
			Error  *APIError       `json:"error"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return Event{}, fmt.Errorf("invalid event JSON: %w", err)
		}
		if envelope.Error != nil {
			return Event{}, envelope.Error
		}
		var name string
		if len(envelope.Event) > 0 && json.Unmarshal(envelope.Event, &name) == nil && name != "" {
			if len(envelope.Data) == 0 {
				return Event{}, errors.New("event is missing data")
			}
			// RawMessage owns its bytes, so subsequent scanner reads cannot
			// invalidate queued events.
			return Event{Event: normalizeEventName(name), Data: envelope.Data}, nil
		}
		if isObject(envelope.Event) {
			frame = envelope.Event
		} else if isObject(envelope.Result) {
			frame = envelope.Result
		} else {
			return Event{}, errors.New("event envelope is missing an event name")
		}
	}
	return Event{}, errors.New("event envelope nesting exceeds limit")
}

// Herdr 0.9.1 schema/events.rs serializes lifecycle EventKind in snake_case,
// unlike Subscription's dot-name spellings. Preserve the action's underscores
// (pane_agent_status_changed becomes pane.agent_status_changed).
func normalizeEventName(name string) string {
	kind, action, ok := strings.Cut(name, "_")
	if ok && action != "" {
		switch kind {
		case "pane", "tab", "workspace", "worktree", "layout":
			return kind + "." + action
		}
	}
	return name
}

func isObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

func newFrameScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxFrameBytes+1)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			if i > maxFrameBytes {
				return 0, nil, errors.New("JSON frame exceeds 8 MiB limit")
			}
			return i + 1, bytes.TrimSuffix(data[:i], []byte{'\r'}), nil
		}
		if len(data) > maxFrameBytes {
			return 0, nil, errors.New("JSON frame exceeds 8 MiB limit")
		}
		if atEOF && len(data) > 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	})
	return scanner
}

func readFrame(scanner *bufio.Scanner) ([]byte, error) {
	if scanner.Scan() {
		return scanner.Bytes(), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func contextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return err
}
