// Package llmw integrates the llm-workspace-cli (llmw) workspace into
// cc-connect as a first-class agent.
//
// The agent does not talk to claude/opencode directly: it wraps the existing
// claudecode and opencode agents from the same repository and delegates all
// protocol, permission and lifecycle handling to them. What llmw adds is a
// "policy layer":
//
//   - `/wikis` lists the wikis of the current llmw workspace (via `llmw list --json`)
//   - entering a wiki spawns the wrapped backend agent with cwd = wiki dir,
//     so the CLI natively reads the model overlay written by llmw
//     (.claude/settings.local.json for claude, opencode.json for opencode)
//   - switching wiki closes the old wrapped session and spawns a new one
//   - the agent session ID encodes the bound wiki ("llmw:<wiki>:<innerID>"),
//     so the binding survives cc-connect restarts via the engine's session
//     persistence.
package llmw

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/chenhg5/cc-connect/agent/claudecode"
	"github.com/chenhg5/cc-connect/agent/opencode"
	"github.com/chenhg5/cc-connect/core"
)

const (
	agentName      = "llmw"
	defaultBackend = "claude"
	commandsSubdir = "llmw-commands"
	menuSentinel   = "<llmw:menu>"

	// menuCommand is the content of the /llmw agent command file, written to
	// the commands dir at New(). First line becomes the command description in
	// platform menus; the sentinel line lets Send() recognise the expansion.
	menuCommand = "llmw workspace 窗口与会话管理（status/list/enter/stop）\n<llmw:menu>"

	// legacyWikisCommand is the v2 content of wikis.md (superseded by the
	// /llmw prefix family, design §7.5). installCommands removes wikis.md
	// when its content still matches this constant (self-cleanup of the
	// agent's own artifact); user-modified files are kept with a warning.
	legacyWikisCommand = "列出当前 workspace 的 wiki（回复序号进入）\n<llmw:wikis>"
)

func init() {
	core.RegisterAgent(agentName, New)
}

// llmwAgent implements core.Agent. It wraps a backend agent (claudecode or
// opencode) per bound wiki and keeps one sessionFacade per engine session ID.
type llmwAgent struct {
	mu          sync.Mutex
	backend     string
	baseOpts    map[string]any // forwarded opts with work_dir and model stripped
	client      *llmwClient
	sessions    map[string]*sessionFacade
	commandsDir string
	ctx         context.Context
	cancel      context.CancelFunc

	// innerFactory is injectable for tests. It mirrors the real factory
	// selection done by newInnerAgent.
	innerFactory func(opts map[string]any) (core.Agent, error)
}

