package llmw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// ---- fake llmw CLI runner (per-command scripted) ----

// scriptRunner answers llmw CLI calls by first-arg dispatch: list → wikis
// JSON, status → windows JSON (stateful: `wiki enter` adds the window row),
// anything else → "". Calls are recorded for assertion.
type scriptRunner struct {
	mu      sync.Mutex
	calls   [][]string
	list    string
	windows []windowRow
	errFor  map[string]string // first arg → error message
}

func (r *scriptRunner) run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{}, args...))
	r.mu.Unlock()
	if msg, ok := r.errFor[args[0]]; ok {
		return nil, errors.New(msg)
	}
	switch args[0] {
	case "list":
		return []byte(r.list), nil
	case "status":
		r.mu.Lock()
		defer r.mu.Unlock()
		return []byte(marshalRows(r.windows)), nil
	case "wiki":
		// `wiki --name=X enter [--window-suffix=S]` creates the window row
		// (models llmw's side effect) unless the first arg errors.
		if len(args) >= 3 && args[2] == "enter" {
			name, suffix := "", "main"
			for _, a := range args {
				if strings.HasPrefix(a, "--name=") {
					name = strings.TrimPrefix(a, "--name=")
				}
				if strings.HasPrefix(a, "--window-suffix=") {
					suffix = strings.TrimPrefix(a, "--window-suffix=")
				}
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.windows = append(r.windows, windowRow{
				Wiki: name, Window: name + "-" + suffix, WindowID: "@" + name + suffix,
				Session: "llm_workspace", Backend: "opencode", State: "waiting",
			})
		}
		return nil, nil
	}
	return nil, nil
}

// callsWith counts recorded calls whose first arg matches.
func (r *scriptRunner) callsWith(first string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == first {
			n++
		}
	}
	return n
}

// ---- fake pane runner ----

// fakePaneRunner scripts the pane driver's view of one window: captures are
// consumed sequentially (last repeats), exports are canned per session id.
type fakePaneRunner struct {
	mu       sync.Mutex
	workDir  string
	injected []string
	captures []string
	sessions string
	exports  map[string]string

	// uiMode simulates the TUI subagent bar: "" = absent, "build"/"plan".
	// sendKey("Tab") toggles it, capture() renders it.
	uiMode    string
	sentKeys  []string
	sentTexts []string

	// lastCapture: when the script is exhausted, capture() repeats it —
	// a drained script reads as a static screen (like the real TUI when
	// nothing changes), not an error.
	lastCapture string

	// exportSeq: per-session evolving export snapshots (pseudo-stream
	// tests). Each exportJSON call pops the next frame; the last frame
	// repeats. Sessions absent here fall back to the static exports map.
	exportSeq map[string][]string
}

func (r *fakePaneRunner) inject(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.injected = append(r.injected, text)
	return nil
}
func (r *fakePaneRunner) submit() error { return nil }
func (r *fakePaneRunner) sendText(text string) error {
	r.mu.Lock()
	r.sentTexts = append(r.sentTexts, text)
	r.mu.Unlock()
	return nil
}

func (r *fakePaneRunner) sendKey(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sentKeys = append(r.sentKeys, key)
	if key == "Tab" && (r.uiMode == "build" || r.uiMode == "plan") {
		if r.uiMode == "build" {
			r.uiMode = "plan"
		} else {
			r.uiMode = "build"
		}
	}
	return nil
}
func (r *fakePaneRunner) capture() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.uiMode != "" {
		bar := r.uiMode // the real TUI capitalizes the subagent name
		if bar == "build" {
			bar = "Build"
		} else if bar == "plan" {
			bar = "Plan"
		}
		return "  ┃  " + bar + " · qwen3.8-max llmw registry\n", nil
	}
	if len(r.captures) == 0 {
		if r.lastCapture != "" {
			return r.lastCapture, nil // script exhausted: static screen
		}
		return "", errors.New("tmux capture-pane: no target")
	}
	c := r.captures[0]
	if len(r.captures) > 1 {
		r.captures = r.captures[1:]
	}
	r.lastCapture = c
	return c, nil
}
func (r *fakePaneRunner) sessionListJSON() (string, error) {
	return r.sessions, nil
}
func (r *fakePaneRunner) exportJSON(sessionID string) (string, error) {
	if seq := r.exportSeq[sessionID]; len(seq) > 0 {
		out := seq[0]
		if len(seq) > 1 {
			r.exportSeq[sessionID] = seq[1:]
		}
		return out, nil
	}
	if out, ok := r.exports[sessionID]; ok {
		return out, nil
	}
	return "", fmt.Errorf("opencode export %s: not scripted", sessionID)
}

// lastInjected returns the most recent injected prompt.
func (r *fakePaneRunner) lastInjected() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.injected) == 0 {
		return ""
	}
	return r.injected[len(r.injected)-1]
}

func (r *fakePaneRunner) sentKeysLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sentKeys)
}

