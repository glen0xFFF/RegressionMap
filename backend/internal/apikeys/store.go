// Package apikeys implements the v2 authentication contract: API keys
// stored as SHA-256 hashes in <DATA_DIR>/keys.json, the local-trust rule,
// the /api/me and /api/keys routes, and the per-key analysis rate limit.
//
// The secret ("rm_live_" + 32 base62 characters) is returned once by Create
// and never stored. keys.json is shared by the server and the "keys" CLI:
// every write takes a lock file and re-reads the file before changing it, and
// readers re-read the file whenever its modification time or size changes,
// so a key created by the CLI is accepted by a running server immediately.
package apikeys

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Scopes.
const (
	ScopeAdmin   = "admin"
	ScopeAnalyze = "analyze"
)

const (
	secretPrefix = "rm_live_"
	secretLen    = 32 // base62 characters after the prefix
	idLen        = 10 // base36 characters after "k_"
	prefixLen    = 12 // characters of the secret kept as the display prefix

	// touchInterval bounds how often last_used_at is written per key.
	touchInterval = time.Minute

	lockWait  = 5 * time.Second
	lockStale = 30 * time.Second
)

var (
	ErrInvalidName  = errors.New("name must be 1-64 printable characters")
	ErrInvalidScope = errors.New("scope must be admin or analyze")
	ErrInvalidKey   = errors.New("invalid or revoked API key")
	ErrNotFound     = errors.New("key not found")
)

// Key is the public view of a key. It never contains the secret or its hash.
type Key struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Revoked    bool       `json:"revoked"`
}

// record is a Key as stored in keys.json.
type record struct {
	Key
	Hash string `json:"sha256"`
}

type file struct {
	Keys []record `json:"keys"`
}

// Store is the keys.json file of one DATA_DIR. It is safe for concurrent use,
// and several Stores (or processes) may share one file.
type Store struct {
	dir  string
	path string
	now  func() time.Time

	mu        sync.Mutex
	cache     []record
	cacheMod  time.Time
	cacheSize int64
	cached    bool
	touched   map[string]time.Time // last touch attempt per key id
}

// NewStore returns the store for <dataDir>/keys.json. The directory is
// created on the first write.
func NewStore(dataDir string) *Store {
	return &Store{
		dir:     dataDir,
		path:    filepath.Join(dataDir, "keys.json"),
		now:     time.Now,
		touched: map[string]time.Time{},
	}
}

// ValidName reports whether name is 1-64 printable characters with no
// control characters and not only spaces.
func ValidName(name string) bool {
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" {
		return false
	}
	n := 0
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return false
		}
		n++
	}
	return n <= 64
}

// ValidScope reports whether scope is admin or analyze.
func ValidScope(scope string) bool { return scope == ScopeAdmin || scope == ScopeAnalyze }

func (s *Store) nowUTC() time.Time { return s.now().UTC().Truncate(time.Second) }

// load returns the records, re-reading keys.json when its modification time
// or size changed since the last read. The caller holds s.mu.
func (s *Store) load(force bool) ([]record, error) {
	fi, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.cache, s.cached = nil, false
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !force && s.cached && fi.ModTime().Equal(s.cacheMod) && fi.Size() == s.cacheSize {
		return s.cache, nil
	}
	b, err := readRetry(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.cache, s.cached = nil, false
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("keys.json is corrupt: %w", err)
	}
	s.cache, s.cacheMod, s.cacheSize, s.cached = f.Keys, fi.ModTime(), fi.Size(), true
	return s.cache, nil
}

