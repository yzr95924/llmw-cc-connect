package llmw

// pane driver (design v3, PoC 2026-08-29) — drives the opencode TUI inside an
// llmw-managed byobu window.
//
//   - input:   tmux load-buffer - + paste-buffer -p + Enter (bracketed paste
//     keeps multi-line prompts intact)
//   - done:    poll capture-pane; a turn is finished when the busy marker
//     ("esc interrupt") has been seen at least once, is now absent, AND the
//     pane is stable. The marker is the only reliable signal: the idle-only
//     hints ("ctrl+p commands") also show while working, and the pane
//     freezes for >=1s mid-turn, so pure stability would fire early (PoC).
//   - content: `opencode export <session>` — screen capture cannot recover
//     scrolled-off content (opencode redraws in place; tmux history never
//     grows, even with alternate-screen off — PoC-verified). Sessions are
//     discovered via `opencode session list --format json` filtered by
//     directory, confirmed by matching the injected prompt text on the
//     first turn, then kept sticky.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

const (
	paneBusyMarker   = "esc interrupt"
	paneStableNeeded = 2
	// permConfirmMarker anchors the SECOND page of the permission dialog,
	// shown after "Allow always" is chosen (probed 2026-08-30: "△ Always
	// allow / This will allow the following patterns until OpenCode is
	// restarted / Confirm Cancel", Confirm preselected). The page carries
	// neither the page-1 anchor nor the busy marker, so without this the
	// done-detector would end the turn while the dialog still waits.
	permConfirmMarker = "This will allow the following patterns"
	// permConfirmDelay spaces the two Enters of an "allow always" reply:
	// the second page renders asynchronously, and a bare fast double-Enter
	// would land the second press on the still-open first page.
	permConfirmDelay = 600 * time.Millisecond
	// newCmd is the TUI "new session" command. Unlike /compact it does NOT
	// trigger the busy marker (probed 2026-08-30: 3s of captures stay idle),
	// so its lifecycle is a synchronous short wait, not pollTurn. Old
	// sessions stay in the opencode DB (resumable via session list).
	newCmd = "/new"
	// paneIdleHint anchors the idle footer ("… ctrl+p commands") — the
	// signal that the TUI swallowed a prompt-box command and is back at
	// rest. Also the primary health anchor for SelfCheck.
	paneIdleHint = "ctrl+p commands"
	// questionChromeSpan bounds the walk-up from the question-dialog footer
	// to the first option row: at most a workdir/branch status row and a
	// blank sit between them (probed 2026-09-05). The dialog block itself
	// is bounded structurally (blank lines), not by a span — see
	// extractQuestionDialog.
	questionChromeSpan = 3
	// questionDialogSpan is the LEGACY fallback span for footer-matched
	// dialogs without parseable option rows (detection still matters for
	// paneBusy and the L1 notice; parsing fails on these by design).
	questionDialogSpan = 20
	paneTurnTimeout    = 10 * time.Minute
	// paneDiscoverTries is how many newest same-directory sessions to check
	// for our injected prompt before falling back to the newest one.
	paneDiscoverTries = 3
)

// panePollInterval and paneSettleDelay are vars so tests can shrink them.
var (
	panePollInterval = 700 * time.Millisecond
	paneSettleDelay  = 400 * time.Millisecond
	// paneStreamInterval spaces the mid-turn export polls that feed
	// progressive tool/text events to the engine (pseudo-stream; the export
	// snapshot is diffed against what was already emitted).
	paneStreamInterval = 2 * time.Second
	// paneBootTimeout bounds the wait for a fresh window's TUI to draw its
	// first anchor after `llmw wiki enter` creates the byobu window — the
	// CLI returns when the WINDOW exists, the opencode TUI keeps booting
	// for seconds after that (the live smoke gate budgets 20s for the same
	// boot). Var so tests can shrink it.
	paneBootTimeout = 20 * time.Second

	// questionInputOpenDelay paces the free-text dialog path: after the
	// free-text entry's digit opens the inline input (and after the paste)
	// the TUI needs a beat before it consumes the next key (probed
	// 2026-09-05 with ~1s manual pacing; 600ms held). Var so tests can
	// shrink it.
	questionInputOpenDelay = 600 * time.Millisecond
)

// paneRunner abstracts the external commands the driver needs (real: tmux on
// the byobu server + the opencode CLI; tests script them).
type paneRunner interface {
	inject(text string) error
	submit() error
	sendKey(key string) error
	sendText(text string) error
	capture() (string, error)
	sessionListJSON() (string, error)
	exportJSON(sessionID string) (string, error)
}

// realPaneRunner drives the real byobu tmux server and opencode CLI.
type realPaneRunner struct {
	target  string // "<session>:<window>"
	workDir string
}

func (r *realPaneRunner) tmuxOut(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	applyHomeEnv(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (r *realPaneRunner) inject(text string) error {
	cmd := exec.Command("tmux", "load-buffer", "-")
	applyHomeEnv(cmd)
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux load-buffer: %w", err)
	}
	if _, err := r.tmuxOut("paste-buffer", "-p", "-t", r.target); err != nil {
		return fmt.Errorf("tmux paste-buffer: %w", err)
	}
	return nil
}

func (r *realPaneRunner) submit() error {
	if _, err := r.tmuxOut("send-keys", "-t", r.target, "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys Enter: %w", err)
	}
	return nil
}

func (r *realPaneRunner) sendKey(key string) error {
	if _, err := r.tmuxOut("send-keys", "-t", r.target, key); err != nil {
		return fmt.Errorf("tmux send-keys %s: %w", key, err)
	}
	return nil
}

// sendText types literal text (send-keys -l): dialog search fields need the
// string as characters, not as tmux key names.
func (r *realPaneRunner) sendText(text string) error {
	if _, err := r.tmuxOut("send-keys", "-t", r.target, "-l", text); err != nil {
		return fmt.Errorf("tmux send-keys -l: %w", err)
	}
	return nil
}

func (r *realPaneRunner) capture() (string, error) {
	out, err := r.tmuxOut("capture-pane", "-p", "-t", r.target)
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return out, nil
}