// scriptTurn arms one full turn: busy captures, then idle; plus a matching
// session-list/export pair so the driver discovers and returns the reply.
func (r *fakePaneRunner) scriptTurn(prompt, reply string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ses := fmt.Sprintf("ses_%d", len(r.exports)+1)
	r.captures = append(r.captures, "idle ctrl+p", "… esc interrupt …", "… esc interrupt …", "idle ctrl+p", "idle ctrl+p")
	var rows []ocSessionRow
	_ = json.Unmarshal([]byte(r.sessions), &rows)
	rows = append(rows, ocSessionRow{ID: ses, Directory: r.workDir, Updated: time.Now().UnixMilli()})
	b, _ := json.Marshal(rows)
	r.sessions = string(b)
	exp := ocExport{}
	exp.Messages = append(exp.Messages, ocMessage{})
	exp.Messages[0].Info.Role = "user"
	exp.Messages[0].Parts = []ocPart{{Type: "text", Text: prompt}}
	m := ocMessage{}
	m.Info.Role = "assistant"
	m.Parts = []ocPart{{Type: "text", Text: reply}}
	exp.Messages = append(exp.Messages, m)
	eb, _ := json.Marshal(exp)
	if r.exports == nil {
		r.exports = map[string]string{}
	}
	r.exports[ses] = string(eb)
}

// ---- test harness ----

