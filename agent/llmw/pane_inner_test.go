package llmw

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// marshalExport serializes a single export object (the opencode CLI contract).
func marshalExport(t *testing.T, e ocExport) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newDriverForTest(t *testing.T) (*paneSession, *fakePaneRunner) {
	t.Helper()
	oldPoll, oldSettle, oldStream := panePollInterval, paneSettleDelay, paneStreamInterval
	panePollInterval, paneSettleDelay, paneStreamInterval = 5*time.Millisecond, time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { panePollInterval, paneSettleDelay, paneStreamInterval = oldPoll, oldSettle, oldStream })
	r := &fakePaneRunner{
		workDir:   "/ws/foo",
		captures:  []string{"idle ctrl+p"},
		exports:   map[string]string{},
		exportSeq: map[string][]string{},
	}
	s := newPaneSession(context.Background(), r, "/ws/foo")
	t.Cleanup(func() { _ = s.Close() })
	return s, r
}

// waitResult reads events until a result event arrives.
func waitResult(t *testing.T, events <-chan core.Event, timeout time.Duration) (texts []string, done bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return texts, false
			}
			if ev.Type == core.EventResult {
				return texts, ev.Done
			}
			if ev.Type == core.EventText && ev.Content != "" {
				texts = append(texts, ev.Content)
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
}

// A full turn: busy captures, then idle; the reply comes from the export
// channel, not the screen.
func TestPaneTurnLifecycle(t *testing.T) {
	s, r := newDriverForTest(t)
	r.scriptTurn("hi", "hello from export")

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, done := waitResult(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("turn must end with Done=true")
	}
	if len(texts) != 1 || texts[0] != "hello from export" {
		t.Fatalf("texts = %v, want the export reply", texts)
	}
	if got := r.lastInjected(); got != "hi" {
		t.Fatalf("injected = %q", got)
	}
}

// Done must require the busy marker to have been seen: a pane that is idle
// from the start (submit not registered yet) must not complete a turn.
func TestPaneDoneRequiresSeenBusy(t *testing.T) {
	s, r := newDriverForTest(t)
	// Sessions/export are armed, but captures stay idle-only.
	r.scriptTurn("hi", "reply")
	r.mu.Lock()
	r.captures = []string{"idle ctrl+p", "idle ctrl+p", "idle ctrl+p"}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("premature event without busy marker: %+v", ev)
	case <-time.After(150 * time.Millisecond):
		// OK — driver correctly waits.
	}
}

