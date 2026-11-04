package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"regressionmap/internal/fixture"
)

func repo(t *testing.T) (string, fixture.Shas) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pagination-demo")
	s, err := fixture.Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, s
}

func TestResolveRev(t *testing.T) {
	dir, s := repo(t)
	ctx := context.Background()
	got, err := ResolveCommit(ctx, dir, s.Baseline[:7])
	if err != nil || got != s.Baseline {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"--output=x", "HEAD;rm", "", "deadbeefdeadbeef", "-p"} {
		if _, err := ResolveCommit(ctx, dir, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	head, err := ResolveCommit(ctx, dir, "HEAD")
	if err != nil || head != s.Bug {
		t.Fatalf("HEAD %q %v", head, err)
	}
}

func TestCommitsNewestFirst(t *testing.T) {
	dir, s := repo(t)
	cs, err := Commits(context.Background(), dir, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].SHA != s.Bug || cs[2].SHA != s.Baseline || cs[0].Subject != "refactor: clarify page end index" {
		t.Fatalf("%+v", cs)
	}
}

func TestDiffAndChangedFiles(t *testing.T) {
	dir, s := repo(t)
	ctx := context.Background()
	files, err := ChangedFiles(ctx, dir, s.Baseline, s.Bug)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[1].Path != "src/pagination.ts" || files[1].Status != "M" {
		t.Fatalf("%+v", files)
	}
	d, err := Diff(ctx, dir, s.Baseline, s.Bug)
	if err != nil || !strings.Contains(d, "+  const end = start + pageSize - 1;") {
		t.Fatalf("%v\n%s", err, d)
	}
}

func TestShowFile(t *testing.T) {
	dir, s := repo(t)
	src, err := ShowFile(context.Background(), dir, s.Baseline, "src/pagination.ts")
	if err != nil || strings.Contains(src, "- 1;") || !strings.Contains(src, "start + pageSize;") {
		t.Fatalf("%v\n%s", err, src)
	}
	if _, err := ShowFile(context.Background(), dir, s.Baseline, "../x"); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestWorktreeAddRemove(t *testing.T) {
	dir, s := repo(t)
	ctx := context.Background()
	wt, cleanup, err := Worktree(ctx, dir, s.Baseline)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(wt, "src", "pagination.ts"))
	if err != nil || strings.Contains(string(b), "- 1;") {
		t.Fatalf("worktree not at baseline: %v", err)
	}
	cleanup()
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed: %v", err)
	}
}

func TestPruneRemovesStaleWorktreeRegistration(t *testing.T) {
	dir, s := repo(t)
	ctx := context.Background()
	wt, _, err := Worktree(ctx, dir, s.Baseline)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: the temp dir vanished without "git worktree remove".
	if err := os.RemoveAll(filepath.Dir(wt)); err != nil {
		t.Fatal(err)
	}
	countWT := func() int {
		out, err := git(ctx, dir, "worktree", "list", "--porcelain")
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(out, "worktree ")
	}
	if n := countWT(); n != 2 {
		t.Fatalf("before prune: %d worktrees, want 2", n)
	}
	if err := Prune(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if n := countWT(); n != 1 {
		t.Fatalf("after prune: %d worktrees, want 1", n)
	}
}

// bigRepo builds a two-commit repository whose second commit adds a file of
// size bytes, so the diff and the file both exceed the default caps.
func bigRepo(t *testing.T, size int) (dir, base, cur string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "base")
	base = run("rev-parse", "HEAD")
	line := strings.Repeat("abcdefghij", 9) + "\n" // 91 bytes
	big := strings.Repeat(line, size/len(line)+1)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "big")
	cur = run("rev-parse", "HEAD")
	return dir, base, cur
}

func TestDiffOverCapReturnsTruncatedOutputAndLimitError(t *testing.T) {
	dir, base, cur := bigRepo(t, MaxDiffBytes+1<<20)
	ctx := context.Background()

	out, err := Diff(ctx, dir, base, cur)
	var le *LimitError
	if !errors.As(err, &le) || le.Limit != MaxDiffBytes {
		t.Fatalf("Diff err = %v, want *LimitError with limit %d", err, MaxDiffBytes)
	}
	if want := fmt.Sprintf("git output exceeds %d bytes", MaxDiffBytes); !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not say %q", err, want)
	}
	if len(out) == 0 || len(out) > MaxDiffBytes || !strings.HasPrefix(out, "diff --git a/big.txt") {
		t.Fatalf("truncated diff len=%d prefix=%q", len(out), out[:min(40, len(out))])
	}

	d, truncated, err := DiffLimited(ctx, dir, base, cur)
	if err != nil || !truncated {
		t.Fatalf("DiffLimited truncated=%v err=%v, want true nil", truncated, err)
	}
	if len(d) == 0 || len(d) > MaxDiffBytes || !strings.HasSuffix(d, "\n") {
		t.Fatalf("DiffLimited len=%d, want 1..%d ending at a line break", len(d), MaxDiffBytes)
	}
}

