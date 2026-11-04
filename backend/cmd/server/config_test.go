package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"regressionmap/internal/apikeys"
	"regressionmap/internal/fixture"
)

func TestLoadKeyPrefersEnvThenFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "key.txt")
	os.WriteFile(f, []byte("  file-key\r\n"), 0o600)

	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if k, _ := loadKey(get(map[string]string{"MODEL_API_KEY": "env-key", "MODEL_API_KEY_FILE": f})); k != "env-key" {
		t.Fatalf("got %q", k)
	}
	if k, _ := loadKey(get(map[string]string{"MODEL_API_KEY_FILE": f})); k != "file-key" {
		t.Fatalf("got %q", k)
	}
	if k, err := loadKey(get(map[string]string{})); k != "" || err != nil {
		t.Fatalf("got %q %v", k, err)
	}
	if _, err := loadKey(get(map[string]string{"MODEL_API_KEY_FILE": f + ".missing"})); err == nil {
		t.Fatal("missing file must error")
	}
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigDefaults(t *testing.T) {
	root := t.TempDir()
	cfg, warns, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != root || cfg.Addr != "127.0.0.1:8787" || cfg.LogFormat != "text" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.ModelBaseURL == "" || cfg.ModelID == "" {
		t.Fatalf("model defaults missing: %+v", cfg)
	}
	if cfg.DataDir != "" || cfg.WebDir != "" || cfg.Key != "" {
		t.Fatalf("optional fields must default empty: %+v", cfg)
	}
	if len(warns) != 0 {
		t.Fatalf("no warnings expected, got %v", warns)
	}
}

func TestLoadConfigRootValidation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain.txt")
	os.WriteFile(file, []byte("x"), 0o600)
	cases := map[string]string{
		"missing":     "",
		"nonexistent": filepath.Join(dir, "nope"),
		"not a dir":   file,
	}
	for name, root := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root}))
			if err == nil || !strings.Contains(err.Error(), "WORKSPACE_ROOT") {
				t.Fatalf("want WORKSPACE_ROOT error, got %v", err)
			}
		})
	}
}

func TestLoadConfigAddrLoopbackOnly(t *testing.T) {
	root := t.TempDir()
	for _, addr := range []string{"127.0.0.1:9000", "localhost:9000", "[::1]:9000", "127.0.0.5:1"} {
		if _, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "ADDR": addr})); err != nil {
			t.Errorf("%s: loopback must be accepted: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8787", ":8787", "192.168.1.10:8787", "[::]:8787", "example.com:80"} {
		_, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "ADDR": addr}))
		if err == nil || !strings.Contains(err.Error(), "REGRESSIONMAP_ALLOW_REMOTE") {
			t.Errorf("%s: non-loopback must be refused with a hint, got %v", addr, err)
		}
	}
	if _, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "ADDR": "garbage"})); err == nil {
		t.Error("unparseable ADDR must error")
	}
}

