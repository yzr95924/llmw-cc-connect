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
	var got []string
	c := newLlmwClient()
	c.run = func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte{}, nil
	}

	if err := c.enterWiki(context.Background(), "kv-store"); err != nil {
		t.Fatalf("enterWiki: %v", err)
	}
	want := []string{"wiki", "--name=kv-store", "enter"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
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

func TestLlmwClientEnterByobuEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMW_WORKSPACE", dir)
	if err := os.WriteFile(filepath.Join(dir, "workspace.toml"), []byte("# t"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newLlmwClient()

	// Missing file → false.
	if c.enterByobuEnabled() {
		t.Fatal("want false for missing workspace_local.toml")
	}
	// Key present without value → false.
	if err := os.WriteFile(filepath.Join(dir, "workspace_local.toml"),
		[]byte("schema_version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c.enterByobuEnabled() {
		t.Fatal("want false when enter_byobu absent")
	}
	// enter_byobu = true → true.
	if err := os.WriteFile(filepath.Join(dir, "workspace_local.toml"),
		[]byte("schema_version = 1\nenter_byobu = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !c.enterByobuEnabled() {
		t.Fatal("want true when enter_byobu = true")
	}
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
	if ag.backend != defaultBackend {
		t.Fatalf("backend = %q, want %q", ag.backend, defaultBackend)
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

func TestNewStripsModelAndWorkDir(t *testing.T) {
	a, err := New(map[string]any{
		"model":       "claude-opus-4-7",
		"work_dir":    "/tmp/whatever",
		"mode":        "default",
		"cc_data_dir": t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag := a.(*llmwAgent)
	if _, ok := ag.baseOpts["model"]; ok {
		t.Fatal("model must be stripped")
	}
	if _, ok := ag.baseOpts["work_dir"]; ok {
		t.Fatal("work_dir must be stripped")
	}
	if ag.baseOpts["mode"] != "default" {
		t.Fatalf("mode must be forwarded, got %v", ag.baseOpts["mode"])
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
		// Bare + short forms.
		{"/llmw", true, llmwCommand{verb: "status"}},
		{"/llmw status", true, llmwCommand{verb: "status"}},
		{"/llmw list", true, llmwCommand{verb: "list"}},
		{"/llmw enter foo", true, llmwCommand{verb: "enter", name: "foo"}},
		{"/llmw stop foo", true, llmwCommand{verb: "stop", name: "foo"}},
		{"/llmw stop foo ingest", true, llmwCommand{verb: "stop", name: "foo", suffix: "ingest"}},
		// Case-insensitive command word; name keeps case (findWiki is CI).
		{"/LLMW List", true, llmwCommand{verb: "list"}},
		{"/llmw enter Foo-Bar", true, llmwCommand{verb: "enter", name: "Foo-Bar"}},
		// CLI-native form.
		{"/llmw wiki --name=foo enter", true, llmwCommand{verb: "enter", name: "foo"}},
		{"/llmw wiki --name=foo stop", true, llmwCommand{verb: "stop", name: "foo"}},
		{"/llmw wiki --name=foo stop --window-suffix=ing --yes", true, llmwCommand{verb: "stop", name: "foo", suffix: "ing"}},
		{"/llmw wiki stop --name=foo -y", true, llmwCommand{verb: "stop", name: "foo"}},
		// Syntax errors still consume the prompt → usage verb.
		{"/llmw bogus", true, llmwCommand{verb: "usage"}},
		{"/llmw status extra", true, llmwCommand{verb: "usage"}},
		{"/llmw enter", true, llmwCommand{verb: "usage"}},
		{"/llmw stop", true, llmwCommand{verb: "usage"}},
		{"/llmw wiki", true, llmwCommand{verb: "usage"}},
		{"/llmw wiki --name=foo", true, llmwCommand{verb: "usage"}},
		{"/llmw wiki --name=foo show", true, llmwCommand{verb: "usage"}},
		{"/llmw wiki --name=foo enter --dry-run", true, llmwCommand{verb: "usage"}},
		// Strict boundary: not llmw commands.
		{"/llmwx", false, llmwCommand{}},
		{"/llmwlist", false, llmwCommand{}},
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
	// Empty table.
	if s := renderWindows(nil); !strings.Contains(s, "没有运行中的窗口") {
		t.Fatalf("empty render = %q", s)
	}
	// Table with alive + dead rows.
	rows := []windowRow{
		{Wiki: "foo", Window: "foo-main", Backend: "claude", State: "working",
			UptimeSeconds: ptrFloat(3700), IdleSeconds: ptrFloat(30)},
		{Wiki: "bar", Window: "bar-ingest", Backend: "opencode", State: "dead",
			Dead: true, DeadSecondsAgo: ptrFloat(2 * 86400)},
	}
	s := renderWindows(rows)
	for _, want := range []string{"WIKI", "foo-main", "working", "1h", "exited 2d ago", "dead"} {
		if !strings.Contains(s, want) {
			t.Errorf("render missing %q:\n%s", want, s)
		}
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
