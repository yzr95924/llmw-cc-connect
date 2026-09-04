package llmw

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// sessionFacade is the stable AgentSession the engine holds. It drives the
// opencode TUI inside an llmw-managed byobu window and swaps the pane target
// when the user enters or switches wikis. The events channel is created once
// and kept open across pane swaps; nothing else changes on the engine side.
type sessionFacade struct {
	a *llmwAgent

	mu      sync.Mutex
	id      string // "llmw:<wiki>:<suffix>"; stable across turns & restarts
	wiki    *wikiEntry
	suffix  string // window suffix ("" = main)
	inner   core.AgentSession
	events  chan core.Event
	pending *pendingSelection
	closed  bool
}

type pendingSelection struct {
	kind       string // "wiki" (from /llmw list) | "window" (/llmw switch) | "stop" (stop confirmation)
	list       []wikiEntry
	windows    []windowRow
	stopName   string // kind "stop": resolved wiki name
	stopSuffix string
	deadline   time.Time
}

func newSessionFacade(a *llmwAgent, id string) *sessionFacade {
	return &sessionFacade{
		a:      a,
		id:     id,
		events: make(chan core.Event, 64),
	}
}

// Send routes /llmw commands (status / list / enter / stop / switch / detach,
// plus the CLI-native form), stop confirmations, numeric selection (wiki or
// window), and forwards ordinary messages to the bound inner session.
//
// Terminal command paths must end with emitResultLocked: the engine only
// completes a turn on an EventResult with Done=true, and a turn that never
// completes holds the session lock, queueing every later message behind it.
func (f *sessionFacade) Send(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errClosed
	}

	// An armed stop confirmation resolves on the NEXT input: y/yes executes
	// (within the TTL), anything else cancels (and the cancelling input is
	// processed normally). A late y/yes after the TTL is consumed with an
	// expiry notice instead of being forwarded — a bare "y" must never leak
	// into the opencode chat as a prompt.
	if f.pending != nil && f.pending.kind == "stop" {
		p := f.pending
		f.pending = nil
		if isConfirmReply(prompt) {
			if time.Now().Before(p.deadline) {
				f.executeStopLocked(p.stopName, p.stopSuffix)
			} else {
				f.emitLocked("⏱ 停止确认已超时（60 秒），未执行；如仍需停止请重新 /llmw_stop")
			}
			f.emitResultLocked()
			return nil
		}
		f.emitLocked("已取消停止 " + p.stopName + " 的操作")
	}

	if c, ok := parseLlmwCommand(prompt); ok {
		f.handleLlmwCommandLocked(c)
		f.emitResultLocked()
		return nil
	}
	if isNumeric(prompt) {
		if f.pendingActiveLocked() && f.pending.kind == "window" {
			if name, suffix, ok := f.numericWindowSelectionLocked(prompt); ok {
				f.enterByNameLocked(name, suffix)
				f.emitResultLocked()
				return nil
			}
			f.emitLocked("序号无效：请输入 1-" + strconv.Itoa(len(f.pending.windows)) + "（或 /llmw switch 重新查看）")
			f.emitResultLocked()
			return nil
		}
		if w, ok := f.numericSelectionLocked(prompt); ok {
			f.enterWikiLocked(w, "")
			f.emitResultLocked()
			return nil
		}
		if f.pendingActiveLocked() {
			f.emitLocked("序号无效：请输入 1-" + strconv.Itoa(len(f.pending.list)) + "（或 /llmw list 重新查看）")
			f.emitResultLocked()
			return nil
		}
		// No active selection: the number is an ordinary message.
	}

	// Ordinary message: clear any pending selection and forward.
	f.pending = nil
	if f.wiki == nil {
		f.emitLocked("请先 /llmw list 选择 wiki（或 /llmw enter <wiki 名>）")
		f.emitResultLocked()
		return nil
	}
	if err := f.ensureInnerLocked(); err != nil {
		f.emitLocked("agent 不可用：" + err.Error() + "。请检查 opencode CLI 安装")
		f.emitResultLocked()
		return nil
	}
	// The pane driver cannot inject attachments into the TUI prompt box —
	// say so instead of dropping them silently (text still goes through).
	if len(images) > 0 || len(files) > 0 {
		f.emitLocked("📎 暂不支持附件（TUI 窗口无法注入图片/文件），仅发送了文本部分")
	}
	return f.inner.Send(prompt, messageID, images, files)
}

func (f *sessionFacade) RespondPermission(requestID string, result core.PermissionResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.inner == nil {
		return errClosed
	}
	return f.inner.RespondPermission(requestID, result)
}

