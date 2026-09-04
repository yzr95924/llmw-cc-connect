package llmw

import (
	"os/exec"
	"os/user"
	"testing"
)

// Regression (2026-09-04): a systemd system service runs with USER but no
// HOME, and byobu's launcher refuses to start against an empty $HOME
// ("Cannot run byobu because [root] does not own []"), which broke every
// /llmw enter from IM. applyHomeEnv must derive HOME from the passwd entry
// so subprocesses work under the daemon environment.
func TestApplyHomeEnvDerivesHomeWhenUnset(t *testing.T) {
	t.Setenv("HOME", "") // simulate the daemon env: HOME unset
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no passwd home for this environment: %v", err)
	}
	if got := homeDirEnv(); got != u.HomeDir {
		t.Fatalf("homeDirEnv() = %q, want passwd home %q", got, u.HomeDir)
	}
	cmd := exec.Command("true")
	applyHomeEnv(cmd)
	found := false
	for _, kv := range cmd.Env {
		if kv == "HOME="+u.HomeDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("applyHomeEnv must export HOME=%q into cmd.Env, got %v", u.HomeDir, cmd.Env)
	}
}

// With HOME present (normal login shell) nothing is injected: the child
// simply inherits the parent environment.
func TestApplyHomeEnvNoopWhenSet(t *testing.T) {
	t.Setenv("HOME", "/home/somewhere")
	if got := homeDirEnv(); got != "/home/somewhere" {
		t.Fatalf("homeDirEnv() = %q, want /home/somewhere", got)
	}
	cmd := exec.Command("true")
	applyHomeEnv(cmd)
	if cmd.Env != nil {
		t.Fatalf("cmd.Env must stay nil (inherit) when HOME is set, got %v", cmd.Env)
	}
}
