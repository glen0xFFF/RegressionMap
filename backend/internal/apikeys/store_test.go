package apikeys

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	secretRe = regexp.MustCompile(`^rm_live_[0-9A-Za-z]{32}$`)
	idRe     = regexp.MustCompile(`^k_[0-9a-z]{10}$`)
)

// fakeClock is a settable clock for throttling and rate-limit tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestCreateFormatsAndStoresOnlyTheHash(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	k, secret, err := s.Create("ci", ScopeAnalyze)
	if err != nil {
		t.Fatal(err)
	}
	if !secretRe.MatchString(secret) {
		t.Fatalf("secret %q does not match rm_live_ + 32 base62", secret)
	}
	if !idRe.MatchString(k.ID) {
		t.Fatalf("id %q does not match k_ + 10 base36", k.ID)
	}
	if k.Prefix != secret[:12] || k.Name != "ci" || k.Scope != ScopeAnalyze || k.Revoked || k.LastUsedAt != nil || k.CreatedAt.IsZero() {
		t.Fatalf("key fields wrong: %+v", k)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), secret[12:]) {
		t.Fatalf("keys.json contains the secret: %s", raw)
	}
	sum := sha256.Sum256([]byte(secret))
	if !strings.Contains(string(raw), hex.EncodeToString(sum[:])) {
		t.Fatalf("keys.json lacks the sha256 hex of the secret: %s", raw)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "keys.json"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("keys.json mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	// No temp or lock files are left behind.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("want only keys.json in DATA_DIR, got %v", names)
	}

	_, secret2, _ := s.Create("ci", ScopeAnalyze)
	if secret2 == secret {
		t.Fatal("two keys got the same secret")
	}
}

func TestCreateValidatesNameAndScope(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, name := range []string{"", "   ", strings.Repeat("a", 65), "bad\nname", "tab\tname", "nul\x00", "esc\x1b[31m", "\u0085next-line", string([]byte{0xff, 0xfe})} {
		if _, _, err := s.Create(name, ScopeAdmin); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: want ErrInvalidName, got %v", name, err)
		}
	}
	for _, name := range []string{"a", strings.Repeat("é", 64), "ci runner #2", "Ünïcødé"} {
		if _, _, err := s.Create(name, ScopeAdmin); err != nil {
			t.Errorf("name %q must be accepted: %v", name, err)
		}
	}
	for _, scope := range []string{"", "Admin", "root", "analyze "} {
		if _, _, err := s.Create("x", scope); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("scope %q: want ErrInvalidScope, got %v", scope, err)
		}
	}
}

func TestVerify(t *testing.T) {
	s := NewStore(t.TempDir())
	k, secret, err := s.Create("admin", ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify(secret)
	if err != nil || got.ID != k.ID || got.Scope != ScopeAdmin {
		t.Fatalf("Verify(valid) = %+v, %v", got, err)
	}
	wrong := secret[:len(secret)-1] + "0"
	if wrong == secret {
		wrong = secret[:len(secret)-1] + "1"
	}
	for _, bad := range []string{"", "rm_live_", wrong, "Bearer " + secret, secret + "x", strings.ToUpper(secret)} {
		if _, err := s.Verify(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Verify(%q): want ErrInvalidKey, got %v", bad, err)
		}
	}
	if err := s.Revoke(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(secret); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked key must be invalid, got %v", err)
	}
}

func TestVerifyOnEmptyStore(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "not-yet-created"))
	if _, err := s.Verify("rm_live_" + strings.Repeat("a", 32)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("want ErrInvalidKey, got %v", err)
	}
	keys, err := s.List()
	if err != nil || len(keys) != 0 {
		t.Fatalf("List on missing dir = %v, %v", keys, err)
	}
}

func TestRevoke(t *testing.T) {
	s := NewStore(t.TempDir())
	k, _, _ := s.Create("a", ScopeAnalyze)
	if err := s.Revoke("k_0000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: want ErrNotFound, got %v", err)
	}
	if err := s.Revoke("../../etc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("garbage id: want ErrNotFound, got %v", err)
	}
	if err := s.Revoke(k.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(k.ID); err != nil {
		t.Fatalf("revoking twice is a no-op, got %v", err)
	}
	keys, _ := s.List()
	if len(keys) != 1 || !keys[0].Revoked {
		t.Fatalf("List after revoke = %+v", keys)
	}
}

