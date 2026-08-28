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

// Send routes commands (wikis / enter / numeric selection) and forwards
// ordinary messages to the bound inner session.
func (f *sessionFacade) Send(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errClosed
	}

	if isWikisCommand(prompt) {
		f.handleWikisLocked()
		return nil
	}
	if isNumeric(prompt) {
		if w, ok := f.numericSelectionLocked(prompt); ok {
			f.enterWikiLocked(w)
			return nil
		}
		if f.pendingActiveLocked() {
			f.emitLocked("序号无效：请输入 1-" + strconv.Itoa(len(f.pending.list)) + "（或 /wikis 重新查看）")
			return nil
		}
		// No active selection: the number is an ordinary message.
	}
	if name, ok := enterCommandName(prompt); ok {
		if name == "" {
			f.emitLocked("用法：/enter <wiki 名>（或 /wikis 查看列表后回复序号）")
		} else {
			f.enterByNameLocked(name)
		}
		return nil
	}

	// Ordinary message: clear any pending selection and forward.
	f.pending = nil
	if f.wiki == nil {
		f.emitLocked("请先 /wikis 选择 wiki（或 /enter <wiki 名>）")
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
		f.emitLocked("未找到 wiki：" + name + "。可用 /wikis 查看列表")
		return
	}
	f.enterWikiLocked(w)
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

// isWikisCommand matches both the CommandProvider expansion (wikis.md
// contains <llmw:wikis>) and the raw "/wikis" text fallback.
func isWikisCommand(prompt string) bool {
	if strings.Contains(prompt, wikisSentinel) {
		return true
	}
	p := strings.TrimSpace(prompt)
	return p == "/wikis" || strings.HasPrefix(p, "/wikis")
}

// enterCommandName extracts the wiki name from "/enter <name>".
func enterCommandName(prompt string) (string, bool) {
	p := strings.TrimSpace(prompt)
	if p != "/enter" && !strings.HasPrefix(p, "/enter ") {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(p, "/enter"))
	return name, true
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
