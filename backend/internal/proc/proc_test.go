package proc

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The tests re-exec the test binary as a tiny helper process so they do not
// depend on any shell or platform tool.
func TestMain(m *testing.M) {
	switch os.Getenv("PROC_HELPER") {
	case "exit3":
		os.Stdout.WriteString("out")
		os.Stderr.WriteString("err")
		os.Exit(3)
	case "big":
		os.Stdout.WriteString(strings.Repeat("x", 10000))
		os.Exit(0)
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(mode string) Spec {
	exe, _ := os.Executable()
	return Spec{Argv: []string{exe}, Env: []string{"PROC_HELPER=" + mode}, Timeout: 3 * time.Second, MaxOutput: 1 << 20}
}

func TestRunCapturesExitCodeAndStreams(t *testing.T) {
	r := Run(context.Background(), helper("exit3"))
	if r.ExitCode != 3 || r.Passed {
		t.Fatalf("exit=%d passed=%v, want 3 false", r.ExitCode, r.Passed)
	}
	if r.Stdout != "out" || r.Stderr != "err" {
		t.Fatalf("stdout=%q stderr=%q", r.Stdout, r.Stderr)
	}
}

func TestRunTruncatesOutput(t *testing.T) {
	s := helper("big")
	s.MaxOutput = 100
	r := Run(context.Background(), s)
	if !r.Truncated || len(r.Stdout) > 100 {
		t.Fatalf("truncated=%v len=%d", r.Truncated, len(r.Stdout))
	}
	if !r.Passed {
		t.Fatal("exit 0 should pass")
	}
}

func TestRunTimesOut(t *testing.T) {
	s := helper("sleep")
	s.Timeout = 300 * time.Millisecond
	start := time.Now()
	r := Run(context.Background(), s)
	if !r.TimedOut || r.Passed {
		t.Fatalf("timedOut=%v passed=%v", r.TimedOut, r.Passed)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestRunMissingBinaryIsFailure(t *testing.T) {
	r := Run(context.Background(), Spec{Argv: []string{"definitely-not-a-binary-xyz"}, Timeout: time.Second, MaxOutput: 100})
	if r.Passed || r.ExitCode == 0 || r.StartError == "" {
		t.Fatalf("got %+v", r)
	}
}
