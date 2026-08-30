// Package llmw integrates the llm-workspace-cli (llmw) workspace into
// cc-connect as a first-class agent, driving the opencode TUI that llmw
// manages inside byobu windows (v3 pane driver — opencode is the only
// supported backend):
//
//   - `/llmw` lists / enters the wikis of the current llmw workspace (via
//     `llmw list/status/wiki enter --json`); entering means attaching the
//     facade to the wiki's byobu window and its opencode TUI pane
//   - prompts are injected with tmux paste-buffer (bracketed paste keeps
//     multi-line text intact), turn completion is detected from the pane's
//     busy marker, and replies are recovered from `opencode export`
//   - permission dialogs in the TUI surface as IM approval buttons (the
//     choice is injected back as Left/Right + Enter key presses)
//   - the agent session ID encodes the bound wiki ("llmw:<wiki>:<pane>"),
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
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	agentName      = "llmw"
	defaultBackend = "opencode"
	commandsSubdir = "llmw-commands"
	menuSentinel   = "<llmw:menu>"

	// menuCommand is the content of the /llmw agent command file, written to
	// the commands dir at New(). First line becomes the command description in
	// platform menus; the sentinel line lets Send() recognise the expansion.
	// The three subcommand aliases (list/switch/stop) exist because Telegram
	// menu taps cannot carry arguments; engine /switch and /list are disabled
	// for llmw (their rebind relies on the implicit resume we removed), so
	// the /llmw family must be self-sufficient in the command menu.
	menuCommand          = "llmw 窗口与会话状态总览\n<llmw:menu>"
	menuCommandLegacyR23 = "llmw workspace 窗口与会话管理（status/list/enter/stop/switch/detach）\n<llmw:menu>"
	menuCommandLegacy    = "llmw workspace 窗口与会话管理（status/list/enter/stop/switch）\n<llmw:menu>"
	menuCommandLegacyV1  = "llmw workspace 窗口与会话管理（status/list/enter/stop）\n<llmw:menu>"
	listMenuCommand      = "列出 workspace 的 wiki（回复序号进入）\n<llmw:menu> list"
	switchMenuCommand    = "切换到其它主机窗口（回复序号）\n<llmw:menu> switch"
	stopMenuCommand      = "退出当前绑定的 wiki 窗口（关闭窗口，先确认）\n<llmw:menu> stop"
	stopMenuCommandV1    = "退出当前绑定的 wiki 窗口\n<llmw:menu> stop"
	detachMenuCommand    = "解绑当前会话（保留主机窗口）\n<llmw:menu> detach"
	enterMenuCommand     = "进入 wiki（手敲 /llmw_enter <名> [suffix]）\n<llmw:menu> enter"
	abortMenuCommand     = "中止当前窗口进行中的回合（发送 ESC）\n<llmw:menu> abort"
	newMenuCommand       = "开启新会话（当前窗口，旧会话保留可续）\n<llmw:menu> new"

	// legacyWikisCommand is the v2 content of wikis.md (superseded by the
	// /llmw prefix family, design §7.5). installCommands removes wikis.md
	// when its content still matches this constant (self-cleanup of the
	// agent's own artifact); user-modified files are kept with a warning.
	legacyWikisCommand = "列出当前 workspace 的 wiki（回复序号进入）\n<llmw:wikis>"
)

func init() {
	core.RegisterAgent(agentName, New)
}

// llmwAgent implements core.Agent. It keeps one sessionFacade per engine
// session ID; each facade drives the opencode TUI inside an llmw-managed
// byobu window (design v3: pane driver, zero headless subprocess).
type llmwAgent struct {
	mu          sync.Mutex
	client      *llmwClient
	sessions    map[string]*sessionFacade
	commandsDir string
	ctx         context.Context
	cancel      context.CancelFunc
	// mode is the target opencode subagent ("" = "build" | "plan"), stored
	// atomically: it is read from doEnterLocked under a.mu, so it must not
	// take the same lock.
	mode atomic.Value
	// model is the pending target model id ("provider/model"), same
	// atomicity rationale as mode: SetModel may run before any facade
	// exists (the engine allows /model right after startup) and the target
	// is applied when the next window attaches (doEnterLocked).
	model atomic.Value

	// paneFactory is injectable for tests. Production builds a paneSession
	// over the real tmux server and the opencode CLI.
	paneFactory func(ctx context.Context, target, workDir string) core.AgentSession

	// dataDir is the cc-connect data dir (opts cc_data_dir) where the
	// /llmw command file is installed.
	dataDir string
}

// SetMode stores the target opencode subagent (build/plan). The engine
// applies it to the running window via LiveModeSwitcher (hot Tab switch in
// the shared TUI); fresh windows also start in the target mode (doEnterLocked).
func (a *llmwAgent) SetMode(mode string) {
	m := normalizePaneMode(mode)
	a.mode.Store(m)
	slog.Info(agentName+": mode changed", "mode", m)
}

// GetMode returns the current target subagent ("build" when unset).
func (a *llmwAgent) GetMode() string {
	if m, _ := a.mode.Load().(string); m != "" {
		return m
	}
	return "build"
}

