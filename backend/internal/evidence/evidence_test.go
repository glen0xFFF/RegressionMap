package evidence

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestBuildRedactsSecretsInTitle(t *testing.T) {
	cases := []struct{ name, secret string }{
		{"uses token=supersecretvalue123", "supersecretvalue123"},
		{"calls api with sk-abcdefghijklmnopqrstuvwx", "sk-abcdefghijklmnopqrstuvwx"},
		{"clones with ghp_abcdefghijklmnopqrstuvwxyz0123", "ghp_abcdefghijklmnopqrstuvwxyz0123"},
		{"aws key AKIAABCDEFGHIJKLMNOP works", "AKIAABCDEFGHIJKLMNOP"},
		{"sends Bearer abc.def.ghi123456 header", "abc.def.ghi123456"},
	}
	for _, c := range cases {
		ev := Build([]Failure{{Name: c.name, File: "test/a.test.ts", Line: 3, Message: "boom"}}, nil, fakeRead(nil), Limits{MaxExcerpt: 2000, MaxTotal: 50000})
		if len(ev) != 1 {
			t.Fatalf("%q: want 1 item, got %+v", c.name, ev)
		}
		if strings.Contains(ev[0].Title, c.secret) {
			t.Errorf("title leaked %q: %q", c.secret, ev[0].Title)
		}
		if !strings.Contains(ev[0].Title, "[REDACTED]") || !strings.HasPrefix(ev[0].Title, "Failing test: ") {
			t.Errorf("title not redacted in place: %q", ev[0].Title)
		}
		if strings.Contains(ev[0].Excerpt, c.secret) {
			t.Errorf("excerpt leaked %q: %q", c.secret, ev[0].Excerpt)
		}
	}
}

func TestBuildKeepsNormalTitlesAndFiles(t *testing.T) {
	fs := []Failure{{Name: "first page returns pageSize items", File: "test/pagination.test.ts", Line: 8,
		Frames: []Frame{{File: "test/pagination.test.ts", Line: 9}}}}
	hs := []Hunk{{File: "src/pagination.ts", NewStart: 7, NewCount: 7, Text: "@@ -7,7 +7,7 @@"}}
	ev := Build(fs, hs, fakeRead(map[string]string{"src/pagination.ts": "a\nb\n"}), Limits{MaxExcerpt: 2000, MaxTotal: 50000})
	want := []struct{ title, file string }{
		{"Failing test: first page returns pageSize items", "test/pagination.test.ts"},
		{"test/pagination.test.ts:9", "test/pagination.test.ts"},
		{"Change in src/pagination.ts", "src/pagination.ts"},
		{"Current source of src/pagination.ts", "src/pagination.ts"},
	}
	if len(ev) != len(want) {
		t.Fatalf("want %d items, got %+v", len(want), ev)
	}
	for i, w := range want {
		if ev[i].Title != w.title || ev[i].File != w.file {
			t.Errorf("item %d: got title=%q file=%q, want title=%q file=%q", i, ev[i].Title, ev[i].File, w.title, w.file)
		}
	}
}

func TestBuildRedactsSecretSegmentInFileButKeepsItemUsable(t *testing.T) {
	secretPath := "config/token=supersecretvalue123/app.ts"
	read := fakeRead(map[string]string{secretPath: "x\ny\nz\n"})
	fs := []Failure{{Name: "t", File: secretPath, Line: 2, Frames: []Frame{{File: secretPath, Line: 2}}}}
	hs := []Hunk{{File: secretPath, NewStart: 1, NewCount: 1, Text: "@@ -1 +1 @@"}}
	ev := Build(fs, hs, read, Limits{MaxExcerpt: 2000, MaxTotal: 50000})
	if len(ev) != 4 {
		t.Fatalf("want 4 items (failure, frame, hunk, source), got %+v", ev)
	}
	for _, e := range ev {
		if strings.Contains(e.File, "supersecretvalue123") || strings.Contains(e.Title, "supersecretvalue123") {
			t.Errorf("%s leaked secret: title=%q file=%q", e.Type, e.Title, e.File)
		}
		if e.File != "config/token=[REDACTED]/app.ts" {
			t.Errorf("%s file should keep its other segments: %q", e.Type, e.File)
		}
	}
	if ev[1].Type != "stack_frame" || !strings.Contains(ev[1].Excerpt, "y") || ev[1].Excerpt == "(source not readable)" {
		t.Errorf("frame source must still be read via the original path: %+v", ev[1])
	}
	if ev[3].Type != "source" || ev[3].Excerpt != "x\ny\nz" {
		t.Errorf("source must still be read via the original path: %+v", ev[3])
	}
}

