package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"regressionmap/internal/apikeys"
)

func cli(t *testing.T, dataDir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := map[string]string{}
	if dataDir != "" {
		env["DATA_DIR"] = dataDir
	}
	code = runKeys(args, envOf(env), &out, &errb)
	return code, out.String(), errb.String()
}

func TestKeysCreatePrintsOnlyTheSecretOnStdout(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := cli(t, dir, "create", "--name", "ci runner", "--scope", "analyze")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !regexp.MustCompile(`^rm_live_[0-9A-Za-z]{32}\n$`).MatchString(out) {
		t.Fatalf("stdout must be exactly the secret line, got %q", out)
	}
	secret := strings.TrimSpace(out)
	if strings.Contains(errOut, secret) || errOut == "" {
		t.Fatalf("stderr must hold a note without the secret: %q", errOut)
	}
	k, err := apikeys.NewStore(dir).Verify(secret)
	if err != nil || k.Name != "ci runner" || k.Scope != "analyze" {
		t.Fatalf("created key not usable: %+v %v", k, err)
	}
	if !strings.Contains(errOut, k.ID) {
		t.Fatalf("note should name the key id: %q", errOut)
	}
}

func TestKeysCreateErrors(t *testing.T) {
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"bad scope":     {"create", "--name", "x", "--scope", "root"},
		"missing scope": {"create", "--name", "x"},
		"missing name":  {"create", "--scope", "admin"},
		"control name":  {"create", "--name", "a\nb", "--scope", "admin"},
		"extra args":    {"create", "--name", "x", "--scope", "admin", "oops"},
		"unknown flag":  {"create", "--nmae", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := cli(t, dir, args...)
			if code == 0 || out != "" || errOut == "" {
				t.Fatalf("want failure with message, got %d %q %q", code, out, errOut)
			}
		})
	}
	if keys, _ := apikeys.NewStore(dir).List(); len(keys) != 0 {
		t.Fatalf("failed creates stored keys: %+v", keys)
	}
}

func TestKeysRequiresDataDir(t *testing.T) {
	for _, args := range [][]string{{"create", "--name", "x", "--scope", "admin"}, {"list"}, {"revoke", "k_0000000000"}} {
		code, out, errOut := cli(t, "", args...)
		if code == 0 || out != "" || !strings.Contains(errOut, "DATA_DIR") {
			t.Fatalf("%v: got %d %q %q", args, code, out, errOut)
		}
	}
}

func TestKeysUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"frobnicate"}, {"revoke"}, {"revoke", "a", "b"}, {"list", "extra"}} {
		code, out, errOut := cli(t, t.TempDir(), args...)
		if code == 0 || out != "" || !strings.Contains(errOut, "usage") {
			t.Fatalf("%v: got %d %q %q", args, code, out, errOut)
		}
	}
}

func TestKeysListAndRevoke(t *testing.T) {
	dir := t.TempDir()
	if code, out, _ := cli(t, dir, "list"); code != 0 || !strings.Contains(out, "ID") {
		t.Fatalf("empty list: %d %q", code, out)
	}
	_, s1, _ := cli(t, dir, "create", "--name", "admin", "--scope", "admin")
	_, s2, _ := cli(t, dir, "create", "--name", "a much longer name", "--scope", "analyze")
	s1, s2 = strings.TrimSpace(s1), strings.TrimSpace(s2)
	keys, _ := apikeys.NewStore(dir).List()
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}

	code, out, errOut := cli(t, dir, "revoke", keys[1].ID)
	if code != 0 {
		t.Fatalf("revoke: %d %q", code, errOut)
	}
	if _, err := apikeys.NewStore(dir).Verify(s2); err == nil {
		t.Fatal("revoked key still verifies")
	}
	if code, _, errOut := cli(t, dir, "revoke", "k_doesnotexi"); code == 0 || !strings.Contains(errOut, "not found") {
		t.Fatalf("revoke unknown: %d %q", code, errOut)
	}
	apikeys.NewStore(dir).Verify(s1) // sets last_used_at

	code, out, _ = cli(t, dir, "list")
	if code != 0 {
		t.Fatalf("list exit %d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %q", out)
	}
	for _, col := range []string{"ID", "NAME", "SCOPE", "PREFIX", "CREATED_AT", "LAST_USED_AT", "REVOKED"} {
		if !strings.Contains(lines[0], col) {
			t.Fatalf("header lacks %s: %q", col, lines[0])
		}
	}
	// Columns are aligned: every column starts at the same offset on each line.
	scopeCol := strings.Index(lines[0], "SCOPE")
	if lines[1][scopeCol:scopeCol+5] != "admin" || lines[2][scopeCol:scopeCol+7] != "analyze" {
		t.Fatalf("columns not aligned:\n%s", out)
	}
	revCol := strings.Index(lines[0], "REVOKED")
	if !strings.HasPrefix(lines[1][revCol:], "false") || !strings.HasPrefix(lines[2][revCol:], "true") {
		t.Fatalf("revoked column wrong:\n%s", out)
	}
	if !strings.Contains(lines[1], keys[0].ID) || !strings.Contains(lines[1], s1[:12]) || !strings.Contains(lines[2], "a much longer name") {
		t.Fatalf("rows wrong:\n%s", out)
	}
	usedCol := strings.Index(lines[0], "LAST_USED_AT")
	if strings.HasPrefix(lines[1][usedCol:], "never") || !strings.HasPrefix(lines[2][usedCol:], "never") {
		t.Fatalf("last_used_at column wrong:\n%s", out)
	}
	if strings.Contains(out, s1) || strings.Contains(out, s2) || strings.Contains(out, s1[12:]) {
		t.Fatal("list leaks a secret")
	}
}