// PermissionModes implements core.ModeSwitcher: the two built-in opencode
// subagents. Build is the full-access default; Plan is read-only planning.
func (a *llmwAgent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "build", Name: "Build", NameZh: "构建模式", Desc: "Full tool access (opencode default)", DescZh: "完整工具权限（默认）"},
		{Key: "plan", Name: "Plan", NameZh: "规划模式", Desc: "Read-only planning, no execution", DescZh: "只读规划，不执行修改"},
	}
}

// New implements core.AgentFactory for agent type "llmw".
//
// opts keys:
//   - "backend": only "opencode" is accepted (v3 pane driver is
//     llmw window. v3 drives the in-window TUI; only opencode is wired so far
//     (claude needs its own state patterns and reply source, see design doc).
//   - everything else is accepted and ignored (the pane driver needs no
//     per-backend agent options; the wiki overlay configures the TUI).
func New(opts map[string]any) (core.Agent, error) {
	backend, _ := opts["backend"].(string)
	if backend == "" {
		backend = defaultBackend
	}
	if backend != "opencode" {
		return nil, fmt.Errorf("llmw: unsupported backend %q (v3 pane driver supports: opencode)", backend)
	}

	a := &llmwAgent{
		client:   newLlmwClient(),
		sessions: make(map[string]*sessionFacade),
	}
	a.dataDir, _ = opts["cc_data_dir"].(string)
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.paneFactory = func(ctx context.Context, target, workDir string) core.AgentSession {
		return newPaneSession(ctx, &realPaneRunner{target: target, workDir: workDir}, workDir)
	}

	if err := a.installCommands(); err != nil {
		slog.Warn("llmw: install commands", "err", err)
	}
	return a, nil
}

func (a *llmwAgent) Name() string { return agentName }

// StartSession creates or resumes a sessionFacade for the engine session.
// sessionID may be empty (brand-new session) or a previously persisted
// "llmw:<wiki>:<suffix>" id, in which case the wiki binding is restored
// (the window itself survives daemon restarts in byobu; re-enter reattaches).
func (a *llmwAgent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if f, ok := a.sessions[sessionID]; ok {
		return f, nil
	}
	f := newSessionFacade(a, sessionID)
	a.sessions[sessionID] = f

	// Deliberately NO implicit resume here. cc-connect calls StartSession
	// identically for post-restart recovery and for engine /switch rebinds,
	// and the engine's persisted agent_session_id cannot be cleared from the
	// agent side — so a resume here would resurrect windows the user stopped
	// and silently re-attach after every daemon restart. Binding is instead
	// always explicit: the facade starts unbound and the user /llmw enters
	// (or /llmw switch → number) to attach. See README "重启后绑定作废".
	if ours := func() bool { _, _, ok := parseFacadeID(sessionID); return ok }(); ours {
		slog.Info("llmw: session created unbound (explicit enter required)", "id", sessionID)
	}
	return f, nil
}

// ValidateSessionID implements core.SessionIDValidator so the engine does not
// clear our persisted session IDs.
func (a *llmwAgent) ValidateSessionID(_ context.Context, id string) bool {
	_, _, ours := parseFacadeID(id)
	return ours
}

// ListSessions enumerates the live llmw host windows (llmw status --json) as
// switchable agent sessions — one per window, id "llmw:<wiki>:<suffix>"
// (suffix "" = the main window, matching facadeID). This powers /switch and
// /list: selecting an entry rebinds the IM conversation to that window via
// StartSession's resume path (doEnterLocked reattaches globally). Dead
// remain-on-exit rows and status errors yield an empty list rather than a
// broken /switch card.
func (a *llmwAgent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	rows, err := a.client.status(ctx)
	if err != nil {
		slog.Warn(agentName+": status for /switch", "err", err)
		return nil, nil
	}
	var out []core.AgentSessionInfo
	for i := range rows {
		r := &rows[i]
		if r.Dead || r.Wiki == "" || r.Window == "" {
			continue
		}
		// The window name is "<wiki>-<suffix>"; the row's own Wiki field
		// disambiguates hyphenated wiki names.
		suffix, ok := windowSuffix(r.Wiki, r.Window)
		if !ok {
			continue
		}
		summary := r.Wiki
		if suffix != "" && suffix != "main" {
			summary += " (" + suffix + ")"
		}
		if r.State != "" {
			summary += " · " + r.State
		}
		var mod time.Time
		if r.ActivityAt != nil {
			mod = time.Unix(*r.ActivityAt, 0)
		}
		out = append(out, core.AgentSessionInfo{
			ID:         facadeID(r.Wiki, suffix),
			Summary:    summary,
			ModifiedAt: mod,
		})
	}
	return out, nil
}