func TestBuildCapsTitleLength(t *testing.T) {
	const mark = "…[truncated]"
	for _, name := range []string{strings.Repeat("x", 10000), strings.Repeat("é", 5000)} {
		ev := Build([]Failure{{Name: name, File: "test/a.test.ts", Line: 1}}, nil, fakeRead(nil), Limits{MaxExcerpt: 100, MaxTotal: 50000})
		if len(ev) != 1 {
			t.Fatalf("want 1 item, got %d", len(ev))
		}
		title := ev[0].Title
		if len(title) > 200+len(mark) {
			t.Errorf("title too long: %d bytes", len(title))
		}
		if !strings.HasSuffix(title, mark) || !strings.HasPrefix(title, "Failing test: ") {
			t.Errorf("title missing prefix or truncation mark: %d bytes, ends %q", len(title), title[max(0, len(title)-24):])
		}
		if !utf8.ValidString(title) {
			t.Errorf("title truncated mid-rune: %q", title)
		}
	}
}

func TestBuildRedactedTitlesStillRespectTotalCap(t *testing.T) {
	var fs []Failure
	for i := 0; i < 50; i++ {
		fs = append(fs, Failure{Name: "uses token=supersecretvalue123 " + itoa(i), File: "test/a.test.ts", Line: i + 1,
			Message: strings.Repeat("m", 1000)})
	}
	ev := Build(fs, nil, fakeRead(nil), Limits{MaxExcerpt: 500, MaxTotal: 3000})
	total := 0
	for _, e := range ev {
		if len(e.Excerpt) > 500+len(truncMark) {
			t.Fatalf("excerpt too long: %d", len(e.Excerpt))
		}
		if strings.Contains(e.Title, "supersecretvalue123") {
			t.Fatalf("title leaked: %q", e.Title)
		}
		total += len(e.Excerpt)
	}
	if total > 3000 || len(ev) == 0 || len(ev) == 50 {
		t.Fatalf("cap not applied: total=%d n=%d", total, len(ev))
	}
}

// Callers that send changed file names next to the evidence (changed_files)
// must redact them the same way Build redacts Item.File, or the secret leaks
// and a model citing the raw path no longer matches the evidence.
func TestRedactPathMatchesBuildFile(t *testing.T) {
	cases := map[string]string{
		"src/app.ts": "src/app.ts",
		"config/token=supersecretvalue123/app.ts":      "config/token=[REDACTED]/app.ts",
		"AKIAABCDEFGHIJKLMNOP/x.ts":                    "[REDACTED]/x.ts",
		"keys/ghp_abcdefghijklmnopqrstuvwxyz0123/k.ts": "keys/[REDACTED]/k.ts",
		"sk-abcdefghijklmnopqrstuv.ts":                 "[REDACTED].ts",
		"":                                             "",
	}
	for in, want := range cases {
		if got := RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
		if in == "" {
			continue
		}
		ev := Build(nil, []Hunk{{File: in, NewStart: 1, NewCount: 1, Text: "@@ -1 +1 @@"}}, fakeRead(nil), Limits{MaxExcerpt: 100, MaxTotal: 1000})
		if len(ev) != 1 || ev[0].File != RedactPath(in) {
			t.Errorf("Build File for %q = %+v, want RedactPath result %q", in, ev, RedactPath(in))
		}
	}
}