// Cursor diffing: two turns on the same sticky session each yield exactly
// their own reply.
func TestPaneCursorDiffAcrossTurns(t *testing.T) {
	s, r := newDriverForTest(t)
	r.scriptTurn("one", "first")

	if err := s.Send("one", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, _ := waitResult(t, s.Events(), 3*time.Second)
	if len(texts) != 1 || texts[0] != "first" {
		t.Fatalf("turn1 texts = %v", texts)
	}

	// Second turn on the SAME sticky session (the export grows).
	r.mu.Lock()
	var sesID string
	for id := range r.exports {
		sesID = id
	}
	exp := ocExport{}
	exp.Messages = append(exp.Messages, ocMessage{})
	exp.Messages[0].Info.Role = "user"
	exp.Messages[0].Parts = []ocPart{{Type: "text", Text: "one"}}
	m1 := ocMessage{}
	m1.Info.Role = "assistant"
	m1.Parts = []ocPart{{Type: "text", Text: "first"}}
	m2 := ocMessage{}
	m2.Info.Role = "user"
	m2.Parts = []ocPart{{Type: "text", Text: "two"}}
	m3 := ocMessage{}
	m3.Info.Role = "assistant"
	m3.Parts = []ocPart{{Type: "text", Text: "second"}}
	exp.Messages = append(exp.Messages, m1, m2, m3)
	r.exports[sesID] = marshalExport(t, exp)
	r.captures = append(r.captures, "idle ctrl+p", "… esc interrupt …", "idle ctrl+p", "idle ctrl+p")
	r.mu.Unlock()

	if err := s.Send("two", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, _ = waitResult(t, s.Events(), 3*time.Second)
	if len(texts) != 1 || texts[0] != "second" {
		t.Fatalf("turn2 texts = %v, want only the new reply", texts)
	}
}

// Discovery must pick the session whose last user message matches the
// injected prompt, not merely the newest one (sibling window disambiguation).
func TestPaneDiscoveryMatchesPrompt(t *testing.T) {
	s, r := newDriverForTest(t)

	r.mu.Lock()
	newest := ocSessionRow{ID: "ses_newest", Directory: "/ws/foo", Updated: 3000}
	ours := ocSessionRow{ID: "ses_ours", Directory: "/ws/foo", Updated: 2000}
	other := ocSessionRow{ID: "ses_otherdir", Directory: "/ws/bar", Updated: 9000}
	r.sessions = marshalRows([]ocSessionRow{newest, ours, other})
	// Newest session's last user message is someone else's text.
	expNewest := ocExport{}
	expNewest.Messages = append(expNewest.Messages, ocMessage{})
	expNewest.Messages[0].Info.Role = "user"
	expNewest.Messages[0].Parts = []ocPart{{Type: "text", Text: "human sibling text"}}
	// Ours contains the injected prompt.
	expOurs := ocExport{}
	expOurs.Messages = append(expOurs.Messages, ocMessage{})
	expOurs.Messages[0].Info.Role = "user"
	expOurs.Messages[0].Parts = []ocPart{{Type: "text", Text: "  find me  "}} // padding tolerant
	m := ocMessage{}
	m.Info.Role = "assistant"
	m.Parts = []ocPart{{Type: "text", Text: "matched reply"}}
	expOurs.Messages = append(expOurs.Messages, m)
	r.exports = map[string]string{
		"ses_newest": marshalExport(t, expNewest),
		"ses_ours":   marshalExport(t, expOurs),
	}
	r.captures = append(r.captures, "idle ctrl+p", "… esc interrupt …", "idle ctrl+p", "idle ctrl+p")
	r.mu.Unlock()

	if err := s.Send("find me", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, _ := waitResult(t, s.Events(), 3*time.Second)
	if got := s.CurrentSessionID(); got != "ses_ours" {
		t.Fatalf("session = %q, want ses_ours (prompt-matched), not the newest", got)
	}
	if len(texts) != 1 || texts[0] != "matched reply" {
		t.Fatalf("texts = %v, want the matched session's reply", texts)
	}
}

// When no candidate session ends with our prompt, discovery must fail
// explicitly — the old "fall back to newest" once exported a foreign
// window's session (cross-session contamination).
func TestPaneDiscoveryNoFallback(t *testing.T) {
	s, r := newDriverForTest(t)

	r.mu.Lock()
	newest := ocSessionRow{ID: "ses_foreign", Directory: "/ws/foo", Updated: 3000}
	r.sessions = marshalRows([]ocSessionRow{newest})
	expForeign := ocExport{}
	expForeign.Messages = append(expForeign.Messages, ocMessage{})
	expForeign.Messages[0].Info.Role = "user"
	expForeign.Messages[0].Parts = []ocPart{{Type: "text", Text: "someone else entirely"}}
	r.exports = map[string]string{"ses_foreign": marshalExport(t, expForeign)}
	r.captures = append(r.captures, "idle ctrl+p", "… esc interrupt …", "idle ctrl+p", "idle ctrl+p")
	r.mu.Unlock()

	if err := s.Send("our prompt", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, done := waitResult(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("turn must still complete (with the failure notice)")
	}
	if len(texts) == 0 || !strings.Contains(texts[0], "读取回复失败") {
		t.Fatalf("want explicit read-failure notice, got %v", texts)
	}
}

// A stale sticky session (export gone — window restarted) must trigger
// rediscovery instead of an error surface.
func TestPaneStickyRediscovery(t *testing.T) {
	s, r := newDriverForTest(t)
	r.scriptTurn("one", "first")
	if err := s.Send("one", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitResult(t, s.Events(), 3*time.Second)
	old := s.CurrentSessionID()

	// Window restarted: old session gone, a new one carries the next prompt.
	r.mu.Lock()
	delete(r.exports, old)
	r.mu.Unlock()
	r.scriptTurn("after restart", "fresh reply")

	if err := s.Send("after restart", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, done := waitResult(t, s.Events(), 3*time.Second)
	if !done || len(texts) != 1 || !strings.Contains(texts[0], "fresh reply") {
		t.Fatalf("texts = %v done=%v, want rediscovered reply", texts, done)
	}
}

// SetLiveMode switches the subagent via Tab and confirms via the status bar:
// build → plan needs exactly one Tab, plan → build another.
func TestPaneSetLiveModeSwitch(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.uiMode = "build"
	r.mu.Unlock()

	if !s.SetLiveMode("plan") {
		t.Fatal("switch to plan must succeed")
	}
	if got := r.sentKeysLen(); got != 1 {
		t.Fatalf("sent keys = %d, want 1 Tab", got)
	}
	if !s.SetLiveMode("build") {
		t.Fatal("switch back to build must succeed")
	}
	if got := r.sentKeysLen(); got != 2 {
		t.Fatalf("sent keys = %d, want 2 Tabs total", got)
	}
}

// Already in the target mode: no key presses at all.
func TestPaneSetLiveModeNoop(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.uiMode = "plan"
	r.mu.Unlock()

	if !s.SetLiveMode("plan") {
		t.Fatal("already in plan — must report success")
	}
	if got := r.sentKeysLen(); got != 0 {
		t.Fatalf("sent keys = %d, want none", got)
	}
}

// No readable subagent bar (foreign dialog / wrong pane): abort without
// blind key presses.
func TestPaneSetLiveModeNoBar(t *testing.T) {
	s, r := newDriverForTest(t)

	if s.SetLiveMode("plan") {
		t.Fatal("switch must fail without a readable subagent bar")
	}
	if got := r.sentKeysLen(); got != 0 {
		t.Fatalf("sent keys = %d, want none (no blind Tab)", got)
	}
}

// A running turn blocks mode switches (Tab would land in the input box).
func TestPaneSetLiveModeBusy(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.uiMode = "build"
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if s.SetLiveMode("plan") {
		t.Fatal("mode switch during a turn must fail")
	}
	if got := r.sentKeysLen(); got != 0 {
		t.Fatalf("sent keys = %d, want none", got)
	}
}

// Regression (2026-08-30): a rediscovered session must yield ONLY the
// current turn's reply — never replay earlier history. A cursor=0
// rediscovery once dumped every assistant message of the session into one
// Telegram message.
func TestPaneRediscoveryDoesNotDumpHistory(t *testing.T) {
	s, r := newDriverForTest(t)

	// One session carrying 3 old turns plus the current one.
	exp := ocExport{}
	for i := 0; i < 3; i++ {
		u := ocMessage{}
		u.Info.Role = "user"
		u.Parts = []ocPart{{Type: "text", Text: fmt.Sprintf("history q%d", i)}}
		a := ocMessage{}
		a.Info.Role = "assistant"
		a.Parts = []ocPart{{Type: "text", Text: fmt.Sprintf("history a%d", i)}}
		exp.Messages = append(exp.Messages, u, a)
	}
	u := ocMessage{}
	u.Info.Role = "user"
	u.Parts = []ocPart{{Type: "text", Text: "current question"}}
	a := ocMessage{}
	a.Info.Role = "assistant"
	a.Parts = []ocPart{{Type: "text", Text: "current answer"}}
	exp.Messages = append(exp.Messages, u, a)

	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_hist", Directory: "/ws/foo", Updated: 9000}})
	r.exports = map[string]string{"ses_hist": marshalExport(t, exp)}
	r.captures = append(r.captures, "idle ctrl+p", "… esc interrupt …", "idle ctrl+p", "idle ctrl+p")
	r.mu.Unlock()

	if err := s.Send("current question", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, _ := waitResult(t, s.Events(), 3*time.Second)
	if len(texts) != 1 || texts[0] != "current answer" {
		t.Fatalf("texts = %v, want only the current turn's reply", texts)
	}
	for _, txt := range texts {
		if strings.Contains(txt, "history a") {
			t.Fatalf("history replayed: %q", txt)
		}
	}
}

// Regression (2026-08-30): a Send into a busy window must be rejected up
// front. opencode queues such messages; the done-detector then fires on the
// foreign turn's completion and the queued reply is lost or misattributed
// (production: "no session ends with our prompt" / "no assistant reply").
func TestPaneSendRejectsBusyWindow(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{"… esc interrupt …", "… esc interrupt …", "… esc interrupt …", "… esc interrupt …"}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, done := waitResult(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("busy reject must complete the turn immediately")
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "窗口正忙") {
		t.Fatalf("texts = %v, want busy notice", texts)
	}
	if got := r.lastInjected(); got != "" {
		t.Fatalf("injected = %q, want nothing sent into the busy window", got)
	}
}

// waitTrace collects the full event trace (texts + tool events) until the
// result event — the pseudo-stream tests need ordering, not just texts.
func waitTrace(t *testing.T, events <-chan core.Event, timeout time.Duration) (texts []string, tools []string, done bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return texts, tools, false
			}
			switch ev.Type {
			case core.EventResult:
				return texts, tools, ev.Done
			case core.EventText:
				if ev.Content != "" {
					texts = append(texts, ev.Content)
				}
			case core.EventToolUse:
				tools = append(tools, ev.ToolName+"|"+ev.ToolInput)
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
}

func ocMsg(role, text string) ocMessage {
	m := ocMessage{}
	m.Info.Role = role
	m.Parts = []ocPart{{Type: "text", Text: text}}
	return m
}

// The pseudo-stream must feed tool events (progress cards) and text deltas
// in true part order, with the final result neither duplicating nor losing
// text.
func TestPaneStreamToolEvents(t *testing.T) {
	s, r := newDriverForTest(t)

	mk := func(msgs ...ocMessage) string {
		e := ocExport{}
		e.Messages = append(e.Messages, msgs...)
		return marshalExport(t, e)
	}
	toolMsg := func(name, callID, cmd string) ocMessage {
		m := ocMessage{}
		m.Info.Role = "assistant"
		m.Parts = []ocPart{{
			Type:   "tool",
			Tool:   name,
			CallID: callID,
			State:  &ocToolState{Status: "completed", Input: []byte(`{"command":"` + cmd + `"}`)},
		}}
		return m
	}
	v1 := mk(ocMsg("user", "hi"), ocMsg("assistant", "开始"))
	v2 := mk(ocMsg("user", "hi"), ocMsg("assistant", "开始"), toolMsg("bash", "c1", "ls -la"), ocMsg("assistant", "完成"))

	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_stream", Directory: "/ws/foo", Updated: 9000}})
	r.exportSeq = map[string][]string{"ses_stream": {v1, v2, v2}}
	r.captures = []string{"idle ctrl+p", "… esc interrupt …", "… esc interrupt …", "… esc interrupt …", "idle ctrl+p", "idle ctrl+p"}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, tools, done := waitTrace(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("turn must complete")
	}
	if len(tools) != 1 || tools[0] != "bash|ls -la" {
		t.Fatalf("tools = %v, want one bash|ls -la event", tools)
	}
	if joined := strings.Join(texts, ""); joined != "开始\n\n完成" {
		t.Fatalf("joined texts = %q, want the full reply exactly once", joined)
	}
}

// A mid-stream text rewrite breaks prefix compatibility: deltas stop, and
// the final reconciliation resends the full reply (accepted duplication).
func TestPaneStreamRewriteFallback(t *testing.T) {
	s, r := newDriverForTest(t)

	mk := func(reply string) string {
		e := ocExport{}
		e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", reply))
		return marshalExport(t, e)
	}
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_rw", Directory: "/ws/foo", Updated: 9000}})
	// Frame 1 feeds discovery's own export probe, frame 2 the first stream
	// tick (stale delta), frame 3+ the rewrite.
	r.exportSeq = map[string][]string{"ses_rw": {mk("旧版本回复"), mk("旧版本回复"), mk("完全不同的新回复")}}
	r.captures = []string{"idle ctrl+p", "… esc interrupt …", "… esc interrupt …", "idle ctrl+p", "idle ctrl+p"}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, _, done := waitTrace(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("turn must complete")
	}
	if len(texts) != 2 || texts[0] != "旧版本回复" || texts[1] != "完全不同的新回复" {
		t.Fatalf("texts = %q, want stale delta then full resend", texts)
	}
}

// Echo guard (C): a sticky session whose last user message is not ours (the
// host typed in the shared window between turns) must abort with an explicit
// error instead of delivering the host's reply.
func TestPaneStickyEchoMismatch(t *testing.T) {
	s, r := newDriverForTest(t)
	r.scriptTurn("one", "first")
	if err := s.Send("one", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitResult(t, s.Events(), 3*time.Second)
	ses := s.CurrentSessionID()

	// The host interjected between our turns: their message is the last
	// user message now.
	r.mu.Lock()
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "one"), ocMsg("assistant", "first"), ocMsg("user", "host said something"), ocMsg("assistant", "host reply"))
	r.exports[ses] = marshalExport(t, e)
	r.captures = append(r.captures, "… esc interrupt …", "idle ctrl+p", "idle ctrl+p")
	r.mu.Unlock()

	if err := s.Send("two", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, _, done := waitTrace(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("turn must complete (with the abort notice)")
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "主机") {
		t.Fatalf("texts = %q, want explicit misattribution abort", texts)
	}
	for _, txt := range texts {
		if strings.Contains(txt, "host reply") {
			t.Fatalf("host reply leaked: %q", txt)
		}
	}
}

// permDialogFrame renders a probe-accurate permission dialog capture: no
// "esc interrupt" hint (the marker vanishes while the dialog waits).
func permDialogFrame(cmd string) string {
	return "  ┃  △ Permission required\n" +
		"  ┃    # Shell command\n" +
		"  ┃\n" +
		"  ┃  $ " + cmd + "\n" +
		"  ┃\n" +
		"  ┃   Allow once   Allow always   Reject\n"
}

// questionDialogFrame renders a probe-accurate question-tool capture
// (opencode 1.18.25, English UI): question text, numbered options with
// description rows, custom-answer entry, workdir status row, and the
// "⇆ select  enter submit  esc dismiss" footer. No "esc interrupt" hint.
func questionDialogFrame(question string, opts ...string) string {
	b := "  ┃\n"
	b += "  ┃  " + question + "\n"
	b += "  ┃\n"
	for i, o := range opts {
		b += "  ┃  " + strconv.Itoa(i+1) + ". " + o + "\n"
	}
	b += "  ┃  " + strconv.Itoa(len(opts)+1) + ". Type your own answer\n"
	b += "  ┃                                                                  /tmp/opencode/probe\n"
	b += "  ┃  ⇆ select  enter submit  esc dismiss\n"
	return b
}

// multiQuestionFrame renders a tabbed page (tab strip above the question)
// with the multi-question footer, probed 2026-09-05.
func multiQuestionFrame(tabs string, question string, opts ...string) string {
	b := "  ┃\n"
	b += "  ┃  " + tabs + "\n"
	b += "  ┃\n"
	b += "  ┃  " + question + "\n"
	b += "  ┃\n"
	for i, o := range opts {
		b += "  ┃  " + strconv.Itoa(i+1) + ". " + o + "\n"
	}
	b += "  ┃  " + strconv.Itoa(len(opts)+1) + ". Type your own answer\n"
	b += "  ┃  ⇆ tab  ↑↓ select  enter confirm  esc dismiss\n"
	return b
}

// confirmPageFrame renders the multi-question Review page (no "select" in
// the footer — that is what distinguishes it from a question page).
func confirmPageFrame() string {
	return "  ┃\n" +
		"  ┃   Beverage   Sugar   Confirm\n" +
		"  ┃\n" +
		"  ┃  Review\n" +
		"  ┃  Beverage: coffee\n" +
		"  ┃  Sugar: yes\n" +
		"  ┃  ⇆ tab  enter submit  esc dismiss\n"
}

// extractQuestionDialog: positive (footer anchor), negatives (permission
// footer, plain text mentioning the words but no footer line).
func TestExtractQuestionDialog(t *testing.T) {
	block, ok := extractQuestionDialog(questionDialogFrame("coffee or tea?", "coffee", "tea"))
	if !ok {
		t.Fatal("question dialog not detected")
	}
	if !strings.Contains(block, "coffee or tea?") || !strings.Contains(block, "esc dismiss") {
		t.Fatalf("block = %q", block)
	}
	if _, ok := extractQuestionDialog(permDialogFrame("echo hi")); ok {
		t.Fatal("permission dialog must not match the question detector")
	}
	if _, ok := extractQuestionDialog("we discussed how to enter submit and esc dismiss flows in prose\nmore prose"); ok {
		t.Fatal("prose mentioning the words must not match (no footer line)")
	}
	// Preview: question line, not options/status/footer.
	if got := questionPreview(block); got != "coffee or tea?" {
		t.Fatalf("preview = %q, want the question line", got)
	}
	// A slash-carrying question line must not be mistaken for the workdir
	// status row (it ends with a question mark).
	b2, ok := extractQuestionDialog(questionDialogFrame("合并到 main/dev 还是 main/master？", "a", "b"))
	if !ok {
		t.Fatal("slashy question dialog not detected")
	}
	if got := questionPreview(b2); got != "合并到 main/dev 还是 main/master？" {
		t.Fatalf("preview = %q, want the slashy question line kept", got)
	}
}

// paneBusy must hold while the question dialog waits (busy union).
func TestPaneBusyQuestionDialog(t *testing.T) {
	if !paneBusy(questionDialogFrame("q?", "a", "b")) {
		t.Fatal("question dialog must count as busy")
	}
	if paneBusy("idle ctrl+p") {
		t.Fatal("idle baseline must not be busy")
	}
	if !paneBusy(permDialogFrame("ls")) {
		t.Fatal("permission dialog must still count as busy (regression)")
	}
}

// A question dialog persisting across several ticks emits exactly ONE IM
// notification; once answered on the host, the turn completes normally.
func TestPaneQuestionDialogNotifiesOnce(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_q", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_q"] = marshalExport(t, e)
	frame := questionDialogFrame("coffee or tea?", "coffee", "tea")
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		frame,
		frame,
		frame,
		"working esc interrupt",
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var notes, texts []string
	var reqs int
	var req *core.Event
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				reqs++
				c := ev
				req = &c
			case core.EventText:
				if strings.Contains(ev.Content, "等待选择") {
					notes = append(notes, ev.Content)
				} else {
					texts = append(texts, ev.Content)
				}
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if reqs != 1 {
		t.Fatalf("question dialog must emit exactly 1 AskUserQuestion request, got %d", reqs)
	}
	if req == nil || req.ToolName != "AskUserQuestion" || len(req.Questions) != 1 {
		t.Fatalf("request = %+v, want AskUserQuestion with one question", req)
	}
	q := req.Questions[0]
	if q.Question != "coffee or tea?" || len(q.Options) != 2 ||
		q.Options[0].Label != "coffee" || q.Options[1].Label != "tea" {
		t.Fatalf("question = %+v, want coffee/tea options without the free-text entry", q)
	}
	if len(notes) != 0 {
		t.Fatalf("parseable dialog must not fall back to the L1 notice: %q", notes)
	}
	if !strings.Contains(strings.Join(texts, ""), "done") {
		t.Fatalf("texts = %q, want the reply delivered after host answer", texts)
	}
}

// A question dialog whose page cannot be parsed (no option rows) degrades
// to the L1 host-window notice — never a broken button card.
func TestPaneQuestionDialogParseFailureFallsBack(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_q2", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_q2"] = marshalExport(t, e)
	frame := "  ┃\n  ┃  odd dialog without options\n  ┃  ⇆ select  enter submit  esc dismiss\n"
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		frame,
		frame,
		"working esc interrupt",
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var notes []string
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				t.Fatalf("unparseable dialog must not emit a request: %+v", ev)
			case core.EventText:
				if strings.Contains(ev.Content, "等待选择") {
					notes = append(notes, ev.Content)
				}
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "主机窗口") {
		t.Fatalf("fallback notes = %q, want the L1 host guidance", notes)
	}
}

// questionFingerprint stability (2026-09-05 incident): the fingerprint is
// derived from the parsed question+labels, so volatile rows inside the
// 20-line block window (streaming text, spinners, "master" branch rows)
// must NOT change it; different options must.
func TestQuestionFingerprintStable(t *testing.T) {
	base := questionDialogFrame("coffee or tea?", "coffee", "tea")
	// Same dialog, volatile rows above and below drifted.
	drifted := "  ┃  ⠋ thinking about it…\n" + base +
		"  ┃  master\n  ┃  ⠙ more spinner\n"
	if questionFingerprint(base) != questionFingerprint(drifted) {
		t.Fatal("volatile rows inside the block must not change the fingerprint")
	}
	if questionFingerprint(base) == questionFingerprint(questionDialogFrame("coffee or tea?", "coffee", "chocolate")) {
		t.Fatal("different option labels must change the fingerprint")
	}
	if questionFingerprint(base) == questionFingerprint(questionDialogFrame("tea or coffee?", "coffee", "tea")) {
		t.Fatal("different question text must change the fingerprint")
	}
}

// questionPreview is structural (2026-09-05 incident): the question line
// directly above the option block wins over keyword heuristics — a bare
// "master" git-branch row below the options must never surface, and junk
// streamed above the question must not either.
func TestQuestionPreviewStructural(t *testing.T) {
	frame := "  ┃  ⠋ processing… 42%  $0.03\n" +
		"  ┃\n" +
		"  ┃  coffee 还是 tea？\n" +
		"  ┃\n" +
		"  ┃  1. coffee\n" +
		"  ┃     coffee\n" +
		"  ┃  2. tea\n" +
		"  ┃     tea\n" +
		"  ┃  3. Type your own answer\n" +
		"  ┃  master\n" +
		"  ┃  ⇆ select  enter submit  esc dismiss\n"
	if got := questionPreview(frame); got != "coffee 还是 tea？" {
		t.Fatalf("preview = %q, want the structural question line", got)
	}
	// Multi-question strip: the tab strip is skipped, current tab's
	// question wins.
	mq := multiQuestionFrame(" Beverage   Sugar   Confirm", "sugar, yes or no?", "yes", "no")
	if got := questionPreview(mq); got != "sugar, yes or no?" {
		t.Fatalf("multi-question preview = %q", got)
	}
}

// The 2026-09-05 production incident, replayed: a busy pane streams
// numbered-list conversation text above the dialog (the incident parsed
// "2. Stable fingerprint -> injection works" as an option) and a bare
// "master" branch-status row sits in the dialog chrome. The tight
// structural block must exclude both.
func TestExtractQuestionDialogTightBlock(t *testing.T) {
	cap := "  ┃  Thought: 471ms\n" +
		"  ┃  Let me verify the fixes:\n" +
		"  ┃  1. card question text correct\n" +
		"  ┃  2. stable fingerprint -> injection works\n" +
		"  ┃  3. turn completes\n" +
		"  ┃\n" +
		"  ┃  coffee 还是 tea？（第三轮复测）\n" +
		"  ┃\n" +
		"  ┃  1. coffee\n" +
		"  ┃     coffee\n" +
		"  ┃  2. tea\n" +
		"  ┃     tea\n" +
		"  ┃  3. Type your own answer\n" +
		"  ┃  master\n" +
		"  ┃  ⇆ select  enter submit  esc dismiss\n"
	block, ok := extractQuestionDialog(cap)
	if !ok {
		t.Fatal("dialog not detected under streamed junk")
	}
	q, nums, freeIdx, ok := parseQuestionPage(block)
	if !ok {
		t.Fatalf("parse failed on tight block: %q", block)
	}
	if len(q.Options) != 2 || q.Options[0].Label != "coffee" || q.Options[1].Label != "tea" {
		t.Fatalf("options = %+v — streamed numbered lines leaked in", q.Options)
	}
	if len(nums) != 2 || nums[0] != 1 || nums[1] != 2 || freeIdx != 3 {
		t.Fatalf("nums = %v free = %d", nums, freeIdx)
	}
	if q.Question != "coffee 还是 tea？（第三轮复测）" {
		t.Fatalf("question = %q — chrome/status rows leaked in", q.Question)
	}
	// Fingerprint only sees the stable fields, so the junk above never
	// mattered for it — but assert it end-to-end anyway.
	if strings.Contains(questionFingerprint(block), "master") {
		t.Fatal("unreachable: fp is a hash")
	}
}

// F2 (2026-09-05 review): digit keys only exist for 1-9 — answering the
// 10th option must degrade to an explicit error, never sendText("10")
// (the leading "1" would select option 1 AND auto-confirm silently).
func TestRespondQuestionBeyondNineOptions(t *testing.T) {
	s, r := newDriverForTest(t)
	opts := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	frame := questionDialogFrame("pick one", opts...)
	r.mu.Lock()
	r.captures = []string{frame}
	r.mu.Unlock()
	block, _ := extractQuestionDialog(frame)
	fp := questionFingerprint(block)

	err := s.RespondPermission(fp, core.PermissionResult{
		Behavior:     "allow",
		UpdatedInput: map[string]any{"answers": map[string]any{"pick one": "j"}},
	})
	if err == nil || !strings.Contains(err.Error(), "主机窗口") {
		t.Fatalf("want out-of-range degradation error, got %v", err)
	}
	if len(r.SentTexts()) != 0 || r.containsKey("Enter") {
		t.Fatal("out-of-range answer must inject nothing")
	}
}

// F3 (2026-09-05 review): a footer-matched span block whose "options" are
// conversation junk (numbered-list text, no "Type your own answer" entry)
// must NOT parse into a card — real dialogs always carry the free-text
// entry (probed contract). The caller degrades to the L1 notice.
func TestParseQuestionPageRejectsJunkWithoutFreeEntry(t *testing.T) {
	junk := "  ┃  1. first fix applied\n" +
		"  ┃  2. stable fingerprint\n" +
		"  ┃  3. turn completes\n" +
		"  ┃\n" +
		"  ┃  ⇆ select  enter submit  esc dismiss\n"
	if _, _, _, ok := parseQuestionPage(junk); ok {
		t.Fatal("junk numbered lines without the free-text entry must not parse")
	}
	// Detection still fires (paneBusy relies on it) — the block is the
	// span fallback, parsing is what rejects it.
	if _, ok := extractQuestionDialog(junk); !ok {
		t.Fatal("footer detection must still fire for busy-union purposes")
	}
}

// F5 (2026-09-05 review): wrapped questions join into one preview line.
func TestQuestionPreviewWrappedQuestion(t *testing.T) {
	frame := "  ┃  这个问题非常长以至于在窗口里\n" +
		"  ┃  折成了两行显示\n" +
		"  ┃\n" +
		"  ┃  1. yes\n" +
		"  ┃  2. no\n" +
		"  ┃  3. Type your own answer\n" +
		"  ┃  ⇆ select  enter submit  esc dismiss\n"
	if got := questionPreview(frame); got != "这个问题非常长以至于在窗口里 折成了两行显示" {
		t.Fatalf("preview = %q, want the joined wrapped lines", got)
	}
}

// Regression (2026-09-05 production trace, round 5): after an IM answer
// injects the digit, the TUI dismiss animation lingers ~2s — the poll
// loop re-detected the same stable fingerprint and RE-EMITTED a duplicate
// card, which then orphaned (false "gone without IM answer" warn). The
// answered dialog keeps its fingerprint (dedup holds) and the gone-branch
// must not treat it as an orphan.
func TestNoReemitAfterIMAnswer(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_nr", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_nr"] = marshalExport(t, e)
	frame := questionDialogFrame("coffee or tea?", "coffee", "tea")
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		frame,
		frame,
		frame, // dismiss animation lingers
		frame,
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Wait for the card, answer it, then count total requests until the
	// turn completes — exactly one must have been emitted.
	var reqs int
	var fp string
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				reqs++
				fp = ev.RequestID
				// Answer as soon as the card is out (engine side would).
				if err := s.RespondPermission(fp, core.PermissionResult{
					Behavior:     "allow",
					UpdatedInput: map[string]any{"answers": map[string]any{"coffee or tea?": "tea"}},
				}); err != nil {
					t.Fatalf("RespondPermission: %v", err)
				}
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if reqs != 1 {
		t.Fatalf("requests = %d, want exactly 1 (no re-emit during dismiss animation)", reqs)
	}
	if got := r.SentTexts(); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("sentTexts = %v, want the single answer digit", got)
	}
	// And the markers are clean after the dialog is gone.
	if fpA, answered, card := s.qState(); fpA != "" || answered || card {
		t.Fatalf("qState = (%q, %v, %v), want cleared after dialog gone", fpA, answered, card)
	}
}

// phantomFrame renders the 2026-09-06 incident shape: pane text that
// CONTAINS the footer anchor verbatim (the detector's own source shown on
// screen) plus junk numbered rows — never a real dialog. The anchor line
// is assembled at runtime: this file must not carry the token trio on one
// line, or the live daemon watching this pane would detect its own test.
func phantomFrame(drift string) string {
	foot := "  ┃  ⇆ select  enter " + "sub" + "mit  esc " + "dis" + "miss"
	return "  ┃  $ grep -n 'func extract' agent/llmw/pane_inner.go\n" +
		"  ┃  " + drift + "\n" +
		"  ┃  2. stable fingerprint → injection works " + drift + "\n" +
		"  ┃  7. master branch row\n" +
		"  ┃                                                                  /tmp/opencode/probe\n" +
		foot + "\n"
}

// The incident regression: footer-matched junk must NEVER emit a card
// (cards hijack IM messages as answers), and the L1 fallback notice fires
// at most once while the junk drifts across ticks.
func TestPhantomAnchorNoCardNoSpam(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_ph", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_ph"] = marshalExport(t, e)
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		phantomFrame("tick A"),
		phantomFrame("tick B"),
		phantomFrame("tick C"),
		phantomFrame("tick C"),
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	cards, notices := 0, 0
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				cards++
			case core.EventText:
				if strings.Contains(ev.Content, "弹窗解析失败") {
					notices++
				}
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if cards != 0 {
		t.Fatalf("cards = %d, want 0 (phantom anchors must never emit cards)", cards)
	}
	if notices > 1 {
		t.Fatalf("fallback notices = %d, want <= 1 across drifting ticks", notices)
	}
	if got := r.SentTexts(); len(got) != 0 {
		t.Fatalf("sentTexts = %v, want none (no keys injected for phantoms)", got)
	}
	if fp, answered, card := s.qState(); fp != "" || answered || card {
		t.Fatalf("qState = (%q, %v, %v), want cleared after dialog gone", fp, answered, card)
	}
}

// Prose quoting the anchor token words WITHOUT the footer glyph must stay
// COMPLETELY silent: during /llmw_compact the TUI shows the session
// summary, and the agent's own summary quoted the discipline line
// token-for-token (2026-09-06 second incident) — the glyph gate in
// extractQuestionDialog keeps this class from anchoring at all, so not
// even the rate-limited L1 notice may fire. Assembled at runtime like
// phantomFrame: this file must not carry the token trio on one line.
func TestProseQuotingTokensSilent(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_prose", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_prose"] = marshalExport(t, e)
	prose := func(drift string) string {
		line := "  \u2503  - never render on one line: \"select\" + \"" + "enter submit" +
			"\" / \"" + "enter confirm" + "\" + \"" + "esc dismiss" + "\" " + drift
		return "  \u2503  ## Important Details\n" +
			"  \u2503  - the shared pane renders all tool output\n" +
			line + "\n" +
			"  \u2503  - another summary bullet\n"
	}
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		prose("tick A"),
		prose("tick B"),
		prose("tick C"),
		prose("tick C"),
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	cards, notices := 0, 0
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				cards++
			case core.EventText:
				if strings.Contains(ev.Content, "\u5f39\u7a97\u89e3\u6790\u5931\u8d25") {
					notices++
				}
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if cards != 0 || notices != 0 {
		t.Fatalf("cards = %d, notices = %d; want 0/0: prose without the glyph must not anchor", cards, notices)
	}
	if got := r.SentTexts(); len(got) != 0 {
		t.Fatalf("sentTexts = %v, want none", got)
	}
}