// dataDirWithKey returns a DATA_DIR holding one key of the given scope.
func dataDirWithKey(t *testing.T, scope string, revoked bool) string {
	t.Helper()
	dir := t.TempDir()
	s := apikeys.NewStore(dir)
	k, _, err := s.Create("boot", scope)
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		if err := s.Revoke(k.ID); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadConfigRemoteAllowedWithOverrideWarns(t *testing.T) {
	root := t.TempDir()
	cfg, warns, err := loadConfig(envOf(map[string]string{
		"WORKSPACE_ROOT": root, "ADDR": "0.0.0.0:8787", "REGRESSIONMAP_ALLOW_REMOTE": "1",
		"DATA_DIR": dataDirWithKey(t, apikeys.ScopeAdmin, false),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "0.0.0.0:8787" || !cfg.AllowRemote {
		t.Fatalf("got %+v", cfg)
	}
	if len(warns) == 0 || !strings.Contains(strings.ToUpper(strings.Join(warns, " ")), "WARNING") {
		t.Fatalf("expected loud warning, got %v", warns)
	}
	// Any value other than "1" is not an override.
	if _, _, err := loadConfig(envOf(map[string]string{
		"WORKSPACE_ROOT": root, "ADDR": "0.0.0.0:8787", "REGRESSIONMAP_ALLOW_REMOTE": "true",
	})); err == nil {
		t.Fatal("override must be exactly 1")
	}
}

func TestLoadConfigOptionalDirsAndLogFormat(t *testing.T) {
	root := t.TempDir()
	web := t.TempDir()
	os.WriteFile(filepath.Join(web, "index.html"), []byte("<html></html>"), 0o600)
	data := filepath.Join(t.TempDir(), "data")
	cfg, _, err := loadConfig(envOf(map[string]string{
		"WORKSPACE_ROOT": root, "WEB_DIR": web, "DATA_DIR": data, "LOG_FORMAT": "json",
		"MODEL_API_KEY": "k-123", "MODEL_ID": "m", "MODEL_BASE_URL": "http://x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebDir != web || cfg.DataDir != data || cfg.LogFormat != "json" || cfg.Key != "k-123" || cfg.ModelID != "m" || cfg.ModelBaseURL != "http://x" {
		t.Fatalf("got %+v", cfg)
	}

	if _, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "LOG_FORMAT": "xml"})); err == nil {
		t.Fatal("unknown LOG_FORMAT must error")
	}
	if _, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "WEB_DIR": filepath.Join(web, "nope")})); err == nil {
		t.Fatal("missing WEB_DIR must error")
	}
	if _, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "WEB_DIR": t.TempDir()})); err == nil {
		t.Fatal("WEB_DIR without index.html must error")
	}
	if _, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "MODEL_API_KEY_FILE": filepath.Join(web, "missing")})); err == nil {
		t.Fatal("unreadable key file must error")
	}
}

func TestStartupLogNeverPrintsKey(t *testing.T) {
	const secret = "sk-very-secret-key-123"
	cfg := config{Root: "/w", Addr: "127.0.0.1:8787", LogFormat: "text", Key: secret, ModelID: "muse-spark-1.3", WebDir: "/web"}
	for _, format := range []string{"text", "json"} {
		var buf bytes.Buffer
		lg, err := newLogger(&buf, format)
		if err != nil {
			t.Fatal(err)
		}
		logStartup(lg, cfg, []string{"WARNING: test"})
		out := buf.String()
		if strings.Contains(out, secret) {
			t.Fatalf("%s: startup log leaked key: %s", format, out)
		}
		if !strings.Contains(out, "127.0.0.1:8787") || !strings.Contains(out, "model_configured") {
			t.Fatalf("%s: startup log missing fields: %s", format, out)
		}
		if !strings.Contains(out, "WARNING: test") {
			t.Fatalf("%s: warnings not logged: %s", format, out)
		}
		if format == "json" && !strings.HasPrefix(strings.TrimSpace(out), "{") {
			t.Fatalf("json logger did not emit JSON: %s", out)
		}
	}
	if _, err := newLogger(&bytes.Buffer{}, "xml"); err == nil {
		t.Fatal("unknown format must error")
	}
}

func TestLoadConfigRemoteRequiresActiveAdminKey(t *testing.T) {
	root := t.TempDir()
	base := map[string]string{"WORKSPACE_ROOT": root, "ADDR": "0.0.0.0:8787", "REGRESSIONMAP_ALLOW_REMOTE": "1"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"no DATA_DIR":       {with(nil), "DATA_DIR"},
		"empty DATA_DIR":    {with(map[string]string{"DATA_DIR": t.TempDir()}), "admin key"},
		"only analyze key":  {with(map[string]string{"DATA_DIR": dataDirWithKey(t, apikeys.ScopeAnalyze, false)}), "admin key"},
		"revoked admin key": {with(map[string]string{"DATA_DIR": dataDirWithKey(t, apikeys.ScopeAdmin, true)}), "admin key"},
		"loopback addr too": {map[string]string{"WORKSPACE_ROOT": root, "REGRESSIONMAP_ALLOW_REMOTE": "1"}, "DATA_DIR"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := loadConfig(envOf(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "keys create") && tc.want == "admin key" {
				t.Fatalf("error must say how to create a key: %v", err)
			}
		})
	}
	// A corrupt keys.json refuses to start too.
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, "keys.json"), []byte("{"), 0o600)
	if _, _, err := loadConfig(envOf(with(map[string]string{"DATA_DIR": bad}))); err == nil {
		t.Fatal("corrupt keys.json must refuse to start")
	}
	_, warns, err := loadConfig(envOf(with(map[string]string{"DATA_DIR": dataDirWithKey(t, apikeys.ScopeAdmin, false)})))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(warns, " "), "no authentication") {
		t.Fatalf("warning still claims the API has no authentication: %v", warns)
	}
}

