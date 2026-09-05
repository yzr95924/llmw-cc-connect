package llmw

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// fakeRunner returns canned stdout for a command; errors when args contain the
// marker "ERR".
func fakeRunner(out string, errMsg string) func(ctx context.Context, args ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		if errMsg != "" {
			return nil, errors.New(errMsg)
		}
		return []byte(out), nil
	}
}

func sampleWikisJSON() string {
	rows := []wikiEntry{
		{Name: "agent-tools", Path: "agent-tools", DisplayName: "agent-tools", Model: "glm-5_2-1m", DirExists: true},
		{Name: "kv-store", Path: "kv-store", DisplayName: "kv_store", Model: "glm-5_2-1m", DirExists: true},
		{Name: "ghost", Path: "ghost", DisplayName: "ghost", DirExists: false},
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

func TestLlmwClientList(t *testing.T) {
	c := newLlmwClient()
	c.run = fakeRunner(sampleWikisJSON(), "")

	wikis, err := c.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(wikis) != 3 {
		t.Fatalf("want 3 wikis, got %d", len(wikis))
	}
	if wikis[0].Name != "agent-tools" || wikis[0].Path != "agent-tools" || wikis[0].Model != "glm-5_2-1m" {
		t.Fatalf("unexpected row: %+v", wikis[0])
	}
	if !wikis[0].DirExists {
		t.Fatal("row 1 should parse DirExists=true")
	}
	if wikis[2].DirExists {
		t.Fatal("row 3 should parse DirExists=false")
	}
}

func TestLlmwClientListError(t *testing.T) {
	c := newLlmwClient()
	c.run = fakeRunner("", "boom: llmw not found")

	if _, err := c.list(context.Background()); err == nil {
		t.Fatal("want error for failing subprocess")
	}
}

func TestLlmwClientListBadJSON(t *testing.T) {
	c := newLlmwClient()
	c.run = fakeRunner("not json", "")

	if _, err := c.list(context.Background()); err == nil {
		t.Fatal("want error for bad JSON")
	}
}

func TestLlmwClientEnterWikiArgs(t *testing.T) {
	var got [][]string
	c := newLlmwClient()
	c.run = func(_ context.Context, args ...string) ([]byte, error) {
		got = append(got, args)
		return []byte{}, nil
	}

	if err := c.enterWiki(context.Background(), "kv-store", ""); err != nil {
		t.Fatalf("enterWiki: %v", err)
	}
	if err := c.enterWiki(context.Background(), "kv-store", "ingest"); err != nil {
		t.Fatalf("enterWiki with suffix: %v", err)
	}
	wantMain := []string{"wiki", "--name=kv-store", "enter"}
	wantSfx := []string{"wiki", "--name=kv-store", "enter", "--window-suffix=ingest"}
	if len(got) != 2 {
		t.Fatalf("calls = %v", got)
	}
	for i := range wantMain {
		if got[0][i] != wantMain[i] {
			t.Fatalf("main args = %v, want %v", got[0], wantMain)
		}
	}
	for i := range wantSfx {
		if got[1][i] != wantSfx[i] {
			t.Fatalf("suffix args = %v, want %v", got[1], wantSfx)
		}
	}
}

func TestLlmwClientWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMW_WORKSPACE", dir)
	// Missing workspace.toml → error.
	c := newLlmwClient()
	if _, err := c.workspaceRoot(); err == nil {
		t.Fatal("want error when workspace.toml missing")
	}
	// Present workspace.toml → ok.
	if err := os.WriteFile(filepath.Join(dir, "workspace.toml"), []byte("# t"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := c.workspaceRoot()
	if err != nil {
		t.Fatalf("workspaceRoot: %v", err)
	}
	if root != filepath.Clean(dir) {
		t.Fatalf("root = %q, want %q", root, dir)
	}
}

func TestLlmwClientDefaultWorkspace(t *testing.T) {
	t.Setenv("LLMW_WORKSPACE", "")
	c := newLlmwClient()
	home, _ := os.UserHomeDir()
	root, err := c.workspaceRoot()
	if err == nil {
		if !strings.HasSuffix(root, "/"+defaultWorkspace) {
			t.Fatalf("default root should end with %q, got %q", defaultWorkspace, root)
		}
	}
	_ = home
}

func TestWikiMatch(t *testing.T) {
	w := &wikiEntry{Name: "kv-store", DisplayName: "kv_store"}
	cases := []struct {
		name string
		want bool
	}{
		{"kv-store", true},
		{"kv_store", true},
		{"KV-Store", true},
		{"Kv_Store", true},
		{"nope", false},
	}
	for _, tc := range cases {
		if got := matchWikiName(w, tc.name); got != tc.want {
			t.Errorf("matchWikiName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseFacadeID(t *testing.T) {
	cases := []struct {
		id          string
		wiki, inner string
		ours        bool
	}{
		{"llmw:foo:abc123", "foo", "abc123", true},
		{"llmw:foo:", "foo", "", true},
		{"llmw:", "", "", true},
		{"llmw", "", "", false},
		{"claudecode:xyz", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		w, i, ours := parseFacadeID(tc.id)
		if w != tc.wiki || i != tc.inner || ours != tc.ours {
			t.Errorf("parseFacadeID(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.id, w, i, ours, tc.wiki, tc.inner, tc.ours)
		}
	}
}

func TestRegistrationAndValidate(t *testing.T) {
	found := false
	for _, n := range core.ListRegisteredAgents() {
		if n == agentName {
			found = true
		}
	}
	if !found {
		t.Fatalf("agent %q not registered", agentName)
	}

	a := &llmwAgent{}
	if !a.ValidateSessionID(context.Background(), "llmw:foo:bar") {
		t.Fatal("llmw id should validate")
	}
	if a.ValidateSessionID(context.Background(), "claudecode:foo") {
		t.Fatal("foreign id should not validate")
	}
}

func TestNew(t *testing.T) {
	// Bad backend.
	if _, err := New(map[string]any{"backend": "nope"}); err == nil {
		t.Fatal("want error for bad backend")
	}
	// Default backend.
	a, err := New(map[string]any{"cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag := a.(*llmwAgent)
	if ag.client == nil || ag.sessions == nil {
		t.Fatal("agent must own a client and facade map")
	}
	// Command dir installed with llmw.md.
	md := filepath.Join(ag.commandsDir, "llmw.md")
	data, err := os.ReadFile(md)
	if err != nil {
		t.Fatalf("llmw.md not installed: %v", err)
	}
	if !strings.Contains(string(data), menuSentinel) {
		t.Fatalf("llmw.md missing sentinel: %q", data)
	}
	// CommandDirs.
	if len(ag.CommandDirs()) != 1 || ag.CommandDirs()[0] != ag.commandsDir {
		t.Fatalf("unexpected CommandDirs: %v", ag.CommandDirs())
	}
}

func TestInstallCommandsCleansLegacyWikis(t *testing.T) {
	// Untouched install → no wikis.md at all.
	a, err := New(map[string]any{"cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag := a.(*llmwAgent)
	if _, err := os.Stat(filepath.Join(ag.commandsDir, "wikis.md")); !os.IsNotExist(err) {
		t.Fatal("fresh install must not create wikis.md")
	}

	// Agent-generated legacy constant → removed.
	dir := t.TempDir()
	cmds := filepath.Join(dir, commandsSubdir)
	if err := os.MkdirAll(cmds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmds, "wikis.md"), []byte(legacyWikisCommand), 0o644); err != nil {
		t.Fatal(err)
	}
	a2, err := New(map[string]any{"cc_data_dir": dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag2 := a2.(*llmwAgent)
	if _, err := os.Stat(filepath.Join(ag2.commandsDir, "wikis.md")); !os.IsNotExist(err) {
		t.Fatal("legacy wikis.md (unmodified constant) must be removed")
	}

	// User-modified legacy file → kept.
	dir2 := t.TempDir()
	cmds2 := filepath.Join(dir2, commandsSubdir)
	if err := os.MkdirAll(cmds2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmds2, "wikis.md"), []byte("customised"), 0o644); err != nil {
		t.Fatal(err)
	}
	a3, err := New(map[string]any{"cc_data_dir": dir2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag3 := a3.(*llmwAgent)
	if _, err := os.Stat(filepath.Join(ag3.commandsDir, "wikis.md")); err != nil {
		t.Fatalf("user-modified wikis.md must be kept: %v", err)
	}
}

func TestParseLlmwCommand(t *testing.T) {
	cases := []struct {
		prompt string
		ok     bool
		want   llmwCommand
	}{
		// Menu sentinel → status.
		{"<llmw:menu>", true, llmwCommand{verb: "status"}},
		{"llmw workspace 管理\n<llmw:menu>", true, llmwCommand{verb: "status"}},
		// Template path (hard cutover): verbs arrive after the sentinel,
		// either baked into the /llmw_xxx template or appended by ExpandPrompt.
		{"llmw workspace 管理\n<llmw:menu>\n\nlist", true, llmwCommand{verb: "list"}},
		{"llmw workspace 管理\n<llmw:menu>\n\nenter foo", true, llmwCommand{verb: "enter", name: "foo"}},
		{"llmw workspace 管理\n<llmw:menu> enter", true, llmwCommand{verb: "list"}}, // menu tap, no args
		{"llmw workspace 管理\n<llmw:menu> enter foo", true, llmwCommand{verb: "enter", name: "foo"}},
		{"llmw workspace 管理\n<llmw:menu> enter foo ingest", true, llmwCommand{verb: "enter", name: "foo", suffix: "ingest"}},
		{"llmw workspace 管理\n<llmw:menu> stop", true, llmwCommand{verb: "stop"}},
		{"llmw workspace 管理\n<llmw:menu> stop foo", true, llmwCommand{verb: "stop", name: "foo"}},
		{"llmw workspace 管理\n<llmw:menu> stop foo ingest", true, llmwCommand{verb: "stop", name: "foo", suffix: "ingest"}},
		{"llmw workspace 管理\n<llmw:menu> switch", true, llmwCommand{verb: "switch"}},
		{"llmw workspace 管理\n<llmw:menu> detach", true, llmwCommand{verb: "detach"}},
		{"llmw workspace 管理\n<llmw:menu> abort", true, llmwCommand{verb: "abort"}},
		{"llmw workspace 管理\n<llmw:menu> abort now", true, llmwCommand{verb: "usage"}},
		{"llmw workspace 管理\n<llmw:menu> new", true, llmwCommand{verb: "new"}},
		{"llmw workspace 管理\n<llmw:menu> compact", true, llmwCommand{verb: "compact"}},
		{"llmw workspace 管理\n<llmw:menu> new foo", true, llmwCommand{verb: "usage"}},
		// Case-insensitive command word; name keeps case (findWiki is CI).
		{"llmw workspace 管理\n<llmw:menu> List", true, llmwCommand{verb: "list"}},
		{"llmw workspace 管理\n<llmw:menu> enter Foo-Bar", true, llmwCommand{verb: "enter", name: "Foo-Bar"}},
		// Typed: bare /llmw is the ONLY accepted form (= status); every
		// subcommand form is redirected (hard cutover to /llmw_xxx).
		{"/llmw", true, llmwCommand{verb: "status"}},
		{"/llmw status", true, llmwCommand{verb: "moved"}},
		{"/llmw list", true, llmwCommand{verb: "moved"}},
		{"/llmw enter foo", true, llmwCommand{verb: "moved"}},
		{"/llmw stop foo", true, llmwCommand{verb: "moved"}},
		{"/llmw switch", true, llmwCommand{verb: "moved"}},
		{"/llmw detach", true, llmwCommand{verb: "moved"}},
		{"/llmw wiki --name=foo enter", true, llmwCommand{verb: "moved"}},
		{"/LLMW List", true, llmwCommand{verb: "moved"}},
		// Template-path syntax errors still consume the prompt → usage verb.
		{"llmw workspace 管理\n<llmw:menu> bogus", true, llmwCommand{verb: "usage"}},
		{"llmw workspace 管理\n<llmw:menu> status extra", true, llmwCommand{verb: "usage"}},
		{"llmw workspace 管理\n<llmw:menu> stop foo ing extra", true, llmwCommand{verb: "usage"}},
		{"llmw workspace 管理\n<llmw:menu> switch foo", true, llmwCommand{verb: "usage"}},
		{"llmw workspace 管理\n<llmw:menu> wiki --name=foo enter", true, llmwCommand{verb: "usage"}},
		// Strict boundary: not llmw commands.
		{"/llmwx", false, llmwCommand{}},
		{"/llmwlist", false, llmwCommand{}},
		{"/llmw_stop", false, llmwCommand{}}, // routed to the llmw_stop custom command, not this parser
		{"/wikis", false, llmwCommand{}},
		{"/enter foo", false, llmwCommand{}},
		{"hello world", false, llmwCommand{}},
		{"", false, llmwCommand{}},
		{"  ", false, llmwCommand{}},
	}
	for _, tc := range cases {
		got, ok := parseLlmwCommand(tc.prompt)
		if ok != tc.ok {
			t.Errorf("parseLlmwCommand(%q) ok = %v, want %v", tc.prompt, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseLlmwCommand(%q) = %+v, want %+v", tc.prompt, got, tc.want)
		}
	}
}

func TestNumeric(t *testing.T) {
	if !isNumeric("3") || isNumeric("3a") || isNumeric("") {
		t.Fatal("isNumeric wrong")
	}
	f := newSessionFacade(&llmwAgent{}, "llmw::")
	f.pending = &pendingSelection{
		list:     []wikiEntry{{Name: "a"}, {Name: "b"}},
		deadline: time.Now().Add(time.Minute),
	}
	if w, ok := f.numericSelectionLocked("2"); !ok || w.Name != "b" {
		t.Fatal("numeric selection 2 should pick b")
	}
	if _, ok := f.numericSelectionLocked("3"); ok {
		t.Fatal("out of range must fail")
	}
	f.pending = &pendingSelection{list: []wikiEntry{{Name: "a"}}, deadline: time.Now().Add(-time.Second)}
	if _, ok := f.numericSelectionLocked("1"); ok {
		t.Fatal("expired selection must fail")
	}
}

func sampleStatusJSON() string {
	uptime := 3600.0
	idle := 120.0
	ago := 90.0
	rows := []windowRow{
		{Wiki: "foo", Window: "foo-main", WindowID: "@3", Session: "llm_workspace",
			Dead: false, Backend: "claude", State: "working",
			UptimeSeconds: &uptime, IdleSeconds: &idle},
		{Wiki: "bar", Window: "bar-ingest", WindowID: "@5", Session: "llm_workspace",
			Dead: true, Backend: "opencode", State: "dead",
			DeadSecondsAgo: &ago},
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

func TestLlmwClientStatus(t *testing.T) {
	c := newLlmwClient()
	c.run = fakeRunner(sampleStatusJSON(), "")

	rows, err := c.status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].Wiki != "foo" || rows[0].State != "working" || rows[0].UptimeSeconds == nil {
		t.Fatalf("unexpected row 0: %+v", rows[0])
	}
	if !rows[1].Dead || rows[1].DeadSecondsAgo == nil {
		t.Fatalf("unexpected row 1: %+v", rows[1])
	}
}

func TestLlmwClientStatusBadJSON(t *testing.T) {
	c := newLlmwClient()
	c.run = fakeRunner("nope", "")
	if _, err := c.status(context.Background()); err == nil {
		t.Fatal("want error for bad JSON")
	}
}

func TestLlmwClientStopWikiArgs(t *testing.T) {
	var got [][]string
	c := newLlmwClient()
	c.run = func(_ context.Context, args ...string) ([]byte, error) {
		got = append(got, args)
		return []byte{}, nil
	}

	if err := c.stopWiki(context.Background(), "foo", ""); err != nil {
		t.Fatalf("stopWiki: %v", err)
	}
	if err := c.stopWiki(context.Background(), "foo", "ing"); err != nil {
		t.Fatalf("stopWiki: %v", err)
	}
	want0 := []string{"wiki", "--name=foo", "stop", "--yes"}
	want1 := []string{"wiki", "--name=foo", "stop", "--yes", "--window-suffix=ing"}
	if len(got) != 2 {
		t.Fatalf("want 2 invocations, got %d", len(got))
	}
	for i := range want0 {
		if got[0][i] != want0[i] {
			t.Fatalf("args0 = %v, want %v", got[0], want0)
		}
	}
	for i := range want1 {
		if got[1][i] != want1[i] {
			t.Fatalf("args1 = %v, want %v", got[1], want1)
		}
	}
}

func TestLlmwClientStopWikiErrorPassthrough(t *testing.T) {
	c := newLlmwClient()
	c.run = fakeRunner("", "llmw wiki --name=foo stop --yes: exit status 1: wiki 'foo' 有 2 个运行中的窗口：...\nhint: 加 --window-suffix=SUFFIX")
	err := c.stopWiki(context.Background(), "foo", "")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "--window-suffix") {
		t.Fatalf("llmw stderr (candidates + hint) must be preserved: %v", err)
	}
}

func TestRenderWindows(t *testing.T) {
	// Empty list.
	if s := renderWindows(nil, nil); !strings.Contains(s, "没有运行中的窗口") {
		t.Fatalf("empty render = %q", s)
	}
	// List with alive + dead rows + context field.
	rows := []windowRow{
		{Wiki: "foo", Window: "foo-main", Backend: "claude", State: "working",
			UptimeSeconds: ptrFloat(3700), IdleSeconds: ptrFloat(30)},
		{Wiki: "bar", Window: "bar-ingest", Backend: "opencode", State: "dead",
			Dead: true, DeadSecondsAgo: ptrFloat(2 * 86400)},
		{Wiki: "baz", Window: "baz-main", Backend: "opencode", State: "waiting"},
	}
	s := renderWindows(rows, map[string]int{"foo-main": 48941})
	for _, want := range []string{
		"foo (foo-main)", "claude", "working", "~48.9k", "up 1h", "idle now",
		"bar (bar-ingest)", "opencode", "dead", "exited 2d ago",
		"baz (baz-main)", "waiting", "ctx …",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("render missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "|") {
		t.Errorf("markdown table syntax leaked into list render:\n%s", s)
	}
}

// Context-size extraction: total wins, zero-token rows are skipped, and the
// fallback sums input+cache when total is absent.
func TestContextSizeFromExport(t *testing.T) {
	var e ocExport
	e.Messages = append(e.Messages, ocMessage{})
	e.Messages[0].Info.Role = "user"
	e.Messages[0].Parts = []ocPart{{Type: "text", Text: "hi"}}
	mk := func(role string, total, in, read int) ocMessage {
		m := ocMessage{}
		m.Info.Role = role
		m.Info.Tokens.Total = total
		m.Info.Tokens.Input = in
		m.Info.Tokens.Cache.Read = read
		return m
	}
	e.Messages = append(e.Messages, mk("assistant", 0, 0, 0)) // mid-turn zeros
	e.Messages = append(e.Messages, mk("assistant", 48941, 386, 48384))
	raw := marshalExport(t, e)
	n, err := contextSizeFromExport(raw)
	if err != nil || n != 48941 {
		t.Fatalf("contextSizeFromExport = (%d, %v), want (48941, nil)", n, err)
	}

	// No total → input + cache.read fallback.
	e2 := ocExport{}
	e2.Messages = append(e2.Messages, mk("assistant", 0, 386, 48384))
	n, err = contextSizeFromExport(marshalExport(t, e2))
	if err != nil || n != 48770 {
		t.Fatalf("fallback = (%d, %v), want (386+48384=48770, nil)", n, err)
	}

	// No assistant token data at all → error.
	e3 := ocExport{}
	e3.Messages = append(e3.Messages, mk("user", 1, 1, 1))
	if _, err := contextSizeFromExport(marshalExport(t, e3)); err == nil {
		t.Fatal("expected error with no assistant tokens")
	}
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{30, "now"}, {90, "1m"}, {3599, "59m"}, {3600, "1h"}, {90000, "1d"},
	}
	for _, tc := range cases {
		if got := fmtDur(tc.in); got != tc.want {
			t.Errorf("fmtDur(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func ptrFloat(v float64) *float64 { return &v }

func TestLlmwAgentModeSwitcher(t *testing.T) {
	a, _, _ := newTestAgent(t)

	if got := a.GetMode(); got != "build" {
		t.Fatalf("default mode = %q, want build", got)
	}
	a.SetMode("plan")
	if got := a.GetMode(); got != "plan" {
		t.Fatalf("mode after SetMode(plan) = %q", got)
	}
	// Unknown keys normalize to build, mirroring the other agents.
	a.SetMode("turbo")
	if got := a.GetMode(); got != "build" {
		t.Fatalf("mode after SetMode(turbo) = %q, want build fallback", got)
	}

	modes := a.PermissionModes()
	if len(modes) != 2 || modes[0].Key != "build" || modes[1].Key != "plan" {
		t.Fatalf("PermissionModes = %+v, want build+plan", modes)
	}
}

// The facade must delegate SetLiveMode to the attached pane session and
// return false when nothing is attached.
func TestFacadeSetLiveModeDelegation(t *testing.T) {
	a, _, runners := newTestAgent(t)
	f := newSessionFacade(a, "llmw:w:main")

	if f.SetLiveMode("plan") {
		t.Fatal("no window attached — must return false")
	}

	r := &fakePaneRunner{workDir: "/ws/foo"}
	r.mu.Lock()
	r.uiMode = "build"
	r.mu.Unlock()
	ps := newPaneSession(a.ctx, r, "/ws/foo")
	defer func() { _ = ps.Close() }()
	f.inner = ps

	if !f.SetLiveMode("plan") {
		t.Fatal("delegation to a live pane session must succeed")
	}
	if got := r.sentKeysLen(); got != 1 {
		t.Fatalf("sent keys = %d, want 1 Tab", got)
	}
	_ = runners
}

func TestLlmwAgentModelSwitcher(t *testing.T) {
	old := modelsListFn
	modelsListFn = func(context.Context) (string, error) {
		return "yzr-a/m1\nyzr-b/m2\nopencode-go/glm-5.3\n\nyzr-a/m1\n", nil
	}
	t.Cleanup(func() { modelsListFn = old })

	a, err := New(map[string]any{"cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag := a.(*llmwAgent)

	models := ag.AvailableModels(context.Background())
	if len(models) != 2 || models[0].Name != "yzr-a/m1" || models[1].Name != "yzr-b/m2" {
		t.Fatalf("models = %+v, want yzr-only deduped [yzr-a/m1 yzr-b/m2]", models)
	}
	// No live window: SetModel must not panic; the target becomes the
	// pending model reported by GetModel (applied on next window enter).
	ag.SetModel("p1/m1")
	if got := ag.GetModel(); got != "p1/m1" {
		t.Fatalf("GetModel = %q, want pending target p1/m1", got)
	}
}

// A /model switch before any window exists must be remembered and applied
// when the next window attaches (2026-08-30: it silently no-opped instead).
func TestLlmwAgentPendingModel(t *testing.T) {
	a, err := New(map[string]any{"cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag := a.(*llmwAgent)
	ag.SetModel("yzr-x/m1")
	if got := ag.TargetModel(); got != "yzr-x/m1" {
		t.Fatalf("TargetModel = %q, want yzr-x/m1", got)
	}
	if got := ag.GetModel(); got != "yzr-x/m1" {
		t.Fatalf("GetModel = %q, want pending target as fallback", got)
	}
}

// doEnterLocked aligns a fresh window with the pending model target.
func TestFacadeEnterAppliesPendingModel(t *testing.T) {
	a, _, panes := newTestAgent(t)
	a.SetModel("yzr-x/m1")

	f := newSessionFacade(a, "llmw:w:main")
	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("enter: %v", err)
	}
	waitResult(t, f.Events(), 5*time.Second)

	// The pane runner is created lazily by the enter itself; the pending
	// model must have driven the switch attempt on it (the default fake
	// capture never shows a "Select model" dialog, so the attempt aborts
	// with Escape after the wait timeout — proving the alignment ran).
	var fr *fakePaneRunner
	for _, r := range panes {
		fr = r
		break
	}
	if fr == nil {
		t.Fatal("no fake pane runner was created by the enter")
	}
	fr.mu.Lock()
	keys := append([]string(nil), fr.sentKeys...)
	texts := append([]string(nil), fr.sentTexts...)
	fr.mu.Unlock()
	want := []string{"C-x", "m", "Escape"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v (switch attempted then aborted)", keys, want)
	}
	for i := range keys {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
	// The fake pane never renders a model dialog, so the driver aborts
	// right after opening it — no search text is ever typed.
	if len(texts) != 0 {
		t.Fatalf("texts = %v, want none (dialog never opened)", texts)
	}
}

// ListSessions maps live llmw windows to switchable agent sessions: the
// main window becomes the canonical "llmw:<wiki>:" id (empty suffix), other
// suffixes keep their own ids, and dead/malformed rows are skipped.
func TestLlmwListSessions(t *testing.T) {
	a, runner, _ := newTestAgent(t)
	runner.mu.Lock()
	runner.windows = []windowRow{
		{Wiki: "agent-tools", Window: "agent-tools-main", Session: "1", State: "working", ActivityAt: int64ptr(1690000000)},
		{Wiki: "agent-tools", Window: "agent-tools-tg", Session: "1", State: "waiting", ActivityAt: int64ptr(1690000100)},
		{Wiki: "bar", Window: "bar-main", Session: "2", Dead: true},
		{Wiki: "foo", Window: "other-window", Session: "3"},
	}
	runner.mu.Unlock()

	got, err := a.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("sessions = %+v, want 2 entries", got)
	}
	if got[0].ID != "llmw:agent-tools:" || got[0].Summary != "agent-tools · working" {
		t.Fatalf("entry0 = %+v, want id llmw:agent-tools: summary 'agent-tools · working'", got[0])
	}
	if got[1].ID != "llmw:agent-tools:tg" || got[1].Summary != "agent-tools (tg) · waiting" {
		t.Fatalf("entry1 = %+v, want id llmw:agent-tools:tg summary 'agent-tools (tg) · waiting'", got[1])
	}
	if !got[1].ModifiedAt.Equal(time.Unix(1690000100, 0)) {
		t.Fatalf("entry1 ModifiedAt = %v, want from ActivityAt", got[1].ModifiedAt)
	}

	// Status errors must not break the /switch card.
	runner.mu.Lock()
	runner.errFor = map[string]string{"status": "boom"}
	runner.mu.Unlock()
	if got, err := a.ListSessions(context.Background()); err != nil || got != nil {
		t.Fatalf("ListSessions on status error = (%v, %v), want (nil, nil)", got, err)
	}
}

func int64ptr(v int64) *int64 { return &v }

// StartSession on a /switch-produced id ("llmw:<wiki>:<suffix>") must
// reattach the facade to that wiki's window (the /switch round-trip).
// Post-restart (or engine /switch) StartSession must NOT implicitly resume:
// the facade is created unbound even when the named window is live, so
// binding is always explicit (/llmw enter / /llmw switch). Regression guard
// for the "stopped window resurrects after daemon restart" bug.
func TestStartSessionUnboundNoImplicitResume(t *testing.T) {
	a, runner, panes := newTestAgent(t)
	runner.mu.Lock()
	runner.windows = []windowRow{
		{Wiki: "foo", Window: "foo-tg", WindowID: "@foo1", Session: "llm_workspace", Backend: "opencode", State: "waiting"},
	}
	runner.mu.Unlock()

	sess, err := a.StartSession(context.Background(), "llmw:foo:tg")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	f, ok := sess.(*sessionFacade)
	if !ok {
		t.Fatalf("StartSession returned %T", sess)
	}
	if got := f.CurrentSessionID(); got != "llmw:foo:tg" {
		t.Fatalf("facade id = %q, want llmw:foo:tg", got)
	}
	if panes["foo"] != nil {
		t.Fatal("implicit resume spawned a pane; facade must start unbound")
	}
	f.mu.Lock()
	bound := f.wiki != nil
	f.mu.Unlock()
	if bound {
		t.Fatal("facade must be unbound after StartSession; use /llmw enter")
	}
}

// Regression (2026-09-05): the engine's /model flow recycles the
// interactive session (cleanupInteractiveState → facade.Close) on the
// assumption that agents restart as headless processes with
// `--resume <id> --model <new>`. For the pane driver that close dropped
// the wiki binding and the next message hit "请先 /llmw list 选择 wiki".
// StartSession must silently re-enter the remembered in-process binding.
func TestStartSessionReentersRememberedBinding(t *testing.T) {
	a, _, panes := newTestAgent(t)

	// Engine starts the session (persisted id), user enters foo.
	sess, err := a.StartSession(context.Background(), "llmw:foo:")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	f := sess.(*sessionFacade)
	defer f.Close()
	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send enter: %v", err)
	}
	if ev := waitEvent(f.events, 3*time.Second); ev == nil || !contains(ev.Content, "已进入 wiki") {
		t.Fatalf("want entry confirmation, got %q", evStr(ev))
	}

	// The engine closes the facade (the /model flow).
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Next message: StartSession must re-enter the remembered binding.
	sess2, err := a.StartSession(context.Background(), "llmw:foo:")
	if err != nil {
		t.Fatalf("StartSession 2: %v", err)
	}
	f2 := sess2.(*sessionFacade)
	defer f2.Close()
	if err := f2.Send("hello again", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := panes["foo"].lastInjected(); got != "hello again" {
		t.Fatalf("injected = %q, want the message forwarded to the re-entered window", got)
	}
}

// Detach must clear the remembered binding: the engine may still recycle
// the session afterwards, and a stale memory would resurrect the binding
// the user explicitly dropped.
func TestStartSessionDoesNotResurrectAfterDetach(t *testing.T) {
	a, _, _ := newTestAgent(t)
	sess, _ := a.StartSession(context.Background(), "llmw:foo:")
	f := sess.(*sessionFacade)
	defer f.Close()
	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send enter: %v", err)
	}
	waitEvent(f.events, 3*time.Second)
	if err := f.Send(llmwCmd("detach"), "", nil, nil); err != nil {
		t.Fatalf("Send detach: %v", err)
	}
	if ev := waitEvent(f.events, 3*time.Second); ev == nil || !contains(ev.Content, "已解绑") {
		t.Fatalf("want detach confirmation, got %q", evStr(ev))
	}
	f.Close()

	sess2, _ := a.StartSession(context.Background(), "llmw:foo:")
	f2 := sess2.(*sessionFacade)
	defer f2.Close()
	if err := f2.Send("hello", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f2.events, 3*time.Second); ev == nil || !contains(ev.Content, "请先 /llmw list") {
		t.Fatalf("detached binding must stay unbound, got %q", evStr(ev))
	}
}

// Stopping the own main window unbinds — the remembered binding must go
// with it, or a later engine session recycle would re-create the window
// the user killed.
func TestStartSessionDoesNotResurrectAfterStopOwnMain(t *testing.T) {
	a, _, _ := newTestAgent(t)
	sess, _ := a.StartSession(context.Background(), "llmw:foo:")
	f := sess.(*sessionFacade)
	defer f.Close()
	if err := f.Send(llmwCmd("enter foo"), "", nil, nil); err != nil {
		t.Fatalf("Send enter: %v", err)
	}
	waitEvent(f.events, 3*time.Second)
	if err := f.Send(llmwCmd("stop foo"), "", nil, nil); err != nil {
		t.Fatalf("Send stop: %v", err)
	}
	waitEvent(f.events, 3*time.Second)
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send confirm: %v", err)
	}
	if ev := waitEvent(f.events, 3*time.Second); ev == nil || !contains(ev.Content, "已解绑") {
		t.Fatalf("want unbind after stop, got %q", evStr(ev))
	}
	f.Close()

	sess2, _ := a.StartSession(context.Background(), "llmw:foo:")
	f2 := sess2.(*sessionFacade)
	defer f2.Close()
	if err := f2.Send("hello", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := waitEvent(f2.events, 3*time.Second); ev == nil || !contains(ev.Content, "请先 /llmw list") {
		t.Fatalf("stopped main window must stay unbound, got %q", evStr(ev))
	}
}

// Binding-memory lifecycle: entering a different wiki forgets the old
// facade id; stopping the own suffix window moves the memory to main.
func TestBindingMemoryLifecycle(t *testing.T) {
	a, _, _ := newTestAgent(t)
	sess, _ := a.StartSession(context.Background(), "")
	f := sess.(*sessionFacade)
	defer f.Close()
	enter := func(args string) {
		t.Helper()
		if err := f.Send(llmwCmd(args), "", nil, nil); err != nil {
			t.Fatalf("Send %q: %v", args, err)
		}
		if ev := waitEvent(f.events, 3*time.Second); ev == nil || !contains(ev.Content, "已进入 wiki") {
			t.Fatalf("enter %q: want confirmation, got %q", args, evStr(ev))
		}
	}
	enter("enter foo")
	if !a.bindingRemembered("llmw:foo:") {
		t.Fatal("enter must remember the binding")
	}
	enter("enter bar")
	if a.bindingRemembered("llmw:foo:") {
		t.Fatal("switching wikis must forget the old facade id binding")
	}
	if !a.bindingRemembered("llmw:bar:") {
		t.Fatal("switching wikis must remember the new binding")
	}
	enter("enter foo tg")
	if !a.bindingRemembered("llmw:foo:tg") {
		t.Fatal("suffix enter must remember its binding")
	}
	if err := f.Send(llmwCmd("stop foo tg"), "", nil, nil); err != nil {
		t.Fatalf("Send stop: %v", err)
	}
	waitEvent(f.events, 3*time.Second)
	if err := f.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send confirm: %v", err)
	}
	if ev := waitEvent(f.events, 3*time.Second); ev == nil || !contains(ev.Content, "换绑 main") {
		t.Fatalf("want suffix-stop rebind notice, got %q", evStr(ev))
	}
	if a.bindingRemembered("llmw:foo:tg") {
		t.Fatal("stopping the suffix window must forget its binding")
	}
	if !a.bindingRemembered("llmw:foo:") {
		t.Fatal("stopping the suffix window must remember the main binding")
	}
}

// Regression (2026-09-05): stopping a window that belongs to ANOTHER
// session's binding must clear that window's binding memory too — a facade
// closed by /model recycle would otherwise re-enter (and re-CREATE) the
// window the user stopped. The live-facade ensureInnerLocked rebuild
// semantic is unchanged.
func TestStopOtherSessionWindowClearsBindingMemory(t *testing.T) {
	a, _, _ := newTestAgent(t)

	// Session one binds foo/tg — the binding memory is recorded.
	sess, _ := a.StartSession(context.Background(), "first")
	f := sess.(*sessionFacade)
	defer f.Close()
	if err := f.Send(llmwCmd("enter foo tg"), "", nil, nil); err != nil {
		t.Fatalf("Send enter: %v", err)
	}
	if ev := waitEvent(f.events, 3*time.Second); ev == nil || !contains(ev.Content, "已进入 wiki") {
		t.Fatalf("want entry confirmation, got %q", evStr(ev))
	}
	if !a.bindingRemembered("llmw:foo:tg") {
		t.Fatal("enter must remember the binding")
	}

	// A second, unbound session stops that window (default stop branch).
	sess2, _ := a.StartSession(context.Background(), "second")
	f2 := sess2.(*sessionFacade)
	defer f2.Close()
	if err := f2.Send(llmwCmd("stop foo tg"), "", nil, nil); err != nil {
		t.Fatalf("Send stop: %v", err)
	}
	waitEvent(f2.events, 3*time.Second)
	if err := f2.Send("y", "", nil, nil); err != nil {
		t.Fatalf("Send confirm: %v", err)
	}
	if ev := waitEvent(f2.events, 3*time.Second); ev == nil || !contains(ev.Content, "主机窗口") {
		t.Fatalf("want foreign-window stop notice, got %q", evStr(ev))
	}
	if a.bindingRemembered("llmw:foo:tg") {
		t.Fatal("stopping another session's window must clear its binding memory")
	}
}

// installCommandFiles: the four menu templates self-install, idempotently
// refresh a known-legacy llmw.md, and keep user-modified files with a warning.
func TestInstallCommandFiles(t *testing.T) {
	a := &llmwAgent{dataDir: t.TempDir()}
	if err := a.installCommands(); err != nil {
		t.Fatalf("installCommands: %v", err)
	}
	dir := a.commandsDir
	for name, want := range map[string]string{
		"llmw.md":         menuCommand,
		"llmw_list.md":    listMenuCommand,
		"llmw_switch.md":  switchMenuCommand,
		"llmw_stop.md":    stopMenuCommand,
		"llmw_detach.md":  detachMenuCommand,
		"llmw_enter.md":   enterMenuCommand,
		"llmw_abort.md":   abortMenuCommand,
		"llmw_new.md":     newMenuCommand,
		"llmw_compact.md": compactMenuCommand,
	} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if string(data) != want {
			t.Fatalf("%s content drifted: got %q want %q", name, data, want)
		}
	}
	// Legacy llmw.md (pre-switch description) is refreshed in place.
	if err := os.WriteFile(filepath.Join(dir, "llmw.md"), []byte(menuCommandLegacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.installCommands(); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "llmw.md"))
	if string(data) != menuCommand {
		t.Fatalf("legacy llmw.md not refreshed: %q", data)
	}
	// User-modified file is preserved.
	custom := "my own desc\n<llmw:menu> list"
	if err := os.WriteFile(filepath.Join(dir, "llmw_list.md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.installCommands(); err != nil {
		t.Fatalf("reinstall 2: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "llmw_list.md"))
	if string(data) != custom {
		t.Fatalf("user-modified llmw_list.md must be kept: %q", data)
	}
}

// CompressCommand wires the engine's /compress to the opencode TUI compact.
func TestLlmwAgentCompressCommand(t *testing.T) {
	a, err := New(map[string]any{"cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := a.(interface{ CompressCommand() string }).CompressCommand(); got != "/compact" {
		t.Fatalf("CompressCommand = %q, want /compact", got)
	}
}