// runOpencodeDir runs the opencode CLI and captures stdout via a temp file,
// NOT exec.Cmd.Output(): the opencode CLI (bun runtime) writes stdout
// asynchronously and exits, so with a pipe (Linux capacity 64KiB) the
// process can exit before the reader drains it, silently truncating large
// outputs at exactly 65536 bytes (verified 2026-08-30). A regular file has
// no such race. Self-contained on purpose: agent/llmw stays zero-dep on the
// upstream agent packages so fork rebases never see cross-package coupling.
func runOpencodeDir(ctx context.Context, workDir string, args ...string) (string, error) {
	tmp, err := os.CreateTemp("", "llmw-oc-*.json")
	if err != nil {
		return "", fmt.Errorf("opencode: temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	c := exec.CommandContext(ctx, "opencode", args...)
	applyHomeEnv(c) // opencode reads ~/.config/opencode
	c.Dir = workDir
	var errb bytes.Buffer
	c.Stdout = tmp
	c.Stderr = &errb
	runErr := c.Run()
	tmp.Close()
	if runErr != nil {
		return "", fmt.Errorf("opencode %v: %w: %s", args, runErr, strings.TrimSpace(errb.String()))
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *realPaneRunner) runOpencode(args ...string) (string, error) {
	return runOpencodeDir(context.Background(), r.workDir, args...)
}

func (r *realPaneRunner) sessionListJSON() (string, error) {
	out, err := r.runOpencode("session", "list", "--format", "json")
	if err != nil {
		return "", fmt.Errorf("opencode session list: %w", err)
	}
	return out, nil
}

func (r *realPaneRunner) exportJSON(sessionID string) (string, error) {
	out, err := r.runOpencode("export", sessionID)
	if err != nil {
		return "", fmt.Errorf("opencode export: %w", err)
	}
	return out, nil
}

// opencode CLI JSON contracts (field names verified against opencode 1.18.25).
type ocSessionRow struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Updated   int64  `json:"updated"`
}

type ocExport struct {
	Messages []ocMessage `json:"messages"`
}

type ocMessage struct {
	Info struct {
		Role   string   `json:"role"`
		Tokens ocTokens `json:"tokens"`
	} `json:"info"`
	Parts []ocPart `json:"parts"`
}

// ocTokens mirrors info.tokens in the export (verified against opencode
// 1.18.25: total/input/output/reasoning + cache{read,write}).
type ocTokens struct {
	Total     int `json:"total"`
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

// contextSize returns the context footprint of a message: opencode's own
// total when present, else input + cache (the prompt actually sent).
func (t ocTokens) contextSize() int {
	if t.Total > 0 {
		return t.Total
	}
	return t.Input + t.Cache.Read + t.Cache.Write
}

// contextSizeFromExport extracts the context size of the LAST assistant
// message that carries token data (zero-token rows appear mid-turn).
func contextSizeFromExport(raw string) (int, error) {
	var e ocExport
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return 0, err
	}
	for i := len(e.Messages) - 1; i >= 0; i-- {
		if e.Messages[i].Info.Role != "assistant" {
			continue
		}
		if n := e.Messages[i].Info.Tokens.contextSize(); n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no token data in export")
}

// contextSizeFn reports the context size (tokens) of the NEWEST opencode
// session under workDir — the /llmw status "上下文" column, so the user can
// judge whether /compress is needed. Injectable for tests. Note: sessions
// here are per wiki directory, so sibling windows of one wiki (main/tg)
// report the same (newest) session — an accepted approximation.
var contextSizeFn = func(ctx context.Context, workDir string) (int, error) {
	raw, err := runOpencodeDir(ctx, workDir, "session", "list", "--format", "json")
	if err != nil {
		return 0, err
	}
	var rows []ocSessionRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return 0, err
	}
	newest, best := "", int64(0)
	for _, r := range rows {
		if r.Directory == workDir && r.Updated > best {
			best, newest = r.Updated, r.ID
		}
	}
	if newest == "" {
		return 0, fmt.Errorf("no opencode session under %s", workDir)
	}
	exp, err := runOpencodeDir(ctx, workDir, "export", newest)
	if err != nil {
		return 0, err
	}
	return contextSizeFromExport(exp)
}

type ocPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Tool-call parts (Type == "tool"): name, dedup key, and state.input
	// for the progress summary.
	Tool   string       `json:"tool,omitempty"`
	CallID string       `json:"callID,omitempty"`
	State  *ocToolState `json:"state,omitempty"`
}

type ocToolState struct {
	Status string          `json:"status,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
}

// paneSession implements core.AgentSession over a byobu window pane.
type paneSession struct {
	runner     paneRunner
	workDir    string
	events     chan core.Event
	ctx        context.Context
	cancel     context.CancelFunc
	alive      atomic.Bool
	closeOnce  sync.Once
	turnActive atomic.Bool // a turn is running; blocks mode switches
	aborted    atomic.Bool // this turn was aborted via Abort(); shapes the completion notice

	mu     sync.Mutex
	sesID  string // sticky opencode session id ("" until first turn)
	cursor int    // messages consumed from the export so far
	permFP string // fingerprint of the permission dialog already reported ("" = none)
	qFP    string // fingerprint of the question dialog already reported ("" = none)
	// qAnswered marks that qFP's dialog was answered FROM IM: the TUI
	// dismiss animation lingers ~2s and would otherwise re-emit the same
	// card (fp dedup holds only while qFP stays set) and, once the dialog
	// is really gone, false-warn an orphan.
	qAnswered bool

	// injectMu serializes every MULTI-STEP key-injection sequence against
	// the pane (Send's inject+submit, Abort's ESC, the permission/question
	// dialog responses, the live mode/model switches). Telegram's poll
	// loop already serializes platform traffic, but a second platform
	// (or a callback racing a message) could interleave two sequences and
	// corrupt the dialog state machine. Leaf lock: never takes s.mu.
	injectMu sync.Mutex
}

func newPaneSession(ctx context.Context, runner paneRunner, workDir string) *paneSession {
	sctx, cancel := context.WithCancel(ctx)
	s := &paneSession{
		runner:  runner,
		workDir: workDir,
		events:  make(chan core.Event, 64),
		ctx:     sctx,
		cancel:  cancel,
	}
	s.alive.Store(true)
	return s
}

// compactCmd is the opencode TUI session-compaction command, exposed to the
// engine via llmwAgent.CompressCommand (/compress and auto-compress). It is
// a TUI command, not a chat message: it never lands in the export as a user
// turn, so its lifecycle skips reply extraction and echo verification.
const compactCmd = "/compact"

// compactDoneMsg is the synthetic reply for a completed compaction turn.
const compactDoneMsg = "✓ 上下文已压缩（opencode /compact）"

func (s *paneSession) Send(prompt string, _ string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	if !s.alive.Load() {
		return fmt.Errorf("pane: session closed")
	}
	// Busy gate (2026-08-30): injecting while a turn is running makes
	// opencode QUEUE the message; the done-detector then fires on the
	// foreign turn's completion and the queued reply is lost or
	// misattributed (both error shapes seen in production). Reject up
	// front; the user retries once the window is idle. A capture failure
	// must not block normal sends — fall through.
	if cap, err := s.runner.capture(); err == nil && paneBusy(cap) {
		const notice = "⏳ 窗口正忙（上一回合或主机操作进行中），这条消息没有发送——请稍后重发"
		s.emit(core.Event{Type: core.EventText, Content: notice})
		s.emit(core.Event{Type: core.EventResult, Done: true})
		return nil
	}
	s.injectMu.Lock()
	err := s.runner.inject(prompt)
	if err == nil {
		// Let the TUI register the bracketed paste before submitting.
		time.Sleep(paneSettleDelay)
		err = s.runner.submit()
	}
	s.injectMu.Unlock()
	if err != nil {
		return err
	}
	if prompt == newCmd {
		return s.runNewSession()
	}
	s.turnActive.Store(true)
	s.aborted.Store(false)
	// Read the intervals on the caller's goroutine and pass them down: the
	// poll goroutine must not read package vars (no happens-before edge
	// with tests that restore them; caught by -race).
	go s.pollTurn(prompt, panePollInterval, paneStreamInterval, prompt == compactCmd)
	return nil
}

// turnStream carries the pseudo-stream state for one turn: which opencode
// session to export, what was already delivered as EventText deltas, and
// which tool calls were already reported. The export snapshot is diffed
// against this state every stream tick.
type turnStream struct {
	ses     string
	emitted string // canonical reply text already sent as EventText deltas
	tools   map[string]bool
	last    time.Time
}

// pollTurn watches the pane until the agent finishes the turn, then emits the
// reply text and the turn-done event. One goroutine per turn; the engine
// serializes turns per session, so polls never overlap in practice.
func (s *paneSession) pollTurn(promptEcho string, pollInterval, streamInterval time.Duration, compact bool) {
	defer s.turnActive.Store(false)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.After(paneTurnTimeout)
	prev := ""
	stable := 0
	seenBusy := false
	confirmSubmitted := false // multi-question Confirm page: Enter sent once
	st := &turnStream{tools: map[string]bool{}}
	// complete ends the turn with an optional final text (the one place that
	// emits EventResult — the engine only releases the session on it).
	complete := func(text string) {
		if text != "" {
			s.emit(core.Event{Type: core.EventText, Content: text})
		}
		s.emit(core.Event{Type: core.EventResult, Done: true})
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-deadline:
			complete("⚠ 回合超时（10 分钟），窗口可能仍在处理")
			return
		case <-ticker.C:
			cur, err := s.runner.capture()
			if err != nil {
				complete("⚠ 窗口不可达（可能已退出）：" + err.Error() + "。重发消息可自动重建")
				return
			}
			busy := paneBusy(cur)
			if busy {
				seenBusy = true
				// Pseudo-stream: poll the export mid-turn and feed
				// progressive tool/text events to the engine. Compaction
				// is not a chat turn — no export diffing.
				if !compact && streamInterval > 0 && time.Since(st.last) >= streamInterval {
					st.last = time.Now()
					s.streamTick(promptEcho, st)
				}
			}
			// Permission dialog: the busy marker VANISHES while the TUI
			// waits for approval (probed 2026-08-30), so its presence is
			// part of the busy union above. Emit one EventPermissionRequest
			// per distinct dialog; the engine renders IM buttons and the
			// user's choice comes back through RespondPermission.
			if block, ok := extractPermDialog(cur); ok {
				if fp := permFingerprint(block); fp != s.currentPermFP() {
					s.setPermFP(fp)
					tool, input := parsePermDialog(block)
					s.emit(core.Event{
						Type:      core.EventPermissionRequest,
						RequestID: fp,
						ToolName:  tool,
						ToolInput: input,
					})
				}
			} else {
				s.setPermFP("") // dialog gone (answered on the host side)
			}
			// Question dialog (question tool): L2 handling — parse the page
			// and emit it as an AskUserQuestion permission request, so the
			// engine renders IM BUTTONS (its pending-permission path
			// bypasses the session lock; plain mid-turn messages would be
			// queued and never reach us). The user's answer comes back
			// through RespondPermission; multi-question dialogs advance
			// tab by tab (each page re-emits) and the final Confirm page
			// is auto-submitted below. Parse failure degrades to the old
			// L1 host-window notice.
			if qblock, ok := extractQuestionDialog(cur); ok {
				if fp := questionFingerprint(qblock); fp != s.currentQFP() {
					s.setQFP(fp)
					if q, _, _, pok := parseQuestionPage(qblock); pok {
						s.emit(core.Event{
							Type:      core.EventPermissionRequest,
							RequestID: fp,
							ToolName:  "AskUserQuestion",
							Questions: []core.UserQuestion{q},
						})
					} else {
						s.emit(core.Event{Type: core.EventText, Content: "❓ opencode 正在等待选择（问题：" + questionPreview(qblock) + "）——弹窗解析失败，请到主机窗口操作，完成后回合自动继续"})
					}
				}
			} else {
				if fp, answered := s.qState(); fp != "" && !answered {
					// The dialog vanished WITHOUT an IM answer (answered or
					// dismissed on the host). The engine's pending card, if
					// one went out, stays unresolved and BLOCKS its turn
					// loop until the next IM message is consumed as the
					// answer (engine resolves pending on any message —
					// handlePendingPermission). Nothing the agent can emit
					// reaches it, so log the wedge shape for diagnosis; the
					// 2026-09-05 incident surfaced exactly this state.
					slog.Warn(agentName+": question dialog gone without IM answer (host-side action?) — an orphaned IM card, if any, resolves on the next message", "fingerprint", fp)
				}
				s.clearQDialog() // dialog gone (answered/dismissed on the host side)
			}
			// Multi-question Confirm page (probed 2026-09-05): after the
			// last question is answered the dialog lands on a Review page
			// whose footer has no "select" (see paneBusy union). Every
			// answer was already explicitly chosen per question — submit
			// automatically instead of leaving the turn hanging. The flag
			// arms only on success, so a failed submit retries next tick
			// instead of wedging the turn to the 10-minute timeout; the
			// key rides injectMu like every other injection.
			if isQuestionConfirmPage(cur) && !confirmSubmitted {
				s.injectMu.Lock()
				err := s.sendPaneKeys("Enter")
				s.injectMu.Unlock()
				if err != nil {
					slog.Warn("llmw pane: confirm page submit failed, will retry", "err", err)
				} else {
					confirmSubmitted = true
				}
			}
			if cur == prev {
				stable++
			} else {
				stable = 0
				prev = cur
			}
			// Done: the marker was seen at least once (guards the
			// submit→spin-up window), is now gone, and the pane settled.
			if seenBusy && !busy && stable >= paneStableNeeded {
				if compact {
					complete(compactDoneMsg)
					return
				}
				reply, err := s.fetchReply(promptEcho)
				if err != nil {
					slog.Warn("llmw pane: fetch reply", "err", err)
					if s.aborted.Load() {
						// Aborted turns often have no assistant text yet —
						// the abort notice beats a "read failed" error.
						complete("⛔ 回合已中止（ESC），无已产出内容")
					} else {
						complete("（读取回复失败：" + err.Error() + "）")
					}
					return
				}
				// Reconcile with the pseudo-stream: deliver only the part
				// not already streamed. A mid-stream text rewrite breaks the
				// prefix match and the full reply is resent (rare, accepted).
				if st.emitted != "" && strings.HasPrefix(reply, st.emitted) {
					reply = reply[len(st.emitted):]
				}
				complete(reply)
				return
			}
		}
	}
}

// runNewSession finishes a "/new" TUI command: wait for the idle footer to
// confirm the TUI swallowed it (no busy cycle exists for /new), then drop
// the sticky session binding so the next ordinary message rediscovers the
// fresh session. Synchronous on purpose — /new is not a turn.
func (s *paneSession) runNewSession() error {
	if s.waitPane(func(c string) bool {
		return strings.Contains(c, paneIdleHint) && !paneBusy(c)
	}, 2*time.Second) {
		s.mu.Lock()
		sesID := s.sesID
		s.sesID = ""
		s.cursor = 0
		s.mu.Unlock()
		slog.Info(agentName+": new session started (old kept in opencode DB)", "old", sesID)
		s.emit(core.Event{Type: core.EventText, Content: "✓ 已开始新会话（opencode /new；旧会话保留，可用 opencode session list 续）"})
	} else {
		s.emit(core.Event{Type: core.EventText, Content: "⚠ /new 未确认执行（窗口未回到空闲态）——请到主机窗口检查；本会话绑定暂未重置"})
	}
	s.emit(core.Event{Type: core.EventResult, Done: true})
	return nil
}

// WaitReady blocks until any TUI anchor is readable — the opencode TUI
// finished booting — or the timeout expires. Every pane read/write before
// the first anchor sees a blank boot screen: the drift self-check false
// alarms, mode/model alignment silently fails, and injected prompts land
// before the TUI consumes input. Returns false on timeout; the caller
// falls through to SelfCheck, which turns a still-blank pane into the
// user-facing drift alarm.
func (s *paneSession) WaitReady(timeout time.Duration) bool {
	return s.waitPane(paneAnchorsReadable, timeout)
}

// SelfCheck reports whether the attached pane still speaks the TUI anchor
// contract: any of the busy marker, the idle footer hint, or the bottom
// mode bar must be readable. "" = healthy; otherwise a user-facing warning
// (opencode upgraded its UI, or the window is not running opencode at all).
// One retry absorbs mid-redraw blank frames. Call WaitReady first on a
// fresh window — this is a drift check, not a boot check.
func (s *paneSession) SelfCheck() string {
	for try := 0; try < 2; try++ {
		cap, err := s.runner.capture()
		if err == nil && paneAnchorsReadable(cap) {
			return ""
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "⚠ 窗口界面契约异常：读不到 opencode 的任何界面锚点（busy/空闲提示/底栏）——opencode 可能已升级或窗口里不是 opencode，请按 README 升级验收清单排查"
}

// paneAnchorsReadable is the pure predicate behind SelfCheck.
func paneAnchorsReadable(cap string) bool {
	if strings.Contains(cap, paneBusyMarker) || strings.Contains(cap, paneIdleHint) {
		return true
	}
	return paneBarRe.FindString(cap) != ""
}

// emit never panics even when Close races a pending send (the recover guards
// the random-select-on-closed-channel race, same pattern as agent/tmux).
func (s *paneSession) emit(ev core.Event) {
	defer func() { _ = recover() }()
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	}
}

// streamTick polls the export snapshot mid-turn and feeds progressive events
// (EventToolUse / EventText deltas) to the engine — the IM progress cards /
// streaming cards are driven by exactly these events. Purely additive UX:
// any failure (discovery miss, export error, parse error) is skipped
// silently; the final fetchReply remains the source of truth.
func (s *paneSession) streamTick(promptEcho string, st *turnStream) {
	if st.ses == "" {
		id, err := s.discoverSession(promptEcho)
		if err != nil {
			return
		}
		st.ses = id
	}
	raw, err := s.runner.exportJSON(st.ses)
	if err != nil {
		return
	}
	var e ocExport
	if json.Unmarshal([]byte(raw), &e) != nil {
		return
	}
	// Walk the parts after the last user IN ORDER so tool events interleave
	// with the text that preceded them (engines flush text at tool events).
	// soFar builds the same canonical string as extractNewText; deltas are
	// only emitted while it stays prefix-compatible with what was already
	// delivered (a mid-stream rewrite simply stops the deltas — the final
	// reconciliation resends the full reply).
	var soFar string
	for i := lastUserIndex(&e) + 1; i < len(e.Messages); i++ {
		if e.Messages[i].Info.Role != "assistant" {
			continue
		}
		for _, p := range e.Messages[i].Parts {
			switch p.Type {
			case "text":
				if t := strings.TrimSpace(p.Text); t != "" {
					if soFar != "" {
						soFar += "\n\n"
					}
					soFar += t
				}
			case "tool":
				if p.CallID != "" && !st.tools[p.CallID] {
					s.emitTextUpTo(st, soFar)
					s.emit(core.Event{Type: core.EventToolUse, ToolName: p.Tool, ToolInput: summarizeOcToolInput(&p)})
					st.tools[p.CallID] = true
				}
			}
		}
	}
	s.emitTextUpTo(st, soFar)
}

// emitTextUpTo emits the canonical-text delta between st.emitted and target.
// Prefix-checked: a rewrite (target no longer extends the emitted text)
// silently emits nothing.
func (s *paneSession) emitTextUpTo(st *turnStream, target string) {
	if len(target) > len(st.emitted) && strings.HasPrefix(target, st.emitted) {
		s.emit(core.Event{Type: core.EventText, Content: target[len(st.emitted):]})
		st.emitted = target
	}
}

// lastUserIndex returns the index of the last user message, or -1.
func lastUserIndex(e *ocExport) int {
	for i := len(e.Messages) - 1; i >= 0; i-- {
		if e.Messages[i].Info.Role == "user" {
			return i
		}
	}
	return -1
}

// lastUserText returns the trimmed first text part of the last user message.
func lastUserText(e *ocExport) string {
	if i := lastUserIndex(e); i >= 0 {
		for _, p := range e.Messages[i].Parts {
			if p.Type == "text" {
				return strings.TrimSpace(p.Text)
			}
		}
	}
	return ""
}

// summarizeOcToolInput renders a one-line input summary for the progress
// card (bash → command, file tools → path, generic → first string value).
func summarizeOcToolInput(p *ocPart) string {
	if p.State == nil || len(p.State.Input) == 0 {
		return ""
	}
	var input map[string]any
	if json.Unmarshal(p.State.Input, &input) != nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := input[k].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch p.Tool {
	case "bash":
		return truncateRunes(pick("command"), 80)
	case "read", "write", "edit", "multiedit", "glob", "grep":
		return truncateRunes(pick("filePath", "file_path", "pattern", "path"), 80)
	case "skill", "task":
		return truncateRunes(pick("name", "subagent_type"), 40)
	}
	for _, v := range input {
		if v, ok := v.(string); ok && strings.TrimSpace(v) != "" {
			return truncateRunes(v, 60)
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// fetchReply reads the turn's assistant text via the opencode export CLI.
// Uses the sticky session id when it still resolves; rediscovers by matching
// the injected prompt otherwise (fresh window, first turn).
func (s *paneSession) fetchReply(promptEcho string) (string, error) {
	s.mu.Lock()
	sesID := s.sesID
	s.mu.Unlock()

	var export ocExport
	if sesID != "" {
		if raw, err := s.runner.exportJSON(sesID); err == nil {
			if err := json.Unmarshal([]byte(raw), &export); err == nil && len(export.Messages) > s.cursor {
				// Echo guard (2026-08-30): if the session's last user message
				// is not ours, the host typed in the shared window between our
				// turns — "text after last user" would deliver the HOST's
				// reply to the IM user. Abort instead of misattributing.
				if lastUserText(&export) != strings.TrimSpace(promptEcho) {
					return "", fmt.Errorf("会话末尾不是本回合输入（窗口可能被主机占用），已中止以免错发")
				}
				return s.extractNewText(&export)
			}
		}
		slog.Warn("llmw pane: sticky session stale, rediscovering", "ses", sesID)
		s.mu.Lock()
		s.sesID = ""
		s.cursor = 0
		s.mu.Unlock()
	}

	id, err := s.discoverSession(promptEcho)
	if err != nil {
		return "", err
	}
	// One retry: the export may race the TUI's final DB flush right after a
	// turn completes (empty or short read), which resolves within a second.
	var raw string
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(800 * time.Millisecond)
		}
		if raw, err = s.runner.exportJSON(id); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(raw), &export); err == nil && len(export.Messages) > 0 {
			s.mu.Lock()
			s.sesID = id
			s.mu.Unlock()
			return s.extractNewText(&export)
		}
	}
	if err == nil {
		err = fmt.Errorf("parse export (len=%d)", len(raw))
	}
	return "", err
}

// extractNewText returns the CURRENT turn's reply: the text parts of all
// assistant messages after the last user message. cc-connect's streaming
// agents equivalently only forward events produced by the live turn — a
// (re)discovered or cursor-reset session must never re-dump earlier history
// (2026-08-30: a cursor=0 rediscovery once replayed the entire session into
// Telegram). The cursor only serves as the sticky fast-path staleness guard.
func (s *paneSession) extractNewText(e *ocExport) (string, error) {
	msgs := e.Messages
	lastUser := lastUserIndex(e)
	var sb strings.Builder
	for i := lastUser + 1; i < len(msgs); i++ {
		if msgs[i].Info.Role != "assistant" {
			continue
		}
		for _, p := range msgs[i].Parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n\n")
				}
				sb.WriteString(strings.TrimSpace(p.Text))
			}
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("no assistant reply after the last user message (len=%d)", len(msgs))
	}
	s.mu.Lock()
	s.cursor = len(msgs)
	s.mu.Unlock()
	return sb.String(), nil
}

// paneModeRe matches the TUI subagent indicator on the status lines: the
// persistent bottom bar "┃  Build · <model> ..." / "┃  Plan · <model> ..." and
// the transient per-turn line "▣  Build · <model> · 12.3s". The LAST match in
// a capture is the persistent bar (it sits below everything else), so it
// reports the CURRENT subagent even right after a switch.
var paneModeRe = regexp.MustCompile(`(Build|Plan)\s*·`)

// currentPaneMode scans a pane capture and returns "build", "plan", or ""
// when no subagent indicator is readable (foreign dialog, wrong pane).
func currentPaneMode(capture string) string {
	mode := ""
	for _, m := range paneModeRe.FindAllStringSubmatch(capture, -1) {
		mode = strings.ToLower(m[1])
	}
	return mode
}

// normalizePaneMode maps user input onto the opencode subagent keys; anything
// unrecognized falls back to "build" (the opencode default).
func normalizePaneMode(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "plan") {
		return "plan"
	}
	return "build"
}

// SetLiveMode switches the window's opencode subagent (Build ↔ Plan) by
// sending Tab key presses and confirming via the status bar — the same hot
// switch a human gets pressing Tab in the shared window. Only allowed while
// no turn is running (Tab during a turn would land in the input box instead
// of switching agents). Returns false when the switch could not be completed.
func (s *paneSession) SetLiveMode(mode string) bool {
	target := normalizePaneMode(mode)
	if !s.alive.Load() || s.turnActive.Load() {
		return false
	}
	s.injectMu.Lock()
	defer s.injectMu.Unlock()
	// build ↔ plan needs one Tab; extra presses cover user-defined subagents
	// cycling through the same key.
	for i := 0; i < 4; i++ {
		cap, err := s.runner.capture()
		if err != nil {
			slog.Warn("llmw pane: mode switch capture failed", "err", err)
			return false
		}
		current := currentPaneMode(cap)
		switch {
		case current == target:
			return true
		case current == "":
			slog.Warn("llmw pane: mode switch aborted, no subagent indicator in pane")
			return false
		}
		if err := s.runner.sendKey("Tab"); err != nil {
			slog.Warn("llmw pane: mode switch send Tab failed", "err", err)
			return false
		}
		// Wait for the TUI to apply the switch before re-reading.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(150 * time.Millisecond)
			cap, err := s.runner.capture()
			if err != nil {
				return false
			}
			if currentPaneMode(cap) != current {
				break
			}
		}
	}
	cap, err := s.runner.capture()
	return err == nil && currentPaneMode(cap) == target
}

// paneBarRe matches the TUI status lines — the input-area bar ("Build ·
// glm-5.2 yzr-glm-5_2-1m") and the spinner line ("Build · glm-5.2 · 47.1s").
// The segment stops at the next "·", so elapsed timers never leak in. The
// LAST match wins (the bottom bar is rendered after the spinner).
var paneBarRe = regexp.MustCompile(`(Build|Plan)\s*·\s*([^·\n]+)`)

// paneModelFromCapture returns the model segment of the bottom bar, e.g.
// "glm-5.2 yzr-glm-5_2-1m" (custom providers: model id + provider id) or
// "GLM-5.3 OpenCode Go" (builtin: display names).
func paneModelFromCapture(cap string) string {
	ms := paneBarRe.FindAllStringSubmatch(cap, -1)
	if len(ms) == 0 {
		return ""
	}
	return strings.TrimSpace(ms[len(ms)-1][2])
}

// GetPaneModel reports the window's current model from the bottom bar.
func (s *paneSession) GetPaneModel() string {
	cap, err := s.runner.capture()
	if err != nil {
		return ""
	}
	return paneModelFromCapture(cap)
}

// GetPaneMode reports the window's current subagent (build/plan) from the
// status bar; "" when the pane cannot be read (callers fall back to the
// agent's target mode).
func (s *paneSession) GetPaneMode() string {
	cap, err := s.runner.capture()
	if err != nil {
		return ""
	}
	return currentPaneMode(cap)
}

// splitModelID splits "provider/model" ids from `opencode models`.
func splitModelID(id string) (provider, name string) {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[:i], id[i+1:]
	}
	return "", id
}

// tokenBoundary reports whether s[i:i+n] is delimited by non-word
// characters, so "glm-5.3" does NOT match inside "glm-5.3-flash".
func tokenBoundary(s string, i, n int) bool {
	isWord := func(c byte) bool {
		return c == '-' || c == '_' || c == '.' || c == '/' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	if i > 0 && isWord(s[i-1]) {
		return false
	}
	if i+n < len(s) && isWord(s[i+n]) {
		return false
	}
	return true
}

// paneRowMatches checks whether a dialog row / bottom-bar segment shows the
// target model: the model name at token boundary, plus the provider token
// (raw id "yzr-glm-5_2-1m" or the spaced display form "opencode go").
func paneRowMatches(row, name, provider string) bool {
	lrow := strings.ToLower(row)
	lname := strings.ToLower(name)
	for i := 0; i+len(lname) <= len(lrow); i++ {
		if lrow[i:i+len(lname)] != lname || !tokenBoundary(lrow, i, len(lname)) {
			continue
		}
		if provider == "" {
			return true
		}
		lp := strings.ToLower(provider)
		if strings.Contains(lrow, lp) || strings.Contains(lrow, strings.ReplaceAll(lp, "-", " ")) {
			return true
		}
	}
	return false
}

// parseModelDialogRows extracts the model rows from a "Select model" dialog
// capture. The search input is skipped BY POSITION (the first line after
// the header): its text can be identical to the target row ("GLM-5.3
// OpenCode Go"), so content-based echo detection would eat real rows.
// Section headers and the ASCII-art empty state are skipped too.
func parseModelDialogRows(cap string) []string {
	lines := strings.Split(cap, "\n")
	var rows []string
	inDialog := false
	skippedInput := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(l, "Select model") {
			inDialog = true
			continue
		}
		if !inDialog {
			continue
		}
		if t == "" {
			if len(rows) > 0 {
				break
			}
			continue
		}
		if !skippedInput {
			skippedInput = true // the search input line (or "Search" placeholder)
			continue
		}
		if strings.Contains(t, "Connect provider") || strings.Contains(t, "ctrl+f") {
			break
		}
		if t == "Favorites" || t == "Recent" {
			continue
		}
		if strings.Contains(t, "█") {
			continue // ASCII-art "no results"
		}
		rows = append(rows, t)
	}
	return rows
}

// SetLiveModel switches the window's model via the TUI model dialog (leader
// ctrl+x then m, probed 2026-08-30). The dialog's search only indexes
// DISPLAY strings, not ids, and fuzzy ranking puts prefix-variants first
// ("GLM-5.3-Flash" outranks "GLM-5.3"), so the driver filters, PARSES the
// rows, walks to the exact match with Down, and confirms with Enter. The
// bottom bar then verifies the switch. Session-scoped: neither the llmw
// overlay nor opencode.json is touched.
func (s *paneSession) SetLiveModel(target string) error {
	if !s.alive.Load() {
		return fmt.Errorf("pane: session closed")
	}
	if s.turnActive.Load() {
		return fmt.Errorf("pane: 回合进行中，暂不能切换模型（请稍后再试）")
	}
	s.injectMu.Lock()
	defer s.injectMu.Unlock()
	provider, name := splitModelID(target)
	if name == "" {
		return fmt.Errorf("pane: 无效模型 %q", target)
	}
	if cap, err := s.runner.capture(); err == nil {
		if bar := paneModelFromCapture(cap); bar != "" && paneRowMatches(bar, name, provider) {
			return nil // already on target
		}
	}
	abort := func(err error) error {
		_ = s.runner.sendKey("Escape")
		return err
	}
	for _, k := range []string{"C-x", "m"} {
		if err := s.runner.sendKey(k); err != nil {
			return fmt.Errorf("pane: send %q: %w", k, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !s.waitPane(func(c string) bool { return strings.Contains(c, "Select model") }, 2*time.Second) {
		return abort(fmt.Errorf("pane: 模型对话框未打开"))
	}
	idx, err := s.searchModelDialog(name, provider)
	if err != nil {
		return abort(err)
	}
	for i := 0; i < idx; i++ {
		if err := s.runner.sendKey("Down"); err != nil {
			return abort(fmt.Errorf("pane: send Down: %w", err))
		}
		time.Sleep(60 * time.Millisecond)
	}
	if err := s.runner.sendKey("Enter"); err != nil {
		return abort(fmt.Errorf("pane: send Enter: %w", err))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		cap, err := s.runner.capture()
		if err == nil {
			bar := paneModelFromCapture(cap)
			if bar != "" && paneRowMatches(bar, name, provider) {
				return nil
			}
			if !strings.Contains(cap, "Select model") {
				return fmt.Errorf("pane: 切换后底栏为 %q，与目标 %q 不符", bar, target)
			}
		}
		if time.Now().After(deadline) {
			return abort(fmt.Errorf("pane: 等待底栏确认模型切换超时"))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// searchModelDialog filters the dialog and returns the target's row index
// (0 = top row, which is preselected). Two-tier search: custom providers
// display their raw id ("glm-5.2 yzr-glm-5_2-1m"), builtin providers a
// spaced display name ("OpenCode Go"), so the dashed form is tried first
// and the spaced form as fallback.
func (s *paneSession) searchModelDialog(name, provider string) (int, error) {
	var tries []string
	if provider == "" {
		tries = []string{name}
	} else {
		tries = []string{name + " " + provider}
		if strings.Contains(provider, "-") {
			tries = append(tries, name+" "+strings.ReplaceAll(provider, "-", " "))
		}
	}
	for _, q := range tries {
		_ = s.runner.sendKey("C-u") // clear the previous search text
		time.Sleep(120 * time.Millisecond)
		if err := s.runner.sendText(q); err != nil {
			return 0, fmt.Errorf("pane: type search: %w", err)
		}
		time.Sleep(400 * time.Millisecond)
		cap, err := s.runner.capture()
		if err != nil {
			return 0, err
		}
		for idx, row := range parseModelDialogRows(cap) {
			if paneRowMatches(row, name, provider) {
				return idx, nil
			}
		}
	}
	return 0, fmt.Errorf("模型 %q 未在对话框中定位到（可在主机窗口手动切换）", name)
}

func (s *paneSession) waitPane(pred func(string) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		cap, err := s.runner.capture()
		if err == nil && pred(cap) {
			return true
		}
	}
	return false
}

// discoverSession finds the opencode session for this window: filter
// `session list` by workDir, newest first, and pick the one whose last user
// message matches the prompt we injected (disambiguates sibling windows on
// the same wiki directory). No fallback: a mismatch is surfaced as an error —
// guessing "newest" once read a foreign window's session.
func (s *paneSession) discoverSession(promptEcho string) (string, error) {
	raw, err := s.runner.sessionListJSON()
	if err != nil {
		return "", err
	}
	var rows []ocSessionRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return "", fmt.Errorf("parse session list: %w", err)
	}
	var cands []ocSessionRow
	for _, r := range rows {
		if r.Directory == s.workDir {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("no opencode session under %s", s.workDir)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Updated > cands[j].Updated })
	n := paneDiscoverTries
	if len(cands) < n {
		n = len(cands)
	}
	for _, c := range cands[:n] {
		raw, err := s.runner.exportJSON(c.ID)
		if err != nil {
			continue
		}
		var e ocExport
		if json.Unmarshal([]byte(raw), &e) != nil || len(e.Messages) == 0 {
			continue
		}
		if lastUserText(&e) == strings.TrimSpace(promptEcho) {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("no session under %s ends with our prompt (checked %d newest)——窗口回合可能未落库（TUI 异常），可试 /llmw stop 后重进", s.workDir, n)
}

// RespondPermission answers the pending permission dialog: the engine calls
// it when the user taps the IM buttons (allow / allow all / deny). The TUI
// dialog is navigated with Left/Right + Enter (probed 2026-08-30; "Allow
// once" is preselected, Tab does nothing). If the dialog is already gone
// (answered on the host in the meantime), this is a no-op — sending keys to
// an idle pane would type into the prompt box.
//
// It also carries back AskUserQuestion answers (engine sends
// UpdatedInput.answers): the option whose label matches is picked with its
// NUMBER key (a digit selects AND auto-confirms — probed 2026-09-05);
// non-matching text goes through the numbered free-text entry (digit opens
// the input, bracketed paste, Enter submits). Key contract 2026-09-05:
// initial cursor = option 1; multi-question dialogs advance tabs
// automatically after each answer; the final Confirm page is auto-entered
// by pollTurn.
func (s *paneSession) RespondPermission(requestID string, result core.PermissionResult) error {
	if !s.alive.Load() {
		return fmt.Errorf("pane: session closed")
	}
	cur, err := s.runner.capture()
	if err != nil {
		return fmt.Errorf("pane: capture for permission response: %w", err)
	}
	if block, ok := extractPermDialog(cur); ok && permFingerprint(block) == requestID {
		s.injectMu.Lock()
		defer s.injectMu.Unlock()
		return s.respondPermDialog(requestID, block, result)
	}
	// Question page: fingerprint match then digit/paste injection.
	if qblock, ok := extractQuestionDialog(cur); ok && questionFingerprint(qblock) == requestID {
		s.injectMu.Lock()
		defer s.injectMu.Unlock()
		return s.respondQuestionPage(qblock, result)
	}
	s.setPermFP("")  // stale request; nothing to answer
	s.clearQDialog() // stale question markers too
	return nil
}

// respondPermDialog drives the permission dialog keys (Left/Right + Enter;
// "Allow always" opens a preselected second page confirmed by a paced
// Enter). Probed 2026-08-30.
func (s *paneSession) respondPermDialog(requestID string, block string, result core.PermissionResult) error {
	var keys []string
	switch result.Behavior {
	case "allow_all":
		if err := s.sendPaneKeys("Right", "Enter"); err != nil {
			return err
		}
		time.Sleep(permConfirmDelay)
		if err := s.sendPaneKeys("Enter"); err != nil {
			return err
		}
	case "deny":
		keys = []string{"Right", "Right", "Enter"} // Reject
	default:
		keys = []string{"Enter"} // Allow once (preselected)
	}
	if len(keys) > 0 {
		if err := s.sendPaneKeys(keys...); err != nil {
			return err
		}
	}
	s.setPermFP("") // an identical re-appearing dialog (agent retry) re-emits
	return nil
}

// respondQuestionPage injects an AskUserQuestion answer into the live
// question dialog. The engine's UpdatedInput.answers carries exactly one
// entry per request (one question per page).
func (s *paneSession) respondQuestionPage(qblock string, result core.PermissionResult) error {
	answer := ""
	if m, ok := result.UpdatedInput["answers"].(map[string]any); ok {
		for _, v := range m {
			if str, ok := v.(string); ok {
				answer = str
			}
		}
	}
	q, nums, freeIdx, ok := parseQuestionPage(qblock)
	if !ok {
		return fmt.Errorf("pane: question page no longer parseable")
	}
	answer = sanitizeDialogAnswer(answer)
	if answer == "" {
		return fmt.Errorf("pane: 空的回答被拒绝（请点按钮或回复数字/文本）")
	}
	slog.Info(agentName+": answering question dialog", "question", q.Question, "answer", answer)
	// Digit keys only exist for 1-9: a two-digit sendText("10") would type
	// "1" FIRST, which selects option 1 AND auto-confirms — a silently
	// wrong answer. Degrade explicitly instead of injecting garbage.
	digitKey := func(n int) error {
		if n < 1 || n > 9 {
			return fmt.Errorf("pane: 选项序号 %d 超出数字键范围（1-9），请到主机窗口操作", n)
		}
		return s.runner.sendText(strconv.Itoa(n))
	}
	for i, opt := range q.Options {
		if strings.EqualFold(opt.Label, answer) {
			// A digit key selects the option AND auto-confirms; on the last
			// question it also advances to the Confirm page.
			if err := digitKey(nums[i]); err != nil {
				return err
			}
			s.markQAnswered() // keep qFP: the dismiss animation lingers ~2s
			return nil
		}
	}
	if freeIdx == 0 {
		return fmt.Errorf("pane: 回答不是任何选项且弹窗没有自由文本入口")
	}
	// Free-text path (probed 2026-09-05): the entry's number opens an
	// inline input; bracketed paste lands in it; Enter submits.
	if err := digitKey(freeIdx); err != nil {
		return err
	}
	time.Sleep(questionInputOpenDelay)
	if err := s.runner.inject(answer); err != nil { // bracketed paste
		return err
	}
	time.Sleep(questionInputOpenDelay)
	if err := s.runner.sendKey("Enter"); err != nil {
		return err
	}
	s.markQAnswered() // keep qFP: the dismiss animation lingers ~2s
	return nil
}

// sendPaneKeys sends keys with small gaps so the TUI registers each press.
func (s *paneSession) sendPaneKeys(keys ...string) error {
	for _, k := range keys {
		if err := s.runner.sendKey(k); err != nil {
			return fmt.Errorf("pane: send key %q: %w", k, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// Abort interrupts the running turn by sending Escape to the TUI (the
// "esc interrupt" hint). Busy is judged from a live capture, so host-started
// turns are abortable too, not only ours. While a dialog (permission /
// question) is up the Escape merely closes it — abort again afterwards to
// interrupt the turn. The paneSession emits the notice itself; the running
// pollTurn then finishes the turn with whatever partial reply the export
// holds (or a plain abort notice when there is none).
func (s *paneSession) Abort() error {
	if !s.alive.Load() {
		return fmt.Errorf("pane: session closed")
	}
	cap, err := s.runner.capture()
	if err != nil {
		return fmt.Errorf("pane: capture for abort: %w", err)
	}
	if !paneBusy(cap) {
		return fmt.Errorf("pane: 当前没有进行中的回合")
	}
	s.injectMu.Lock()
	defer s.injectMu.Unlock()
	if err := s.runner.sendKey("Escape"); err != nil {
		return fmt.Errorf("pane: send Escape: %w", err)
	}
	s.aborted.Store(true)
	s.emit(core.Event{Type: core.EventText, Content: "⛔ 已发送中止信号（ESC），回合将以已产出的内容收尾"})
	return nil
}

// paneBusy reports whether the pane is mid-turn: the interrupt hint is
// shown, a permission dialog (first page OR the "Always allow" confirm
// page) is waiting for an answer, a question page is waiting for a
// selection, or the multi-question Confirm page is up (all of them hide
// the hint while they are up).
func paneBusy(cap string) bool {
	if strings.Contains(cap, paneBusyMarker) {
		return true
	}
	if strings.Contains(cap, permConfirmMarker) {
		return true
	}
	if _, ok := extractPermDialog(cap); ok {
		return true
	}
	if _, ok := extractQuestionDialog(cap); ok {
		return true
	}
	return isQuestionConfirmPage(cap)
}

func (s *paneSession) currentPermFP() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.permFP
}

func (s *paneSession) setPermFP(fp string) {
	s.mu.Lock()
	s.permFP = fp
	s.mu.Unlock()
}

func (s *paneSession) currentQFP() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qFP
}

// qState returns the question-dialog fingerprint and whether the current
// dialog was already answered from IM.
func (s *paneSession) qState() (fp string, answered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qFP, s.qAnswered
}

// setQFP records the emitted dialog's fingerprint and resets the answered
// marker (a fresh dialog page is being announced).
func (s *paneSession) setQFP(fp string) {
	s.mu.Lock()
	s.qFP = fp
	s.qAnswered = false
	s.mu.Unlock()
}

// markQAnswered keeps the fingerprint (suppressing re-emit while the
// dismissed dialog lingers on screen ~2s, 2026-09-05 production trace)
// and marks the dialog IM-answered, so the dialog-gone branch does not
// false-warn an orphan.
func (s *paneSession) markQAnswered() {
	s.mu.Lock()
	s.qAnswered = true
	s.mu.Unlock()
}

// clearQDialog drops both markers (dialog gone or stale request).
func (s *paneSession) clearQDialog() {
	s.mu.Lock()
	s.qFP = ""
	s.qAnswered = false
	s.mu.Unlock()
}

// extractQuestionDialog pulls the question-tool dialog out of a capture.
// Probed 2026-08-30 against opencode 1.18.25 (English UI): the dialog
// renders the question text, numbered options ("1. coffee", possibly with
// description rows, plus a trailing "Type your own answer" entry), then the
// footer row "⇆ select  enter submit  esc dismiss". That footer combination
// is the anchor — the permission dialog footer says "enter confirm / esc
// reject", so the two cannot be confused. The block spans a fixed window of
// lines above the footer (question + options) and is hashed for dedup.
// extractQuestionDialog pulls the question-tool dialog out of a capture as
// a TIGHT, structurally bounded block (2026-09-05 redesign). The first
// version grabbed a fixed 20-line window above the footer — in a busy pane
// the dialog itself is only ~10 lines and everything above is streaming
// conversation text, whose numbered-list lines ("2. Stable fingerprint →
// injection works") parsed as dialog options and whose chrome rows
// ("master" git-branch status) surfaced as the question (both shapes seen
// in the 2026-09-05 production incident). The dialog's real structure is:
//
//	[blank] question [blank] N. option (desc) ... free-text entry [chrome] footer
//
// so the block is bounded by walking up from the footer: skip chrome rows
// (workdir/branch status, ≤3), collect the contiguous option region
// (option rows plus description rows that sit directly below an option),
// stop at the blank line above it, skip one blank, and take the contiguous
// question run up to the next blank. Conversation junk above the dialog is
// separated by that blank and never enters the block.
func extractQuestionDialog(cap string) (string, bool) {
	lines := strings.Split(cap, "\n")
	foot := -1
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		// Single-question footers say "enter submit"; multi-question
		// QUESTION pages say "enter confirm" (their final Confirm page
		// says "enter submit" again but has NO "select" — probed
		// 2026-09-05). "select" is what keeps the Confirm page out.
		hasEnter := strings.Contains(l, "enter submit") || strings.Contains(l, "enter confirm")
		if strings.Contains(l, "select") && hasEnter && strings.Contains(l, "esc dismiss") {
			foot = i
			break
		}
	}
	if foot < 0 {
		return "", false
	}
	trimGutter := func(s string) string {
		return strings.TrimLeft(strings.TrimSpace(s), " ┃│>❯")
	}
	isOpt := func(s string) bool {
		return paneNumberedOptionRe.MatchString(s)
	}
	// Walk up from the footer (skipping chrome rows) to the LAST option
	// row — the numbered free-text entry or the lowest real option.
	lastOpt := -1
	for j := foot - 1; j >= 0 && j >= foot-questionChromeSpan; j-- {
		l := trimGutter(lines[j])
		if l == "" {
			continue
		}
		if isOpt(l) {
			lastOpt = j
			break
		}
	}
	if lastOpt < 0 {
		// No option rows in reach: still a dialog footer (paneBusy and the
		// L1 fallback rely on detection) — return the legacy span block;
		// parseQuestionPage will fail on it and degrade to the L1 notice.
		start := foot - questionDialogSpan
		if start < 0 {
			start = 0
		}
		return strings.TrimSpace(strings.Join(lines[start:foot+1], "\n")), true
	}
	// Option region: contiguous option rows upward; a non-option row is a
	// description when the row BELOW it is an option row; anything else
	// ends the region.
	top := lastOpt
	for k := lastOpt; k >= 0; k-- {
		l := trimGutter(lines[k])
		if l == "" {
			break
		}
		if isOpt(l) {
			top = k
			continue
		}
		if k+1 <= lastOpt && isOpt(trimGutter(lines[k+1])) {
			continue // description row (sits directly below its option)
		}
		break
	}
	// Question run: skip one blank above the options, then take the
	// contiguous non-blank lines (the question may wrap) until a blank.
	// qEnd is the block START (above the options); degenerate only when no
	// content exists above the option region.
	q := top - 1
	for q >= 0 && trimGutter(lines[q]) == "" {
		q--
	}
	qEnd := q
	for q >= 0 && trimGutter(lines[q]) != "" {
		qEnd = q
		q--
	}
	if qEnd < 0 {
		qEnd = top // no question text; block = options only
	}
	return strings.TrimSpace(strings.Join(lines[qEnd:foot+1], "\n")), true
}

// questionFingerprint derives the dedup id from the dialog block.
// questionFingerprint derives a STABLE dedup id for the dialog: it hashes
// the parsed question text and option labels, NOT the raw block. The raw
// 20-line block window carries volatile rows (streaming text, spinners,
// token counters) whose every-tick drift made raw-block fingerprints
// unstable — the 2026-09-05 production incident: the same dialog re-emitted
// duplicate cards on every drift tick, and card answers arrived as "stale"
// no-ops because the fingerprint had already moved (fix: hash only what
// cannot change while the dialog is up). Unparseable dialogs (L1 fallback)
// hash the footer line + preview text.
func questionFingerprint(block string) string {
	h := sha256.New()
	if q, _, _, ok := parseQuestionPage(block); ok {
		io.WriteString(h, q.Question)
		for _, opt := range q.Options {
			io.WriteString(h, "\x00"+opt.Label)
		}
	} else {
		io.WriteString(h, "raw:\x00"+questionPreview(block)+"\x00")
		for _, l := range strings.Split(block, "\n") {
			if strings.Contains(l, "esc dismiss") {
				io.WriteString(h, l)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// paneNumberedOptionRe matches the dialog's option rows ("1. coffee").
var paneNumberedOptionRe = regexp.MustCompile(`^\d+\. `)

// paneNumberedOptionLabelRe captures the option number and label.
var paneNumberedOptionLabelRe = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)

// isQuestionConfirmPage reports whether the capture shows the
// multi-question dialog's final Confirm page: its footer carries
// "⇆ tab ... enter submit ... esc dismiss" but NO "select" — the question
// pages' footers always have "select". Probed 2026-09-05 (opencode
// 1.18.28).
func isQuestionConfirmPage(cap string) bool {
	for _, l := range strings.Split(cap, "\n") {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "⇆ tab") && strings.Contains(t, "enter submit") &&
			strings.Contains(t, "esc dismiss") && !strings.Contains(t, "select") {
			return true
		}
	}
	return false
}

// parseQuestionPage parses one page of the question dialog into an engine
// UserQuestion plus the TUI option numbers. Layout probed 2026-09-05
// against opencode 1.18.28: option rows are "N. label" with optional
// indented description rows below; the free-text entry ("Type your own
// answer") is itself numbered; multi-question pages carry a leading tab
// strip (" A   B   Confirm") above the question text, which the
// question-text scan never reaches (it stops at the question line before
// the strip). Returns the question (free-text entry excluded from
// Options), nums aligned with Options (the TUI digit of each option),
// the free-text entry's digit (0 = absent), and whether a question page
// was recognized.
func parseQuestionPage(block string) (q core.UserQuestion, nums []int, freeIdx int, ok bool) {
	lines := strings.Split(block, "\n")
	type optRow struct {
		n, line int
		label   string
	}
	var rows []optRow
	for i, raw := range lines {
		l := strings.TrimLeft(strings.TrimSpace(raw), " ┃│>❯")
		m := paneNumberedOptionLabelRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		rows = append(rows, optRow{n, i, strings.TrimSpace(m[2])})
	}
	if len(rows) == 0 {
		return q, nil, 0, false
	}
	for _, r := range rows {
		if strings.Contains(r.label, "Type your own answer") {
			freeIdx = r.n
			continue // UI affordance, not a real choice — free text reaches it
		}
		opt := core.UserQuestionOption{Label: r.label}
		// Indented description row directly below the option (the free-text
		// entry gains one only AFTER the user opens its input).
		if r.line+1 < len(lines) {
			d := strings.TrimLeft(strings.TrimSpace(lines[r.line+1]), " ┃│>❯")
			if d != "" && !paneNumberedOptionRe.MatchString(d) &&
				!strings.Contains(d, "esc dismiss") && !strings.Contains(d, "⇆ tab") {
				opt.Description = d
			}
		}
		q.Options = append(q.Options, opt)
		nums = append(nums, r.n)
	}
	if len(q.Options) == 0 {
		return q, nil, 0, false
	}
	// Contract guard (2026-09-05): every real question dialog carries the
	// numbered "Type your own answer" entry — probed on single- and
	// multi-question forms. A parsed "dialog" WITHOUT it is conversation
	// junk (numbered-list text) leaking through the legacy span fallback;
	// reject so the caller degrades to the L1 notice instead of emitting
	// a garbage card.
	if freeIdx == 0 {
		return q, nil, 0, false
	}
	q.Question = questionPreview(block)
	return q, nums, freeIdx, true
}

// sanitizeDialogAnswer flattens an IM free-text answer for the dialog's
// single-line input: newlines would submit early.
func sanitizeDialogAnswer(ans string) string {
	ans = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(ans)
	ans = strings.TrimSpace(ans)
	if runes := []rune(ans); len(runes) > 2000 {
		ans = string(runes[:2000])
	}
	return ans
}

// questionPreview extracts the question text. It is STRUCTURAL first
// (2026-09-05): the question sits directly above the first "N. " option
// row, separated by a blank — take the WHOLE contiguous run (questions
// wrap) up to the next blank. Keyword heuristics misfired in real wide
// windows (a bare "master" git-branch row inside the block was returned
// as the question); the old upward keyword scan stays as the fallback for
// layouts without option rows.
func questionPreview(block string) string {
	lines := strings.Split(block, "\n")
	firstOpt := -1
	for i, raw := range lines {
		l := strings.TrimLeft(strings.TrimSpace(raw), " ┃│>❯")
		if paneNumberedOptionRe.MatchString(l) {
			firstOpt = i
			break
		}
	}
	if firstOpt >= 0 {
		j := firstOpt - 1
		for j >= 0 && strings.TrimLeft(strings.TrimSpace(lines[j]), " ┃│>❯") == "" {
			j--
		}
		var run []string
		for ; j >= 0; j-- {
			l := strings.TrimLeft(strings.TrimSpace(lines[j]), " ┃│>❯")
			if l == "" || strings.Contains(l, "esc dismiss") || strings.Contains(l, "⇆ tab") {
				break
			}
			run = append([]string{l}, run...) // prepend: walking upward
		}
		if len(run) > 0 {
			out := strings.Join(run, " ")
			if runes := []rune(out); len(runes) > 80 {
				return string(runes[:80]) + "…"
			}
			return out
		}
	}
	// Fallback: scan up from the footer, skipping known non-question rows.
	for i := len(lines) - 1; i >= 0; i-- {
		// Strip the TUI gutter prefix ("  ┃  1. coffee" → "1. coffee").
		l := strings.TrimLeft(strings.TrimSpace(lines[i]), " ┃│>❯")
		if l == "" || strings.Contains(l, "esc dismiss") {
			continue
		}
		if paneNumberedOptionRe.MatchString(l) {
			continue
		}
		if strings.Contains(l, "Type your own answer") {
			continue
		}
		// Workdir status row (e.g. ".../probe") — unless the line is itself
		// a question ("合并到 main/dev 还是 main/master？" carries slashes
		// but is the question text, not a path).
		if strings.Contains(l, "/tmp/") || strings.Count(l, "/") >= 2 {
			if !strings.HasSuffix(l, "?") && !strings.HasSuffix(l, "？") {
				continue
			}
		}
		runes := []rune(l)
		if len(runes) > 80 {
			return string(runes[:80]) + "…"
		}
		return l
	}
	return "（见主机窗口）"
}

// extractPermDialog pulls the permission dialog block out of a capture: the
// lines from the "Permission required" header through the options row. The
// block is stable while the dialog waits (unlike the whole pane, which has
// a running spinner/timer), so its hash works as a dedup fingerprint.
func extractPermDialog(cap string) (string, bool) {
	lines := strings.Split(cap, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "Permission required") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := start
	for j := start + 1; j < len(lines) && j < start+15; j++ {
		if strings.Contains(lines[j], "Allow once") {
			end = j
			break
		}
		end = j
	}
	return strings.TrimSpace(strings.Join(lines[start:end+1], "\n")), true
}

// permFingerprint derives the dedup/request id from the dialog block.
func permFingerprint(block string) string {
	sum := sha256.Sum256([]byte(block))
	return hex.EncodeToString(sum[:])[:16]
}

// parsePermDialog extracts a tool name and one-line input summary from the
// dialog block for the IM prompt (header "# Shell command" style, then the
// details).
func parsePermDialog(block string) (tool, input string) {
	tool = "permission"
	lines := strings.Split(block, "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		// The header sits inside the TUI gutter ("  ┃    # Shell command"),
		// so match by substring, not prefix.
		idx := strings.Index(l, "# ")
		if idx < 0 {
			continue
		}
		l = strings.TrimSpace(l[idx+2:])
		switch {
		case strings.Contains(l, "Shell command"):
			tool = "bash"
		case strings.Contains(l, "Edit"), strings.Contains(l, "Write"), strings.Contains(l, "Patch"):
			tool = "edit"
		case strings.Contains(l, "Read"):
			tool = "read"
		case strings.Contains(l, "Web"):
			tool = "webfetch"
		}
		var content []string
		for _, m := range lines[i+1:] {
			m = strings.TrimSpace(m)
			if m == "" || strings.Contains(m, "Allow once") {
				break
			}
			content = append(content, m)
		}
		if len(content) > 0 {
			input = strings.Join(content, " ⏎ ")
		}
		break
	}
	if input == "" {
		input = block
	}
	return tool, truncateRunes(input, 200)
}

func (s *paneSession) Events() <-chan core.Event { return s.events }

func (s *paneSession) CurrentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sesID
}

func (s *paneSession) Alive() bool { return s.alive.Load() }

func (s *paneSession) Close() error {
	s.closeOnce.Do(func() {
		s.alive.Store(false)
		s.cancel()
		close(s.events)
	})
	return nil
}