func TestLoadConfigAllowedHosts(t *testing.T) {
	root := t.TempDir()
	cfg, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root,
		"ALLOWED_HOSTS": " RegressionMap.Glenox.NET , ,foo.example,10.0.0.5,::1"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.AllowedHosts, ","); got != "regressionmap.glenox.net,foo.example,10.0.0.5,::1" {
		t.Fatalf("AllowedHosts = %q", got)
	}
	cfg, _, _ = loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root}))
	if len(cfg.AllowedHosts) != 0 {
		t.Fatalf("default AllowedHosts = %v", cfg.AllowedHosts)
	}
	for _, bad := range []string{"https://x.example", "x.example:443", "*.example", "a b.example", "-bad.example",
		"bad-.example", "bad..example", "x.example/path", "user@x.example", strings.Repeat("a", 64) + ".example", "ex\x00ample"} {
		_, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "ALLOWED_HOSTS": "ok.example," + bad}))
		if err == nil || !strings.Contains(err.Error(), "ALLOWED_HOSTS") {
			t.Errorf("%q: want ALLOWED_HOSTS error, got %v", bad, err)
		}
	}
}

func TestLoadConfigRateLimit(t *testing.T) {
	root := t.TempDir()
	cfg, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root}))
	if err != nil || cfg.RateLimitPerHour != 20 {
		t.Fatalf("default = %d, %v", cfg.RateLimitPerHour, err)
	}
	cfg, _, err = loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "RATE_LIMIT_PER_HOUR": " 5 "}))
	if err != nil || cfg.RateLimitPerHour != 5 {
		t.Fatalf("5 = %d, %v", cfg.RateLimitPerHour, err)
	}
	for _, bad := range []string{"0", "-1", "abc", "1.5", "99999999999999999999"} {
		if _, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "RATE_LIMIT_PER_HOUR": bad})); err == nil || !strings.Contains(err.Error(), "RATE_LIMIT_PER_HOUR") {
			t.Errorf("%q: want RATE_LIMIT_PER_HOUR error, got %v", bad, err)
		}
	}
}

func TestStartupLogShowsAuthSettings(t *testing.T) {
	var buf bytes.Buffer
	lg, _ := newLogger(&buf, "json")
	logStartup(lg, config{Addr: "127.0.0.1:8787", AllowedHosts: []string{"regressionmap.glenox.net"}, RateLimitPerHour: 7}, nil)
	out := buf.String()
	if !strings.Contains(out, "regressionmap.glenox.net") || !strings.Contains(out, `"rate_limit_per_hour":7`) {
		t.Fatalf("startup log lacks auth settings: %s", out)
	}
}