// The rate-limit gate itself: forced to zero, drifting junk re-notifies;
// that proves the default-interval suppression in the test above is the
// timer's doing, not an accident of fingerprint equality.
func TestFallbackRateLimitGate(t *testing.T) {
	old := questionFallbackMinInterval
	questionFallbackMinInterval = 0
	defer func() { questionFallbackMinInterval = old }()

	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_rl", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_rl"] = marshalExport(t, e)
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		phantomFrame("d1"),
		phantomFrame("d2"),
		phantomFrame("d3"),
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	notices := 0
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventText:
				if strings.Contains(ev.Content, "弹窗解析失败") {
					notices++
				}
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if notices < 2 {
		t.Fatalf("notices = %d, want >= 2 with the gate forced open", notices)
	}
}

// parseQuestionPage strict shape (2026-09-06): numbered rows must run
// 1..N with the free-text entry last; enumerated QUESTION text above the
// options must not break the parse (longest valid suffix wins).
func TestParseQuestionPageStrictShape(t *testing.T) {
	if _, _, _, ok := parseQuestionPage(questionDialogFrame("q?", "a", "b")); !ok {
		t.Fatal("fixture frame must parse")
	}
	gap := "  ┃\n  ┃  pick one\n  ┃\n  ┃  1. a\n  ┃  2. b\n  ┃  4. Type your own answer\n"
	if _, _, _, ok := parseQuestionPage(gap); ok {
		t.Fatal("numbering gap (1,2,4) must be rejected")
	}
	entryNotLast := "  ┃\n  ┃  pick one\n  ┃\n  ┃  1. a\n  ┃  2. Type your own answer\n  ┃  3. b\n"
	if _, _, _, ok := parseQuestionPage(entryNotLast); ok {
		t.Fatal("free-text entry not last must be rejected")
	}
	noSuffix := "  ┃\n  ┃  pick one\n  ┃\n  ┃  2. x\n  ┃  5. y\n  ┃  9. Type your own answer\n"
	if _, _, _, ok := parseQuestionPage(noSuffix); ok {
		t.Fatal("no 1..N suffix must be rejected")
	}
	enumQ := "  ┃\n  ┃  choose mode:\n  ┃  1. fast\n  ┃  2. slow\n  ┃\n  ┃  1. fast\n  ┃  2. slow\n  ┃  3. Type your own answer\n"
	q, nums, freeIdx, ok := parseQuestionPage(enumQ)
	if !ok {
		t.Fatal("enumerated question text above options must still parse (suffix)")
	}
	if len(q.Options) != 2 || q.Options[0].Label != "fast" || q.Options[1].Label != "slow" {
		t.Fatalf("options = %+v, want fast/slow from the suffix region", q.Options)
	}
	if !reflect.DeepEqual(nums, []int{1, 2}) || freeIdx != 3 {
		t.Fatalf("nums = %v freeIdx = %d, want [1 2] / 3", nums, freeIdx)
	}
}

