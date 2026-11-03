// Package gitx wraps the git CLI. Every call passes an argument array;
// user-supplied revisions are validated before they reach git.
package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"
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

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s failed: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s failed: %v", args[0], err)
	}
	return string(out), nil
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

func Diff(ctx context.Context, dir, base, cur string) (string, error) {
	if err := requireSHA(base, cur); err != nil {
		return "", err
	}
	return git(ctx, dir, "diff", "--no-color", "--no-renames", "--no-ext-diff", "-U3", base, cur)
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