// New implements core.AgentFactory for agent type "llmw".
//
// opts keys:
//   - "backend": "claude" (default) or "opencode" — which wrapped agent to use
//   - everything else (cc_data_dir, cc_project, mode, ...) is forwarded to the
//     wrapped agent factory, except "work_dir" and "model" which are stripped:
//     work_dir is set per wiki, model must come from the llmw overlay (the
//     CLI reads it natively when cwd is the wiki dir).
func New(opts map[string]any) (core.Agent, error) {
	backend, _ := opts["backend"].(string)
	if backend == "" {
		backend = defaultBackend
	}
	if backend != "claude" && backend != "opencode" {
		return nil, fmt.Errorf("llmw: unsupported backend %q (supported: claude, opencode)", backend)
	}

	base := make(map[string]any, len(opts))
	for k, v := range opts {
		if k == "work_dir" || k == "model" {
			continue
		}
		base[k] = v
	}

	a := &llmwAgent{
		backend:  backend,
		baseOpts: base,
		client:   newLlmwClient(),
		sessions: make(map[string]*sessionFacade),
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.innerFactory = a.realInnerFactory

	if err := a.installCommands(); err != nil {
		slog.Warn("llmw: install commands", "err", err)
	}
	return a, nil
}

func (a *llmwAgent) Name() string { return agentName }

// StartSession creates or resumes a sessionFacade for the engine session.
// sessionID may be empty (brand-new session) or a previously persisted
// "llmw:<wiki>:<innerID>" id, in which case the wiki binding is restored.
func (a *llmwAgent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if f, ok := a.sessions[sessionID]; ok {
		return f, nil
	}
	f := newSessionFacade(a, sessionID)
	a.sessions[sessionID] = f

	wikiName, innerID, ours := parseFacadeID(sessionID)
	if ours && wikiName != "" {
		w, err := a.findWiki(ctx, wikiName)
		if err != nil {
			slog.Warn("llmw: resume failed, wiki not found", "wiki", wikiName, "err", err)
			return f, nil
		}
		f.resumeIntoLocked(ctx, w, innerID)
	}
	return f, nil
}

// ValidateSessionID implements core.SessionIDValidator so the engine does not
// clear our persisted session IDs.
func (a *llmwAgent) ValidateSessionID(_ context.Context, id string) bool {
	_, _, ours := parseFacadeID(id)
	return ours
}

// ListSessions reports no backend-level sessions: sessions are managed by the
// engine per IM session, so the /list command shows nothing extra for llmw.
func (a *llmwAgent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}

func (a *llmwAgent) Stop() error {
	a.mu.Lock()
	var all []*sessionFacade
	for _, f := range a.sessions {
		all = append(all, f)
	}
	a.mu.Unlock()

	a.cancel()
	var errs []error
	for _, f := range all {
		if err := f.closeLocked(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("llmw: stop: %v", errs)
	}
	return nil
}

// removeFacade drops a facade from the session map (called from closeLocked).
func (a *llmwAgent) removeFacade(f *sessionFacade) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.sessions {
		if v == f {
			delete(a.sessions, k)
			return
		}
	}
}

// CommandDirs implements core.CommandProvider so the /wikis command appears in
// the command list and in platform menus (e.g. Telegram setMyCommands).
func (a *llmwAgent) CommandDirs() []string {
	return []string{a.commandsDir}
}

// installCommands writes the /llmw agent command file into a writable
// directory (under cc_data_dir) that the engine scans for agent commands.
// Existing files are not overwritten so users can customise them. It also
// removes the superseded v2 wikis.md when it still holds the agent-generated
// constant (hard cutover, design §7.5).
func (a *llmwAgent) installCommands() error {
	dataDir, _ := a.baseOpts["cc_data_dir"].(string)
	if dataDir == "" {
		cfgDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("llmw: no cc_data_dir and user config dir unavailable: %w", err)
		}
		dataDir = filepath.Join(cfgDir, "cc-connect")
	}
	dir := filepath.Join(dataDir, commandsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("llmw: mkdir commands dir: %w", err)
	}
	path := filepath.Join(dir, "llmw.md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(menuCommand), 0o644); err != nil {
			return fmt.Errorf("llmw: write llmw.md: %w", err)
		}
	}
	a.cleanupLegacyWikisCommand(dir)
	a.commandsDir = dir
	return nil
}

// cleanupLegacyWikisCommand deletes the v2 wikis.md command file when it still
// contains the agent-generated constant, so the platform command menu no
// longer advertises the removed /wikis command. A user-modified file is kept
// (it may hold custom content) with a warning.
func (a *llmwAgent) cleanupLegacyWikisCommand(dir string) {
	path := filepath.Join(dir, "wikis.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return // absent: nothing to clean
	}
	if string(data) == legacyWikisCommand {
		if err := os.Remove(path); err != nil {
			slog.Warn("llmw: remove legacy wikis.md", "err", err)
		}
		return
	}
	slog.Warn("llmw: legacy wikis.md kept (user-modified); /wikis no longer advertised, remove manually if unwanted", "path", path)
}

// realInnerFactory builds the wrapped backend agent with opts for the wiki.
func (a *llmwAgent) realInnerFactory(opts map[string]any) (core.Agent, error) {
	if a.backend == "opencode" {
		return opencode.New(opts)
	}
	return claudecode.New(opts)
}

// newInnerAgent returns a wrapped backend agent whose work_dir is the given wiki.
func (a *llmwAgent) newInnerAgent(w *wikiEntry) (core.Agent, error) {
	root, err := a.client.workspaceRoot()
	if err != nil {
		return nil, err
	}
	opts := make(map[string]any, len(a.baseOpts)+1)
	for k, v := range a.baseOpts {
		opts[k] = v
	}
	opts["work_dir"] = filepath.Join(root, w.Path)
	return a.innerFactory(opts)
}

// findWiki resolves a wiki by name or display_name (exact or case-insensitive).
func (a *llmwAgent) findWiki(ctx context.Context, name string) (*wikiEntry, error) {
	list, err := a.client.list(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if matchWikiName(&list[i], name) {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("wiki %q not found", name)
}

func matchWikiName(w *wikiEntry, name string) bool {
	if w.Name == name || w.DisplayName == name {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.ToLower(w.Name) == lower || strings.ToLower(w.DisplayName) == lower
}

// parseFacadeID parses "llmw:<wiki>:<innerID>". Returns ours=false when the
// id is not an llmw id.
func parseFacadeID(id string) (wiki, innerID string, ours bool) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) < 2 || parts[0] != "llmw" {
		return "", "", false
	}
	wiki = parts[1]
	if len(parts) == 3 {
		innerID = parts[2]
	}
	return wiki, innerID, true
}
