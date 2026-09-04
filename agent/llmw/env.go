package llmw

import (
	"os"
	"os/exec"
	"os/user"
)

// homeDirEnv resolves the home directory for the daemon's subprocess
// toolchain. systemd system services set USER but NOT HOME, and every leg of
// the llmw stack needs it: byobu's launcher refuses to start unless the user
// owns $HOME ([ ! -O "$HOME" ] with HOME empty prints "Cannot run byobu
// because [user] does not own []"), llmw resolves ~/yzr-llm-wiki-workspace,
// and opencode reads ~/.config/opencode. Order: $HOME, then the passwd entry
// of the current uid (Python's Path.home() does the same fallback, which is
// why `llmw list` kept working while byobu did not).
func homeDirEnv() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return ""
}

// applyHomeEnv exports a derived HOME into cmd's environment when the daemon
// process lacks one, so llmw / byobu / tmux / opencode children behave like
// they do under a login shell. No-op (inherit everything) when HOME is set.
func applyHomeEnv(cmd *exec.Cmd) {
	if os.Getenv("HOME") != "" {
		return
	}
	h := homeDirEnv()
	if h == "" {
		return
	}
	cmd.Env = append(os.Environ(), "HOME="+h)
}
