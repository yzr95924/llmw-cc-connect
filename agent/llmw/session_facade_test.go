package llmw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// ---- fake backend agent ----

type fakeInnerSession struct {
	id     string
	events chan core.Event
	mu     sync.Mutex
	dead   bool
	closed bool
	sent   []string
	agent  *fakeInnerAgent
}

func (s *fakeInnerSession) Send(prompt, messageID string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	s.sent = append(s.sent, prompt)
	return nil
}
func (s *fakeInnerSession) RespondPermission(string, core.PermissionResult) error { return nil }
func (s *fakeInnerSession) Events() <-chan core.Event                             { return s.events }
func (s *fakeInnerSession) CurrentSessionID() string                              { return s.id }
func (s *fakeInnerSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.dead && !s.closed
}
func (s *fakeInnerSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.dead = true
	close(s.events)
	return nil
}

type fakeInnerAgent struct {
	opts     map[string]any
	startErr error
}

func (a *fakeInnerAgent) Name() string { return "fake" }
func (a *fakeInnerAgent) StartSession(_ context.Context, sessionID string) (core.AgentSession, error) {
	if a.startErr != nil {
		return nil, a.startErr
	}
	id := sessionID
	if id == "" {
		id = "fake-" + time.Now().Format("150405.000000000")
	}
	return &fakeInnerSession{id: id, events: make(chan core.Event), agent: a}, nil
}
func (a *fakeInnerAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (a *fakeInnerAgent) Stop() error { return nil }

// ---- test harness ----

// newTestAgent builds an llmwAgent with a fake llmw subprocess and fake inner
// factory, over a temp workspace with two wiki dirs.
func newTestAgent(t *testing.T, byobu bool) (*llmwAgent, *fakeInnerAgent, map[string]*fakeInnerAgent) {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("LLMW_WORKSPACE", ws)
	for _, w := range []string{"foo", "bar"} {
		if err := os.MkdirAll(filepath.Join(ws, w), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "workspace.toml"), []byte("# t"), 0o644); err != nil {
		t.Fatal(err)
	}
	byobuCfg := "schema_version = 1\n"
	if byobu {
		byobuCfg += "enter_byobu = true\n"
	}
	if err := os.WriteFile(filepath.Join(ws, "workspace_local.toml"), []byte(byobuCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	rows := []wikiEntry{
		{Name: "foo", Path: "foo", DisplayName: "foo", Model: "m1", DirExists: true},
		{Name: "bar", Path: "bar", DisplayName: "bar", Model: "m2", DirExists: true},
	}
	runner := fakeRunner(marshalWikis(t, rows), "")

	a := &llmwAgent{
		backend:  "claude",
		baseOpts: map[string]any{},
		client:   newLlmwClient(),
		sessions: make(map[string]*sessionFacade),
	}
	a.client.run = runner
	a.ctx, a.cancel = context.WithCancel(context.Background())

	created := make(map[string]*fakeInnerAgent)
	a.innerFactory = func(opts map[string]any) (core.Agent, error) {
		fa := &fakeInnerAgent{opts: opts}
		wd, _ := opts["work_dir"].(string)
		created[filepath.Base(wd)] = fa
		return fa, nil
	}
	return a, nil, created
}

func marshalWikis(t *testing.T, rows []wikiEntry) string {
	t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// drain reads events until none are pending (small grace window).
func drain(events <-chan core.Event, timeout time.Duration) []core.Event {
	var out []core.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

func waitEvent(events <-chan core.Event, timeout time.Duration) *core.Event {
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if ev.Content != "" {
				return &ev
			}
		case <-deadline:
			return nil
		}
	}
}

func newFacadeForTest(a *llmwAgent) *sessionFacade {
	f := newSessionFacade(a, "")
	return f
}

func TestFacadeUnboundMessage(t *testing.T) {
	a, _, _ := newTestAgent(t, false)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send("hello", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "先 /wikis") {
		t.Fatalf("want guidance event, got %+v", ev)
	}
}

func TestFacadeWikisAndNumericEntry(t *testing.T) {
	a, _, created := newTestAgent(t, false)
	f := newFacadeForTest(a)
	defer f.Close()

	// /wikis → listing with 2 entries.
	if err := f.Send("/wikis", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "1. foo") || !contains(ev.Content, "2. bar") {
		t.Fatalf("want wiki listing, got %q", evStr(ev))
	}

	// Reply "2" → enter bar; inner spawned with cwd=bar; confirmation event.
	if err := f.Send("2", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev = waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已进入 wiki：bar") {
		t.Fatalf("want entry confirmation, got %q", evStr(ev))
	}
	if f.wiki == nil || f.wiki.Name != "bar" {
		t.Fatalf("facade should be bound to bar, got %+v", f.wiki)
	}
	if f.inner == nil {
		t.Fatal("inner session missing")
	}
	if _, ok := created["bar"]; !ok {
		t.Fatal("inner agent for bar not created")
	}
	wd, _ := created["bar"].opts["work_dir"].(string)
	if wd == "" || filepath.Base(wd) != "bar" {
		t.Fatalf("inner work_dir should be the bar wiki dir, got %q", wd)
	}

	// Ordinary message forwarded to inner.
	if err := f.Send("what is in this wiki?", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sess := f.inner.(*fakeInnerSession)
	if len(sess.sent) != 1 || sess.sent[0] != "what is in this wiki?" {
		t.Fatalf("inner.sent = %v", sess.sent)
	}
}

func TestFacadeEnterByNameAndSwitch(t *testing.T) {
	a, _, created := newTestAgent(t, false)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send("/enter foo", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("want entry foo, got %q", evStr(ev))
	}
	oldInner := f.inner

	// Switch to bar.
	if err := f.Send("/enter bar", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：bar") {
		t.Fatalf("want entry bar, got %q", evStr(ev))
	}
	if f.inner == oldInner {
		t.Fatal("inner should be swapped on switch")
	}
	if !stringsHasPrefix(f.id, "llmw:bar:") {
		t.Fatalf("facade id should encode bar, got %q", f.id)
	}
	if _, ok := created["bar"]; !ok {
		t.Fatal("bar inner agent should exist")
	}

	// Same-wiki re-enter → idempotent no-op.
	if err := f.Send("/enter bar", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已在 wiki：bar") {
		t.Fatalf("want already-in confirmation, got %q", evStr(ev))
	}
	if f.inner == nil || !f.inner.Alive() {
		t.Fatal("inner should be untouched")
	}
}

func TestFacadeUnknownWiki(t *testing.T) {
	a, _, _ := newTestAgent(t, false)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send("/enter nope", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "未找到 wiki：nope") {
		t.Fatalf("want not-found event, got %q", evStr(ev))
	}
}

func TestFacadeLazyRebuild(t *testing.T) {
	a, _, _ := newTestAgent(t, false)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send("/enter foo", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)
	oldInner := f.inner

	// Kill inner.
	oldInner.(*fakeInnerSession).Close()

	// Next ordinary message triggers rebuild.
	if err := f.Send("hello again", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f.inner == oldInner || f.inner == nil {
		t.Fatal("inner should be rebuilt")
	}
	if f.wiki == nil || f.wiki.Name != "foo" {
		t.Fatalf("wiki binding should survive rebuild, got %+v", f.wiki)
	}
	sess := f.inner.(*fakeInnerSession)
	if len(sess.sent) != 1 || sess.sent[0] != "hello again" {
		t.Fatalf("message should be forwarded to rebuilt inner, got %v", sess.sent)
	}
}

func TestFacadeByobuSync(t *testing.T) {
	var entered []string
	a, _, _ := newTestAgent(t, true) // byobu enabled
	a.client.run = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "wiki" && args[2] == "enter" {
			entered = append(entered, args[1])
			return []byte{}, nil
		}
		return fakeRunner(marshalWikis(t, []wikiEntry{
			{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true},
		}), "")(context.Background())
	}
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send("/enter foo", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)
	if len(entered) != 1 || entered[0] != "--name=foo" {
		t.Fatalf("enterWiki should be called with --name=foo, got %v", entered)
	}
}

func TestFacadeByobuSyncFailureDegrades(t *testing.T) {
	a, _, _ := newTestAgent(t, true)
	a.client.run = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "wiki" {
			return nil, errFake("byobu failed")
		}
		return fakeRunner(marshalWikis(t, []wikiEntry{
			{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true},
		}), "")(context.Background())
	}
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send("/enter foo", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("byobu failure must not block entry, got %q", evStr(ev))
	}
}

func TestFacadeResume(t *testing.T) {
	a, _, created := newTestAgent(t, false)
	a.client.run = fakeRunner(marshalWikis(t, []wikiEntry{
		{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true},
	}), "")

	sess, err := a.StartSession(context.Background(), "llmw:foo:old-inner-id")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	f := sess.(*sessionFacade)
	defer f.Close()
	if f.wiki == nil || f.wiki.Name != "foo" {
		t.Fatalf("resume should bind foo, got %+v", f.wiki)
	}
	if _, ok := created["foo"]; !ok {
		t.Fatal("foo inner agent should be created on resume")
	}
	// Same id returns the same facade.
	sess2, err := a.StartSession(context.Background(), "llmw:foo:old-inner-id")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if sess2 != sess {
		t.Fatal("same session id must return the same facade")
	}
}

func TestFacadeEventsChannelStable(t *testing.T) {
	a, _, _ := newTestAgent(t, false)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send("/enter foo", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)
	// Switch wiki; events channel object must stay the same.
	ch := f.events
	if err := f.Send("/enter bar", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)
	if f.events != ch {
		t.Fatal("events channel must be stable across wiki switches")
	}
}

// ---- tiny helpers ----

func contains(s, sub string) bool { return strings.Contains(s, sub) }
func stringsHasPrefix(s, prefix string) bool {
	return strings.HasPrefix(s, prefix)
}

func evStr(ev *core.Event) string {
	if ev == nil {
		return "<nil>"
	}
	return ev.Content
}

type fakeErr struct{ msg string }

func (e fakeErr) Error() string { return e.msg }

func errFake(msg string) error { return fakeErr{msg: msg} }