func (f *sessionFacade) Events() <-chan core.Event { return f.events }

// CurrentSessionID returns the facade id "llmw:<wiki>:<suffix>". It is stable
// across turns and daemon restarts (the window and its opencode session live
// in byobu, not in this process), so no lazy refresh is needed.
func (f *sessionFacade) CurrentSessionID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id
}

func (f *sessionFacade) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inner != nil && f.inner.Alive()
}

func (f *sessionFacade) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeLocked()
}

func (f *sessionFacade) closeLocked() error {
	if f.closed {
		return nil
	}
	f.closed = true
	f.pending = nil
	var err error
	if f.inner != nil {
		err = f.inner.Close()
		f.inner = nil
	}
	f.a.removeFacade(f)
	close(f.events)
	return err
}

// pump forwards inner events to the stable facade channel until the inner
// channel closes (inner session Close / process death) or the agent stops.
// The recover guards the send-on-closed-channel race against closeLocked
// (draining the inner buffer after inner.Close while f.events is already
// closed) — same pattern as paneSession.emit.
func (f *sessionFacade) pump(innerEvents <-chan core.Event) {
	defer func() { _ = recover() }()
	for ev := range innerEvents {
		select {
		case f.events <- ev:
		case <-f.a.ctx.Done():
			return
		}
	}
}

// ensureInnerLocked lazily reattaches a dead pane (window killed, daemon
// restart): re-running the enter flow is idempotent — llmw wiki enter reuses
// a live window or rebuilds a dead one.
func (f *sessionFacade) ensureInnerLocked() error {
	if f.inner != nil && f.inner.Alive() {
		return nil
	}
	if f.wiki == nil {
		return errClosed
	}
	f.doEnterLocked(f.wiki, f.suffix, false)
	if f.inner == nil {
		return errClosed
	}
	return nil
}

// enterWikiLocked handles first entry and switching; entering the same wiki
// and suffix again is a no-op with a confirmation reply (idempotent). On a
// failed enter the numeric selection stays armed, so re-replying the same
// number retries instead of being told the list expired.
func (f *sessionFacade) enterWikiLocked(w *wikiEntry, suffix string) {
	if f.wiki != nil && f.wiki.Name == w.Name && f.suffix == suffix {
		f.emitLocked("已在 wiki：" + displayWiki(w))
		return
	}
	if err := f.doEnterLocked(w, suffix, true); err == nil {
		f.pending = nil
	}
}

// enterByNameLocked looks the wiki up in the current workspace and enters it.
// The name must resolve against `llmw list --json` (whitelist against
// injection into the llmw subprocess).
func (f *sessionFacade) enterByNameLocked(name, suffix string) {
	ctx, cancel := context.WithTimeout(f.a.ctx, listTimeout)
	defer cancel()
	w, err := f.a.findWiki(ctx, name)
	if err != nil {
		f.emitLocked("未找到 wiki：" + name + "。可用 /llmw list 查看列表")
		return
	}
	f.enterWikiLocked(w, suffix)
}

// stopWindowLocked kills the wiki's host tmux window via the llmw CLI. Wiki
// name is whitelist-resolved like enter; llmw errors (no candidate / multiple
// candidates with a listing + hint) are forwarded verbatim (design §7.5).
//
// name == "" is the bare form: stop the CURRENTLY BOUND window. When the
// stopped window is the binding and it is NOT main, the facade lazily
// rebinds to main (suffix cleared; the next message re-enters main) — without
// this, ensureInnerLocked would silently resurrect the stopped window on the
// next message, making "exit" impossible. Stopping the bound MAIN window
// UNBINDS the facade (f.wiki cleared; f.id kept as the facade-map key): the
// wiki session is gone, so pretending a binding remains would misreport
// status — the next message gets the "not bound" guidance instead.
func (f *sessionFacade) stopWindowLocked(name, suffix string) {
	bare := name == ""
	if bare {
		if f.wiki == nil {
			f.emitLocked("本会话未绑定 wiki，请先 /llmw enter <名>")
			return
		}
		name, suffix = f.wiki.Name, f.suffix
	}
	ctx, cancel := context.WithTimeout(f.a.ctx, listTimeout)
	defer cancel()
	w, err := f.a.findWiki(ctx, name)
	if err != nil {
		f.emitLocked("未找到 wiki：" + name + "。可用 /llmw list 查看列表")
		return
	}
	// Align with the llmw CLI (--yes): stopping a window is destructive (it
	// kills the host window), so every stop asks first. Detach is the
	// non-destructive exit.
	win := w.Name
	if suffix != "" {
		win = w.Name + "-" + suffix
	}
	f.pending = &pendingSelection{kind: "stop", stopName: w.Name, stopSuffix: suffix, deadline: time.Now().Add(pendingSelectionTTL)}
	f.emitLocked("⚠️ 将停止窗口 " + win + "（主机窗口关闭；opencode 会话落盘可重开续上）。回复 y 确认，其它输入取消。只想解绑请用 /llmw detach")
}