// windowSuffix extracts the suffix from "<wiki>-<suffix>", mapping the main
// window back to "" (the canonical facade id for main is "llmw:<wiki>:").
// ok=false when the window does not belong to the wiki at all.
func windowSuffix(wiki, window string) (string, bool) {
	prefix := wiki + "-"
	if !strings.HasPrefix(window, prefix) {
		if window == wiki {
			return "", true
		}
		return "", false
	}
	suffix := strings.TrimPrefix(window, prefix)
	if suffix == "main" {
		return "", true
	}
	return suffix, true
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
		// f.Close takes f.mu — closeLocked must never be called unlocked
		// (Send may be inside f.inner.Send under f.mu concurrently).
		if err := f.Close(); err != nil {
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
	dataDir := a.dataDir
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
	menuFiles := []struct {
		name    string
		current string
		legacy  []string
	}{
		{"llmw.md", menuCommand, []string{menuCommandLegacyR23, menuCommandLegacy, menuCommandLegacyV1}},
		{"llmw_list.md", listMenuCommand, nil},
		{"llmw_switch.md", switchMenuCommand, nil},
		{"llmw_stop.md", stopMenuCommand, []string{stopMenuCommandV1}},
		{"llmw_detach.md", detachMenuCommand, nil},
		{"llmw_enter.md", enterMenuCommand, nil},
		{"llmw_abort.md", abortMenuCommand, nil},
		{"llmw_new.md", newMenuCommand, nil},
	}
	for _, mf := range menuFiles {
		if err := a.installCommandFile(filepath.Join(dir, mf.name), mf.current, mf.legacy); err != nil {
			return err
		}
	}
	a.cleanupLegacyWikisCommand(dir)
	a.commandsDir = dir
	return nil
}

// installCommandFile writes a menu command template: create when absent,
// refresh when the file still holds a known agent-generated version (so
// description updates propagate), keep with a warning when user-modified.
func (a *llmwAgent) installCommandFile(path, current string, legacy []string) error {
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		if err := os.WriteFile(path, []byte(current), 0o644); err != nil {
			return fmt.Errorf("llmw: write %s: %w", filepath.Base(path), err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("llmw: read %s: %w", filepath.Base(path), err)
	}
	switch string(data) {
	case current:
		return nil
	default:
		for _, old := range legacy {
			if string(data) == old {
				if err := os.WriteFile(path, []byte(current), 0o644); err != nil {
					return fmt.Errorf("llmw: refresh %s: %w", filepath.Base(path), err)
				}
				return nil
			}
		}
	}
	slog.Warn("llmw: menu command kept (user-modified)", "path", path)
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

// newInnerAgent 的子进程实现已随 v3 pane driver 删除：
// inner 会话现在由 facade 直接构造 paneSession（见 doEnterLocked），
// 不再经 core agent registry 包装 headless CLI。

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

// modelsListFn lists "provider/model" ids from the GLOBAL opencode config
// (neutral workdir: wiki overlays must not leak into the list). Swappable
// for tests.
var modelsListFn = func(ctx context.Context) (string, error) {
	return runOpencodeDir(ctx, "", "models")
}

// AvailableModels implements core.ModelSwitcher: the /model command lists
// these and routes the chosen one back through SetModel.
func (a *llmwAgent) AvailableModels(ctx context.Context) []core.ModelOption {
	raw, err := modelsListFn(ctx)
	if err != nil {
		slog.Warn("llmw: opencode models", "err", err)
		return nil
	}
	seen := map[string]bool{}
	var opts []core.ModelOption
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		// Only the yzr* providers: they are the ones the llmw overlay
		// actually configures — the builtin opencode catalogs (128 models)
		// are noise for this workspace.
		if provider, _ := splitModelID(line); !strings.HasPrefix(provider, "yzr") {
			continue
		}
		seen[line] = true
		opts = append(opts, core.ModelOption{Name: line})
	}
	return opts
}

// SetModel implements core.ModelSwitcher. Unlike headless agents (effect on
// next spawn), the pane driver switches the LIVE window immediately; when
// no window is attached yet the target is stored and applied on the next
// doEnterLocked (2026-08-30: a /model right after startup silently no-opped
// otherwise). The interface has no error channel, so live-switch failures
// are logged; the next /model re-reads the bar via GetModel and
// self-corrects.
func (a *llmwAgent) SetModel(model string) {
	if model != "" {
		a.model.Store(model)
	}
	for _, f := range a.facades() {
		if err := f.SetLiveModel(model); err != nil {
			slog.Warn("llmw: live model switch", "model", model, "err", err)
		}
	}
}

// CompressCommand implements core.ContextCompressor: the engine's /compress
// (and auto-compress) forwards this TUI command through session.Send; the
// pane driver runs the compaction lifecycle without reply extraction.
func (a *llmwAgent) CompressCommand() string { return compactCmd }

// TargetModel returns the stored model target ("" = overlay default).
func (a *llmwAgent) TargetModel() string {
	if m, _ := a.model.Load().(string); m != "" {
		return m
	}
	return ""
}

// GetModel reports the first live window's current model ("" when no
// window is entered).
func (a *llmwAgent) GetModel() string {
	for _, f := range a.facades() {
		if m := f.GetLiveModel(); m != "" {
			return m
		}
	}
	return a.TargetModel()
}

func (a *llmwAgent) facades() []*sessionFacade {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*sessionFacade, 0, len(a.sessions))
	for _, f := range a.sessions {
		out = append(out, f)
	}
	return out
}
