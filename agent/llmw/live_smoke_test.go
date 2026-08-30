//go:build live

package llmw

// Live smoke against the REAL opencode TUI — the machine gate behind the
// README upgrade checklist: after an opencode (or tmux) upgrade, run
//
//	go test -tags live -run TestLivePaneSmoke -v -timeout 600s ./agent/llmw/
//
// and the four stages below verify the screen-anchor contract end to end
// through PRODUCTION code paths (newPaneSession + realPaneRunner). The unit
// tests only ever see synthetic frames, so they stay green when opencode
// changes its UI — this test is what actually goes red.
//
// Sandbox discipline: own tmux session (llmw-smoke) + temp dir; never
// touches the production byobu windows. Costs ~4 small LLM calls.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const liveSmokeSession = "llmw-smoke"

func liveSmokeTmux(args ...string) error {
	return exec.Command("tmux", args...).Run()
}

// liveSmokeWaitEvent reads session events until a match or timeout; the
// accumulated text is returned alongside for assertions.
func liveSmokeWaitEvent(t *testing.T, events <-chan core.Event, timeout time.Duration, match func(core.Event) bool) (core.Event, string) {
	t.Helper()
	var texts []string
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events channel closed")
			}
			if ev.Type == core.EventText {
				texts = append(texts, ev.Content)
			}
			if match(ev) {
				return ev, strings.Join(texts, "\n")
			}
		case <-deadline:
			t.Fatalf("no matching event within %s; texts so far:\n%s", timeout, strings.Join(texts, "\n"))
		}
	}
}

func TestLivePaneSmoke(t *testing.T) {
	for _, bin := range []string{"tmux", "opencode"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s not in PATH: %v", bin, err)
		}
	}
	// Refuse to reuse an existing session — never kill a foreign one.
	if liveSmokeTmux("has-session", "-t", liveSmokeSession) == nil {
		t.Fatalf("tmux session %q already exists; clean it up first (tmux kill-session -t %s)", liveSmokeSession, liveSmokeSession)
	}
	dir := t.TempDir()
	if err := liveSmokeTmux("new-session", "-d", "-s", liveSmokeSession, "-x", "178", "-y", "50", "-c", dir, "opencode"); err != nil {
		t.Fatalf("sandbox tmux session: %v", err)
	}
	t.Cleanup(func() { _ = liveSmokeTmux("kill-session", "-t", liveSmokeSession) })

	s := newPaneSession(context.Background(), &realPaneRunner{target: liveSmokeSession + ":0", workDir: dir}, dir)
	t.Cleanup(func() { _ = s.Close() })

	// ---- Stage 0: the TUI booted (idle footer readable within 20s) ----
	if !s.waitPane(func(c string) bool { return strings.Contains(c, paneIdleHint) }, 20*time.Second) {
		t.Fatal("stage 0: opencode TUI did not reach the idle state within 20s (boot failure or UI drift)")
	}
	t.Log("stage 0 ✓ TUI booted")

	// ---- Stage 1: SelfCheck against the real screen ----
	if warn := s.SelfCheck(); warn != "" {
		t.Fatalf("stage 1: SelfCheck failed against the real TUI: %s", warn)
	}
	t.Log("stage 1 ✓ anchor self-check")

	// ---- Stage 2: full turn roundtrip (inject → busy cycle → export reply) ----
	if err := s.Send("Reply with exactly this word and nothing else: pong", "", nil, nil); err != nil {
		t.Fatalf("stage 2: Send: %v", err)
	}
	_, all := liveSmokeWaitEvent(t, s.Events(), 120*time.Second, func(ev core.Event) bool {
		return ev.Type == core.EventResult && ev.Done
	})
	if !strings.Contains(all, "pong") {
		t.Fatalf("stage 2: reply did not contain 'pong':\n%s", all)
	}
	t.Log("stage 2 ✓ turn roundtrip (busy marker + session discovery + export)")

	// ---- Stage 3: permission dialog surfaces and allow_once answers it ----
	marker := fmt.Sprintf("/root/llmw-smoke-marker-%d.txt", os.Getpid())
	t.Cleanup(func() { _ = os.Remove(marker) })
	if err := s.Send("Run this exact bash command and wait for it to finish: touch "+marker, "", nil, nil); err != nil {
		t.Fatalf("stage 3: Send: %v", err)
	}
	req, _ := liveSmokeWaitEvent(t, s.Events(), 90*time.Second, func(ev core.Event) bool {
		return ev.Type == core.EventPermissionRequest
	})
	if err := s.RespondPermission(req.RequestID, core.PermissionResult{Behavior: "allow"}); err != nil {
		t.Fatalf("stage 3: RespondPermission: %v", err)
	}
	liveSmokeWaitEvent(t, s.Events(), 120*time.Second, func(ev core.Event) bool {
		return ev.Type == core.EventResult && ev.Done
	})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stage 3: marker file not created after allow: %v", err)
	}
	t.Log("stage 3 ✓ permission dialog (page-1 anchor + key mapping + allow)")

	// ---- Stage 4: question dialog notification (footer anchor) ----
	if err := s.Send("Use the question tool to ask me: coffee or tea? Then wait for my answer.", "", nil, nil); err != nil {
		t.Fatalf("stage 4: Send: %v", err)
	}
	liveSmokeWaitEvent(t, s.Events(), 90*time.Second, func(ev core.Event) bool {
		return ev.Type == core.EventText && strings.Contains(ev.Content, "等待选择")
	})
	t.Log("stage 4 ✓ question dialog notification (footer anchor)")
	// Dismiss the dialog on the host side (as the user would with ESC) and
	// let the turn wind down; the reply may be anything.
	s.runner.sendKey("Escape")
	liveSmokeWaitEvent(t, s.Events(), 120*time.Second, func(ev core.Event) bool {
		return ev.Type == core.EventResult && ev.Done
	})
	t.Log("live smoke PASS — TUI anchor contract intact")
}