// executeStopLocked performs the actual stop after confirmation. Whenever
// the stopped window IS the current binding — bare stop (resolved from the
// binding) or a named stop of the same wiki+suffix — the binding must move
// out of the way, or ensureInnerLocked would silently resurrect the window
// the user just stopped on the next message.
func (f *sessionFacade) executeStopLocked(name, suffix string) {
	ctx, cancel := context.WithTimeout(f.a.ctx, listTimeout)
	defer cancel()
	w, err := f.a.findWiki(ctx, name)
	if err != nil {
		f.emitLocked("未找到 wiki：" + name + "。可用 /llmw list 查看列表")
		return
	}
	sctx, scancel := context.WithTimeout(f.a.ctx, stopTimeout)
	defer scancel()
	if err := f.a.client.stopWiki(sctx, w.Name, suffix); err != nil {
		f.emitLocked("停止失败：" + err.Error())
		return
	}
	win := w.Name
	if suffix != "" {
		win = w.Name + "-" + suffix
	}
	stoppedOwn := f.wiki != nil && f.wiki.Name == w.Name && f.suffix == suffix
	switch {
	case stoppedOwn && suffix != "":
		// Stopped our own non-main binding: move to main so the stopped
		// window is not resurrected by the next message.
		f.suffix = ""
		f.id = facadeID(f.wiki.Name, "")
		f.emitLocked("✓ 已停止 " + w.Name + " 的窗口 " + win + "；本会话已换绑 main 窗口，下条消息将进入 main")
	case stoppedOwn && suffix == "":
		// Stopped our own main binding: the wiki session is down, so unbind —
		// next message asks the user to /llmw enter again.
		f.wiki = nil
		f.suffix = ""
		f.emitLocked("✓ 已停止 " + w.Name + " 的 main 窗口；wiki 会话已下线，本会话已解绑（/llmw enter <名> 重新进入）")
	default:
		f.emitLocked("✓ 已停止 " + w.Name + " 的主机窗口（" + win + "）")
	}
}

// isConfirmReply matches the stop confirmation answer.
func isConfirmReply(prompt string) bool {
	p := strings.ToLower(strings.TrimSpace(prompt))
	return p == "y" || p == "yes"
}

// handleNewLocked starts a fresh opencode session in the bound window
// (TUI /new): old session stays in the DB, the window context resets. Not
// destructive, so no confirmation gate.
func (f *sessionFacade) handleNewLocked() {
	if f.wiki == nil {
		f.emitLocked("本会话未绑定 wiki，请先 /llmw enter <名>")
		return
	}
	if err := f.ensureInnerLocked(); err != nil {
		f.emitLocked("agent 不可用：" + err.Error() + "。请检查 opencode CLI 安装")
		return
	}
	if err := f.inner.Send(newCmd, "", nil, nil); err != nil {
		f.emitLocked("开新会话失败：" + err.Error())
		f.emitResultLocked()
	}
	// Success path: the pane session emits its own text + result events.
}

// handleAbortLocked interrupts the running turn in the bound window via
// ESC. ensureInner reattaches a dead pane first, so a HOST-started turn in
// the window is abortable too.
func (f *sessionFacade) handleAbortLocked() {
	if f.wiki == nil {
		f.emitLocked("本会话未绑定 wiki，请先 /llmw enter <名>")
		return
	}
	if err := f.ensureInnerLocked(); err != nil {
		f.emitLocked("agent 不可用：" + err.Error() + "。请检查 opencode CLI 安装")
		return
	}
	ab, ok := f.inner.(interface{ Abort() error })
	if !ok {
		f.emitLocked("内部会话不支持中止")
		return
	}
	if err := ab.Abort(); err != nil {
		f.emitLocked("中止失败：" + err.Error())
		return
	}
	// Success notice comes from the pane session as an EventText; the
	// running turn completes on its own (partial reply or abort notice).
}

