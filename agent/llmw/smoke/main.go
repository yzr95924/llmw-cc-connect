// Smoke test for the llmw agent without IM:
//
//	go run ./agent/llmw/smoke
//
// Exercises the real llmw CLI and workspace, but does NOT spawn an inner
// claude/opencode process (no API calls). Use it to verify wiring after a
// rebuild. Keep in sync with agent/llmw/README.md.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	_ "github.com/chenhg5/cc-connect/agent/llmw" // register the agent
	"github.com/chenhg5/cc-connect/core"
)

func main() {
	ag, err := core.CreateAgent("llmw", map[string]any{
		"backend":     "claude",
		"cc_data_dir": os.TempDir(),
	})
	if err != nil {
		fmt.Println("CreateAgent:", err)
		os.Exit(1)
	}
	fmt.Println("agent:", ag.Name())

	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		fmt.Println("StartSession:", err)
		os.Exit(1)
	}
	defer sess.Close()

	go func() {
		for ev := range sess.Events() {
			if ev.Content != "" {
				fmt.Println("--- EVENT:", ev.Content)
			}
		}
	}()

	// NOTE: stop targets a non-existent wiki on purpose — killing a real
	// byobu window of an existing wiki could destroy a live agent session.
	// The happy path of stop is covered by unit tests.
	for _, cmd := range []string{
		"/llmw list",
		"/llmw enter agent-tools",
		"/llmw status",
		"/llmw wiki --name=no-such-wiki stop",
	} {
		fmt.Println(">>> send:", cmd)
		if err := sess.Send(cmd, "", nil, nil); err != nil {
			fmt.Println("Send:", err)
			os.Exit(1)
		}
		time.Sleep(1500 * time.Millisecond)
	}
	fmt.Println("smoke done (inner agent NOT spawned; see unit tests for that)")
}
