package gitx

import (
	"context"
	"os"
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