func TestDiffLimitedUnderCapIsComplete(t *testing.T) {
	dir, s := repo(t)
	d, truncated, err := DiffLimited(context.Background(), dir, s.Baseline, s.Bug)
	if err != nil || truncated || !strings.Contains(d, "+  const end = start + pageSize - 1;") {
		t.Fatalf("truncated=%v err=%v\n%s", truncated, err, d)
	}
}

func TestShowFileOverCapIsError(t *testing.T) {
	dir, _, cur := bigRepo(t, MaxOutputBytes+1024)
	out, err := ShowFile(context.Background(), dir, cur, "big.txt")
	var le *LimitError
	if !errors.As(err, &le) || le.Limit != MaxOutputBytes || out != "" {
		t.Fatalf("ShowFile len=%d err=%v, want empty and *LimitError(%d)", len(out), err, MaxOutputBytes)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("git output exceeds %d bytes", MaxOutputBytes)) {
		t.Fatalf("unclear error %q", err)
	}
}

// lowerCap shrinks the general output cap for one test so name-status and
// log overflow without generating megabytes of file names.
func lowerCap(t *testing.T, n int) {
	t.Helper()
	old := outputCap
	outputCap = n
	t.Cleanup(func() { outputCap = old })
}

func TestChangedFilesAndCommitsOverCapAreErrors(t *testing.T) {
	dir, s := repo(t)
	ctx := context.Background()
	lowerCap(t, 16)
	var le *LimitError
	if fs, err := ChangedFiles(ctx, dir, s.Baseline, s.Bug); !errors.As(err, &le) || fs != nil {
		t.Fatalf("ChangedFiles = %v, %v; want nil and *LimitError", fs, err)
	}
	if cs, err := Commits(ctx, dir, 50); !errors.As(err, &le) || cs != nil {
		t.Fatalf("Commits = %v, %v; want nil and *LimitError", cs, err)
	}
	if !strings.Contains(le.Error(), "git output exceeds 16 bytes") {
		t.Fatalf("unclear error %q", le)
	}
}

func TestCappedBufferBoundsStderr(t *testing.T) {
	b := &capBuf{max: 8}
	n, err := b.Write([]byte("0123456789abcdef"))
	if n != 16 || err != nil || b.String() != "01234567" || !b.over {
		t.Fatalf("n=%d err=%v buf=%q over=%v", n, err, b.String(), b.over)
	}
	if _, err := b.Write([]byte("more")); err != nil || b.String() != "01234567" {
		t.Fatalf("second write grew buffer: %q %v", b.String(), err)
	}
	if stderrCap <= 0 || stderrCap > 1<<20 {
		t.Fatalf("stderrCap = %d, want a small positive cap", stderrCap)
	}
}

// Programs git starts (hooks, configured helpers) can be controlled by the
// analysed repository, so git must get the same secret-free environment as
// proc's children. A post-checkout hook fires during Worktree.
func TestGitChildrenDoNotSeeSecrets(t *testing.T) {
	dir, s := repo(t)
	dump := filepath.Join(t.TempDir(), "hook-env.txt")
	hook := "#!/bin/sh\nenv > '" + filepath.ToSlash(dump) + "'\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODEL_API_KEY", "leakkey")
	t.Setenv("MODEL_API_KEY_FILE", "leakfile")
	t.Setenv("WEBHOOK_SECRET", "leakwh")
	t.Setenv("GH_TOKEN", "leaktok")
	t.Setenv("RM_VISIBLE_PROBE", "visible")

	_, cleanup, err := Worktree(context.Background(), dir, s.Baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("hook did not run: %v", err)
	}
	env := string(b)
	for _, name := range []string{"MODEL_API_KEY", "MODEL_API_KEY_FILE", "WEBHOOK_SECRET", "GH_TOKEN"} {
		for _, l := range strings.Split(env, "\n") {
			if strings.HasPrefix(l, name+"=") {
				t.Errorf("git hook child saw %s", strings.TrimSpace(l))
			}
		}
	}
	if !strings.Contains(env, "RM_VISIBLE_PROBE=visible") {
		t.Errorf("normal variable did not reach git child")
	}
}
