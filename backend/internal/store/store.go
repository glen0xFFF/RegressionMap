// Package store persists analyses as local JSON files under
// <DataDir>/analyses/<id>.json. IDs are validated against a strict pattern
// before they are ever joined into a filesystem path.
package store

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidID = errors.New("invalid analysis id")
	ErrNotFound  = errors.New("analysis not found")
)

var idRe = regexp.MustCompile(`^a[0-9a-z-]{1,40}$`)

// ValidID reports whether id is safe to use as a file name.
func ValidID(id string) bool { return idRe.MatchString(id) }

var (
	idMu   sync.Mutex
	lastMS int64
)

// NewID returns "a" + 9 base36 digits of a strictly increasing millisecond
// clock + "-" + 8 random base36 digits. Fixed width makes string order equal
// creation order; the random suffix keeps ids unique across restarts.
func NewID() string {
	idMu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= lastMS {
		ms = lastMS + 1
	}
	lastMS = ms
	idMu.Unlock()

	var b [8]byte
	_, _ = rand.Read(b[3:]) // 40 random bits
	r := strconv.FormatUint(binary.BigEndian.Uint64(b[:]), 36)
	return "a" + pad(strconv.FormatInt(ms, 36), 9) + "-" + pad(r, 8)
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat("0", n-len(s)) + s
}

// Store is a directory of analysis JSON files.
type Store struct{ dir string }

// New returns a store rooted at dataDir. Directories are created on first Save.
func New(dataDir string) *Store { return &Store{dir: filepath.Join(dataDir, "analyses")} }

func (s *Store) path(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrInvalidID
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// Save writes data atomically (temp file in the same directory, then rename)
// with 0600 permissions.
func (s *Store) Save(id string, data []byte) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// CreateTemp already uses 0600; Chmod makes it explicit (no-op on Windows).
	_ = f.Chmod(0o600)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Load returns the stored JSON for id, ErrNotFound, or ErrInvalidID.
func (s *Store) Load(id string) ([]byte, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// List returns all stored ids, newest first. A missing directory is empty.
func (s *Store) List() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, e := range ents {
		id, found := strings.CutSuffix(e.Name(), ".json")
		if found && e.Type().IsRegular() && ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}
