package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestLoadConfigRemoteAllowedWithOverrideWarns(t *testing.T) {
	root := t.TempDir()
	cfg, warns, err := loadConfig(envOf(map[string]string{
		"WORKSPACE_ROOT": root, "ADDR": "0.0.0.0:8787", "REGRESSIONMAP_ALLOW_REMOTE": "1",
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
