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
	// Command dir installed with wikis.md.
	md := filepath.Join(ag.commandsDir, "wikis.md")
	data, err := os.ReadFile(md)
	if err != nil {
		t.Fatalf("wikis.md not installed: %v", err)
	}
	if !strings.Contains(string(data), wikisSentinel) {
		t.Fatalf("wikis.md missing sentinel: %q", data)
	}
	// CommandDirs.
	if len(ag.CommandDirs()) != 1 || ag.CommandDirs()[0] != ag.commandsDir {
		t.Fatalf("unexpected CommandDirs: %v", ag.CommandDirs())
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

func TestCommandRecognition(t *testing.T) {
	cases := []struct {
		prompt string
		wikis  bool
		enter  bool
		name   string
	}{
		{"<llmw:wikis>", true, false, ""},
		{"列出当前 workspace 的 wiki（回复序号进入）\n<llmw:wikis>", true, false, ""},
		{"/wikis", true, false, ""},
		{"/wikis 2", true, false, ""},
		{"hello world", false, false, ""},
		{"/enter foo", false, true, "foo"},
		{"/enter  kv-store", false, true, "kv-store"},
		{"/enter", false, true, ""},
		{"/enterprise", false, false, ""},
	}
	for _, tc := range cases {
		if got := isWikisCommand(tc.prompt); got != tc.wikis {
			t.Errorf("isWikisCommand(%q) = %v, want %v", tc.prompt, got, tc.wikis)
		}
		name, ok := enterCommandName(tc.prompt)
		if ok != tc.enter || name != tc.name {
			t.Errorf("enterCommandName(%q) = (%q,%v), want (%q,%v)", tc.prompt, name, ok, tc.name, tc.enter)
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
