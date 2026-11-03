package fixture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"regressionmap/internal/proc"
)

func nodeTest(t *testing.T, dir string) proc.Result {
	t.Helper()
	return proc.Run(context.Background(), proc.Spec{Argv: []string{"node", "--test"}, Dir: dir, Timeout: 30 * time.Second, MaxOutput: 1 << 20})
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBuildCreatesPassingBaselineAndFailingHead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pagination-demo")
	shas, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(shas.Baseline) != 40 || len(shas.Bug) != 40 || shas.Baseline == shas.Bug {
		t.Fatalf("bad shas %+v", shas)
	}

	head := nodeTest(t, dir)
	if head.Passed {
		t.Fatalf("HEAD should fail, got pass:\n%s", head.Stdout)
	}
	if !strings.Contains(head.Stdout, "pagination.test.ts") {
		t.Fatalf("expected failure location in output:\n%s", head.Stdout)
	}

	git(t, dir, "checkout", "-q", shas.Baseline)
	if base := nodeTest(t, dir); !base.Passed {
		t.Fatalf("baseline should pass:\n%s", base.Stdout)
	}
}

func TestApplyFixRestoresPassingTests(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pagination-demo")
	if _, err := Build(dir); err != nil {
		t.Fatal(err)
	}
	if err := ApplyFix(dir); err != nil {
		t.Fatal(err)
	}
	if r := nodeTest(t, dir); !r.Passed {
		t.Fatalf("fixed tree should pass:\n%s", r.Stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
}