// handleDetachLocked unbinds WITHOUT touching the host window: the pane
// mirror is closed and the facade goes unbound, so /llmw status shows 未绑定
// and the next message asks to re-enter. This is the safe exit — the window
// (possibly a host-side workspace the user is actively using) keeps running.
func (f *sessionFacade) handleDetachLocked() {
	if f.wiki == nil {
		f.emitLocked("本会话未绑定 wiki")
		return
	}
	name := f.wiki.Name
	if f.inner != nil {
		_ = f.inner.Close()
		f.inner = nil
	}
	f.wiki = nil
	f.suffix = ""
	f.pending = nil
	f.emitLocked("✓ 已解绑 " + name + "（主机窗口保留；/llmw enter <名> 或 /llmw switch 重新接入）")
}

// handleStatusLocked renders the host window table (llmw status --json) plus
// this session's wiki binding and the command usage line (design §7.5).
func (f *sessionFacade) handleStatusLocked() {
	ctx, cancel := context.WithTimeout(f.a.ctx, statusTimeout)
	defer cancel()
	rows, err := f.a.client.status(ctx)
	if err != nil {
		f.emitLocked("llmw 不可用：" + err.Error() + "。请检查 llmw 是否在 PATH / 是否正确安装")
		return
	}
	// Context sampling shells out per window (session list + export); give it
	// its own budget beyond the 5s status call, each window capped at
	// contextSizeTimeout.
	sctx, scancel := context.WithTimeout(f.a.ctx, statusSizeBudget)
	defer scancel()
	sizes := f.contextSizesLocked(sctx, rows)
	var b strings.Builder
	b.WriteString("📊 主机窗口\n\n")
	b.WriteString(renderWindows(rows, sizes))
	if f.wiki != nil {
		state := "dead"
		if f.inner != nil && f.inner.Alive() {
			state = "alive"
		}
		win := f.wiki.Name + "-main"
		if f.suffix != "" {
			win = f.wiki.Name + "-" + f.suffix
		}
		b.WriteString("\n\n📌 本会话绑定：" + displayWiki(f.wiki) + "（窗口 " + win + "，" + state + "）")
	} else {
		b.WriteString("\n\n📌 本会话未绑定 wiki（/llmw list 选序号，或 /llmw enter <名>）")
	}
	b.WriteString("\n" + llmwUsage)
	f.emitLocked(b.String())
}

// handleLlmwCommandLocked dispatches a parsed /llmw command (design §7.5).
// The parser guarantees name != "" for enter and maps bad syntax to the
// "usage" verb; a bare stop carries name == "" (bound window), so handlers
// get well-formed commands only.
func (f *sessionFacade) handleLlmwCommandLocked(c llmwCommand) {
	switch c.verb {
	case "status":
		f.handleStatusLocked()
	case "list":
		f.handleWikisLocked()
	case "enter":
		f.enterByNameLocked(c.name, c.suffix)
	case "stop":
		f.stopWindowLocked(c.name, c.suffix)
	case "switch":
		f.handleSwitchLocked()
	case "detach":
		f.handleDetachLocked()
	case "abort":
		f.handleAbortLocked()
	case "new":
		f.handleNewLocked()
	case "moved":
		f.emitLocked("命令已统一为下划线形式：/llmw_list、/llmw_enter、/llmw_switch、/llmw_stop、/llmw_detach（裸 /llmw = 状态）")
		f.emitLocked(llmwUsage)
	default: // "usage"
		f.emitLocked(llmwUsage)
	}
}

