package evidence

import (
	"os"
	"strings"
	"testing"
)

func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseNodeTestFailures(t *testing.T) {
	fs := ParseNodeTest(load(t, "node_test_fail.txt"), "C:/work/pagination-demo")
	if len(fs) != 2 {
		t.Fatalf("want 2 failures, got %d: %+v", len(fs), fs)
	}
	f := fs[0]
	if f.Name != "first page returns pageSize items" {
		t.Errorf("name %q", f.Name)
	}
	if f.File != "test/pagination.test.ts" || f.Line != 8 {
		t.Errorf("location %s:%d", f.File, f.Line)
	}
	if !strings.Contains(f.Message, "actual: [ 1, 2 ]") || !strings.Contains(f.Message, "expected: [ 1, 2, 3 ]") {
		t.Errorf("message missing actual/expected:\n%s", f.Message)
	}
	if len(f.Frames) != 1 || f.Frames[0].File != "test/pagination.test.ts" || f.Frames[0].Line != 9 {
		t.Errorf("frames %+v (node internals must be dropped)", f.Frames)
	}
}

func TestParseNodeTestPassingOutputHasNoFailures(t *testing.T) {
	if fs := ParseNodeTest("✔ a (1ms)\nℹ tests 1\nℹ fail 0\n", "C:/x"); len(fs) != 0 {
		t.Fatalf("got %+v", fs)
	}
}

func TestParseDiffHunks(t *testing.T) {
	hs := ParseDiff(load(t, "diff.txt"))
	if len(hs) != 3 {
		t.Fatalf("want 3 hunks, got %d", len(hs))
	}
	h := hs[1]
	if h.File != "src/pagination.ts" || h.NewStart != 7 || h.NewCount != 7 {
		t.Fatalf("hunk %+v", h)
	}
	if !strings.Contains(h.Text, "+  const end = start + pageSize - 1;") {
		t.Fatalf("hunk text:\n%s", h.Text)
	}
}

func TestRedactSecrets(t *testing.T) {
	in := "key=sk-abcdefghijklmnopqrstuvwx\nAuthorization: Bearer abc.def.ghi123456\nAKIAABCDEFGHIJKLMNOP\npassword = \"hunter22\"\nsafe line"
	out := Redact(in)
	for _, leak := range []string{"sk-abcdefghijklmnopqrstuvwx", "abc.def.ghi123456", "AKIAABCDEFGHIJKLMNOP", "hunter22"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %q", leak, out)
		}
	}
	if !strings.Contains(out, "safe line") {
		t.Error("over-redacted")
	}
}

func fakeRead(files map[string]string) func(string) (string, error) {
	return func(p string) (string, error) {
		if s, ok := files[p]; ok {
			return s, nil
		}
		return "", os.ErrNotExist
	}
}

func TestBuildAssignsStableIDsAndRealLines(t *testing.T) {
	failures := ParseNodeTest(load(t, "node_test_fail.txt"), "C:/work/pagination-demo")
	hunks := ParseDiff(load(t, "diff.txt"))
	src := "l1\nl2\nl3\nl4\nl5\nl6\nl7\ntest(\"first\", () => {\n  assert.deepEqual(paginate(items, 1, 3).items, [1, 2, 3]);\n});\nl11\nl12\n  assert.deepEqual(x);\nl14\n"
	ev := Build(failures, hunks, fakeRead(map[string]string{"test/pagination.test.ts": src}), Limits{MaxExcerpt: 2000, MaxTotal: 50000})

	if len(ev) == 0 || ev[0].ID != "E1" {
		t.Fatalf("ids: %+v", ev)
	}
	seen := map[string]bool{}
	types := map[string]int{}
	for i, e := range ev {
		if seen[e.ID] {
			t.Fatalf("duplicate id %s", e.ID)
		}
		seen[e.ID] = true
		if want := "E" + itoa(i+1); e.ID != want {
			t.Fatalf("id %s at %d", e.ID, i)
		}
		types[e.Type]++
		if e.LineEnd < e.LineStart {
			t.Fatalf("bad range %+v", e)
		}
	}
	if types["test_failure"] != 2 || types["diff_hunk"] != 3 || types["stack_frame"] != 2 {
		t.Fatalf("types %+v", types)
	}
	for _, e := range ev {
		if e.Type == "stack_frame" && e.Title == "test/pagination.test.ts:9" {
			if !strings.Contains(e.Excerpt, "assert.deepEqual(paginate(items, 1, 3)") || e.LineStart > 9 || e.LineEnd < 9 {
				t.Fatalf("frame excerpt wrong: %+v", e)
			}
			return
		}
	}
	t.Fatal("no stack frame for test/pagination.test.ts:9")
}

func TestBuildRespectsTotalCap(t *testing.T) {
	hunks := []Hunk{}
	for i := 0; i < 50; i++ {
		hunks = append(hunks, Hunk{File: "a.ts", NewStart: 1, NewCount: 1, Text: strings.Repeat("y", 1000)})
	}
	ev := Build(nil, hunks, fakeRead(nil), Limits{MaxExcerpt: 500, MaxTotal: 3000})
	total := 0
	for _, e := range ev {
		if len(e.Excerpt) > 500+len(truncMark) {
			t.Fatalf("excerpt too long: %d", len(e.Excerpt))
		}
		total += len(e.Excerpt)
	}
	if total > 3000+len(truncMark)*10 || len(ev) == 50 {
		t.Fatalf("cap not applied: total=%d n=%d", total, len(ev))
	}
}
