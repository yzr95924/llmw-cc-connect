package llmw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	llmwBin             = "llmw"
	defaultWorkspace    = "yzr-llm-wiki-workspace"
	listTimeout         = 5 * time.Second
	enterTimeout        = 15 * time.Second
	pendingSelectionTTL = 60 * time.Second
	statusTimeout       = 5 * time.Second
	contextSizeTimeout  = 8 * time.Second  // per live window
	statusSizeBudget    = 30 * time.Second // whole /llmw status op
	stopTimeout         = 15 * time.Second
)

// errClosed is returned for operations on a closed facade.
var errClosed = errors.New("llmw: session closed")

// wikiEntry mirrors one row of `llmw list --json` (llmw/workspace/manager.py).
type wikiEntry struct {
	Name         string   `json:"name"`
	Path         string   `json:"path"`
	DisplayName  string   `json:"display_name"`
	Tags         []string `json:"tags"`
	Model        string   `json:"model"`
	ModelSource  string   `json:"model_source"`
	DirExists    bool     `json:"wiki_dir_exists"`
	LastActivity string   `json:"last_activity"`
}

// windowRow mirrors one row of `llmw status --json` (llmw/wiki/status.py
// _row_to_dict). state values are the ASCII contract: dead / shell / working /
// waiting / unknown; uptime/idle/dead-ago seconds are optional (nil when the
// tmux markers are missing).
type windowRow struct {
	Wiki           string   `json:"wiki"`
	Window         string   `json:"window"`
	WindowID       string   `json:"window_id"`
	Session        string   `json:"session"`
	Dead           bool     `json:"dead"`
	StartedAt      *int64   `json:"started_at"`
	ActivityAt     *int64   `json:"activity_at"`
	DeadAt         *int64   `json:"dead_at"`
	Backend        string   `json:"backend"`
	Pcmd           string   `json:"pcmd"`
	UptimeSeconds  *float64 `json:"uptime_seconds"`
	IdleSeconds    *float64 `json:"idle_seconds"`
	DeadSecondsAgo *float64 `json:"dead_seconds_ago"`
	State          string   `json:"state"`
}

// llmwClient shells out to the llmw CLI. run and root are injectable for tests.
type llmwClient struct {
	run  func(ctx context.Context, args ...string) ([]byte, error)
	root func() (string, error)
}

func newLlmwClient() *llmwClient {
	c := &llmwClient{}
	c.run = c.runReal
	c.root = c.workspaceRootReal
	return c
}

func (c *llmwClient) runReal(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, llmwBin, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("llmw %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.Bytes(), nil
}

// workspaceRoot resolves the llmw workspace root (injectable via c.root).
func (c *llmwClient) workspaceRoot() (string, error) { return c.root() }

// workspaceRootReal resolves the llmw workspace root: $LLMW_WORKSPACE or the
// default ~/yzr-llm-wiki-workspace, mirroring llmw/config.py.
func (c *llmwClient) workspaceRootReal() (string, error) {
	root := os.Getenv("LLMW_WORKSPACE")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("llmw: cannot resolve home dir: %w", err)
		}
		root = filepath.Join(home, defaultWorkspace)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("llmw: resolve workspace root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "workspace.toml")); err != nil {
		return "", fmt.Errorf("llmw: workspace root %q has no workspace.toml (set LLMW_WORKSPACE or run llmw init)", abs)
	}
	return abs, nil
}

// list returns the wikis of the current workspace via `llmw list --json`.
func (c *llmwClient) list(ctx context.Context) ([]wikiEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	out, err := c.run(ctx, "list", "--json")
	if err != nil {
		return nil, err
	}
	var wikis []wikiEntry
	if err := json.Unmarshal(out, &wikis); err != nil {
		return nil, fmt.Errorf("llmw: parse list --json: %w", err)
	}
	return wikis, nil
}

// enterWiki ensures the wiki's byobu window exists (`llmw wiki enter` is
// idempotent — reuses a live window, rebuilds a dead one — and daemon-safe:
// non-TTY callers never attach). suffix "" means the default "main" window.
func (c *llmwClient) enterWiki(ctx context.Context, name, suffix string) error {
	ctx, cancel := context.WithTimeout(ctx, enterTimeout)
	defer cancel()
	args := []string{"wiki", "--name=" + name, "enter"}
	if suffix != "" && suffix != "main" {
		args = append(args, "--window-suffix="+suffix)
	}
	if _, err := c.run(ctx, args...); err != nil {
		return err
	}
	return nil
}

// status returns the running host windows via `llmw status --json` (design
// §7.5). Rows include both live windows and dead remain-on-exit remnants; the
// caller renders the state contract values as-is.
func (c *llmwClient) status(ctx context.Context) ([]windowRow, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	out, err := c.run(ctx, "status", "--json")
	if err != nil {
		return nil, err
	}
	var rows []windowRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("llmw: parse status --json: %w", err)
	}
	return rows, nil
}

// stopWiki kills the wiki's host tmux window via `llmw wiki --name=X stop
// --yes` (design §7.5). When multiple windows exist llmw exits non-zero with
// a listing + hint in stderr; the caller forwards that message verbatim.
func (c *llmwClient) stopWiki(ctx context.Context, name, suffix string) error {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	args := []string{"wiki", "--name=" + name, "stop", "--yes"}
	if suffix != "" {
		args = append(args, "--window-suffix="+suffix)
	}
	if _, err := c.run(ctx, args...); err != nil {
		return err
	}
	return nil
}
