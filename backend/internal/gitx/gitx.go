// Package gitx wraps the git CLI. Every call passes an argument array;
// user-supplied revisions are validated before they reach git.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"regressionmap/internal/proc"
)

type Commit struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
}

type ChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

var (
	revRe = regexp.MustCompile(`^(HEAD|[0-9a-fA-F]{4,40})$`)
	shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Output caps. git output is read through a bounded buffer, so a huge diff
// or file can never be held in memory whole.
const (
	MaxDiffBytes   = 8 << 20 // git diff (patch text)
	MaxOutputBytes = 1 << 20 // every other git call: name-status, log, show, ...
)

// Variables so tests can shrink them; production code never assigns them.
var (
	diffCap   = MaxDiffBytes
	outputCap = MaxOutputBytes
	stderrCap = 64 << 10
)

// LimitError reports that git wrote more than Limit bytes to stdout.
type LimitError struct {
	Cmd   string
	Limit int
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("git %s failed: git output exceeds %d bytes", e.Cmd, e.Limit)
}

var errCapReached = errors.New("git output cap reached")

// capBuf keeps at most max bytes. Past the cap it records over; with stop set
// it also fails the write (ending the copy from git's pipe) and calls onOver,
// otherwise it silently discards the rest (used for stderr). The buffer is a
// named field, not embedded: an embedded bytes.Buffer would promote ReadFrom,
// and io.Copy in os/exec would then bypass Write and the cap entirely.
type capBuf struct {
	buf    bytes.Buffer
	max    int
	over   bool
	stop   bool
	onOver func()
}

func (c *capBuf) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if len(p) <= room {
		return c.buf.Write(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
	} else {
		room = 0
	}
	c.over = true
	if !c.stop {
		return len(p), nil
	}
	if c.onOver != nil {
		c.onOver()
	}
	return room, errCapReached
}

func (c *capBuf) String() string { return c.buf.String() }

// run executes git with stdout capped at limit. On overflow it returns the
// first limit bytes together with a *LimitError.
func run(ctx context.Context, dir string, limit int, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	// Secrets are stripped: hooks and helpers git runs can be repo-controlled.
	cmd.Env = proc.SafeEnv("GIT_TERMINAL_PROMPT=0")
	out := &capBuf{max: limit, stop: true, onOver: cancel}
	errb := &capBuf{max: stderrCap}
	cmd.Stdout, cmd.Stderr = out, errb
	err := cmd.Run()
	if out.over {
		return out.String(), &LimitError{Cmd: args[0], Limit: limit}
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s failed: %s", args[0], strings.TrimSpace(errb.String()))
		}
		return "", fmt.Errorf("git %s failed: %v", args[0], err)
	}
	return out.String(), nil
}

// git runs a non-diff command; output over outputCap is an error, never data.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := run(ctx, dir, outputCap, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// ResolveCommit turns HEAD or a hex sha prefix into a full commit id.
func ResolveCommit(ctx context.Context, dir, rev string) (string, error) {
	if !revRe.MatchString(rev) {
		return "", fmt.Errorf("revision must be HEAD or a hex commit id")
	}
	out, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("unknown revision %q", rev)
	}
	sha := strings.TrimSpace(out)
	if !shaRe.MatchString(sha) {
		return "", fmt.Errorf("unexpected rev-parse output")
	}
	return sha, nil
}

func Commits(ctx context.Context, dir string, n int) ([]Commit, error) {
	out, err := git(ctx, dir, "log", fmt.Sprintf("-n%d", n), "--format=%H%x1f%s")
	if err != nil {
		return nil, err
	}
	cs := []Commit{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(l, "\x1f", 2)
		if len(parts) != 2 || !shaRe.MatchString(parts[0]) {
			continue
		}
		cs = append(cs, Commit{SHA: parts[0], Short: parts[0][:7], Subject: parts[1]})
	}
	return cs, nil
}

func requireSHA(shas ...string) error {
	for _, s := range shas {
		if !shaRe.MatchString(s) {
			return fmt.Errorf("internal: %q is not a resolved commit id", s)
		}
	}
	return nil
}

func ChangedFiles(ctx context.Context, dir, base, cur string) ([]ChangedFile, error) {
	if err := requireSHA(base, cur); err != nil {
		return nil, err
	}
	out, err := git(ctx, dir, "diff", "--name-status", "--no-renames", base, cur)
	if err != nil {
		return nil, err
	}
	fs := []ChangedFile{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(l, "\t", 2)
		if len(parts) == 2 {
			fs = append(fs, ChangedFile{Status: parts[0], Path: parts[1]})
		}
	}
	return fs, nil
}

// Diff returns the patch from base to cur. If it exceeds MaxDiffBytes it
// returns the output cut at the last complete line within the cap AND a
// *LimitError, so a caller that only checks err fails loudly.
func Diff(ctx context.Context, dir, base, cur string) (string, error) {
	if err := requireSHA(base, cur); err != nil {
		return "", err
	}
	out, err := run(ctx, dir, diffCap, "diff", "--no-color", "--no-renames", "--no-ext-diff", "-U3", base, cur)
	var le *LimitError
	if errors.As(err, &le) {
		return wholeLines(out), err
	}
	return out, err
}

// DiffLimited is Diff for callers that can work with a partial patch: an
// over-cap diff comes back cut at a line boundary with truncated set and a
// nil error.
func DiffLimited(ctx context.Context, dir, base, cur string) (diff string, truncated bool, err error) {
	out, err := Diff(ctx, dir, base, cur)
	var le *LimitError
	if errors.As(err, &le) {
		return out, true, nil
	}
	return out, false, err
}

func wholeLines(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return ""
}

// ShowFile reads a repo-relative file at a commit.
func ShowFile(ctx context.Context, dir, sha, p string) (string, error) {
	if err := requireSHA(sha); err != nil {
		return "", err
	}
	if c := path.Clean(p); c != p || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") || strings.Contains(c, ":") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return git(ctx, dir, "show", sha+":"+p)
}

// Worktree checks out sha into a fresh temporary directory, detached.
func Worktree(ctx context.Context, dir, sha string) (string, func(), error) {
	if err := requireSHA(sha); err != nil {
		return "", nil, err
	}
	parent, err := os.MkdirTemp("", "regressionmap-wt-")
	if err != nil {
		return "", nil, err
	}
	wt := parent + string(os.PathSeparator) + "wt"
	if _, err := git(ctx, dir, "worktree", "add", "--detach", "--quiet", wt, sha); err != nil {
		os.RemoveAll(parent)
		return "", nil, err
	}
	cleanup := func() {
		_, _ = git(context.Background(), dir, "worktree", "remove", "--force", wt)
		os.RemoveAll(parent)
		_, _ = git(context.Background(), dir, "worktree", "prune")
	}
	return wt, cleanup, nil
}

// Prune drops worktree registrations whose directories no longer exist.
func Prune(ctx context.Context, dir string) error {
	_, err := git(ctx, dir, "worktree", "prune")
	return err
}
