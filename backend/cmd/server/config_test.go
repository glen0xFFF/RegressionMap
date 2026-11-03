package main

import (
	"os"
	"path/filepath"
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
