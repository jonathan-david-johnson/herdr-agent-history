package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jonathan-david-johnson/herdr-agent-history/internal/herdr"
	"github.com/jonathan-david-johnson/herdr-agent-history/internal/history"
)

type fakeAPI struct {
	mu          sync.Mutex
	snapshot    herdr.Snapshot
	focusErr    error
	snapshotErr error
	focusCalls  []string
	events      chan herdr.Event
	failures    chan error
}

func (f *fakeAPI) Snapshot(context.Context) (herdr.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.snapshot
	s.Agents = append([]herdr.Agent(nil), s.Agents...)
	return s, f.snapshotErr
}
func (f *fakeAPI) Focus(ctx context.Context, pane string) error {
	f.mu.Lock()
	if f.focusErr != nil {
		err := f.focusErr
		f.mu.Unlock()
		return err
	}
	f.focusCalls = append(f.focusCalls, pane)
	f.snapshot.FocusedPaneID = pane
	for i := range f.snapshot.Agents {
		f.snapshot.Agents[i].Focused = f.snapshot.Agents[i].PaneID == pane
		if f.snapshot.Agents[i].Focused && f.snapshot.Agents[i].Status == "done" {
			f.snapshot.Agents[i].Status = "idle"
		}
	}
	f.mu.Unlock()
	if f.events != nil {
		select {
		case f.events <- focusEvent(pane):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (f *fakeAPI) Subscribe(context.Context) (<-chan herdr.Event, <-chan error, error) {
	return f.events, f.failures, nil
}
func focusEvent(pane string) herdr.Event {
	b, _ := json.Marshal(map[string]string{"pane_id": pane})
	return herdr.Event{Event: "pane.focused", Data: b}
}
func agent(id, pane, status string, focused bool) herdr.Agent {
	return herdr.Agent{TerminalID: id, PaneID: pane, Status: status, Focused: focused, Agent: "pi"}
}
func setup(t *testing.T) (*Engine, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{snapshot: herdr.Snapshot{FocusedPaneID: "pB", Agents: []herdr.Agent{agent("A", "pA", "idle", false), agent("B", "pB", "working", true), agent("C", "pC", "done", false)}}}
	e := NewEngine(f, "/fake.sock", "")
	if err := e.Connected(f.snapshot); err != nil {
		t.Fatal(err)
	}
	return e, f
}
func handle(t *testing.T, e *Engine, action string) Response {
	t.Helper()
	r := e.Handle(context.Background(), Request{Action: action})
	if !r.OK {
		t.Fatalf("%s: %s", action, r.Error)
	}
	return r
}
func entries(t *testing.T, e *Engine, want []string, current string) {
	t.Helper()
	if !reflect.DeepEqual(e.state.Entries, want) || e.state.Current() != current {
		t.Fatalf("history=%v cursor=%d current=%s; want %v current=%s", e.state.Entries, e.state.Cursor, e.state.Current(), want, current)
	}
}

func TestCompletionThenBackForwardSurvivesAcknowledgement(t *testing.T) {
	e, _ := setup(t)
	r := handle(t, e, "next")
	if r.Trace.Reason != "completion" || r.Trace.PaneID != "pC" {
		t.Fatal(r.Trace)
	}
	entries(t, e, []string{"B", "C"}, "C")
	if err := e.Event(context.Background(), focusEvent("pC")); err != nil {
		t.Fatal(err)
	}
	entries(t, e, []string{"B", "C"}, "C")
	handle(t, e, "previous")
	if err := e.Event(context.Background(), focusEvent("pB")); err != nil {
		t.Fatal(err)
	}
	r = handle(t, e, "next")
	if r.Trace.Reason != "history_next" || r.Trace.Target != "C" {
		t.Fatal(r.Trace)
	}
	entries(t, e, []string{"B", "C"}, "C")
}

func TestExternalVisitRetainsForwardTail(t *testing.T) {
	e, f := setup(t)
	e.state = history.State{Entries: []string{"A", "B"}, Cursor: 1}
	handle(t, e, "previous")
	if err := e.Event(context.Background(), focusEvent("pA")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.snapshot.FocusedPaneID = "pC"
	f.mu.Unlock()
	if err := e.Event(context.Background(), focusEvent("pC")); err != nil {
		t.Fatal(err)
	}
	entries(t, e, []string{"A", "C", "B"}, "C")
	r := handle(t, e, "next")
	if r.Trace.Target != "B" || r.Trace.Reason != "history_next" {
		t.Fatal(r.Trace)
	}
}

func TestFailedFocusDoesNotCommitPlannedHistory(t *testing.T) {
	e, f := setup(t)
	before := e.state.Clone()
	f.focusErr = errors.New("pane disappeared")
	r := e.Handle(context.Background(), Request{Action: "next"})
	if r.OK || r.Error == "" {
		t.Fatal(r)
	}
	if !reflect.DeepEqual(e.state, before) || len(e.pending) != 0 {
		t.Fatalf("failed focus committed state: %v", e.state)
	}
}

func TestTerminalIdentitySurvivesPaneMove(t *testing.T) {
	e, f := setup(t)
	e.state = history.State{Entries: []string{"A", "B"}, Cursor: 1}
	f.mu.Lock()
	f.snapshot.Agents[0].PaneID = "new-workspace:pA"
	f.mu.Unlock()
	r := handle(t, e, "previous")
	if r.Trace.PaneID != "new-workspace:pA" || r.Trace.Target != "A" {
		t.Fatal(r.Trace)
	}
	entries(t, e, []string{"A", "B"}, "A")
}

func TestDelayedOwnEchoDoesNotReinsertDestination(t *testing.T) {
	e, _ := setup(t)
	e.state = history.State{Entries: []string{"A", "B", "C"}, Cursor: 1}
	handle(t, e, "previous")
	if err := e.Event(context.Background(), focusEvent("pA")); err != nil {
		t.Fatal(err)
	}
	entries(t, e, []string{"A", "B", "C"}, "A")
}

func TestExpiredEchoIsTreatedAsExternalFocus(t *testing.T) {
	e, _ := setup(t)
	e.state = history.State{Entries: []string{"A", "B", "C"}, Cursor: 1}
	e.pending = []pendingFocus{{"pA", time.Now().Add(-time.Second)}}
	if err := e.Event(context.Background(), focusEvent("pA")); err != nil {
		t.Fatal(err)
	}
	entries(t, e, []string{"B", "A", "C"}, "A")
}

func TestDisconnectedAndDifferentSessionFailSafely(t *testing.T) {
	e, _ := setup(t)
	if e.Handle(context.Background(), Request{Action: "next", HerdrSocket: "/other.sock"}).OK {
		t.Fatal("accepted another session")
	}
	if err := e.Disconnected(); err != nil {
		t.Fatal(err)
	}
	if e.Handle(context.Background(), Request{Action: "next"}).OK {
		t.Fatal("navigated while disconnected")
	}
	r := handle(t, e, "inspect")
	if r.Status.Connected || r.Status.ObservationGaps != 1 {
		t.Fatal(r.Status)
	}
}

func TestResetAndPersistence(t *testing.T) {
	e, f := setup(t)
	e.stateDir = t.TempDir()
	handle(t, e, "next")
	clone := NewEngine(f, "/fake.sock", e.stateDir)
	if err := clone.Load(); err != nil {
		t.Fatal(err)
	}
	entries(t, clone, []string{"B", "C"}, "C")
	info, err := os.Stat(filepath.Join(e.stateDir, "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode %o", info.Mode().Perm())
	}
	handle(t, e, "reset")
	entries(t, e, []string{"C"}, "C")
	if err := os.WriteFile(filepath.Join(e.stateDir, "history.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if clone.Load() == nil {
		t.Fatal("silently accepted corrupt state")
	}
}

func TestMissingAgentPrunedAndSnapshotFailureNoFocus(t *testing.T) {
	e, f := setup(t)
	e.state = history.State{Entries: []string{"missing", "B"}, Cursor: 1}
	r := handle(t, e, "previous")
	if r.Trace.Target != "B" || len(e.state.Entries) != 1 {
		t.Fatal(r.Trace, e.state)
	}
	f.snapshotErr = errors.New("offline")
	if e.Handle(context.Background(), Request{Action: "next"}).OK || len(f.focusCalls) != 0 {
		t.Fatal("focus despite unavailable snapshot")
	}
}

func waitInvoke(t *testing.T, ctx context.Context, dir string, check func(Response) bool) Response {
	t.Helper()
	timer := time.NewTicker(5 * time.Millisecond)
	defer timer.Stop()
	for {
		r, err := Invoke(ctx, dir, Request{Action: "inspect", HerdrSocket: "/fake.sock"})
		if err == nil && check(r) {
			return r
		}
		select {
		case <-ctx.Done():
			t.Fatalf("tracker not ready: %v", ctx.Err())
		case <-timer.C:
		}
	}
}

func TestRunIPCAndSingleInstance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dir := t.TempDir()
	f := &fakeAPI{snapshot: herdr.Snapshot{FocusedPaneID: "pA", Agents: []herdr.Agent{agent("A", "pA", "idle", true), agent("B", "pB", "done", false)}}, events: make(chan herdr.Event, 20), failures: make(chan error, 1)}
	stopped := make(chan error, 1)
	go func() { stopped <- Run(ctx, f, "/fake.sock", dir) }()
	waitInvoke(t, ctx, dir, func(r Response) bool { return r.Status.Connected })
	if err := Run(ctx, f, "/fake.sock", dir); err == nil {
		t.Fatal("second tracker acquired singleton lock")
	}
	r, err := Invoke(ctx, dir, Request{Action: "next", HerdrSocket: "/fake.sock"})
	if err != nil || r.Trace.Target != "B" {
		t.Fatal(r, err)
	}
	waitInvoke(t, ctx, dir, func(r Response) bool { return r.Status.History.Current() == "B" })
	if _, err := Invoke(ctx, dir, Request{Action: "next", HerdrSocket: "/wrong.sock"}); err == nil {
		t.Fatal("accepted wrong socket")
	}
	if _, err := Invoke(ctx, dir, Request{Action: "next", HerdrSocket: "/fake.sock", DeadlineUnixMS: time.Now().Add(-time.Second).UnixMilli()}); err == nil {
		t.Fatal("executed an expired navigation request")
	}
	f.mu.Lock()
	calls := len(f.focusCalls)
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("expired or wrong-session action changed focus: %d calls", calls)
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tracker did not stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "control.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket not removed", err)
	}
}

func TestPumpDeliversBufferedFocusesBeforeDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f := &fakeAPI{snapshot: herdr.Snapshot{Agents: []herdr.Agent{}}, events: make(chan herdr.Event, 2), failures: make(chan error, 1)}
	f.events <- focusEvent("first")
	f.events <- focusEvent("second")
	f.failures <- errors.New("connection lost")
	close(f.events)
	close(f.failures)
	messages := make(chan streamMessage, 10)
	go pump(ctx, f, messages)
	var panes []string
	for {
		select {
		case msg := <-messages:
			if msg.event != nil {
				var d struct {
					PaneID string `json:"pane_id"`
				}
				json.Unmarshal(msg.event.Data, &d)
				panes = append(panes, d.PaneID)
			}
			if msg.err != nil {
				if !reflect.DeepEqual(panes, []string{"first", "second"}) {
					t.Fatalf("lost buffered focuses: %v", panes)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("no disconnect delivered")
		}
	}
}

func TestCompletionAcknowledgementUsesStateSequence(t *testing.T) {
	for _, test := range []struct {
		seq    uint64
		reason string
	}{{5, "history_wrap_next"}, {6, "completion"}} {
		t.Run(test.reason, func(t *testing.T) {
			e, f := setup(t)
			e.state = history.State{Entries: []string{"C", "B"}, Cursor: 1}
			e.acknowledged["C"] = 5
			f.snapshot.Agents[2].StateChangeSeq = test.seq
			r := handle(t, e, "next")
			if r.Trace.Reason != test.reason {
				t.Fatalf("already acknowledged or fresh completion misclassified: %+v", r.Trace)
			}
		})
	}
}
