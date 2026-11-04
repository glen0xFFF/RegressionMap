package proc

import (
	"context"
	"os"
	"sort"
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

// Spec.Allow (TEST_ENV_ALLOW) passes names the suffix rule would withhold,
// in any letter case, but never the server's own secrets.
func TestRunPassesAllowedNamesButNeverServerSecrets(t *testing.T) {
	for _, k := range []string{"DB_PASSWORD", "STRIPE_TEST_KEY", "GH_TOKEN", "MODEL_API_KEY", "MODEL_API_KEY_FILE", "WEBHOOK_SECRET"} {
		t.Setenv(k, "v-"+k)
	}
	s := helper("env")
	s.Allow = []string{"DB_PASSWORD", "stripe_test_key", "MODEL_API_KEY", "model_api_key_file", "WEBHOOK_SECRET"}
	r := Run(context.Background(), s)
	if !r.Passed {
		t.Fatalf("env helper failed: %+v", r)
	}
	got := map[string]string{}
	for _, l := range strings.Split(r.Stdout, "\n") {
		if i := strings.Index(l[min(1, len(l)):], "="); i >= 0 {
			got[strings.ToUpper(l[:i+1])] = l[i+2:]
		}
	}
	if got["DB_PASSWORD"] != "v-DB_PASSWORD" || got["STRIPE_TEST_KEY"] != "v-STRIPE_TEST_KEY" {
		t.Errorf("allowed names not passed: DB_PASSWORD=%q STRIPE_TEST_KEY=%q", got["DB_PASSWORD"], got["STRIPE_TEST_KEY"])
	}
	for _, k := range []string{"GH_TOKEN", "MODEL_API_KEY", "MODEL_API_KEY_FILE", "WEBHOOK_SECRET"} {
		if v, ok := got[k]; ok {
			t.Errorf("child saw %s=%q", k, v)
		}
	}
	// SafeEnv (git) takes no allow list: the allowed name stays withheld.
	for _, kv := range SafeEnv() {
		if strings.HasPrefix(strings.ToUpper(kv), "DB_PASSWORD=") {
			t.Errorf("SafeEnv passed %s", kv)
		}
	}
}

// WithheldNames lists, sorted, the names of the variables set in this
// process that test runs do not receive.
func TestWithheldNames(t *testing.T) {
	for _, k := range []string{"RMTEST_B_TOKEN", "RMTEST_A_PASSWORD", "RMTEST_C_KEY", "RMTEST_PLAIN"} {
		t.Setenv(k, "value-must-not-appear")
	}
	t.Setenv("MODEL_API_KEY", "value-must-not-appear")
	got := WithheldNames([]string{"rmtest_c_key", "MODEL_API_KEY"})
	var mine []string
	for _, n := range got {
		if strings.Contains(n, "value-must-not-appear") || strings.Contains(n, "=") {
			t.Fatalf("WithheldNames returned a value: %q", n)
		}
		if strings.HasPrefix(strings.ToUpper(n), "RMTEST_") || strings.EqualFold(n, "MODEL_API_KEY") {
			mine = append(mine, strings.ToUpper(n))
		}
	}
	if strings.Join(mine, ",") != "MODEL_API_KEY,RMTEST_A_PASSWORD,RMTEST_B_TOKEN" {
		t.Fatalf("withheld %v (all %v)", mine, got)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("not sorted: %v", got)
	}
}

func TestAlwaysWithheld(t *testing.T) {
	for name, want := range map[string]bool{
		"MODEL_API_KEY": true, "model_api_key": true, "Model_Api_Key_File": true, "webhook_secret": true,
		"DB_PASSWORD": false, "MODEL_API_KEYS": false, "PATH": false,
	} {
		if got := AlwaysWithheld(name); got != want {
			t.Errorf("AlwaysWithheld(%q) = %v, want %v", name, got, want)
		}
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
