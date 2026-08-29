package llmw

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// sessionFacade is the stable AgentSession the engine holds. It wraps a
// backend session (claudecode or opencode) and swaps it when the user enters
// or switches wikis. The events channel is created once and kept open across
// inner session swaps; nothing else changes on the engine side.
type sessionFacade struct {
	a *llmwAgent

	mu      sync.Mutex
	id      string // "llmw:<wiki>:<innerID>"; updated on wiki switch
	wiki    *wikiEntry
	inner   core.AgentSession
	events  chan core.Event
	pending *pendingSelection
	closed  bool
}

type pendingSelection struct {
	list     []wikiEntry
	deadline time.Time
}

func newSessionFacade(a *llmwAgent, id string) *sessionFacade {
	return &sessionFacade{
		a:      a,
		id:     id,
		events: make(chan core.Event, 64),
	}
}

// Send routes /llmw commands (status / list / enter / stop, plus the
// CLI-native form), numeric selection, and forwards ordinary messages to the
// bound inner session.
func (f *sessionFacade) Send(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errClosed
	}

	if c, ok := parseLlmwCommand(prompt); ok {
		f.handleLlmwCommandLocked(c)
		return nil
	}
	if isNumeric(prompt) {
		if w, ok := f.numericSelectionLocked(prompt); ok {
			f.enterWikiLocked(w)
			return nil
		}
		if f.pendingActiveLocked() {
			f.emitLocked("序号无效：请输入 1-" + strconv.Itoa(len(f.pending.list)) + "（或 /llmw list 重新查看）")
			return nil
		}
		// No active selection: the number is an ordinary message.
	}

	// Ordinary message: clear any pending selection and forward.
	f.pending = nil
	if f.wiki == nil {
		f.emitLocked("请先 /llmw list 选择 wiki（或 /llmw enter <wiki 名>）")
		return nil
	}
	if err := f.ensureInnerLocked(); err != nil {
		f.emitLocked("agent 不可用：" + err.Error() + "。请检查 claude/opencode CLI 安装")
		return nil
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
func (f *sessionFacade) pump(innerEvents <-chan core.Event) {
	for ev := range innerEvents {
		select {
		case f.events <- ev:
		case <-f.a.ctx.Done():
			return
		}
	}
}

// ensureInnerLocked lazily rebuilds a dead inner session (lazy rebuild).
func (f *sessionFacade) ensureInnerLocked() error {
	if f.inner != nil && f.inner.Alive() {
		return nil
	}
	if f.wiki == nil {
		return errClosed
	}
	f.spawnInnerLocked(f.a.ctx, f.wiki, "")
	if f.inner == nil {
		return errClosed
	}
	return nil
}

// enterWikiLocked handles first entry and switching; entering the same wiki
// again is a no-op with a confirmation reply (idempotent).
func (f *sessionFacade) enterWikiLocked(w *wikiEntry) {
	if f.wiki != nil && f.wiki.Name == w.Name {
		f.emitLocked("已在 wiki：" + displayWiki(w))
		return
	}
	f.pending = nil
	f.doEnterLocked(w)
}

// enterByNameLocked looks the wiki up in the current workspace and enters it.
// The name must resolve against `llmw list --json` (whitelist against
// injection into the llmw subprocess).
func (f *sessionFacade) enterByNameLocked(name string) {
	ctx, cancel := context.WithTimeout(f.a.ctx, listTimeout)
	defer cancel()
	w, err := f.a.findWiki(ctx, name)
	if err != nil {
		f.emitLocked("未找到 wiki：" + name + "。可用 /llmw list 查看列表")
		return
	}
	f.enterWikiLocked(w)
}

// stopWindowLocked kills the wiki's host tmux window via the llmw CLI. Wiki
// name is whitelist-resolved like enter; llmw errors (no candidate / multiple
// candidates with a listing + hint) are forwarded verbatim (design §7.5).
func (f *sessionFacade) stopWindowLocked(name, suffix string) {
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
	f.emitLocked("✓ 已停止 " + w.Name + " 的主机窗口")
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
	var b strings.Builder
	b.WriteString(renderWindows(rows))
	if f.wiki != nil {
		state := "dead"
		if f.inner != nil && f.inner.Alive() {
			state = "alive"
		}
		b.WriteString("\n本会话绑定：" + displayWiki(f.wiki) + "（内层 " + state + "）")
	} else {
		b.WriteString("\n本会话未绑定 wiki（/llmw list 后回复序号，或 /llmw enter <名>）")
	}
	b.WriteString("\n" + llmwUsage)
	f.emitLocked(b.String())
}

// handleLlmwCommandLocked dispatches a parsed /llmw command (design §7.5).
// The parser guarantees name != "" for enter/stop and maps bad syntax to the
// "usage" verb, so handlers get well-formed commands only.
func (f *sessionFacade) handleLlmwCommandLocked(c llmwCommand) {
	switch c.verb {
	case "status":
		f.handleStatusLocked()
	case "list":
		f.handleWikisLocked()
	case "enter":
		f.enterByNameLocked(c.name)
	case "stop":
		f.stopWindowLocked(c.name, c.suffix)
	default: // "usage"
		f.emitLocked(llmwUsage)
	}
}

// doEnterLocked performs the actual entry: byobu window sync (only when
// enabled — linear mode enter would block), then spawning the inner session.
func (f *sessionFacade) doEnterLocked(w *wikiEntry) {
	if f.a.client.enterByobuEnabled() {
		ctx, cancel := context.WithTimeout(f.a.ctx, enterTimeout)
		if err := f.a.client.enterWiki(ctx, w.Name); err != nil {
			// IM side does not depend on byobu; overlay refresh may lag.
			slog.Warn("llmw: byobu sync failed (continuing without)", "wiki", w.Name, "err", err)
		}
		cancel()
	}
	f.spawnInnerLocked(f.a.ctx, w, "")
	if f.inner != nil {
		f.emitLocked("✓ 已进入 wiki：" + displayWiki(w))
	}
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
	f.pending = &pendingSelection{list: available, deadline: time.Now().Add(pendingSelectionTTL)}
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

// resumeIntoLocked restores a wiki binding from a persisted facade id without
// emitting an entry confirmation (no user action happened).
func (f *sessionFacade) resumeIntoLocked(ctx context.Context, w *wikiEntry, innerID string) {
	f.spawnInnerLocked(ctx, w, innerID)
}

// spawnInnerLocked closes the previous inner session and spawns a new wrapped
// backend session bound to w. On failure the facade stays bound to w with no
// inner session; the next ordinary message triggers a lazy rebuild.
func (f *sessionFacade) spawnInnerLocked(ctx context.Context, w *wikiEntry, innerID string) {
	if f.inner != nil {
		_ = f.inner.Close()
		f.inner = nil
	}
	f.wiki = w

	innerAgent, err := f.a.newInnerAgent(w)
	if err != nil {
		slog.Error("llmw: build inner agent", "wiki", w.Name, "err", err)
		f.emitLocked("无法启动 agent：" + err.Error())
		return
	}
	spawnCtx, cancel := context.WithTimeout(ctx, spawnTimeout)
	defer cancel()
	sess, err := innerAgent.StartSession(spawnCtx, innerID)
	if err != nil && innerID != "" {
		// Stale inner id (e.g. opencode session list changed) — start fresh.
		slog.Warn("llmw: inner resume failed, starting fresh", "wiki", w.Name, "inner_id", innerID, "err", err)
		sess, err = innerAgent.StartSession(spawnCtx, "")
	}
	if err != nil {
		slog.Error("llmw: start inner session", "wiki", w.Name, "err", err)
		f.emitLocked("无法启动 agent 会话：" + err.Error() + "。请检查 claude/opencode CLI 安装")
		return
	}
	f.inner = sess
	f.id = facadeID(w.Name, sess.CurrentSessionID())
	go f.pump(sess.Events())
}

// emitLocked enqueues a synthetic text event on the stable channel.
func (f *sessionFacade) emitLocked(content string) {
	f.events <- core.Event{Type: core.EventText, Content: content, SessionID: f.id}
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
const llmwUsage = "用法：/llmw [status] | /llmw list | /llmw enter <wiki> | /llmw stop <wiki> [suffix]（或 CLI 原生 /llmw wiki --name=X enter|stop）"

// llmwCommand is a parsed /llmw command. verb ∈ {status, list, enter, stop,
// usage}; verb "usage" means the /llmw prefix matched but the syntax did not —
// the prompt is consumed and the usage line is echoed instead of forwarding
// it to the inner agent.
type llmwCommand struct {
	verb   string
	name   string // wiki name (enter / stop)
	suffix string // optional window suffix (stop)
}

// parseLlmwCommand recognises the /llmw prefix family with a strict boundary
// (design §7.5): "/llmw" must end the prompt or be followed by whitespace, so
// "/llmwx" is an ordinary message. Matching is case-insensitive; wiki names
// keep their case (findWiki matches case-insensitively). Two grammar forms:
//
//   - short form:      /llmw [status] | /llmw list | /llmw enter <name> |
//                      /llmw stop <name> [suffix]
//   - CLI-native form: /llmw wiki --name=X enter|stop [--window-suffix=Y] [--yes]
//
// The menu expansion (llmw.md) carries the menuSentinel and maps to status.
func parseLlmwCommand(prompt string) (llmwCommand, bool) {
	if strings.Contains(prompt, menuSentinel) {
		return llmwCommand{verb: "status"}, true
	}
	p := strings.TrimSpace(prompt)
	if p == "" {
		return llmwCommand{}, false
	}
	fields := strings.Fields(p)
	if strings.ToLower(fields[0]) != "/llmw" {
		return llmwCommand{}, false
	}
	args := fields[1:]

	if len(args) == 0 {
		return llmwCommand{verb: "status"}, true
	}
	switch strings.ToLower(args[0]) {
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
		if len(args) == 2 {
			return llmwCommand{verb: "enter", name: args[1]}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "stop":
		switch len(args) {
		case 2:
			return llmwCommand{verb: "stop", name: args[1]}, true
		case 3:
			return llmwCommand{verb: "stop", name: args[1], suffix: args[2]}, true
		}
		return llmwCommand{verb: "usage"}, true
	case "wiki":
		return parseLlmwCLINative(args[1:]), true
	}
	return llmwCommand{verb: "usage"}, true
}

// parseLlmwCLINative parses the tokens after "/llmw wiki" in CLI grammar:
// --name=X (required), trailing verb enter|stop (required), --window-suffix=Y
// (optional), --yes/-y (accepted and ignored — always implied on the IM
// path). Anything else falls back to the usage line.
func parseLlmwCLINative(tokens []string) llmwCommand {
	var name, suffix, verb string
	for _, tok := range tokens {
		switch {
		case strings.HasPrefix(tok, "--name="):
			name = strings.TrimPrefix(tok, "--name=")
		case strings.HasPrefix(tok, "--window-suffix="):
			suffix = strings.TrimPrefix(tok, "--window-suffix=")
		case tok == "--yes" || tok == "-y":
			// implied: the IM path always runs non-interactive
		case tok == "enter" || tok == "stop":
			if verb != "" {
				return llmwCommand{verb: "usage"}
			}
			verb = tok
		default:
			return llmwCommand{verb: "usage"}
		}
	}
	if verb == "" || name == "" {
		return llmwCommand{verb: "usage"}
	}
	return llmwCommand{verb: verb, name: name, suffix: suffix}
}

// renderWindows renders `llmw status --json` rows as an IM text table. State
// uses llmw's ASCII contract values verbatim (dead / shell / working /
// waiting / unknown); dead rows show the idle column as "exited <dur> ago".
func renderWindows(rows []windowRow) string {
	if len(rows) == 0 {
		return "当前没有运行中的窗口（llmw status）"
	}
	header := []string{"WIKI", "WINDOW", "BACKEND", "STATE", "UPTIME", "IDLE"}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		uptime := "-"
		if r.UptimeSeconds != nil {
			uptime = fmtDur(*r.UptimeSeconds)
		}
		idle := "-"
		if r.Dead {
			if r.DeadSecondsAgo != nil {
				idle = "exited " + fmtDur(*r.DeadSecondsAgo) + " ago"
			} else {
				idle = "exited"
			}
		} else if r.IdleSeconds != nil {
			idle = fmtDur(*r.IdleSeconds)
		}
		backend := r.Backend
		if backend == "" {
			backend = "-"
		}
		state := r.State
		if state == "" {
			state = "unknown"
		}
		cells = append(cells, []string{r.Wiki, r.Window, backend, state, uptime, idle})
	}
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, c := range cells {
		for i, v := range c {
			if len(v) > widths[i] {
				widths[i] = len(v)
			}
		}
	}
	var b strings.Builder
	for i, h := range header {
		b.WriteString(padRight(h, widths[i]))
		if i < len(header)-1 {
			b.WriteString("  ")
		}
	}
	for _, c := range cells {
		b.WriteString("\n")
		for i, v := range c {
			b.WriteString(padRight(v, widths[i]))
			if i < len(c)-1 {
				b.WriteString("  ")
			}
		}
	}
	return b.String()
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

func padRight(s string, w int) string {
	for len(s) < w {
		s += " "
	}
	return s
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