// newTestAgent builds an llmwAgent over a temp workspace with two wikis (foo,
// bar), a scripted llmw CLI runner, and a pane factory that records the fake
// pane runner per wiki dir. Poll timings are shrunk for the test duration.
func newTestAgent(t *testing.T) (*llmwAgent, *scriptRunner, map[string]*fakePaneRunner) {
	t.Helper()
	oldPoll, oldSettle := panePollInterval, paneSettleDelay
	panePollInterval, paneSettleDelay = 5*time.Millisecond, time.Millisecond
	t.Cleanup(func() { panePollInterval, paneSettleDelay = oldPoll, oldSettle })

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

	wikis := []wikiEntry{
		{Name: "foo", Path: "foo", DisplayName: "foo", Model: "m1", DirExists: true},
		{Name: "bar", Path: "bar", DisplayName: "bar", Model: "m2", DirExists: true},
	}
	wikisJSON, _ := json.Marshal(wikis)
	runner := &scriptRunner{list: string(wikisJSON)}

	a := &llmwAgent{
		client:   newLlmwClient(),
		sessions: make(map[string]*sessionFacade),
	}
	a.client.run = runner.run
	a.ctx, a.cancel = context.WithCancel(context.Background())

	panes := make(map[string]*fakePaneRunner)
	a.paneFactory = func(ctx context.Context, target, workDir string) core.AgentSession {
		// The real idle footer anchor — SelfCheck reads it on attach.
		fr := &fakePaneRunner{workDir: workDir, captures: []string{"idle ctrl+p commands"}}
		panes[filepath.Base(workDir)] = fr
		return newPaneSession(ctx, fr, workDir)
	}
	return a, runner, panes
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

// llmwCmd builds a custom-command template expansion (menu description +
// sentinel + args), mimicking what the engine's ExpandPrompt feeds to Send
// after the /llmw_xxx hard cutover.
func llmwCmd(args string) string {
	if args == "" {
		return "llmw workspace 管理\n" + menuSentinel
	}
	return "llmw workspace 管理\n" + menuSentinel + " " + args
}

func newFacadeForTest(a *llmwAgent) *sessionFacade {
	f := newSessionFacade(a, "")
	return f
}

func TestFacadeUnboundMessage(t *testing.T) {
	a, _, _ := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send("hello", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "先 /llmw list") {
		t.Fatalf("want guidance event, got %+v", ev)
	}
}

func TestFacadeWikisAndNumericEntry(t *testing.T) {
	a, _, panes := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	// /llmw list → listing with 2 entries.
	if err := f.Send(llmwCmd("list"), "", nil, nil); err != nil {
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
	if _, ok := panes["bar"]; !ok {
		t.Fatal("pane for bar not attached")
	}

	// Ordinary message is injected into the bar pane.
	panes["bar"].scriptTurn("what is in this wiki?", "stuff")
	if err := f.Send("what is in this wiki?", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := panes["bar"].lastInjected(); got != "what is in this wiki?" {
		t.Fatalf("injected = %q, want the ordinary message", got)
	}
}

func TestFacadeEnterByNameAndSwitch(t *testing.T) {
	a, _, panes := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("want entry foo, got %q", evStr(ev))
	}
	oldInner := f.inner

	// Switch to bar.
	if err := f.Send(llmwCmd("enter bar"), "", nil, nil); err != nil {
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
	if _, ok := panes["bar"]; !ok {
		t.Fatal("bar pane should be attached")
	}

	// Same-wiki re-enter → idempotent no-op.
	if err := f.Send(llmwCmd("enter bar"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已在 wiki：bar") {
		t.Fatalf("want already-in confirmation, got %q", evStr(ev))
	}
	if f.inner == nil || !f.inner.Alive() {
		t.Fatal("inner should be untouched")
	}
}

// Regression (2026-09-04, "llmw_list 后回复数字报错"): when llmw wiki enter
// fails (e.g. byobu refusing to run under a HOME-less daemon env), the
// facade used to keep the phantom binding — status showed a bound wiki, a
// re-reply of the same number short-circuited to "已在 wiki", and the user
// was stuck. After a failed enter the binding must roll back to unbound AND
// the numeric selection must stay armed so the same number retries.
func TestFacadeEnterFailureRollsBackAndRearms(t *testing.T) {
	a, runner, panes := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	// /llmw list arms the numeric selection (foo=1, bar=2).
	if err := f.Send(llmwCmd("list"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "1. foo") {
		t.Fatalf("want wiki listing, got %q", evStr(ev))
	}

	// Make `llmw wiki enter` fail the way the real incident did.
	runner.mu.Lock()
	runner.errFor = map[string]string{
		"wiki": "llmw wiki enter: exit status 2: byobu: Cannot run byobu because [root] does not own []",
	}
	runner.mu.Unlock()

	if err := f.Send("1", "", nil, nil); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "建立窗口失败") {
		t.Fatalf("want enter failure notice, got %q", evStr(ev))
	}
	f.mu.Lock()
	bound := f.wiki != nil
	f.mu.Unlock()
	if bound {
		t.Fatal("failed enter must roll the binding back (no phantom binding)")
	}

	// The selection stays armed: after the failure is fixed, the SAME number
	// retries the enter instead of falling through as a chat message.
	runner.mu.Lock()
	runner.errFor = nil
	runner.mu.Unlock()
	if err := f.Send("1", "", nil, nil); err != nil {
		t.Fatalf("Send 1 (retry): %v", err)
	}
	if ev := waitEvent(f.events, 3*time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("want entry on numeric retry, got %q", evStr(ev))
	}
	if _, ok := panes["foo"]; !ok {
		t.Fatal("pane for foo not attached after retry")
	}
}

// A failed SWITCH of an already-bound facade (numeric reply to /llmw switch)
// must restore the previous binding, not leave the new wiki half-bound.
func TestFacadeSwitchFailureRestoresBinding(t *testing.T) {
	a, runner, _ := newTestAgent(t)
	runner.mu.Lock()
	runner.windows = []windowRow{
		{Wiki: "foo", Window: "foo-main", WindowID: "@f0", Session: "llm_workspace", Backend: "opencode", State: "waiting"},
		{Wiki: "bar", Window: "bar-tg", WindowID: "@b1", Session: "llm_workspace", Backend: "opencode", State: "waiting"},
	}
	runner.mu.Unlock()
	f := newFacadeForTest(a)
	defer f.Close()

	// Enter foo first (live window: attach, no llmw enter call).
	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("want entry foo, got %q", evStr(ev))
	}

	// bar-tg is a LIVE window: the switch path attaches without calling
	// `llmw wiki enter`, so the failure has to come from a later step —
	// make workspace resolution explode (the rollback is what matters).
	if err := f.Send(llmwCmd("switch"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "切换主机窗口") {
		t.Fatalf("want window listing, got %q", evStr(ev))
	}
	a.client.root = func() (string, error) { return "", errFake("workspace gone") }

	if err := f.Send("2", "", nil, nil); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "解析 workspace 根失败") {
		t.Fatalf("want failure notice, got %q", evStr(ev))
	}
	f.mu.Lock()
	wiki := f.wiki
	f.mu.Unlock()
	if wiki == nil || wiki.Name != "foo" {
		t.Fatalf("failed switch must restore the foo binding, got %+v", wiki)
	}
}

func TestFacadeUnknownWiki(t *testing.T) {
	a, _, _ := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("enter nope"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "未找到 wiki：nope") {
		t.Fatalf("want not-found event, got %q", evStr(ev))
	}
}

// TestFacadeCommandTurnEndsWithResult guards against the engine deadlock
// where a command turn emits only text and never an EventResult: the engine
// only completes a turn on EventResult{Done:true}, so every later message
// queues behind the uncompleted turn (Telegram shows a stuck "processing"
// indicator). Every terminal command path must end with a result event.
func TestFacadeCommandTurnEndsWithResult(t *testing.T) {
	a, _, _ := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	for _, prompt := range []string{"/llmw", "/llmw list", "/llmw enter nope", "/llmw enter foo"} {
		if err := f.Send(prompt, "", nil, nil); err != nil {
			t.Fatalf("Send(%q): %v", prompt, err)
		}
		// The turn's events: >=1 text followed by a terminal result.
		var evs []core.Event
		deadline := time.After(2 * time.Second)
	collect:
		for {
			select {
			case ev := <-f.events:
				evs = append(evs, ev)
				if ev.Type == core.EventResult {
					break collect
				}
			case <-deadline:
				t.Fatalf("Send(%q): no EventResult within timeout; events so far: %d", prompt, len(evs))
			}
		}
		if !evs[len(evs)-1].Done {
			t.Fatalf("Send(%q): terminal result must have Done=true", prompt)
		}
		// Text must precede the result (not an empty turn).
		foundText := false
		for _, ev := range evs[:len(evs)-1] {
			if ev.Type == core.EventText {
				foundText = true
			}
		}
		if !foundText {
			t.Fatalf("Send(%q): want text before result, got %v", prompt, evs)
		}
	}

	// Ordinary message path forwards to inner and must NOT get a synthetic
	// result from the facade (the inner emits its own).
	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)
	if err := f.Send("hello", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f.inner == nil {
		t.Fatal("inner should exist after ordinary message")
	}
}

func TestFacadeLazyRebuild(t *testing.T) {
	a, runner, panes := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)
	oldInner := f.inner
	entersBefore := runner.callsWith("wiki")

	// Kill the pane object (daemon restart simulation — the byobu window
	// itself stays alive). Next ordinary message reattaches WITHOUT a
	// second llmw enter: status still lists the live window.
	_ = oldInner.Close()
	if err := f.Send("hello again", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f.inner == oldInner || f.inner == nil {
		t.Fatal("pane should be re-attached")
	}
	if !f.inner.Alive() {
		t.Fatal("re-attached pane should be alive")
	}
	if f.wiki == nil || f.wiki.Name != "foo" {
		t.Fatalf("wiki binding should survive rebuild, got %+v", f.wiki)
	}
	if got := runner.callsWith("wiki"); got != entersBefore {
		t.Fatalf("live window must be re-attached without re-enter (before=%d after=%d)", entersBefore, got)
	}
	if got := panes["foo"].lastInjected(); got != "hello again" {
		t.Fatalf("message should be injected into the rebuilt pane, got %q", got)
	}
}

func TestFacadeResume(t *testing.T) {
	a, runner, panes := newTestAgent(t)
	// Even with the foo-ing window live in status, StartSession must not
	// implicitly attach it.
	ing := windowRow{Wiki: "foo", Window: "foo-ing", WindowID: "@ing", Session: "llm_workspace", Backend: "opencode", State: "waiting"}
	runner.windows = append(runner.windows, ing)

	sess, err := a.StartSession(context.Background(), "llmw:foo:ing")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	f := sess.(*sessionFacade)
	defer f.Close()
	// No implicit resume (2026-08-30): even with the foo-ing window live, the
	// facade stays unbound — explicit /llmw enter or /llmw switch attaches.
	if f.wiki != nil {
		t.Fatalf("facade must be unbound after StartSession, got %+v", f.wiki)
	}
	if f.id != "llmw:foo:ing" {
		t.Fatalf("facade id = %q, want llmw:foo:ing", f.id)
	}
	if _, ok := panes["foo"]; ok {
		t.Fatal("no pane may be attached without explicit enter")
	}
	// Same id returns the same facade.
	sess2, err := a.StartSession(context.Background(), "llmw:foo:ing")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if sess2 != sess {
		t.Fatal("same session id must return the same facade")
	}
}

func TestFacadeEventsChannelStable(t *testing.T) {
	a, _, _ := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)
	// Switch wiki; events channel object must stay the same.
	ch := f.events
	if err := f.Send(llmwCmd("enter bar"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)
	if f.events != ch {
		t.Fatal("events channel must be stable across wiki switches")
	}
}

// statusRunner dispatches llmw subprocess calls: list → wikis JSON, status →
// status JSON, wiki stop → recorded + success (or canned error).
type statusRunner struct {
	wikis     []wikiEntry
	windows   []windowRow
	stopErr   string
	stopCalls [][]string
}

func (r *statusRunner) run(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case len(args) >= 2 && args[0] == "list":
		return []byte(marshalRows(r.wikis)), nil
	case len(args) >= 1 && args[0] == "status":
		return []byte(marshalRows(r.windows)), nil
	case len(args) >= 3 && args[0] == "wiki" && args[2] == "stop":
		r.stopCalls = append(r.stopCalls, args)
		if r.stopErr != "" {
			return nil, errFake(r.stopErr)
		}
		return []byte{}, nil
	case len(args) >= 3 && args[0] == "wiki" && args[2] == "enter":
		return []byte{}, nil
	}
	return nil, errFake("unexpected llmw call: " + strings.Join(args, " "))
}

func marshalRows[T any](rows []T) string {
	b, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func newStatusTestAgent(t *testing.T, r *statusRunner) *llmwAgent {
	t.Helper()
	a, _, _ := newTestAgent(t)
	a.client.run = r.run
	return a
}

func TestFacadeStatusUnbound(t *testing.T) {
	uptime := 100.0
	r := &statusRunner{
		wikis:   []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
		windows: []windowRow{{Wiki: "foo", Window: "foo-main", Backend: "claude", State: "working", UptimeSeconds: &uptime}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("status"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil {
		t.Fatal("want status event")
	}
	for _, want := range []string{"foo-main", "working", "本会话未绑定", "用法：/llmw"} {
		if !contains(ev.Content, want) {
			t.Errorf("status missing %q:\n%s", want, ev.Content)
		}
	}
}

func TestFacadeStatusBoundAndEmpty(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
		// A live foo-main window so the enter flow can attach a pane.
		windows: []windowRow{{Wiki: "foo", Window: "foo-main", Session: "llm_workspace", Backend: "opencode", State: "waiting"}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvent(f.events, time.Second)

	// Windows disappear before the status query → empty table, still bound.
	r.windows = nil
	if err := f.Send("/llmw", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil {
		t.Fatal("want status event")
	}
	for _, want := range []string{"没有运行中的窗口", "本会话绑定：foo"} {
		if !contains(ev.Content, want) {
			t.Errorf("status missing %q:\n%s", want, ev.Content)
		}
	}
}

func TestFacadeStatusUnavailable(t *testing.T) {
	r := &statusRunner{wikis: []wikiEntry{{Name: "foo", DirExists: true}}}
	a := newStatusTestAgent(t, r)
	a.client.run = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "status" {
			return nil, errFake("llmw status: boom")
		}
		return r.run(context.Background(), args...)
	}
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("status"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "llmw 不可用") {
		t.Fatalf("want unavailable event, got %q", evStr(ev))
	}
}

func TestFacadeStopWindow(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	// Short-form stop asks for confirmation first (aligned with CLI --yes).
	if err := f.Send(llmwCmd("stop foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	if len(r.stopCalls) != 0 {
		t.Fatalf("stop executed without confirmation: %v", r.stopCalls)
	}
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send y: %v", err)
	}
	ev = waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已停止 foo") {
		t.Fatalf("want stop confirmation, got %q", evStr(ev))
	}
	want := []string{"wiki", "--name=foo", "stop", "--yes"}
	if len(r.stopCalls) != 1 || len(r.stopCalls[0]) != len(want) {
		t.Fatalf("stopCalls = %v", r.stopCalls)
	}
	for i := range want {
		if r.stopCalls[0][i] != want[i] {
			t.Fatalf("stop args = %v, want %v", r.stopCalls[0], want)
		}
	}
}

// A non-y input cancels an armed stop confirmation; the cancelling input is
// still processed normally (here: /llmw status).
func TestFacadeStopConfirmCancelled(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("stop foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	if err := f.Send(llmwCmd("status"), "", nil, nil); err != nil {
		t.Fatalf("Send status: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已取消停止 foo") {
		t.Fatalf("want cancel notice, got %q", evStr(ev))
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "主机窗口") {
		t.Fatalf("status should still render after cancel, got %q", evStr(ev))
	}
	if len(r.stopCalls) != 0 {
		t.Fatalf("cancelled stop must not execute: %v", r.stopCalls)
	}
}

// /llmw detach unbinds without touching the host window.
func TestFacadeDetach(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	f.mu.Lock()
	f.wiki = &wikiEntry{Name: "foo", Path: "foo"}
	f.suffix = "tg"
	f.id = facadeID("foo", "tg")
	f.mu.Unlock()

	if err := f.Send(llmwCmd("detach"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已解绑 foo") || !contains(ev.Content, "窗口保留") {
		t.Fatalf("want detach confirmation, got %q", evStr(ev))
	}
	f.mu.Lock()
	bound := f.wiki != nil
	f.mu.Unlock()
	if bound {
		t.Fatal("facade must be unbound after detach")
	}
	if len(r.stopCalls) != 0 {
		t.Fatalf("detach must not stop any window: %v", r.stopCalls)
	}
}

// /llmw detach with no binding: plain notice.
func TestFacadeDetachUnbound(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("detach"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "未绑定") {
		t.Fatalf("want unbound notice, got %q", evStr(ev))
	}
}

// Typed "/llmw_stop foo ing" (template form with name + suffix): confirm,
// then the stop call carries --window-suffix=ing. The CLI-native paste form
// was removed with the /llmw_xxx hard cutover.
func TestFacadeStopWindowWithSuffix(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("stop foo ing"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send y: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已停止 foo") {
		t.Fatalf("want stop confirmation, got %q", evStr(ev))
	}
	if len(r.stopCalls) != 1 {
		t.Fatalf("stopCalls = %v", r.stopCalls)
	}
	got := r.stopCalls[0]
	if got[len(got)-1] != "--window-suffix=ing" {
		t.Fatalf("suffix flag missing: %v", got)
	}
}

// /llmw switch lists the LIVE windows with numbers; a numeric reply rebinds
// the conversation to the chosen window via the enter path.
func TestFacadeSwitchListAndNumericRebind(t *testing.T) {
	a, runner, panes := newTestAgent(t)
	runner.mu.Lock()
	runner.windows = []windowRow{
		{Wiki: "foo", Window: "foo-main", WindowID: "@f0", Session: "llm_workspace", Backend: "opencode", State: "waiting"},
		{Wiki: "bar", Window: "bar-tg", WindowID: "@b1", Session: "llm_workspace", Backend: "opencode", State: "waiting"},
	}
	runner.mu.Unlock()
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("switch"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "切换主机窗口") || !contains(ev.Content, "1. foo-main") || !contains(ev.Content, "2. bar-tg") {
		t.Fatalf("want window listing, got %q", evStr(ev))
	}

	if err := f.Send("2", "", nil, nil); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	ev = waitEvent(f.events, 3*time.Second)
	if ev == nil || !contains(ev.Content, "已进入 wiki：bar") {
		t.Fatalf("want entry confirmation, got %q", evStr(ev))
	}
	f.mu.Lock()
	wiki, suffix := "", ""
	if f.wiki != nil {
		wiki = f.wiki.Name
	}
	suffix = f.suffix
	f.mu.Unlock()
	if wiki != "bar" || suffix != "tg" {
		t.Fatalf("rebound to (%q, %q), want (bar, tg)", wiki, suffix)
	}
	if _, ok := panes["bar"]; !ok {
		t.Fatal("no pane spawned for bar (tg window)")
	}
}

// A number outside the /llmw switch listing is rejected with a switch-scoped
// hint (not the /llmw list one), and the selection stays armed.
func TestFacadeSwitchNumericInvalid(t *testing.T) {
	a, runner, _ := newTestAgent(t)
	runner.mu.Lock()
	runner.windows = []windowRow{
		{Wiki: "foo", Window: "foo-main", WindowID: "@f0", Session: "llm_workspace", Backend: "opencode", State: "waiting"},
	}
	runner.mu.Unlock()
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("switch"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil {
		t.Fatal("missing listing event")
	}
	if err := f.Send("9", "", nil, nil); err != nil {
		t.Fatalf("Send 9: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "序号无效") || !contains(ev.Content, "/llmw switch") {
		t.Fatalf("want invalid-number hint, got %q", evStr(ev))
	}
	// Still armed: "1" now rebinds.
	if err := f.Send("1", "", nil, nil); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if ev := waitEvent(f.events, 3*time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("want entry after valid retry, got %q", evStr(ev))
	}
}

// Bare /llmw stop on a main binding: stops the bound wiki's main window and
// UNBINDS the facade — the wiki session is gone, so status must not keep
// showing a binding.
func TestFacadeBareStopBoundMain(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	f.mu.Lock()
	f.wiki = &wikiEntry{Name: "foo", Path: "foo"}
	f.suffix = ""
	f.id = facadeID("foo", "")
	f.mu.Unlock()

	if err := f.Send(llmwCmd("stop"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send y: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已停止 foo 的 main 窗口") || !contains(ev.Content, "已解绑") {
		t.Fatalf("want bare-main stop + unbind confirmation, got %q", evStr(ev))
	}
	want := []string{"wiki", "--name=foo", "stop", "--yes"}
	if len(r.stopCalls) != 1 {
		t.Fatalf("stopCalls = %v", r.stopCalls)
	}
	for i := range want {
		if r.stopCalls[0][i] != want[i] {
			t.Fatalf("stop args = %v, want %v", r.stopCalls[0], want)
		}
	}
	if f.wiki != nil || f.suffix != "" {
		t.Fatalf("want unbound facade, got wiki=%+v suffix=%q", f.wiki, f.suffix)
	}
	if f.id != facadeID("foo", "") {
		t.Fatalf("facade id (map key) must be kept, got %q", f.id)
	}
}

// pump must survive f.events being closed underneath it (send-on-closed
// panics without the recover; regression for the closeLocked drain race).
func TestFacadePumpSurvivesClosedEvents(t *testing.T) {
	a, _, _ := newTestAgent(t)
	f := newSessionFacade(a, "llmw:foo:")
	close(f.events)
	ch := make(chan core.Event, 1)
	ch <- core.Event{Type: core.EventText, Content: "x"}
	done := make(chan struct{})
	go func() { defer close(done); f.pump(ch) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit (or panicked blocking the goroutine)")
	}
}

// A y/yes that arrives AFTER the confirmation TTL is consumed with an
// expiry notice — it must never be forwarded into the opencode prompt.
func TestFacadeStopConfirmExpiredYConsumed(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("stop foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	f.mu.Lock()
	f.pending.deadline = time.Now().Add(-time.Second)
	f.mu.Unlock()
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send y: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已超时") {
		t.Fatalf("want expiry notice, got %q", evStr(ev))
	}
	if len(r.stopCalls) != 0 {
		t.Fatalf("expired confirmation must not execute stop: %v", r.stopCalls)
	}
}

// NAMED stop of the currently bound non-main window must behave exactly
// like the bare form (rebind to main) — otherwise the next message
// resurrects the window the user just stopped.
func TestFacadeNamedStopOwnSuffixRebindsMain(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	f.mu.Lock()
	f.wiki = &wikiEntry{Name: "foo", Path: "foo"}
	f.suffix = "tg"
	f.id = facadeID("foo", "tg")
	f.mu.Unlock()

	if err := f.Send(llmwCmd("stop foo tg"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send y: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已停止 foo 的窗口 foo-tg") || !contains(ev.Content, "已换绑 main") {
		t.Fatalf("want named suffix-stop + rebind confirmation, got %q", evStr(ev))
	}
	if f.suffix != "" || f.id != facadeID("foo", "") {
		t.Fatalf("want lazy rebind to main, got suffix=%q id=%q", f.suffix, f.id)
	}
}

// NAMED stop of the currently bound MAIN window unbinds, same as the bare
// form.
func TestFacadeNamedStopOwnMainUnbinds(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	f.mu.Lock()
	f.wiki = &wikiEntry{Name: "foo", Path: "foo"}
	f.mu.Unlock()

	if err := f.Send(llmwCmd("stop foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send y: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已停止 foo 的 main 窗口") || !contains(ev.Content, "已解绑") {
		t.Fatalf("want named main-stop + unbind confirmation, got %q", evStr(ev))
	}
	if f.wiki != nil {
		t.Fatalf("want unbound facade, got wiki=%+v", f.wiki)
	}
}

// /llmw_abort while unbound asks to enter first.
func TestFacadeAbortUnbound(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("abort"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "未绑定") {
		t.Fatalf("want unbound hint, got %q", evStr(ev))
	}
}

// /llmw_abort on a busy window reaches the pane: Escape is sent and the
// abort notice surfaces through the facade channel.
func TestFacadeAbortBusyForwardsEscape(t *testing.T) {
	a, _, panes := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("want entry confirmation, got %q", evStr(ev))
	}
	// A HOST-started turn: the pane reads busy with no IM turn running —
	// deterministic (no poll loop consuming the script) and it pins the
	// "abort works for host-started turns too" semantics.
	fr := panes["foo"]
	fr.mu.Lock()
	fr.captures = []string{"working esc interrupt"}
	fr.mu.Unlock()
	if err := f.Send(llmwCmd("abort"), "", nil, nil); err != nil {
		t.Fatalf("Send abort: %v", err)
	}
	// Wait for the abort notice through the facade channel (raw read:
	// waitEvent swallows empty-Content results).
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-f.events:
			if !ok {
				t.Fatal("events closed")
			}
			if ev.Type == core.EventText && contains(ev.Content, "中止信号") {
				fr.mu.Lock()
				keys := append([]string(nil), fr.sentKeys...)
				fr.mu.Unlock()
				for _, k := range keys {
					if k == "Escape" {
						return // abort reached the pane
					}
				}
				t.Fatalf("notice surfaced but no Escape sent, keys = %v", keys)
			}
		case <-deadline:
			t.Fatal("abort notice never surfaced")
		}
	}
}

// /llmw_new on a bound window reaches the pane as the TUI /new command.
func TestFacadeNewSessionForwards(t *testing.T) {
	a, _, panes := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("want entry confirmation, got %q", evStr(ev))
	}
	fr := panes["foo"]
	fr.mu.Lock()
	fr.captures = []string{"idle ctrl+p commands", "idle ctrl+p commands"}
	fr.mu.Unlock()

	if err := f.Send(llmwCmd("new"), "", nil, nil); err != nil {
		t.Fatalf("Send new: %v", err)
	}
	if got := fr.lastInjected(); got != newCmd {
		t.Fatalf("injected = %q, want %q", got, newCmd)
	}
	// Raw read (waitEvent swallows empty-Content results): the pane emits
	// its own confirmation + result through the pump.
	confirmed := false
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-f.events:
			if !ok {
				t.Fatal("events closed")
			}
			if ev.Type == core.EventText && contains(ev.Content, "已开始新会话") {
				confirmed = true
			}
			if confirmed && ev.Type == core.EventResult && ev.Done {
				return // drain the result too: a pending pump send races the deferred Close
			}
		case <-deadline:
			t.Fatal("new-session confirmation never surfaced")
		}
	}
}

// Entering a window whose pane speaks none of the TUI anchors surfaces the
// drift warning instead of failing silently on the first turn.
func TestFacadeEnterSelfCheckWarns(t *testing.T) {
	wikis := []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}}
	wikisJSON, _ := json.Marshal(wikis)
	runner := &scriptRunner{list: string(wikisJSON)}
	runner.windows = []windowRow{{Wiki: "foo", Window: "foo-main", WindowID: "@1", Session: "llm_workspace", Backend: "opencode", State: "waiting"}}

	a := &llmwAgent{
		client:   newLlmwClient(),
		sessions: map[string]*sessionFacade{},
	}
	a.client.run = runner.run
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.paneFactory = func(ctx context.Context, target, workDir string) core.AgentSession {
		// A foreign screen: no busy marker, no idle hint, no mode bar.
		fr := &fakePaneRunner{workDir: workDir, captures: []string{"$ bash prompt"}}
		return newPaneSession(ctx, fr, workDir)
	}
	f := newSessionFacade(a, "llmw:test:")
	defer f.Close()

	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var warned, entered bool
	deadline := time.After(3 * time.Second)
	for !(warned && entered) {
		select {
		case ev, ok := <-f.events:
			if !ok {
				t.Fatal("events closed")
			}
			if ev.Type != core.EventText {
				continue
			}
			if contains(ev.Content, "已进入 wiki") {
				entered = true
			}
			if contains(ev.Content, "界面契约异常") {
				warned = true
			}
		case <-deadline:
			t.Fatalf("want enter + drift warning, entered=%v warned=%v", entered, warned)
		}
	}
}

// Attachments cannot be injected into the TUI — the user gets an explicit
// notice instead of a silent drop (the text part still goes through).
func TestFacadeAttachmentNotice(t *testing.T) {
	a, _, panes := newTestAgent(t)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "已进入 wiki：foo") {
		t.Fatalf("want entry confirmation, got %q", evStr(ev))
	}
	panes["foo"].scriptTurn("with a photo", "done")
	if err := f.Send("with a photo", "", []core.ImageAttachment{{MimeType: "image/png", FileName: "p.png"}}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "暂不支持附件") {
		t.Fatalf("want attachment notice, got %q", evStr(ev))
	}
	if got := panes["foo"].lastInjected(); got != "with a photo" {
		t.Fatalf("injected = %q, want the text part forwarded", got)
	}
	// waitEvent swallows empty-Content events (EventResult carries none), so
	// read the channel raw until the turn-done marker arrives.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-f.events:
			if !ok {
				t.Fatal("events channel closed before turn completion")
			}
			if ev.Type == core.EventResult && ev.Done {
				return // notice + forwarded text + completed turn
			}
		case <-deadline:
			t.Fatal("turn did not complete after attachment notice")
		}
	}
}

// Bare /llmw stop on a non-main binding: stops that window AND lazily
// rebinds the facade to main so the next message does not resurrect the
// stopped window.
func TestFacadeBareStopBoundSuffixRebindsMain(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	f.mu.Lock()
	f.wiki = &wikiEntry{Name: "foo", Path: "foo"}
	f.suffix = "tg"
	f.id = facadeID("foo", "tg")
	f.mu.Unlock()

	if err := f.Send(llmwCmd("stop"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send y: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "已停止 foo 的窗口 foo-tg") || !contains(ev.Content, "已换绑 main") {
		t.Fatalf("want suffix-stop + rebind confirmation, got %q", evStr(ev))
	}
	if f.suffix != "" || f.id != facadeID("foo", "") {
		t.Fatalf("want lazy rebind to main, got suffix=%q id=%q", f.suffix, f.id)
	}
}

// Bare /llmw stop with no binding: actionable error, no CLI call.
func TestFacadeBareStopUnbound(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("stop"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "未绑定 wiki") {
		t.Fatalf("want unbound guidance, got %q", evStr(ev))
	}
	if len(r.stopCalls) != 0 {
		t.Fatalf("no stop call expected, got %v", r.stopCalls)
	}
}

func TestFacadeStopWindowAmbiguityPassthrough(t *testing.T) {
	r := &statusRunner{
		wikis:   []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
		stopErr: "wiki 'foo' 有 2 个运行中的窗口：foo-main / foo-ingest\nhint: 加 --window-suffix=SUFFIX",
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("stop foo"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f.events, time.Second); ev == nil || !contains(ev.Content, "回复 y 确认") {
		t.Fatalf("want confirmation prompt, got %q", evStr(ev))
	}
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send y: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "停止失败") || !contains(ev.Content, "--window-suffix") {
		t.Fatalf("llmw ambiguity error must be forwarded verbatim, got %q", evStr(ev))
	}
}

func TestFacadeStopUnknownWiki(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("stop nope"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "未找到 wiki：nope") {
		t.Fatalf("want not-found event, got %q", evStr(ev))
	}
}

func TestFacadeUsageOnBadSyntax(t *testing.T) {
	r := &statusRunner{
		wikis: []wikiEntry{{Name: "foo", Path: "foo", DisplayName: "foo", DirExists: true}},
	}
	a := newStatusTestAgent(t, r)
	f := newFacadeForTest(a)
	defer f.Close()

	if err := f.Send(llmwCmd("bogus"), "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvent(f.events, time.Second)
	if ev == nil || !contains(ev.Content, "用法：/llmw") {
		t.Fatalf("want usage event, got %q", evStr(ev))
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