// parseQuestionPage: option labels, TUI digits, free-text entry index,
// description rows, the multi-question tab strip, and the no-options
// negative (probed layout 2026-09-05, opencode 1.18.28).
func TestParseQuestionPage(t *testing.T) {
	block, ok := extractQuestionDialog(questionDialogFrame("coffee or tea?", "coffee", "tea"))
	if !ok {
		t.Fatal("frame not detected")
	}
	q, nums, freeIdx, ok := parseQuestionPage(block)
	if !ok {
		t.Fatal("parse failed")
	}
	if q.Question != "coffee or tea?" {
		t.Fatalf("question = %q", q.Question)
	}
	if len(q.Options) != 2 || q.Options[0].Label != "coffee" || q.Options[1].Label != "tea" {
		t.Fatalf("options = %+v (free-text entry must be excluded)", q.Options)
	}
	if len(nums) != 2 || nums[0] != 1 || nums[1] != 2 {
		t.Fatalf("nums = %v, want TUI digits [1 2]", nums)
	}
	if freeIdx != 3 {
		t.Fatalf("freeIdx = %d, want 3", freeIdx)
	}

	// Multi-question page: tab strip above the question is not mistaken
	// for content; the footer says "enter confirm" instead of "submit".
	mblock, ok := extractQuestionDialog(multiQuestionFrame(" Beverage   Sugar   Confirm", "sugar, yes or no?", "yes", "no"))
	if !ok {
		t.Fatal("multi-question frame not detected")
	}
	mq, mnums, mfree, ok := parseQuestionPage(mblock)
	if !ok || mq.Question != "sugar, yes or no?" || len(mq.Options) != 2 {
		t.Fatalf("multi-question parse = %+v ok=%v", mq, ok)
	}
	if mnums[0] != 1 || mnums[1] != 2 || mfree != 3 {
		t.Fatalf("multi nums = %v free = %d", mnums, mfree)
	}

	// No option rows → not parseable (caller falls back to the L1 notice).
	if _, _, _, ok := parseQuestionPage("  ┃  no options here\n  ┃  ⇆ select  enter submit  esc dismiss\n"); ok {
		t.Fatal("no-option page must not parse")
	}
}

