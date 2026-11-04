// Package proc runs an external program from an argument array (never a shell
// string) with a deadline and a cap on captured output.
package proc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Spec describes one process to run.
type Spec struct {
	Argv      []string
	Dir       string
	Env       []string // extra KEY=VALUE pairs, appended to the filtered parent env
	Timeout   time.Duration
	MaxOutput int // per stream, in bytes
	// Allow names variables passed although their names end in a secret
	// suffix (TEST_ENV_ALLOW), in any letter case. It never re-admits the
	// server's own secrets (AlwaysWithheld).
	Allow []string
}

// Result is what actually happened. Passed is derived from the exit status only.
type Result struct {
	ExitCode   int    `json:"exit_code"`
	Passed     bool   `json:"passed"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	StartError string `json:"start_error,omitempty"`
}

// secretEnv lists variables never passed to child processes. Child processes
// run test code from the analysed repository, so anything that names or holds
// a credential stays in the server: MODEL_API_KEY_FILE is a path a child
// could simply read.
var secretEnv = []string{"MODEL_API_KEY", "MODEL_API_KEY_FILE", "WEBHOOK_SECRET"}

// secretSuffixes catches the conventional credential names (FOO_KEY,
// GH_TOKEN, DB_PASSWORD, ...) that the explicit list cannot anticipate.
var secretSuffixes = []string{"_KEY", "_SECRET", "_TOKEN", "_PASSWORD"}

// AlwaysWithheld reports whether name is one of the server's own secrets,
// which no child receives and no allow list can re-admit. Names compare
// case-insensitively (Windows env names are).
func AlwaysWithheld(name string) bool {
	for _, s := range secretEnv {
		if strings.EqualFold(name, s) {
			return true
		}
	}
	return false
}

// isSecretEnv reports whether the variable name must be withheld from
// children when nothing is allowed.
func isSecretEnv(name string) bool { return withheld(name, nil) }

// withheld reports whether a child must not receive the variable name:
// always for the server's secrets, and for a secret-suffixed name unless
// allow names it.
func withheld(name string, allow []string) bool {
	if AlwaysWithheld(name) {
		return true
	}
	n := strings.ToUpper(name)
	for _, suf := range secretSuffixes {
		if strings.HasSuffix(n, suf) {
			for _, a := range allow {
				if strings.EqualFold(a, name) {
					return false
				}
			}
			return true
		}
	}
	return false
}

// WithheldNames returns, sorted and without values, the names of the
// variables in this process's environment that a test run given allow does
// not receive. The server logs them at startup.
func WithheldNames(allow []string) []string {
	seen := map[string]bool{}
	names := []string{}
	for _, kv := range os.Environ() {
		if n := envName(kv); withheld(n, allow) && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// envName returns the name part of a KEY=VALUE entry. Windows keeps
// per-drive entries such as "=C:=C:\x", whose name starts with '='.
func envName(kv string) string {
	if i := strings.Index(kv[min(1, len(kv)):], "="); i >= 0 {
		return kv[:i+1]
	}
	return kv
}

type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

// SafeEnv returns the parent environment without any secret variable, plus
// extra KEY=VALUE pairs. Every package that starts a program which can run
// repository-controlled code (test runners here, git hooks in gitx) uses it.
// It takes no allow list: git never receives TEST_ENV_ALLOW names.
func SafeEnv(extra ...string) []string { return childEnv(extra, nil) }

func childEnv(extra, allow []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !withheld(envName(kv), allow) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// Run executes spec and always returns a Result; failures are reported in it.
func Run(ctx context.Context, spec Spec) Result {
	if len(spec.Argv) == 0 {
		return Result{ExitCode: -1, StartError: "empty argv"}
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = childEnv(spec.Env, spec.Allow)
	cmd.WaitDelay = 2 * time.Second
	out := &capped{max: spec.MaxOutput}
	errb := &capped{max: spec.MaxOutput}
	cmd.Stdout, cmd.Stderr = out, errb

	start := time.Now()
	err := cmd.Run()
	r := Result{
		DurationMS: time.Since(start).Milliseconds(),
		Stdout:     out.buf.String(),
		Stderr:     errb.buf.String(),
		Truncated:  out.truncated || errb.truncated,
	}
	if ctx.Err() == context.DeadlineExceeded {
		r.TimedOut = true
		r.ExitCode = -1
		return r
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		r.ExitCode = 0
	case errors.As(err, &exitErr):
		r.ExitCode = exitErr.ExitCode()
	default:
		r.ExitCode = -1
		r.StartError = err.Error()
	}
	r.Passed = r.ExitCode == 0 && !r.TimedOut && r.StartError == ""
	return r
}