func TestLoadConfigWebhookSecret(t *testing.T) {
	root := t.TempDir()
	cfg, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root}))
	if err != nil || cfg.WebhookSecret != "" {
		t.Fatalf("unset: %q %v", cfg.WebhookSecret, err)
	}
	short := "short-webhook-secret-0123456789" // 31 characters
	_, _, err = loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "WEBHOOK_SECRET": short}))
	if err == nil || !strings.Contains(err.Error(), "WEBHOOK_SECRET") || !strings.Contains(err.Error(), "32") {
		t.Fatalf("short secret: %v", err)
	}
	if strings.Contains(err.Error(), short) {
		t.Fatalf("error echoes the secret: %v", err)
	}
	ok := "exactly-32-characters-secret-abc"
	if len(ok) != 32 {
		t.Fatal("fixture length")
	}
	cfg, _, err = loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "WEBHOOK_SECRET": ok}))
	if err != nil || cfg.WebhookSecret != ok {
		t.Fatalf("32 chars: %q %v", cfg.WebhookSecret, err)
	}
}

func TestLoadConfigTestEnvAllow(t *testing.T) {
	root := t.TempDir()
	cfg, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root}))
	if err != nil || len(cfg.TestEnvAllow) != 0 {
		t.Fatalf("unset: %v %v", cfg.TestEnvAllow, err)
	}
	cfg, _, err = loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "TEST_ENV_ALLOW": " DB_PASSWORD , ,stripe_test_key,_X1,"}))
	if err != nil || strings.Join(cfg.TestEnvAllow, ",") != "DB_PASSWORD,stripe_test_key,_X1" {
		t.Fatalf("parsed %q %v", cfg.TestEnvAllow, err)
	}
	// An entry that is not a name is refused without being echoed: a
	// mistaken NAME=value entry would otherwise print the value.
	for _, bad := range []string{"1DB", "DB-PASSWORD", "DB_PASSWORD=hunter2value", "DB PASSWORD", "DÉ_KEY", "$X"} {
		_, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "TEST_ENV_ALLOW": "OK_NAME," + bad}))
		if err == nil || !strings.Contains(err.Error(), "TEST_ENV_ALLOW") || !strings.Contains(err.Error(), "entry 2") {
			t.Errorf("%q: want a TEST_ENV_ALLOW entry 2 error, got %v", bad, err)
			continue
		}
		if strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), "hunter2value") {
			t.Errorf("%q: error echoes the entry: %v", bad, err)
		}
	}
	// The server's own secrets can never be allowed, in any letter case. The
	// error names the variable, never its value.
	for _, c := range []struct{ entry, name string }{
		{"MODEL_API_KEY", "MODEL_API_KEY"}, {"model_api_key_file", "MODEL_API_KEY_FILE"}, {"Webhook_Secret", "WEBHOOK_SECRET"},
	} {
		_, _, err := loadConfig(envOf(map[string]string{"WORKSPACE_ROOT": root, "TEST_ENV_ALLOW": "DB_PASSWORD," + c.entry,
			"MODEL_API_KEY": "sk-model-key-value-123", "WEBHOOK_SECRET": "webhook-secret-value-0123456789abcdef"}))
		if err == nil || !strings.Contains(err.Error(), "TEST_ENV_ALLOW") || !strings.Contains(err.Error(), c.name) {
			t.Errorf("%q: want an error naming %s, got %v", c.entry, c.name, err)
			continue
		}
		if strings.Contains(err.Error(), "sk-model-key-value-123") || strings.Contains(err.Error(), "webhook-secret-value") {
			t.Errorf("%q: error leaks a value: %v", c.entry, err)
		}
	}
}