// readRetry reads p, retrying briefly: on Windows a concurrent rename can
// make an open fail for a moment.
func readRetry(p string) ([]byte, error) {
	var err error
	for i := 0; i < 20; i++ {
		var b []byte
		if b, err = os.ReadFile(p); err == nil || errors.Is(err, fs.ErrNotExist) {
			return b, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, err
}

// update runs fn on a fresh copy of the records under the cross-process
// lock and writes the result atomically.
func (s *Store) update(fn func([]record) ([]record, error)) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := s.load(true)
	if err != nil {
		return err
	}
	cp := append([]record(nil), recs...)
	cp, err = fn(cp)
	if err != nil {
		return err
	}
	if err := s.write(cp); err != nil {
		return err
	}
	// Prime the cache with what was just written.
	_, err = s.load(true)
	return err
}

// lock takes <dir>/keys.json.lock (O_EXCL), waiting up to lockWait. A lock
// older than lockStale is left over from a crashed process and is removed.
func (s *Store) lock() (func(), error) {
	lp := s.path + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(lp) }, nil
		}
		if fi, serr := os.Stat(lp); serr == nil && time.Since(fi.ModTime()) > lockStale {
			os.Remove(lp)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("keys.json is locked (%s): %w", lp, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// write replaces keys.json atomically (temp file in the same directory,
// fsync, rename) with mode 0600.
func (s *Store) write(recs []record) error {
	if recs == nil {
		recs = []record{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(file{Keys: recs}); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".keys-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_ = f.Chmod(0o600)
	_, err = f.Write(buf.Bytes())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// On Windows the rename fails while a reader has the file open; retry.
		for i := 0; i < 50; i++ {
			if err = os.Rename(tmp, s.path); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

const (
	base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// randomString returns n characters of alphabet from crypto/rand, using
// rejection sampling so every character is equally likely.
func randomString(alphabet string, n int) (string, error) {
	limit := 256 - 256%len(alphabet)
	out := make([]byte, 0, n)
	var buf [64]byte
	for len(out) < n {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < n {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Create stores a new key and returns it with its secret. The secret is not
// recoverable afterwards.
func (s *Store) Create(name, scope string) (Key, string, error) {
	if !ValidName(name) {
		return Key{}, "", ErrInvalidName
	}
	if !ValidScope(scope) {
		return Key{}, "", ErrInvalidScope
	}
	body, err := randomString(base62, secretLen)
	if err != nil {
		return Key{}, "", err
	}
	secret := secretPrefix + body

	s.mu.Lock()
	defer s.mu.Unlock()
	var k Key
	err = s.update(func(recs []record) ([]record, error) {
		var id string
		for {
			r, err := randomString(base36, idLen)
			if err != nil {
				return nil, err
			}
			id = "k_" + r
			if indexOf(recs, id) < 0 {
				break
			}
		}
		k = Key{ID: id, Name: name, Scope: scope, Prefix: secret[:prefixLen], CreatedAt: s.nowUTC()}
		return append(recs, record{Key: k, Hash: hashSecret(secret)}), nil
	})
	if err != nil {
		return Key{}, "", err
	}
	return k, secret, nil
}

func indexOf(recs []record, id string) int {
	for i := range recs {
		if recs[i].ID == id {
			return i
		}
	}
	return -1
}

// wellFormed reports whether secret has the rm_live_ + 32 base62 shape.
func wellFormed(secret string) bool {
	body, ok := strings.CutPrefix(secret, secretPrefix)
	if !ok || len(body) != secretLen {
		return false
	}
	for i := 0; i < len(body); i++ {
		if !strings.ContainsRune(base62, rune(body[i])) {
			return false
		}
	}
	return true
}

// Verify returns the active key whose hash matches secret, or ErrInvalidKey.
// Every stored hash is compared in constant time. On success last_used_at is
// updated, at most once per minute per key.
func (s *Store) Verify(secret string) (Key, error) {
	if !wellFormed(secret) {
		return Key{}, ErrInvalidKey
	}
	want := []byte(hashSecret(secret))

	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.load(false)
	if err != nil {
		return Key{}, err
	}
	match := -1
	for i := range recs {
		if subtle.ConstantTimeCompare([]byte(recs[i].Hash), want) == 1 {
			match = i
		}
	}
	if match < 0 || recs[match].Revoked {
		return Key{}, ErrInvalidKey
	}
	k := recs[match].Key
	now := s.nowUTC()
	if (k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) >= touchInterval) &&
		(s.touched[k.ID].IsZero() || now.Sub(s.touched[k.ID]) >= touchInterval) {
		s.touched[k.ID] = now
		// Best effort: a failed write must not fail the request.
		_ = s.update(func(recs []record) ([]record, error) {
			if i := indexOf(recs, k.ID); i >= 0 {
				recs[i].LastUsedAt = &now
			}
			return recs, nil
		})
		k.LastUsedAt = &now
	}
	return k, nil
}

// Revoke marks the key revoked. Revoking is permanent; revoking an already
// revoked key is a no-op. An unknown id is ErrNotFound.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(recs []record) ([]record, error) {
		i := indexOf(recs, id)
		if i < 0 {
			return nil, ErrNotFound
		}
		recs[i].Revoked = true
		return recs, nil
	})
}

// List returns every key, revoked ones included, in creation order.
func (s *Store) List() ([]Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.load(false)
	if err != nil {
		return nil, err
	}
	out := make([]Key, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Key)
	}
	return out, nil
}

// HasActiveAdmin reports whether at least one non-revoked admin key exists.
func (s *Store) HasActiveAdmin() (bool, error) {
	keys, err := s.List()
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if k.Scope == ScopeAdmin && !k.Revoked {
			return true, nil
		}
	}
	return false, nil
}