// doEnterLocked performs the actual entry (design v3):
//
//  1. Global window lookup via `llmw status --json` — if a live window for
//     <wiki>-<suffix> exists in ANY session (including one the human opened
//     in their own byobu session), attach to it directly. This is the
//     "IM enter == host enter" semantics: both sides drive the same window.
//  2. Only when no live window exists: `llmw wiki enter` creates one
//     (idempotent, daemon-safe), then re-resolve and attach.
//
// On failure the binding is rolled back to what it was before the attempt:
// an enter of a new wiki must not leave a phantom binding (status would
// report it, a re-reply of the same number would short-circuit to "已在
// wiki", and the user would be stuck with a facade that owns no window). The
// lazy re-enter path is unaffected: there the previous binding IS the same
// wiki, so "rollback" keeps it and the next message retries. announce=false
// is the resume path. Returns nil only when a pane session was attached.
func (f *sessionFacade) doEnterLocked(w *wikiEntry, suffix string, announce bool) error {
	if f.inner != nil {
		_ = f.inner.Close()
		f.inner = nil
	}
	prevWiki, prevSuffix := f.wiki, f.suffix
	f.wiki = w
	f.suffix = suffix

	windowName := w.Name + "-main"
	if suffix != "" {
		windowName = w.Name + "-" + suffix
	}

	row := f.lookupWindowLocked(windowName)
	if row == nil {
		ectx, ecancel := context.WithTimeout(f.a.ctx, enterTimeout)
		err := f.a.client.enterWiki(ectx, w.Name, suffix)
		ecancel()
		if err != nil {
			slog.Warn("llmw: wiki enter failed (no window)", "wiki", w.Name, "suffix", suffix, "err", err)
			f.emitLocked("建立窗口失败：" + err.Error() + "。请检查 byobu/opencode 环境")
			f.wiki, f.suffix = prevWiki, prevSuffix
			return err
		}
		row = f.lookupWindowLocked(windowName)
	}
	if row == nil {
		f.emitLocked("窗口 " + windowName + " 未在 llmw status 中出现（可能启动失败）")
		f.wiki, f.suffix = prevWiki, prevSuffix
		return fmt.Errorf("llmw: window %s not found after enter", windowName)
	}

	root, err := f.a.client.workspaceRoot()
	if err != nil {
		f.emitLocked("解析 workspace 根失败：" + err.Error())
		f.wiki, f.suffix = prevWiki, prevSuffix
		return err
	}
	workDir := filepath.Join(root, w.Path)
	target := row.Session + ":" + row.Window
	f.inner = f.a.paneFactory(f.a.ctx, target, workDir)
	f.id = facadeID(w.Name, suffix)
	// Passive drift alarm: if none of the TUI anchors is readable the pane
	// is either not opencode or its UI contract changed (upgrade) — tell
	// the user instead of failing silently on the next turn.
	if sc, ok := f.inner.(interface{ SelfCheck() string }); ok {
		if warn := sc.SelfCheck(); warn != "" {
			f.emitLocked(warn)
		}
	}
	// Fresh TUI windows always start in Build; realign with the agent-level
	// target mode if the user switched away from build earlier.
	if target := f.a.GetMode(); target != "build" {
		if sw, ok := f.inner.(interface{ SetLiveMode(string) bool }); ok && sw.SetLiveMode(target) {
			slog.Info(agentName+": fresh window aligned to mode", "mode", target)
		} else {
			slog.Warn(agentName+": fresh window mode alignment failed", "mode", target)
		}
	}
	// Same for a pending /model target set before the window existed.
	if m := f.a.TargetModel(); m != "" {
		if sw, ok := f.inner.(interface{ SetLiveModel(string) error }); ok {
			if err := sw.SetLiveModel(m); err != nil {
				slog.Warn(agentName+": fresh window model alignment failed", "model", m, "err", err)
			} else {
				slog.Info(agentName+": fresh window aligned to model", "model", m)
			}
		}
	}
	go f.pump(f.inner.Events())
	if announce {
		f.emitLocked("✓ 已进入 wiki：" + displayWiki(w) + "（窗口 " + row.Window + "）")
	}
	return nil
}

// SetLiveMode applies a mode change to the running inner pane session (hot
// Tab switch in the shared window). Returns false when no window is attached
// or a turn is running; the engine then falls back to restart semantics.
func (f *sessionFacade) SetLiveMode(mode string) bool {
	f.mu.Lock()
	inner := f.inner
	f.mu.Unlock()
	if inner == nil || !inner.Alive() {
		return false
	}
	sw, ok := inner.(interface{ SetLiveMode(string) bool })
	if !ok {
		return false
	}
	return sw.SetLiveMode(mode)
}

// SetLiveModel switches the opencode model of the window behind this
// facade. Session-scoped: the llmw overlay and opencode.json stay untouched.
func (f *sessionFacade) SetLiveModel(model string) error {
	f.mu.Lock()
	inner := f.inner
	f.mu.Unlock()
	if inner == nil || !inner.Alive() {
		return fmt.Errorf("llmw: 尚未进入 wiki 窗口（先 /llmw enter）")
	}
	sw, ok := inner.(interface{ SetLiveModel(string) error })
	if !ok {
		return fmt.Errorf("llmw: inner session 不支持模型切换")
	}
	return sw.SetLiveModel(model)
}

// GetLiveModel reports the window's current model ("" when not entered).
func (f *sessionFacade) GetLiveModel() string {
	f.mu.Lock()
	inner := f.inner
	f.mu.Unlock()
	if inner == nil {
		return ""
	}
	gm, ok := inner.(interface{ GetPaneModel() string })
	if !ok {
		return ""
	}
	return gm.GetPaneModel()
}

