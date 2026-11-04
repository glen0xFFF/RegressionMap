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
	case "env":
		for _, kv := range os.Environ() {
			os.Stdout.WriteString(kv + "\n")
		}
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

// childEnvNames runs the helper and returns the upper-cased names of every
// variable the child process actually received.
func childEnvNames(t *testing.T) map[string]string {
	t.Helper()
	r := Run(context.Background(), helper("env"))
	if !r.Passed {
		t.Fatalf("env helper failed: %+v", r)
	}
	got := map[string]string{}
	for _, l := range strings.Split(r.Stdout, "\n") {
		if i := strings.Index(l[min(1, len(l)):], "="); i >= 0 {
			i++
			got[strings.ToUpper(l[:i])] = l[i+1:]
		}
	}
	return got
}

func TestRunStripsSecretsFromChildEnv(t *testing.T) {
	secrets := []string{
		"MODEL_API_KEY", "MODEL_API_KEY_FILE", "WEBHOOK_SECRET",
		"STRIPE_KEY", "github_token", "Db_Password", "SESSION_SECRET", "NPM_TOKEN",
	}
	for _, k := range secrets {
		t.Setenv(k, "leak-"+k)
	}
	t.Setenv("REGRESSIONMAP_NORMAL", "keep-me")
	t.Setenv("KEYBOARD_LAYOUT", "keep-too") // contains KEY but does not end with _KEY
	got := childEnvNames(t)
	for _, k := range secrets {
		if v, ok := got[strings.ToUpper(k)]; ok {
			t.Errorf("child saw secret %s=%q", k, v)
		}
	}
	if got["REGRESSIONMAP_NORMAL"] != "keep-me" || got["KEYBOARD_LAYOUT"] != "keep-too" {
		t.Errorf("normal vars not passed through: %q %q", got["REGRESSIONMAP_NORMAL"], got["KEYBOARD_LAYOUT"])
	}
	if _, ok := got["PATH"]; !ok {
		t.Error("PATH not passed through")
	}
	if got["PROC_HELPER"] != "env" {
		t.Error("Spec.Env extras not appended")
	}
}

func TestIsSecretEnv(t *testing.T) {
	for name, want := range map[string]bool{
		"MODEL_API_KEY": true, "MODEL_API_KEY_FILE": true, "WEBHOOK_SECRET": true,
		"aws_secret": true, "X_TOKEN": true, "PG_PASSWORD": true, "my_key": true,
		"PATH": false, "HOME": false, "USERPROFILE": false, "KEY": false,
		"TOKENIZER": false, "PASSWORD_HINT_SHOWN": false, "SYSTEMROOT": false,
	} {
		if got := isSecretEnv(name); got != want {
			t.Errorf("isSecretEnv(%q) = %v, want %v", name, got, want)
		}
	}
}
