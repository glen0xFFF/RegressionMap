// Package proc runs an external program from an argument array (never a shell
// string) with a deadline and a cap on captured output.
package proc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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

// secretEnv lists variables never passed to child processes.
var secretEnv = []string{"MODEL_API_KEY"}

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

func childEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		keep := true
		for _, s := range secretEnv {
			if strings.HasPrefix(strings.ToUpper(kv), s+"=") {
				keep = false
			}
		}
		if keep {
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
	cmd.Env = childEnv(spec.Env)
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