// lookupWindowLocked finds the live status row for the wiki's window in any
// session, or nil. Dead remain-on-exit remnants are skipped.
func (f *sessionFacade) lookupWindowLocked(windowName string) *windowRow {
	ctx, cancel := context.WithTimeout(f.a.ctx, statusTimeout)
	defer cancel()
	rows, err := f.a.client.status(ctx)
	if err != nil {
		return nil
	}
	for i := range rows {
		if rows[i].Window == windowName && !rows[i].Dead && rows[i].Wiki != "" {
			return &rows[i]
		}
	}
	return nil
}

// handleWikisLocked lists wikis and arms the numeric selection.
func (f *sessionFacade) handleWikisLocked() {
	ctx, cancel := context.WithTimeout(f.a.ctx, listTimeout)
	defer cancel()
	list, err := f.a.client.list(ctx)
	if err != nil {
		f.emitLocked("llmw 不可用：" + err.Error() + "。请检查 llmw 是否在 PATH / 是否正确安装")
		return
	}

	available := make([]wikiEntry, 0, len(list))
	for i := range list {
		if list[i].DirExists {
			available = append(available, list[i])
		}
	}
	if len(available) == 0 {
		f.emitLocked("当前 workspace 还没有可用的 wiki")
		return
	}

	var b strings.Builder
	b.WriteString("当前 workspace 的 wiki（回复序号进入）：")
	for i := range available {
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(displayWiki(&available[i]))
	}
	b.WriteString("\n（或直接 /llmw enter <名> 进入）")
	f.emitLocked(b.String())
	f.pending = &pendingSelection{kind: "wiki", list: available, deadline: time.Now().Add(pendingSelectionTTL)}
}

// pendingActiveLocked reports whether a /wikis selection is still valid.
func (f *sessionFacade) pendingActiveLocked() bool {
	return f.pending != nil && time.Now().Before(f.pending.deadline)
}

// numericSelectionLocked returns the wiki selected by a numeric reply to the
// last /wikis listing, if the selection is still valid (includes the deadline).
func (f *sessionFacade) numericSelectionLocked(prompt string) (*wikiEntry, bool) {
	if f.pending == nil || time.Now().After(f.pending.deadline) {
		return nil, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(prompt))
	if err != nil || n < 1 || n > len(f.pending.list) {
		return nil, false
	}
	w := f.pending.list[n-1]
	return &w, true
}

// numericWindowSelectionLocked returns the wiki name + window suffix selected
// by a numeric reply to the last /llmw switch listing, if still valid.
func (f *sessionFacade) numericWindowSelectionLocked(prompt string) (name, suffix string, ok bool) {
	if f.pending == nil || f.pending.kind != "window" || time.Now().After(f.pending.deadline) {
		return "", "", false
	}
	n, err := strconv.Atoi(strings.TrimSpace(prompt))
	if err != nil || n < 1 || n > len(f.pending.windows) {
		return "", "", false
	}
	r := f.pending.windows[n-1]
	suf, valid := windowSuffix(r.Wiki, r.Window)
	if !valid {
		return "", "", false
	}
	return r.Wiki, suf, true
}

// handleSwitchLocked lists the LIVE llmw host windows and arms a numeric
// selection that rebinds this conversation to the chosen window (enter path:
// reattach a live window, never create). Engine /switch is disabled for llmw
// because its rebind relies on the implicit StartSession resume we removed.
func (f *sessionFacade) handleSwitchLocked() {
	ctx, cancel := context.WithTimeout(f.a.ctx, statusTimeout)
	defer cancel()
	rows, err := f.a.client.status(ctx)
	if err != nil {
		f.emitLocked("llmw 不可用：" + err.Error())
		return
	}
	wins := make([]windowRow, 0, len(rows))
	for _, r := range rows {
		if r.Dead || r.Wiki == "" || r.Window == "" {
			continue
		}
		if _, valid := windowSuffix(r.Wiki, r.Window); !valid {
			continue
		}
		wins = append(wins, r)
	}
	if len(wins) == 0 {
		f.emitLocked("当前没有运行中的窗口（/llmw enter <名> [suffix] 新建）")
		return
	}
	var b strings.Builder
	b.WriteString("切换主机窗口（回复序号）：")
	for i := range wins {
		mark := ""
		if f.wiki != nil && f.wiki.Name == wins[i].Wiki {
			if suf, _ := windowSuffix(wins[i].Wiki, wins[i].Window); suf == f.suffix {
				mark = " ← 当前"
			}
		}
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(wins[i].Window)
		b.WriteString("（" + wins[i].State + "）" + mark)
	}
	f.emitLocked(b.String())
	f.pending = &pendingSelection{kind: "window", windows: wins, deadline: time.Now().Add(pendingSelectionTTL)}
}

