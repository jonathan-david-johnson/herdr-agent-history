// Package tracker owns the ordered visit history and serves navigation actions.
package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jonathan-david-johnson/herdr-agent-history/internal/herdr"
	"github.com/jonathan-david-johnson/herdr-agent-history/internal/history"
)

const Version = 1

type API interface {
	Snapshot(context.Context) (herdr.Snapshot, error)
	Focus(context.Context, string) error
	Subscribe(context.Context) (<-chan herdr.Event, <-chan error, error)
}

type Request struct {
	Action         string `json:"action"`
	HerdrSocket    string `json:"herdr_socket,omitempty"`
	DeadlineUnixMS int64  `json:"deadline_unix_ms,omitempty"`
}

type Trace struct {
	Action string    `json:"action"`
	Source string    `json:"source,omitempty"`
	Target string    `json:"target,omitempty"`
	PaneID string    `json:"pane_id,omitempty"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type Status struct {
	Version         int               `json:"version"`
	HerdrSocket     string            `json:"herdr_socket"`
	Connected       bool              `json:"connected"`
	History         history.State     `json:"history"`
	Acknowledged    map[string]uint64 `json:"acknowledged_state_sequences"`
	Agents          []herdr.Agent     `json:"agents"`
	LastAction      *Trace            `json:"last_action,omitempty"`
	ObservationGaps int               `json:"observation_gaps"`
	Limitation      string            `json:"limitation"`
}

type Response struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
	Trace  *Trace  `json:"trace,omitempty"`
}

type savedState struct {
	Version         int               `json:"version"`
	HerdrSocket     string            `json:"herdr_socket"`
	History         history.State     `json:"history"`
	Acknowledged    map[string]uint64 `json:"acknowledged_state_sequences"`
	LastAction      *Trace            `json:"last_action,omitempty"`
	ObservationGaps int               `json:"observation_gaps"`
}

type pendingFocus struct {
	pane    string
	expires time.Time
}

// Engine is owned by one event loop; no goroutine may mutate it concurrently.
type Engine struct {
	api          API
	state        history.State
	acknowledged map[string]uint64
	snapshot     herdr.Snapshot
	socket       string
	stateDir     string
	connected    bool
	lastAction   *Trace
	gaps         int
	pending      []pendingFocus
}

func NewEngine(api API, socket, stateDir string) *Engine {
	return &Engine{api: api, state: history.New(), acknowledged: make(map[string]uint64), socket: socket, stateDir: stateDir}
}

func (e *Engine) Load() error {
	b, err := os.ReadFile(filepath.Join(e.stateDir, "history.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved savedState
	if err := json.Unmarshal(b, &saved); err != nil {
		return fmt.Errorf("invalid saved history (use reset after moving the file aside): %w", err)
	}
	if saved.Version != Version {
		return fmt.Errorf("unsupported saved history version %d", saved.Version)
	}
	if saved.HerdrSocket != e.socket {
		return fmt.Errorf("state belongs to a different Herdr socket: %s", saved.HerdrSocket)
	}
	e.state = saved.History
	if saved.Acknowledged != nil {
		e.acknowledged = saved.Acknowledged
	}
	e.lastAction = saved.LastAction
	e.gaps = saved.ObservationGaps
	return nil
}

func (e *Engine) save() error {
	if e.stateDir == "" {
		return nil
	} // Tests may use an in-memory engine.
	data, err := json.MarshalIndent(savedState{Version: Version, HerdrSocket: e.socket, History: e.state, Acknowledged: e.acknowledged, LastAction: e.lastAction, ObservationGaps: e.gaps}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(e.stateDir, ".history-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(e.stateDir, "history.json"))
}

func (e *Engine) installSnapshot(s herdr.Snapshot) {
	e.snapshot = s
	live := make(map[string]bool, len(s.Agents))
	for _, a := range s.Agents {
		live[a.ID()] = true
	}
	e.state.Prune(live)
	for id := range e.acknowledged {
		if !live[id] {
			delete(e.acknowledged, id)
		}
	}
}

func (e *Engine) remember(a herdr.Agent) { e.acknowledged[a.ID()] = a.StateChangeSeq }

func (e *Engine) byPane(pane string) (herdr.Agent, bool) {
	for _, a := range e.snapshot.Agents {
		if a.PaneID == pane {
			return a, true
		}
	}
	return herdr.Agent{}, false
}

func (e *Engine) focused(s herdr.Snapshot) (herdr.Agent, bool) {
	for _, a := range s.Agents {
		if a.PaneID == s.FocusedPaneID {
			return a, true
		}
	}
	for _, a := range s.Agents {
		if a.Focused {
			return a, true
		}
	}
	return herdr.Agent{}, false
}

func (e *Engine) Connected(s herdr.Snapshot) error {
	e.installSnapshot(s)
	e.connected = true
	e.pending = nil
	if a, ok := e.focused(s); ok {
		e.state.Visit(a.ID())
		e.remember(a)
	}
	return e.save()
}

func (e *Engine) Disconnected() error {
	if e.connected {
		e.gaps++
	}
	e.connected = false
	e.pending = nil
	return e.save()
}

// Event suppresses the expected echoes of successful plugin focus commands.
// Herdr has no client/correlation IDs here, so same-target external focus can
// be ambiguous. The queue is short-lived and scoped to this single-client MVP.
func (e *Engine) Event(ctx context.Context, event herdr.Event) error {
	if event.Event != "pane.focused" {
		s, err := e.api.Snapshot(ctx)
		if err != nil {
			return err
		}
		e.installSnapshot(s)
		return e.save()
	}
	var data struct {
		PaneID string `json:"pane_id"`
	}
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return err
	}
	if data.PaneID == "" {
		return errors.New("pane.focused event has no pane_id")
	}
	now := time.Now()
	for len(e.pending) > 0 && now.After(e.pending[0].expires) {
		e.pending = e.pending[1:]
	}
	if len(e.pending) > 0 && e.pending[0].pane == data.PaneID {
		e.pending = e.pending[1:]
		return nil // Already committed atomically by the navigation action.
	}
	s, err := e.api.Snapshot(ctx)
	if err != nil {
		return err
	}
	e.installSnapshot(s)
	a, ok := e.byPane(data.PaneID)
	if ok {
		e.state.Visit(a.ID())
		e.remember(a)
	}
	return e.save()
}

func (e *Engine) Status() Status {
	ack := make(map[string]uint64, len(e.acknowledged))
	for id, seq := range e.acknowledged {
		ack[id] = seq
	}
	return Status{Version: Version, HerdrSocket: e.socket, Connected: e.connected, History: e.state.Clone(), Acknowledged: ack, Agents: append([]herdr.Agent(nil), e.snapshot.Agents...), LastAction: e.lastAction, ObservationGaps: e.gaps,
		Limitation: "Single Herdr client only; done is server-side seen state. Own-focus attribution is heuristic; visits during disconnection cannot be reconstructed."}
}

func (e *Engine) Handle(ctx context.Context, req Request) Response {
	if req.HerdrSocket != "" && req.HerdrSocket != e.socket {
		return Response{Error: "action belongs to a different Herdr session; this tracker only serves " + e.socket}
	}
	if req.Action == "inspect" {
		status := e.Status()
		return Response{OK: true, Status: &status}
	}
	if req.Action != "previous" && req.Action != "next" && req.Action != "reset" {
		return Response{Error: "unknown action: " + req.Action}
	}
	if !e.connected {
		return Response{Error: "tracker is disconnected from Herdr; navigation is disabled until it reconnects"}
	}
	s, err := e.api.Snapshot(ctx)
	if err != nil {
		return Response{Error: "refresh agents: " + err.Error()}
	}
	e.installSnapshot(s)
	// Reconcile focus at action time as well: this repairs a missed/delayed event,
	// but cannot recreate any intermediate visits omitted by a disconnected API.
	if a, ok := e.focused(s); ok {
		if a.ID() != e.state.Current() {
			e.state.Visit(a.ID())
		}
		e.remember(a)
	}
	if req.Action == "reset" {
		e.state = history.New()
		e.acknowledged = make(map[string]uint64)
		e.pending = nil
		if a, ok := e.focused(s); ok {
			e.state.Visit(a.ID())
			e.remember(a)
		}
		e.lastAction = &Trace{Action: "reset", Reason: "history_reset", At: time.Now()}
		if err := e.save(); err != nil {
			return Response{Error: err.Error()}
		}
		status := e.Status()
		return Response{OK: true, Status: &status}
	}
	proposed := e.state.Clone()
	source := proposed.Current()
	var decision history.Decision
	if req.Action == "previous" {
		decision = proposed.Previous()
	} else {
		var all, done []string
		current := source
		for _, a := range s.Agents {
			all = append(all, a.ID())
			seq, seen := e.acknowledged[a.ID()]
			if a.Status == "done" && (!seen || a.StateChangeSeq > seq) {
				done = append(done, a.ID())
			}
		}
		decision = proposed.Next(done, current, all)
	}
	trace := &Trace{Action: req.Action, Source: source, Target: decision.Target, Reason: decision.Reason, At: time.Now()}
	if decision.Target != "" && decision.Target != source {
		var target herdr.Agent
		found := false
		for _, a := range s.Agents {
			if a.ID() == decision.Target {
				target = a
				found = true
				break
			}
		}
		if !found {
			return Response{Error: "destination disappeared before focus"}
		}
		trace.PaneID = target.PaneID
		if err := e.api.Focus(ctx, target.PaneID); err != nil {
			return Response{Error: "focus failed; history cursor was not advanced: " + err.Error()}
		}
		e.remember(target)
		e.pending = append(e.pending, pendingFocus{target.PaneID, time.Now().Add(3 * time.Second)})
		e.snapshot.FocusedPaneID = target.PaneID
		for i := range e.snapshot.Agents {
			a := &e.snapshot.Agents[i]
			a.Focused = a.PaneID == target.PaneID
			if a.Focused && a.Status == "done" {
				a.Status = "idle" // Explicit API focus acknowledges a server completion.
			}
		}
	}
	e.state = proposed
	e.lastAction = trace
	if err := e.save(); err != nil {
		return Response{Error: "focus may have changed, but saving history failed: " + err.Error(), Trace: trace}
	}
	log.Printf("action=%s source=%s target=%s reason=%s", trace.Action, trace.Source, trace.Target, trace.Reason)
	return Response{OK: true, Trace: trace}
}

type streamMessage struct {
	snapshot *herdr.Snapshot
	event    *herdr.Event
	err      error
}
type incoming struct {
	request  Request
	response chan Response
	deadline time.Time
}

func pump(ctx context.Context, api API, out chan<- streamMessage) {
	send := func(m streamMessage) bool {
		select {
		case out <- m:
			return true
		case <-ctx.Done():
			return false
		}
	}
	delay := 250 * time.Millisecond
	for ctx.Err() == nil {
		streamCtx, cancel := context.WithCancel(ctx)
		events, failures, err := api.Subscribe(streamCtx)
		if err == nil {
			var s herdr.Snapshot
			s, err = api.Snapshot(streamCtx) // Subscription acknowledged before snapshot.
			if err == nil && send(streamMessage{snapshot: &s}) {
				delay = 250 * time.Millisecond
			stream:
				for {
					select {
					case <-ctx.Done():
						cancel()
						return
					case event, ok := <-events:
						if !ok {
							err = errors.New("Herdr event stream closed")
							break stream
						}
						if !send(streamMessage{event: &event}) {
							cancel()
							return
						}
					case streamErr, ok := <-failures:
						if ok && streamErr != nil {
							err = streamErr
						} else {
							err = errors.New("Herdr event stream disconnected")
						}
						// The reader can report EOF while its event queue is still
						// buffered. Deliver those events before recording the gap.
					drainEvents:
						for {
							select {
							case event, open := <-events:
								if !open {
									break drainEvents
								}
								if !send(streamMessage{event: &event}) {
									cancel()
									return
								}
							default:
								break drainEvents
							}
						}
						break stream
					}
				}
			}
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if !send(streamMessage{err: err}) {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}

// Run serves a private Unix control socket. systemd supervises this process;
// plugin actions never spawn unsupervised trackers or mutate state themselves.
func Run(ctx context.Context, api API, socket, stateDir string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, "tracker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another tracker owns this state directory: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	control := filepath.Join(stateDir, "control.sock")
	if err := os.Remove(control); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", control)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(control)
	if err := os.Chmod(control, 0600); err != nil {
		return err
	}
	engine := NewEngine(api, socket, stateDir)
	if err := engine.Load(); err != nil {
		return err
	}
	requests := make(chan incoming, 32)
	go func() { <-ctx.Done(); listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveConnection(ctx, conn, requests)
		}
	}()
	messages := make(chan streamMessage, 512)
	go pump(ctx, api, messages)
	process := func(msg streamMessage) error {
		if msg.snapshot != nil {
			log.Printf("subscribed to Herdr; reconciling %d live agents", len(msg.snapshot.Agents))
			return engine.Connected(*msg.snapshot)
		}
		if msg.err != nil {
			log.Printf("observation gap: %v; reconnecting", msg.err)
			return engine.Disconnected()
		}
		if msg.event != nil {
			if err := engine.Event(ctx, *msg.event); err != nil {
				log.Printf("event handling gap: %v; the next action will reconcile live focus", err)
				engine.gaps++
				return engine.save()
			}
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-messages:
			if err := process(msg); err != nil {
				return err
			}
		case req := <-requests:
			// Prefer all already-observed focus events over a navigation command.
		drain:
			for {
				select {
				case msg := <-messages:
					if err := process(msg); err != nil {
						return err
					}
				default:
					break drain
				}
			}
			var result Response
			if time.Now().After(req.deadline) {
				result = Response{Error: "navigation request expired in the queue; focus was not changed"}
			} else {
				actionCtx, actionCancel := context.WithDeadline(ctx, req.deadline)
				result = engine.Handle(actionCtx, req.request)
				actionCancel()
			}
			select {
			case req.response <- result:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func serveConnection(ctx context.Context, conn net.Conn, requests chan<- incoming) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	var req Request
	if err := json.NewDecoder(&boundedReader{conn: conn, remaining: 4096}).Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(Response{Error: "invalid control request: " + err.Error()})
		return
	}
	req.Action = strings.TrimSpace(req.Action)
	deadline := time.Now().Add(8 * time.Second)
	if req.DeadlineUnixMS != 0 {
		if requested := time.UnixMilli(req.DeadlineUnixMS); requested.Before(deadline) {
			deadline = requested
		}
	}
	item := incoming{request: req, response: make(chan Response, 1), deadline: deadline}
	select {
	case requests <- item:
	case <-ctx.Done():
		return
	}
	select {
	case result := <-item.response:
		json.NewEncoder(conn).Encode(result)
	case <-ctx.Done():
	}
}

type boundedReader struct {
	conn      net.Conn
	remaining int
}

func (r *boundedReader) Read(b []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("control request exceeds 4096 bytes")
	}
	if len(b) > r.remaining {
		b = b[:r.remaining]
	}
	n, err := r.conn.Read(b)
	r.remaining -= n
	return n, err
}

func Invoke(ctx context.Context, stateDir string, req Request) (Response, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(stateDir, "control.sock"))
	if err != nil {
		return Response{}, fmt.Errorf("history tracker unavailable; start herdr-agent-history.service: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	if req.DeadlineUnixMS == 0 {
		req.DeadlineUnixMS = deadline.UnixMilli()
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var result Response
	if err := json.NewDecoder(conn).Decode(&result); err != nil {
		return Response{}, err
	}
	if !result.OK {
		return result, errors.New(result.Error)
	}
	return result, nil
}
