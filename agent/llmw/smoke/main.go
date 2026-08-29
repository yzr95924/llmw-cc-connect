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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	_ "github.com/chenhg5/cc-connect/agent/claudecode" // register backends for real spawn
	_ "github.com/chenhg5/cc-connect/agent/llmw"       // register the agent under test
	_ "github.com/chenhg5/cc-connect/agent/opencode"
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

	if err := checkContract(); err != nil {
		fmt.Println("CONTRACT FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("CONTRACT OK: llmw list/status --json fields match llmw_client.go mirrors")
}

// checkContract guards the hand-mirrored JSON structs in llmw_client.go: if
// the llmw CLI renames a field, parsing degrades silently (zero values) — this
// check turns that into a loud smoke failure.
func checkContract() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := checkKeys(ctx, []string{"list", "--json"},
		[]string{"name", "path", "display_name", "model", "wiki_dir_exists"}); err != nil {
		return err
	}
	return checkKeys(ctx, []string{"status", "--json"},
		[]string{"wiki", "window", "session", "backend", "state", "dead"})
}

func checkKeys(ctx context.Context, args, keys []string) error {
	cmd := exec.CommandContext(ctx, "llmw", args...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("llmw %s: %w", args[0], err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		return fmt.Errorf("llmw %s --json: parse: %w", args[0], err)
	}
	// Empty output is legitimate (e.g. no windows running); field names can
	// only be checked when at least one row exists.
	if len(rows) == 0 {
		fmt.Printf("contract check (%s): no rows, field names unchecked\n", args[0])
		return nil
	}
	for _, k := range keys {
		if _, ok := rows[0][k]; !ok {
			return fmt.Errorf("llmw %s --json: field %q missing (llmw_client.go mirror is stale)", args[0], k)
		}
	}
	return nil
}
