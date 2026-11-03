package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewIDFormatUniqueAndTimeSortable(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 2000; i++ {
		id := NewID()
		if !ValidID(id) {
			t.Fatalf("NewID produced invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		if id <= prev {
			t.Fatalf("ids not strictly increasing: %q after %q", id, prev)
		}
		prev = id
	}
}

func TestValidIDRejectsTraversalAndJunk(t *testing.T) {
	bad := []string{
		"", "a", "A1", "b1", "a/b", `a\b`, "a..", "../a1", "a1/../../x", `a1\..\x`,
		"a1.json", "a1\x00", "a1 ", "a:1", "a" + string(bytes.Repeat([]byte("x"), 41)), "/a1", "C:a1",
	}
	for _, id := range bad {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
	for _, id := range []string{"a1", "a24", "a0mh4k2x1z-3k9d8f2a", "a" + string(bytes.Repeat([]byte("z"), 40))} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false", id)
		}
	}
}

func TestSaveLoadRoundTripAtomic(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	id := NewID()
	if err := s.Save(id, []byte(`{"id":"x","status":"running"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(id, []byte(`{"id":"x","status":"done"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(id)
	if err != nil || string(got) != `{"id":"x","status":"done"}` {
		t.Fatalf("Load = %q, %v", got, err)
	}
	p := filepath.Join(dir, "analyses", id+".json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("expected file at %s: %v", p, err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v, want 0600", st.Mode().Perm())
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "analyses"))
	if len(ents) != 1 {
		t.Fatalf("leftover temp files: %v", ents)
	}
}

func TestLoadMissingIsNotFound(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Load("a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSaveAndLoadRejectInvalidIDs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	s := New(dir)
	for _, id := range []string{"../a1", "a1/../../escape", `..\..\escape`, "a1.json", "", "A1"} {
		if err := s.Save(id, []byte("{}")); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Save(%q) err = %v, want ErrInvalidID", id, err)
		}
		if _, err := s.Load(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Load(%q) err = %v, want ErrInvalidID", id, err)
		}
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if e.Name() != "data" {
			t.Errorf("file escaped the data dir: %s", e.Name())
		}
	}
}

func TestListNewestFirstIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ids := []string{NewID(), NewID(), NewID()}
	for _, i := range []int{1, 0, 2} {
		if err := s.Save(ids[i], []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "analyses", "notes.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "analyses", ".tmp-123.json"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "analyses", "B1.json"), []byte("x"), 0o600)
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ids[2], ids[1], ids[0]}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("List = %v, want %v", got, want)
	}
	if got, err := New(filepath.Join(dir, "missing")).List(); err != nil || len(got) != 0 {
		t.Fatalf("List on empty store = %v, %v", got, err)
	}
}