// TEST_ENV_ALLOW reaches api.Config: an allowed secret-suffixed variable
// reaches a verification run. The working tree holds an uncommitted test
// that passes only when the variable is set.
func TestBuildHandlerWiresTestEnvAllow(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "pagination-demo")
	shas, err := fixture.Build(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.ApplyFix(repo); err != nil {
		t.Fatal(err)
	}
	envTest := "import { test } from \"node:test\";\nimport assert from \"node:assert/strict\";\n" +
		"test(\"allowed variable is set\", () => { assert.equal(process.env.RMWIRE_DB_PASSWORD, \"wired-value\"); });\n"
	if err := os.WriteFile(filepath.Join(repo, "test", "env.test.mjs"), []byte(envTest), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RMWIRE_DB_PASSWORD", "wired-value")
	h, _, err := buildHandler(config{Root: root, ModelID: "m", TestEnvAllow: []string{"RMWIRE_DB_PASSWORD"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	call := func(method, path, body string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	var a struct{ ID, Status, Error string }
	if code := call("POST", "/api/analyses", `{"repo":"pagination-demo","baseline":"`+shas.Baseline+`","command_id":"node-test"}`, &a); code != 202 {
		t.Fatalf("POST /api/analyses: %d", code)
	}
	deadline := time.Now().Add(90 * time.Second)
	for a.Status != "done" && a.Status != "error" {
		if time.Now().After(deadline) {
			t.Fatal("analysis did not finish")
		}
		time.Sleep(100 * time.Millisecond)
		call("GET", "/api/analyses/"+a.ID, "", &a)
	}
	var v struct {
		Run struct {
			Passed   bool   `json:"passed"`
			ExitCode int    `json:"exit_code"`
			Stdout   string `json:"stdout"`
		} `json:"run"`
	}
	if code := call("POST", "/api/analyses/"+a.ID+"/verify", `{"command_id":"node-test","approved":true}`, &v); code != 200 || !v.Run.Passed {
		t.Fatalf("verification without the allowed variable: http %d exit %d\n%.600s", code, v.Run.ExitCode, v.Run.Stdout)
	}
}

// The startup log names the variables test runs do not receive, sorted,
// and never their values; an allowed name is not among them.
func TestStartupLogNamesWithheldTestEnv(t *testing.T) {
	t.Setenv("RMCFG_B_PASSWORD", "withheld-value-b")
	t.Setenv("RMCFG_A_TOKEN", "withheld-value-a")
	t.Setenv("RMCFG_C_KEY", "allowed-value-c")
	for _, format := range []string{"text", "json"} {
		var buf bytes.Buffer
		lg, _ := newLogger(&buf, format)
		logStartup(lg, config{Addr: "127.0.0.1:8787", TestEnvAllow: []string{"RMCFG_C_KEY"}}, nil)
		out := buf.String()
		if strings.Contains(out, "withheld-value") || strings.Contains(out, "allowed-value") {
			t.Fatalf("%s: startup log leaked a value: %s", format, out)
		}
		a, b := strings.Index(out, "RMCFG_A_TOKEN"), strings.Index(out, "RMCFG_B_PASSWORD")
		if !strings.Contains(out, "test_env_withheld") || a < 0 || b < 0 || a > b {
			t.Fatalf("%s: startup log lacks the sorted withheld names: %s", format, out)
		}
		if i := strings.Index(out, "test_env_allow"); i < 0 || !strings.Contains(out[i:], "RMCFG_C_KEY") {
			t.Fatalf("%s: startup log lacks the allow list: %s", format, out)
		}
		if w := out[strings.Index(out, "test_env_withheld"):]; strings.Contains(w[:strings.Index(w, "test_env_allow")+1], "RMCFG_C_KEY") {
			t.Fatalf("%s: an allowed name is listed as withheld: %s", format, out)
		}
	}
}

func TestStartupLogShowsWebhooksEnabledNeverSecret(t *testing.T) {
	const secret = "webhook-secret-must-never-be-logged-0123"
	for _, tc := range []struct {
		secret, text, json string
	}{
		{secret, "webhooks_enabled=true", `"webhooks_enabled":true`},
		{"", "webhooks_enabled=false", `"webhooks_enabled":false`},
	} {
		for _, format := range []string{"text", "json"} {
			var buf bytes.Buffer
			lg, _ := newLogger(&buf, format)
			logStartup(lg, config{Addr: "127.0.0.1:8787", WebhookSecret: tc.secret}, nil)
			out := buf.String()
			if strings.Contains(out, secret) {
				t.Fatalf("%s: startup log leaked the webhook secret: %s", format, out)
			}
			want := tc.text
			if format == "json" {
				want = tc.json
			}
			if !strings.Contains(out, want) {
				t.Fatalf("%s: startup log lacks %s: %s", format, want, out)
			}
		}
	}
}