func TestListIsCreationOrderAndHasNoHash(t *testing.T) {
	s := NewStore(t.TempDir())
	a, _, _ := s.Create("first", ScopeAdmin)
	b, _, _ := s.Create("second", ScopeAnalyze)
	keys, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].ID != a.ID || keys[1].ID != b.ID {
		t.Fatalf("List = %+v", keys)
	}
}

// The CLI writes keys.json while the server holds its own Store; the
// server must see new keys and revocations without a restart.
func TestStoreSeesChangesFromAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	server := NewStore(dir)
	if keys, _ := server.List(); len(keys) != 0 {
		t.Fatal("expected empty")
	}
	cli := NewStore(dir)
	k, secret, err := cli.Create("from-cli", ScopeAnalyze)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := server.Verify(secret); err != nil || got.ID != k.ID {
		t.Fatalf("server did not see CLI key: %+v %v", got, err)
	}
	if err := NewStore(dir).Revoke(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Verify(secret); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("server did not see CLI revocation: %v", err)
	}
}

func TestConcurrentWritersLoseNoKeys(t *testing.T) {
	dir := t.TempDir()
	stores := []*Store{NewStore(dir), NewStore(dir), NewStore(dir)}
	const n = 30
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := stores[i%len(stores)].Create("k", ScopeAnalyze); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	keys, err := NewStore(dir).List()
	if err != nil || len(keys) != n {
		t.Fatalf("want %d keys, got %d (%v)", n, len(keys), err)
	}
}

func TestLastUsedIsThrottledToOncePerMinute(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock()
	s := NewStore(dir)
	s.now = clk.Now
	k, secret, _ := s.Create("a", ScopeAnalyze)
	if k.LastUsedAt != nil {
		t.Fatal("new key must have null last_used_at")
	}
	t0 := clk.Now()
	if _, err := s.Verify(secret); err != nil {
		t.Fatal(err)
	}
	lastUsed := func() time.Time {
		keys, _ := NewStore(dir).List()
		if keys[0].LastUsedAt == nil {
			t.Fatal("last_used_at not written")
		}
		return *keys[0].LastUsedAt
	}
	if !lastUsed().Equal(t0) {
		t.Fatalf("last_used_at = %v, want %v", lastUsed(), t0)
	}
	fi1, _ := os.Stat(filepath.Join(dir, "keys.json"))
	clk.Add(30 * time.Second)
	s.Verify(secret)
	if !lastUsed().Equal(t0) {
		t.Fatalf("write not throttled: %v", lastUsed())
	}
	fi2, _ := os.Stat(filepath.Join(dir, "keys.json"))
	if !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("keys.json rewritten within the throttle window")
	}
	clk.Add(31 * time.Second)
	s.Verify(secret)
	if want := t0.Add(61 * time.Second); !lastUsed().Equal(want) {
		t.Fatalf("last_used_at = %v, want %v", lastUsed(), want)
	}
}

func TestHasActiveAdmin(t *testing.T) {
	s := NewStore(t.TempDir())
	if ok, err := s.HasActiveAdmin(); ok || err != nil {
		t.Fatalf("empty: %v %v", ok, err)
	}
	s.Create("a", ScopeAnalyze)
	if ok, _ := s.HasActiveAdmin(); ok {
		t.Fatal("analyze key is not admin")
	}
	k, _, _ := s.Create("b", ScopeAdmin)
	if ok, _ := s.HasActiveAdmin(); !ok {
		t.Fatal("admin key not found")
	}
	s.Revoke(k.ID)
	if ok, _ := s.HasActiveAdmin(); ok {
		t.Fatal("revoked admin key counted")
	}
}

func TestCorruptFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "keys.json"), []byte("{not json"), 0o600)
	s := NewStore(dir)
	if _, err := s.Verify("rm_live_" + strings.Repeat("a", 32)); err == nil {
		t.Fatal("corrupt store must not verify")
	}
	if _, _, err := s.Create("x", ScopeAdmin); err == nil {
		t.Fatal("corrupt store must not be overwritten by Create")
	}
}