// paneBusy: the Confirm page (footer without "select") keeps the turn
// busy; a question footer with "select" is not misread as a confirm page.
func TestPaneBusyConfirmPage(t *testing.T) {
	if !isQuestionConfirmPage(confirmPageFrame()) {
		t.Fatal("confirm page not detected")
	}
	if !paneBusy(confirmPageFrame()) {
		t.Fatal("confirm page must count as busy (done-detector guard)")
	}
	if isQuestionConfirmPage(questionDialogFrame("q?", "a", "b")) {
		t.Fatal("question page must not be misread as confirm page")
	}
	if isQuestionConfirmPage(multiQuestionFrame(" A   B   Confirm", "q?", "a", "b")) {
		t.Fatal("multi-question question page must not be misread as confirm")
	}
}

// respondQuestionPage via RespondPermission: a matching option label is
// injected as its NUMBER (digit keys select AND auto-confirm — probed
// 2026-09-05); unmatched text goes through the free-text entry (digit
// opens the input, paste, Enter); a stale fingerprint injects nothing.
func TestRespondQuestionDigit(t *testing.T) {
	s, r := newDriverForTest(t)
	frame := questionDialogFrame("coffee or tea?", "coffee", "tea")
	r.mu.Lock()
	r.captures = []string{frame}
	r.mu.Unlock()
	block, _ := extractQuestionDialog(frame)
	fp := questionFingerprint(block)

	err := s.RespondPermission(fp, core.PermissionResult{
		Behavior:     "allow",
		UpdatedInput: map[string]any{"answers": map[string]any{"coffee or tea?": "tea"}},
	})
	if err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	if got := r.SentTexts(); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("sentTexts = %v, want digit 2 only", got)
	}
}

func TestRespondQuestionFreeText(t *testing.T) {
	oldDelay := questionInputOpenDelay
	questionInputOpenDelay = time.Millisecond
	t.Cleanup(func() { questionInputOpenDelay = oldDelay })

	s, r := newDriverForTest(t)
	frame := questionDialogFrame("coffee or tea?", "coffee", "tea")
	r.mu.Lock()
	r.captures = []string{frame}
	r.mu.Unlock()
	block, _ := extractQuestionDialog(frame)
	fp := questionFingerprint(block)

	err := s.RespondPermission(fp, core.PermissionResult{
		Behavior:     "allow",
		UpdatedInput: map[string]any{"answers": map[string]any{"coffee or tea?": "加两块糖，谢谢"}},
	})
	if err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	if got := r.SentTexts(); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("sentTexts = %v, want the free-text entry digit 3", got)
	}
	if got := r.lastInjected(); got != "加两块糖，谢谢" {
		t.Fatalf("injected = %q, want the pasted answer", got)
	}
	if !r.containsKey("Enter") {
		t.Fatal("free-text path must submit with Enter")
	}
}

func TestRespondQuestionStale(t *testing.T) {
	s, r := newDriverForTest(t)
	frame := questionDialogFrame("coffee or tea?", "coffee", "tea")
	r.mu.Lock()
	r.captures = []string{frame}
	r.mu.Unlock()

	if err := s.RespondPermission("deadbeef", core.PermissionResult{
		Behavior:     "allow",
		UpdatedInput: map[string]any{"answers": map[string]any{"q": "tea"}},
	}); err != nil {
		t.Fatalf("stale RespondPermission must be a no-op, got %v", err)
	}
	if len(r.SentTexts()) != 0 || r.containsKey("Enter") {
		t.Fatal("stale fingerprint must inject nothing")
	}
}

// A multi-question dialog: question page emits the AskUserQuestion card;
// after the (host-side or IM) answer the dialog lands on the Confirm page,
// which pollTurn submits automatically — the turn then completes normally.
func TestConfirmPageAutoSubmit(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_mq", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_mq"] = marshalExport(t, e)
	qframe := multiQuestionFrame(" Beverage   Sugar   Confirm", "coffee or tea?", "coffee", "tea")
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		qframe,
		qframe,
		confirmPageFrame(),
		confirmPageFrame(),
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var reqs int
	var texts []string
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				reqs++
			case core.EventText:
				texts = append(texts, ev.Content)
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if reqs != 1 {
		t.Fatalf("question page must emit exactly 1 request, got %d", reqs)
	}
	if !r.containsKey("Enter") {
		t.Fatal("confirm page must be auto-submitted with Enter")
	}
	if !strings.Contains(strings.Join(texts, ""), "done") {
		t.Fatalf("texts = %q, want the reply after confirm submit", texts)
	}
}

// A dialog persisting across several ticks emits exactly ONE permission
// request; once answered on the host, the turn completes normally.
func TestPanePermissionDialogEmitsOnce(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_perm", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_perm"] = marshalExport(t, e)
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		permDialogFrame("echo hi"),
		permDialogFrame("echo hi"),
		permDialogFrame("echo hi"),
		"working esc interrupt",
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var reqs []core.Event
	var texts []string
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				reqs = append(reqs, ev)
			case core.EventText:
				texts = append(texts, ev.Content)
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if len(reqs) != 1 {
		t.Fatalf("permission requests = %d, want exactly 1 (deduped)", len(reqs))
	}
	if reqs[0].ToolName != "bash" || !strings.Contains(reqs[0].ToolInput, "echo hi") {
		t.Fatalf("request = %+v, want bash + echo hi summary", reqs[0])
	}
	if reqs[0].RequestID == "" {
		t.Fatal("request id must be the dialog fingerprint")
	}
	if !strings.Contains(strings.Join(texts, ""), "done") {
		t.Fatalf("texts = %q, want the reply delivered", texts)
	}
}

// permConfirmFrame renders the SECOND permission page ("Always allow",
// probed 2026-08-30). It carries neither the page-1 anchor nor the busy
// marker — paneBusy must still hold (or the done-detector ends the turn
// while the page waits).
func permConfirmFrame(pattern string) string {
	return "  ┃  △ Always allow\n" +
		"  ┃\n" +
		"  ┃  This will allow the following patterns until OpenCode is restarted\n" +
		"  ┃\n" +
		"  ┃  - " + pattern + "\n" +
		"  ┃\n" +
		"  ┃   Confirm   Cancel\n"
}

func TestPaneBusyPermConfirmPage(t *testing.T) {
	if !paneBusy(permConfirmFrame("/root/*")) {
		t.Fatal("the Always-allow confirm page must count as busy")
	}
}