// emitLocked enqueues a synthetic text event on the stable channel.
func (f *sessionFacade) emitLocked(content string) {
	f.events <- core.Event{Type: core.EventText, Content: content, SessionID: f.id}
}

// emitResultLocked signals turn completion to the engine. Content is left
// empty — the engine falls back to the accumulated text events for the reply.
func (f *sessionFacade) emitResultLocked() {
	f.events <- core.Event{Type: core.EventResult, Done: true, SessionID: f.id}
}

func facadeID(wiki, innerID string) string {
	return "llmw:" + wiki + ":" + innerID
}

func displayWiki(w *wikiEntry) string {
	name := w.DisplayName
	if name == "" {
		name = w.Name
	}
	if w.Model != "" {
		return name + " — 模型: " + w.Model
	}
	return name
}

// llmwUsage is the /llmw command usage line (design §7.5).
const llmwUsage = "💡 用法：/llmw（状态）| /llmw_list | /llmw_enter <wiki> [suffix] | /llmw_stop [wiki] [suffix] | /llmw_switch | /llmw_detach | /llmw_abort | /llmw_new"

// llmwCommand is a parsed /llmw command. verb ∈ {status, list, enter, stop,
// switch, usage}; verb "usage" means the /llmw prefix matched but the syntax did not —
// the prompt is consumed and the usage line is echoed instead of forwarding
// it to the inner agent.
type llmwCommand struct {
	verb   string
	name   string // wiki name (enter / stop)
	suffix string // optional window suffix (stop)
}