// Abort while a turn runs: Escape is sent, a notice is emitted, and the
// turn completes with whatever partial reply the export holds.
func TestPaneAbortBusyTurn(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_ab", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "partial work"))
	r.exports["ses_ab"] = marshalExport(t, e)
	r.captures = []string{
		"idle ctrl+p",
		"working esc interrupt",
		"working esc interrupt",
		"working esc interrupt",
		"working esc interrupt",
		"working esc interrupt",
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Wait until the turn is visibly running (first busy frame consumed by
	// the poll loop), then abort; retry while a poll tick races us to the
	// remaining busy frames.
	deadline := time.After(3 * time.Second)
	for abortedYet := false; !abortedYet; {
		r.mu.Lock()
		busyLeft := len(r.captures)
		r.mu.Unlock()
		if busyLeft <= 6 {
			err := s.Abort()
			if err == nil {
				abortedYet = true
				break
			}
			if !strings.Contains(err.Error(), "没有进行中的回合") {
				t.Fatalf("Abort: %v", err)
			}
		}
		select {
		case <-deadline:
			t.Fatal("could not abort while the turn was busy")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	r.mu.Lock()
	keys := append([]string(nil), r.sentKeys...)
	r.mu.Unlock()
	if len(keys) != 1 || keys[0] != "Escape" {
		t.Fatalf("keys = %v, want a single Escape", keys)
	}
	texts, _, done := waitTrace(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("aborted turn must still complete")
	}
	joined := strings.Join(texts, "")
	if !strings.Contains(joined, "中止信号") {
		t.Fatalf("want abort notice, got %q", texts)
	}
	if !strings.Contains(joined, "partial work") {
		t.Fatalf("want partial reply delivered, got %q", texts)
	}
}

// "/new" lifecycle: the TUI command is injected, the pane returns to the
// idle footer, the sticky session binding resets, and a confirmation event
// plus result are emitted — without a busy cycle (probed: /new never shows
// the busy marker).
func TestPaneNewSessionResetsBinding(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{
		"idle ctrl+p commands",
		"idle ctrl+p commands",
	}
	r.mu.Unlock()
	s.mu.Lock()
	s.sesID = "ses_old"
	s.cursor = 5
	s.mu.Unlock()

	if err := s.Send(newCmd, "", nil, nil); err != nil {
		t.Fatalf("Send /new: %v", err)
	}
	texts, _, done := waitTrace(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("/new must complete without a busy cycle")
	}
	if got := strings.Join(texts, ""); !strings.Contains(got, "已开始新会话") {
		t.Fatalf("want new-session confirmation, got %q", texts)
	}
	if got := panes_lastInjected(r); got != newCmd {
		t.Fatalf("injected = %q, want %q", got, newCmd)
	}
	s.mu.Lock()
	sesID, cursor := s.sesID, s.cursor
	s.mu.Unlock()
	if sesID != "" || cursor != 0 {
		t.Fatalf("sticky binding must reset, got ses=%q cursor=%d", sesID, cursor)
	}
}

func panes_lastInjected(r *fakePaneRunner) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.injected) == 0 {
		return ""
	}
	return r.injected[len(r.injected)-1]
}

// "/new" on a pane that never returns to the idle footer: warning event,
// result still emitted (Send must always complete), binding NOT reset.
func TestPaneNewSessionUncertainKeepsBinding(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{"$ foreign shell"}
	r.mu.Unlock()
	s.mu.Lock()
	s.sesID = "ses_old"
	s.mu.Unlock()

	if err := s.Send(newCmd, "", nil, nil); err != nil {
		t.Fatalf("Send /new: %v", err)
	}
	texts, _, done := waitTrace(t, s.Events(), 4*time.Second)
	if !done {
		t.Fatal("uncertain /new must still emit a result")
	}
	if got := strings.Join(texts, ""); !strings.Contains(got, "未确认执行") {
		t.Fatalf("want uncertainty warning, got %q", texts)
	}
	s.mu.Lock()
	sesID := s.sesID
	s.mu.Unlock()
	if sesID != "ses_old" {
		t.Fatalf("binding must be kept on uncertainty, got %q", sesID)
	}
}

// SelfCheck: healthy when any anchor reads (idle hint / busy / mode bar),
// warning when the pane speaks none of them.
func TestPaneSelfCheck(t *testing.T) {
	s, r := newDriverForTest(t)
	for _, healthy := range []string{
		"idle ctrl+p commands",
		"working esc interrupt",
		"  ┃  Build · glm-5.2 yzr-glm-5_2-1m",
	} {
		r.mu.Lock()
		r.captures = []string{healthy}
		r.lastCapture = ""
		r.mu.Unlock()
		if warn := s.SelfCheck(); warn != "" {
			t.Fatalf("healthy capture %q must pass, got %q", healthy, warn)
		}
	}
	r.mu.Lock()
	r.captures = []string{"$ not opencode"}
	r.lastCapture = ""
	r.mu.Unlock()
	if warn := s.SelfCheck(); warn == "" || !strings.Contains(warn, "界面契约异常") {
		t.Fatalf("foreign pane must warn, got %q", warn)
	}
}

// WaitReady absorbs the fresh-window boot race: blank frames first, anchor
// after → true; a pane that never shows an anchor → false on timeout.
func TestPaneWaitReady(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{"", "", "idle ctrl+p commands"}
	r.lastCapture = ""
	r.mu.Unlock()
	if !s.WaitReady(5 * time.Second) {
		t.Fatal("WaitReady must absorb blank boot frames and return true once an anchor appears")
	}

	s2, r2 := newDriverForTest(t)
	r2.mu.Lock()
	r2.captures = []string{"", ""}
	r2.lastCapture = ""
	r2.mu.Unlock()
	if s2.WaitReady(300 * time.Millisecond) {
		t.Fatal("WaitReady must time out (false) when no anchor ever appears")
	}
}

// Abort on an idle pane is a plain error (no keys sent).
func TestPaneAbortIdle(t *testing.T) {
	s, r := newDriverForTest(t)
	r.captures = []string{"idle ctrl+p"}
	if err := s.Abort(); err == nil || !strings.Contains(err.Error(), "没有进行中的回合") {
		t.Fatalf("want idle error, got %v", err)
	}
	r.mu.Lock()
	keys := append([]string(nil), r.sentKeys...)
	r.mu.Unlock()
	if len(keys) != 0 {
		t.Fatalf("idle abort must not send keys, got %v", keys)
	}
}

// The IM button choice maps to the probed key sequence on the pane.
func TestPanePermissionRespondKeys(t *testing.T) {
	cases := []struct {
		behavior string
		want     []string
	}{
		{"allow", []string{"Enter"}},
		// allow_all is TWO stages: Right+Enter picks "Allow always", then a
		// paced Enter confirms the second page (probed 2026-08-30).
		{"allow_all", []string{"Right", "Enter", "Enter"}},
		{"deny", []string{"Right", "Right", "Enter"}},
	}
	for _, tc := range cases {
		t.Run(tc.behavior, func(t *testing.T) {
			s, r := newDriverForTest(t)
			r.mu.Lock()
			r.sessions = marshalRows([]ocSessionRow{{ID: "ses_pk", Directory: "/ws/foo", Updated: 9000}})
			e := ocExport{}
			e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
			r.exports["ses_pk"] = marshalExport(t, e)
			r.captures = []string{
				"idle ctrl+p",
				permDialogFrame("echo hi"),
				permDialogFrame("echo hi"),
				permDialogFrame("echo hi"),
				permDialogFrame("echo hi"),
				permDialogFrame("echo hi"),
				permDialogFrame("echo hi"),
				"idle ctrl+p",
				"idle ctrl+p",
			}
			r.mu.Unlock()

			if err := s.Send("hi", "", nil, nil); err != nil {
				t.Fatalf("Send: %v", err)
			}
			var reqID string
			timer := time.After(3 * time.Second)
			for reqID == "" {
				select {
				case ev := <-s.Events():
					if ev.Type == core.EventPermissionRequest {
						reqID = ev.RequestID
					}
				case <-timer:
					t.Fatal("no permission request emitted")
				}
			}
			if err := s.RespondPermission(reqID, core.PermissionResult{Behavior: tc.behavior}); err != nil {
				t.Fatalf("RespondPermission: %v", err)
			}
			waitResult(t, s.Events(), 3*time.Second)

			r.mu.Lock()
			got := append([]string(nil), r.sentKeys...)
			r.mu.Unlock()
			if len(got) != len(tc.want) {
				t.Fatalf("keys = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("keys = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// If the host answered the dialog manually before the IM button lands, the
// response is a no-op: no keys are sent (they would type into the prompt).
func TestPanePermissionRespondStale(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_st", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_st"] = marshalExport(t, e)
	r.captures = []string{
		"idle ctrl+p",
		permDialogFrame("echo hi"),
		permDialogFrame("echo hi"),
		permDialogFrame("echo hi"),
		permDialogFrame("echo hi"),
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var reqID string
	timer := time.After(3 * time.Second)
	for reqID == "" {
		select {
		case ev := <-s.Events():
			if ev.Type == core.EventPermissionRequest {
				reqID = ev.RequestID
			}
		case <-timer:
			t.Fatal("no permission request emitted")
		}
	}
	// The host answered in between: every subsequent capture is idle.
	r.mu.Lock()
	r.captures = []string{"idle ctrl+p", "idle ctrl+p", "idle ctrl+p", "idle ctrl+p"}
	r.mu.Unlock()

	if err := s.RespondPermission(reqID, core.PermissionResult{Behavior: "allow"}); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	waitResult(t, s.Events(), 3*time.Second)
	r.mu.Lock()
	keys := len(r.sentKeys)
	r.mu.Unlock()
	if keys != 0 {
		t.Fatalf("sent %d keys on a stale dialog, want 0", keys)
	}
}

// While a dialog waits (busy marker hidden), the turn must NOT be declared
// done — the dialog presence is part of the busy union.
func TestPanePermissionDialogHoldsTurn(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{"idle ctrl+p"}
	r.captures = append(r.captures, permDialogFrame("echo hi"))
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := 0
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == core.EventPermissionRequest {
				reqs++
			}
			if ev.Type == core.EventResult {
				t.Fatalf("turn declared done while dialog pending (after %d reqs)", reqs)
			}
		case <-timer.C:
			if reqs == 0 {
				t.Fatal("no permission request emitted while dialog pending")
			}
			return
		}
	}
}

// The full model-switch flow on a builtin id: two-tier search (raw dashed
// provider fails, spaced form narrows), prefix-variant rows rejected by
// token boundary ("GLM-5.3-Flash" must not match target "glm-5.3"), Down
// walk to the exact row, Enter, and bottom-bar verification.
func TestPaneSetLiveModel(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{
		"  ┃  Build · glm-5.2 yzr-glm-5_2-1m\n", // initial bar: not target
		"Select model\nSearch\nFavorites\nGLM-5.2 Z.AI Coding Plan\nglm-5.2 yzr-glm-5_2-1m\nConnect provider ctrl+a Favorite ctrl+f\n",
		"Select model\nglm-5.3 opencode-go\nConnect provider ctrl+a Favorite ctrl+f\n█▀▀█ █▀▀█\n",                                                 // search1: no results
		"Select model\nglm-5.3 opencode go\nGLM-5.3-Flash (2x usage) OpenCode Go\nGLM-5.3 OpenCode Go\nConnect provider ctrl+a Favorite ctrl+f\n", // search2: rows
		"  ┃  Build · GLM-5.3 OpenCode Go\n", // after Enter: new bar
	}
	r.mu.Unlock()

	if err := s.SetLiveModel("opencode-go/glm-5.3"); err != nil {
		t.Fatalf("SetLiveModel: %v", err)
	}
	r.mu.Lock()
	keys := append([]string(nil), r.sentKeys...)
	texts := append([]string(nil), r.sentTexts...)
	r.mu.Unlock()
	wantKeys := []string{"C-x", "m", "C-u", "C-u", "Down", "Enter"}
	if len(keys) != len(wantKeys) {
		t.Fatalf("keys = %v, want %v", keys, wantKeys)
	}
	for i := range keys {
		if keys[i] != wantKeys[i] {
			t.Fatalf("keys = %v, want %v", keys, wantKeys)
		}
	}
	wantTexts := []string{"glm-5.3 opencode-go", "glm-5.3 opencode go"}
	if len(texts) != 2 || texts[0] != wantTexts[0] || texts[1] != wantTexts[1] {
		t.Fatalf("texts = %v, want %v", texts, wantTexts)
	}
}

// Already on the target model: a no-op success without touching the TUI.
func TestPaneSetLiveModelAlreadyOnTarget(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{"  ┃  Build · glm-5.2 yzr-glm-5_2-1m\n"}
	r.mu.Unlock()

	if err := s.SetLiveModel("yzr-glm-5_2-1m/glm-5.2"); err != nil {
		t.Fatalf("SetLiveModel: %v", err)
	}
	r.mu.Lock()
	nk, nt := len(r.sentKeys), len(r.sentTexts)
	r.mu.Unlock()
	if nk != 0 || nt != 0 {
		t.Fatalf("sent %d keys / %d texts on a no-op switch, want 0/0", nk, nt)
	}
}

// Target not locatable in the dialog: Escape dismisses (current model
// untouched) and the error says so.
func TestPaneSetLiveModelNotFound(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{
		"  ┃  Build · glm-5.2 yzr-glm-5_2-1m\n",
		"Select model\nSearch\nSomething Else OpenCode Go\nConnect provider ctrl+a Favorite ctrl+f\n",
		"Select model\nghost zzz\nSomething Else OpenCode Go\nConnect provider ctrl+a Favorite ctrl+f\n",
	}
	r.mu.Unlock()

	err := s.SetLiveModel("zzz/ghost")
	if err == nil || !strings.Contains(err.Error(), "未在对话框中定位") {
		t.Fatalf("err = %v, want not-located error", err)
	}
	r.mu.Lock()
	keys := append([]string(nil), r.sentKeys...)
	r.mu.Unlock()
	if len(keys) == 0 || keys[len(keys)-1] != "Escape" {
		t.Fatalf("keys = %v, want trailing Escape", keys)
	}
}

func TestPaneSetLiveModelRejectsBusy(t *testing.T) {
	s, r := newDriverForTest(t)
	s.turnActive.Store(true)
	if err := s.SetLiveModel("opencode-go/glm-5.3"); err == nil || !strings.Contains(err.Error(), "回合") {
		t.Fatalf("err = %v, want busy rejection", err)
	}
	r.mu.Lock()
	nk, nt := len(r.sentKeys), len(r.sentTexts)
	r.mu.Unlock()
	if nk != 0 || nt != 0 {
		t.Fatalf("busy switch sent %d keys / %d texts, want 0/0", nk, nt)
	}
}

// A compact turn: busy cycle → idle → synthetic confirmation. No export is
// consulted (compaction is a TUI command, not a chat turn).
func TestPaneCompactTurn(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.captures = []string{
		"idle ctrl+p",
		"… esc interrupt …", "… esc interrupt …", // busy (compaction running)
		"idle ctrl+p", "idle ctrl+p", // settled
	}
	r.mu.Unlock()

	if err := s.Send(compactCmd, "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	texts, done := waitResult(t, s.Events(), 3*time.Second)
	if !done {
		t.Fatal("compact turn must end with Done=true")
	}
	if len(texts) != 1 || texts[0] != compactDoneMsg {
		t.Fatalf("texts = %v, want the synthetic compact confirmation", texts)
	}
	if got := r.lastInjected(); got != compactCmd {
		t.Fatalf("injected = %q, want %q", got, compactCmd)
	}
	r.mu.Lock()
	exports := len(r.exports)
	r.mu.Unlock()
	if exports != 0 {
		t.Fatalf("exports armed = %d, want 0 (no reply extraction for compact)", exports)
	}
}

// TestEchoImmunityAllAnchors is the 2026-09-06 wedge regression: the pane
// renders the agent's own tool output above the chrome, and echoes of
// EVERY anchor (probe commands quoting the busy marker, code quotes of the
// perm-confirm sentence, README anchor lists quoting the question footer
// WITH its glyph and the confirm footer without select, "Select model"
// quotes) kept paneBusy true forever — IM was stuck on the busy reply
// after a compact. A pane whose transcript carries all those echoes but
// whose chrome is idle must read idle and healthy, and no detector may
// fire. Tokens are assembled at runtime (phantomFrame discipline).
func TestEchoImmunityAllAnchors(t *testing.T) {
	bm := "esc " + "inter" + "rupt"
	pc := "This will allow the following " + "pat" + "terns"
	foot := "  ┃  ⇆ select  enter " + "sub" + "mit  esc " + "dis" + "miss"
	confoot := "  ┃  ⇆ tab  enter " + "sub" + "mit  esc " + "dis" + "miss"
	frame := "  ┃  print('busy' if '" + bm + "' in cap else 'not busy')\n" +
		"  ┃  permConfirmMarker = \"" + pc + "...\"\n" +
		"  ┃  `Permission required` / `Allow " + "once`（权限页 1）\n" +
		"  ┃  README anchor doc: " + foot + "\n" +
		"  ┃  README confirm doc: " + confoot + "（Confirm 页）\n" +
		"  ┃  `Select model`（模型对话框）\n" +
		"  ┃  working " + bm + "\n" +
		"  ┃\n" +
		"  ┃  Build · glm-5.3 yzr-glm-5_3-1m\n" +
		"  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n" +
		"  tab agents  ctrl+p commands\n"
	if paneBusy(frame) {
		t.Fatal("echoed anchors above the chrome must not read as busy")
	}
	if _, ok := extractPermDialog(frame); ok {
		t.Fatal("same-line perm echo must not extract")
	}
	if _, ok := extractQuestionDialog(frame); ok {
		t.Fatal("quoted glyph footer in the transcript must not anchor")
	}
	if isQuestionConfirmPage(frame) {
		t.Fatal("quoted confirm footer in the transcript must not count")
	}
	if modelDialogPresent(frame) {
		t.Fatal("quoted Select model without the action bar must not count")
	}
	if !paneAnchorsReadable(frame) {
		t.Fatal("idle chrome (bar + hint) must still read healthy")
	}
	// Quoted footer with content BELOW it (the compact-summary wedge
	// shape): the footer is not bottom-anchored, so it must not anchor.
	junkBelow := "  ┃  doc: " + foot + "\n" +
		"  ┃  streaming text below the quote\n" +
		"  ┃  more streaming\n" +
		"  ┃  even more\n" +
		"  ┃  Build · glm-5.3\n" +
		"  ╹▀▀▀▀▀▀▀▀▀▀\n" +
		"  tab agents  ctrl+p commands\n"
	if _, ok := extractQuestionDialog(junkBelow); ok {
		t.Fatal("quoted footer with content below must not anchor (not bottom-anchored)")
	}
}

// TestBusyMarkerZoneOnly: the spinner row is the bottom-most non-empty row
// (probed on both 1.18.28 and 1.18.29); a busy-marker echo up in the
// transcript with idle chrome below must read idle.
func TestBusyMarkerZoneOnly(t *testing.T) {
	bm := "esc " + "inter" + "rupt"
	if !paneBusy("  ⬝⬝⬝⬝  " + bm + "  12.8K") {
		t.Fatal("real spinner row must read busy")
	}
	echo := "  ┃  transcript quotes " + bm + " in a probe command\n" +
		"  ┃\n" +
		"  ┃  Build · glm-5.3\n" +
		"  tab agents  ctrl+p commands\n"
	if paneBusy(echo) {
		t.Fatal("busy-marker echo above idle chrome must read idle")
	}
}

// TestPermDialogProbedShape: the 1.18.29 probe layout — transcript above,
// "△ Permission required" header, detail rows, options row 3rd from bottom
// — must extract (options-first walk-up) and hold busy.
func TestPermDialogProbedShape(t *testing.T) {
	frame := "  ┃  streaming text above the dialog\n" +
		"  ┃  more transcript\n" +
		"  ┃\n" +
		"  ┃  △ Permission required\n" +
		"  ┃    # Shell command\n" +
		"  ┃  $ touch /root/marker.txt\n" +
		"  ┃  - /root/marker.txt\n" +
		"  ┃   Allow once   Allow always   Reject\n" +
		"  ┃\n" +
		"  ┃\n"
	block, ok := extractPermDialog(frame)
	if !ok {
		t.Fatal("probed perm shape must extract")
	}
	if !strings.Contains(block, "Permission required") || !strings.Contains(block, "Allow once") {
		t.Fatalf("block = %q", block)
	}
	if !paneBusy(frame) {
		t.Fatal("permission page 1 must hold busy")
	}
}

// TestModelDialogPairGate: the model dialog is a floating overlay — its
// header text alone (README quote) must never count; the header paired
// with the "Connect provider" action bar must.
func TestModelDialogPairGate(t *testing.T) {
	echo := "  ┃  `Select model`（模型对话框）\n" +
		"  tab agents  ctrl+p commands\n"
	if modelDialogPresent(echo) {
		t.Fatal("header echo without the action bar must not count")
	}
	if rows := parseModelDialogRows(echo); len(rows) != 0 {
		t.Fatalf("rows on echo = %v, want none", rows)
	}
	real := "  ┃  Select model\n" +
		"  ┃  GLM-5.3 OpenCode Go\n" +
		"  ┃  GLM-5.3-Flash OpenCode Go\n" +
		"  ┃  qwen3.8-max yzr\n" +
		"  ┃  Connect provider ctrl+a  Favorites\n"
	if !modelDialogPresent(real) {
		t.Fatal("header + action bar must count")
	}
	rows := parseModelDialogRows(real)
	if len(rows) != 2 || !strings.Contains(rows[0], "GLM-5.3-Flash") {
		t.Fatalf("rows = %v, want the two model rows after the input line", rows)
	}
}

// TestPaneIdleZoneOnly: /llmw_new's confirmation reads the idle hint from
// the bottom chrome zone only — a README echo of the hint text high in the
// transcript must not confirm (2026-09-06 audit follow-up; the predicate
// was a full-pane Contains).
func TestPaneIdleZoneOnly(t *testing.T) {
	echo := "  ┃  README: wait for idle footer `" + paneIdleHint + "` quote\n" +
		"  ┃  transcript filler one\n" +
		"  ┃  transcript filler two\n" +
		"  ┃  transcript filler three\n" +
		"  ┃  transcript filler four\n" +
		"  ┃  transcript filler five\n" +
		"  ┃\n" +
		"  ┃  Build · glm-5.3\n" +
		"  ╹▀▀▀▀▀▀▀▀▀▀▀\n" +
		"  /ws/probe  ⊙ 1 MCP /status\n"
	if paneIdle(echo) {
		t.Fatal("idle-hint echo in the transcript must not read idle")
	}
	real := echo + "  tab agents  " + paneIdleHint + "\n"
	if !paneIdle(real) {
		t.Fatal("real hint in the bottom chrome must read idle")
	}
}

// TestModelDialogEchoAboveRealDialog: parsing must anchor to the header
// NEAREST above the action bar (what modelDialogLocate finds), never the
// topmost echoed "Select model" line — otherwise garbage transcript rows
// between the echo and the real dialog feed wrong Down-key counts.
func TestModelDialogEchoAboveRealDialog(t *testing.T) {
	frame := "  ┃  `Select model`（模型对话框）echoed high up\n" +
		"  ┃  transcript filler row\n" +
		"  ┃  Select model\n" +
		"  ┃  GLM-5.3 OpenCode Go\n" +
		"  ┃  GLM-5.3-Flash OpenCode Go\n" +
		"  ┃  qwen3.8-max yzr\n" +
		"  ┃  Connect provider ctrl+a  Favorites\n"
	if !modelDialogPresent(frame) {
		t.Fatal("real header+action-bar pair must be present")
	}
	rows := parseModelDialogRows(frame)
	if len(rows) != 2 || !strings.Contains(rows[0], "GLM-5.3-Flash") {
		t.Fatalf("rows = %v, want the two rows below the REAL header (echo ignored)", rows)
	}
}

// TestModelDialogFooterPairHardening: the README zone doc quotes the bare
// words "Connect provider" — that echo plus a header echo must NOT count
// as the dialog; the real action bar carries ctrl+a / Favorites on the
// same row (probed 2026-09-06).
func TestModelDialogFooterPairHardening(t *testing.T) {
	echo := "  ┃  配对闸：header + `Connect provider` 行\n" +
		"  ┃  filler\n" +
		"  ┃  `Select model`（模型对话框）\n" +
		"  tab agents  ctrl+p commands\n"
	if modelDialogPresent(echo) {
		t.Fatal("bare Connect provider echo must not pair")
	}
	if rows := parseModelDialogRows(echo); len(rows) != 0 {
		t.Fatalf("rows on bare echo = %v, want none", rows)
	}
}

// TestTallQuestionDialogStillParses: a 6-option dialog with description
// rows and a wrapped question spans ~20 non-empty rows — paneDialogZone=30
// must keep the question text inside the walk-up so the L2 card still
// parses (zone 18 clipped it to a fallback notice).
func TestTallQuestionDialogStillParses(t *testing.T) {
	b := "  ┃  transcript above the tall dialog\n" +
		"  ┃  more filler\n" +
		"  ┃\n" +
		"  ┃  A long question that wraps across multiple lines, first\n" +
		"  ┃  second line of the wrapped question text\n" +
		"  ┃\n"
	for i, o := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
		b += "  ┃  " + strconv.Itoa(i+1) + ". " + o + "\n"
		b += "  ┃     description of " + o + "\n"
	}
	b += "  ┃  7. Type your own answer\n"
	b += "  ┃                                                                  /tmp/opencode/probe\n"
	b += "  ┃  ⇆ select  enter " + "sub" + "mit  esc " + "dis" + "miss\n"
	block, ok := extractQuestionDialog(b)
	if !ok {
		t.Fatal("tall dialog must extract (zone 30)")
	}
	q, _, _, pok := parseQuestionPage(block)
	if !pok {
		t.Fatal("tall dialog must parse to a card, not degrade to a notice")
	}
	if !strings.Contains(q.Question, "wraps across multiple lines") || len(q.Options) != 6 {
		t.Fatalf("question = %q, options = %d (want 6 — the free-text entry is an affordance, not an option)", q.Question, len(q.Options))
	}
}

// TestTallPermDialogStillExtracts: a long wrapped command plus pattern
// rows can push the header ~24 line-rows above the options row — the
// walk-up bound (25) must still reach it or the dialog goes invisible and
// the turn hangs to the 10-minute timeout.
func TestTallPermDialogStillExtracts(t *testing.T) {
	b := "  ┃  △ Permission required\n" +
		"  ┃    # Shell command\n" +
		"  ┃  $ cd /root/some/very/long/path && make build-noweb && cp ./llmw\n"
	for i := 0; i < 16; i++ {
		b += "  ┃  wrapped-command-fragment-line-" + strconv.Itoa(i) + "\n"
	}
	b += "  ┃  - /root/some/very/long/path/*\n"
	for i := 0; i < 4; i++ {
		b += "  ┃\n"
	}
	b += "  ┃   Allow once   Allow always   Reject\n" +
		"  ┃\n"
	block, ok := extractPermDialog(b)
	if !ok {
		t.Fatal("tall perm dialog (header ~24 rows up) must extract (walk-up 25)")
	}
	if !strings.Contains(block, "Permission required") || !strings.Contains(block, "Allow once") {
		t.Fatalf("block = %q", block)
	}
}

// TestTornFrameDoesNotReemitCard: a torn mid-redraw frame (blank capture)
// between two identical dialog frames must not clear the question state
// and re-emit a duplicate card — the debounce requires two consecutive
// misses (paneStableNeeded idea).
func TestTornFrameDoesNotReemitCard(t *testing.T) {
	s, r := newDriverForTest(t)
	r.mu.Lock()
	r.sessions = marshalRows([]ocSessionRow{{ID: "ses_tf", Directory: "/ws/foo", Updated: 9000}})
	e := ocExport{}
	e.Messages = append(e.Messages, ocMsg("user", "hi"), ocMsg("assistant", "done"))
	r.exports["ses_tf"] = marshalExport(t, e)
	frame := questionDialogFrame("coffee or tea?", "coffee", "tea")
	r.captures = []string{
		"idle ctrl+p",
		"working " + "esc " + "inter" + "rupt",
		frame,
		frame, // consumed by RespondPermission's verification capture
		"",    // torn mid-redraw frame: one miss must NOT clear qFP
		frame, // same fingerprint: no re-emit
		"idle ctrl+p",
		"idle ctrl+p",
	}
	r.mu.Unlock()

	if err := s.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs, fp := 0, ""
	deadline := time.After(3 * time.Second)
	done := false
	for !done {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				reqs++
				fp = ev.RequestID
				if err := s.RespondPermission(fp, core.PermissionResult{
					Behavior:     "allow",
					UpdatedInput: map[string]any{"answers": map[string]any{"coffee or tea?": "tea"}},
				}); err != nil {
					t.Fatalf("RespondPermission: %v", err)
				}
			case core.EventResult:
				done = ev.Done
			}
		case <-deadline:
			t.Fatal("no result event within timeout")
		}
	}
	if reqs != 1 {
		t.Fatalf("requests = %d, want 1 (torn frame must not re-emit)", reqs)
	}
	if got := r.SentTexts(); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("sentTexts = %v, want the answer digit", got)
	}
}