// parseLlmwCommand recognises the /llmw command surface (design §7.5, hard
// cutover 2026-08-30): every command lives as its own /llmw_xxx custom
// command (menu entry + typed), and each template expands to menu title +
// sentinel + verb (+ user args appended by ExpandPrompt). The ONLY typed form
// still accepted is a bare "/llmw" (= status); any other typed "/llmw <words>"
// resolves to verb "moved" and redirects the user to the underscore form (the
// CLI-native "/llmw wiki --name=…" paste form was removed with it).
//
// Template path grammar (text after the sentinel):
//
//	status | list | enter [<wiki> [suffix]] | stop [<wiki> [suffix]] |
//	switch | detach | abort | new
//
// Bare template "enter" (menu tap without args) degrades to verb "list" —
// the wiki listing is exactly what an argument-less enter needs.
func parseLlmwCommand(prompt string) (llmwCommand, bool) {
	var args []string
	typed := false
	if idx := strings.Index(prompt, menuSentinel); idx >= 0 {
		if rest := strings.TrimSpace(prompt[idx+len(menuSentinel):]); rest != "" {
			args = strings.Fields(rest)
		}
	} else {
		p := strings.TrimSpace(prompt)
		if p == "" {
			return llmwCommand{}, false
		}
		fields := strings.Fields(p)
		if strings.ToLower(fields[0]) != "/llmw" {
			return llmwCommand{}, false
		}
		typed = true
		args = fields[1:]
	}

	if len(args) == 0 {
		return llmwCommand{verb: "status"}, true
	}
	if typed { // hard cutover: no typed subcommands, only bare /llmw
		return llmwCommand{verb: "moved"}, true
	}
	switch strings.ToLower(args[0]) {
	case "abort":
		if len(args) == 1 {
			return llmwCommand{verb: "abort"}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "new":
		if len(args) == 1 {
			return llmwCommand{verb: "new"}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "status":
		if len(args) == 1 {
			return llmwCommand{verb: "status"}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "list":
		if len(args) == 1 {
			return llmwCommand{verb: "list"}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "enter":
		switch len(args) {
		case 1: // menu tap without args → the wiki listing
			return llmwCommand{verb: "list"}, true
		case 2:
			return llmwCommand{verb: "enter", name: args[1]}, true
		case 3: // enter <wiki> <suffix> — parallel window
			return llmwCommand{verb: "enter", name: args[1], suffix: args[2]}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "stop":
		switch len(args) {
		case 1: // bare /llmw stop — stops the currently bound window
			return llmwCommand{verb: "stop"}, true
		case 2:
			return llmwCommand{verb: "stop", name: args[1]}, true
		case 3:
			return llmwCommand{verb: "stop", name: args[1], suffix: args[2]}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "switch":
		if len(args) == 1 {
			return llmwCommand{verb: "switch"}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "detach":
		if len(args) == 1 {
			return llmwCommand{verb: "detach"}, true
		}
		return llmwCommand{verb: "usage"}, true
	}
	return llmwCommand{verb: "usage"}, true
}

// contextSizesLocked resolves the wiki dir of every LIVE window row and
// samples its context size (best effort, per-window timeout; missing or
// failed entries are simply absent from the map and render as "…"). The
// wiki list and workspace root are fetched once; a window whose wiki is not
// in the list is skipped.
func (f *sessionFacade) contextSizesLocked(ctx context.Context, rows []windowRow) map[string]int {
	live := 0
	for i := range rows {
		if !rows[i].Dead && rows[i].Wiki != "" {
			live++
		}
	}
	if live == 0 {
		return nil
	}
	list, err := f.a.client.list(ctx)
	if err != nil {
		slog.Warn(agentName+": status wiki list", "err", err)
		return nil
	}
	root, err := f.a.client.workspaceRoot()
	if err != nil {
		slog.Warn(agentName+": status workspace root", "err", err)
		return nil
	}
	byName := make(map[string]string, len(list)) // wiki name → abs dir
	for i := range list {
		byName[list[i].Name] = filepath.Join(root, list[i].Path)
	}
	out := make(map[string]int, live)
	for i := range rows {
		r := &rows[i]
		if r.Dead || r.Wiki == "" || r.Window == "" {
			continue
		}
		dir, ok := byName[r.Wiki]
		if !ok {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, contextSizeTimeout)
		n, err := contextSizeFn(wctx, dir)
		cancel()
		if err != nil {
			slog.Warn(agentName+": context size", "wiki", r.Wiki, "err", err)
			continue
		}
		out[r.Window] = n
	}
	return out
}

// fmtTokens renders a token count for the status table (~48.9k / 1.2M).
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("~%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("~%.1fk", float64(n)/1000)
	default:
		return strconv.Itoa(n)
	}
}

// renderWindows renders `llmw status --json` rows as one compact line per
// window. Not a markdown table: DingTalk's bot markdown cannot render tables
// at all, and Telegram tables need a <pre> block — plain lines read fine on
// every platform. Field set matches the old table (backend / state / context
// / uptime / idle); state uses llmw's ASCII contract values verbatim (dead /
// shell / working / waiting / unknown). Dead windows show "exited <dur> ago"
// instead of ctx/up/idle.
func renderWindows(rows []windowRow, ctxSizes map[string]int) string {
	if len(rows) == 0 {
		return "当前没有运行中的窗口"
	}
	var b strings.Builder
	for _, r := range rows {
		win := r.Wiki
		if r.Window != "" && r.Window != r.Wiki {
			win = r.Wiki + " (" + r.Window + ")"
		}
		backend := r.Backend
		if backend == "" {
			backend = "-"
		}
		state := r.State
		if state == "" {
			state = "unknown"
		}
		line := "- " + win + " — " + backend + " · " + state
		if r.Dead {
			if r.DeadSecondsAgo != nil {
				line += " · exited " + fmtDur(*r.DeadSecondsAgo) + " ago"
			} else {
				line += " · exited"
			}
		} else {
			ctxCol := "…"
			if n, ok := ctxSizes[r.Window]; ok {
				ctxCol = fmtTokens(n)
			}
			line += " · ctx " + ctxCol
			if r.UptimeSeconds != nil {
				line += " · up " + fmtDur(*r.UptimeSeconds)
			}
			if r.IdleSeconds != nil {
				line += " · idle " + fmtDur(*r.IdleSeconds)
			}
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// fmtDur mirrors llmw's duration format (<60s "now"; <60m "Nm"; <24h "Nh";
// else "Nd") so IM output matches the CLI's status table.
func fmtDur(seconds float64) string {
	if seconds < 60 {
		return "now"
	}
	minutes := int(seconds / 60)
	if minutes < 60 {
		return strconv.Itoa(minutes) + "m"
	}
	hours := int(seconds / 3600)
	if hours < 24 {
		return strconv.Itoa(hours) + "h"
	}
	return strconv.Itoa(hours/24) + "d"
}

// isNumeric reports whether the prompt is a plain integer.
func isNumeric(prompt string) bool {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return false
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
