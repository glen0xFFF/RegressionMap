package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
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

func fakeRead(files map[string]string) ReadFunc {
	return func(p string) (string, bool, error) {
		if s, ok := files[p]; ok {
			return s, false, nil
		}
		return "", false, os.ErrNotExist
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
	// The path inside a title is swapped for its redacted form whole, so the
	// segments after the secret survive and the title matches File.
	wantTitles := []string{"Failing test: t", "config/token=[REDACTED]/app.ts:2",
		"Change in config/token=[REDACTED]/app.ts", "Current source of config/token=[REDACTED]/app.ts"}
	for i, w := range wantTitles {
		if ev[i].Title != w {
			t.Errorf("%s title = %q, want %q", ev[i].Type, ev[i].Title, w)
		}
	}
}

func TestBuildCapsTitleLength(t *testing.T) {
	const mark = "…[truncated]"
	// "Failing test: " is 14 bytes, so with "é" (2 bytes) alone byte 200 is
	// always a rune start; the leading "x" puts the cut inside a rune.
	for _, name := range []string{strings.Repeat("x", 10000), strings.Repeat("é", 5000), "x" + strings.Repeat("é", 5000)} {
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
	// A title of exactly 200 bytes is within the cap and keeps every byte.
	exact := "Failing test: " + strings.Repeat("y", 200-len("Failing test: "))
	ev := Build([]Failure{{Name: exact[len("Failing test: "):], File: "test/a.test.ts", Line: 1}}, nil, fakeRead(nil), Limits{MaxExcerpt: 300, MaxTotal: 50000})
	if len(ev) != 1 || ev[0].Title != exact {
		t.Errorf("200-byte title changed: got %d bytes %q", len(ev[0].Title), ev[0].Title)
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

// A file over the read cap comes back in part. Frames inside the part keep
// their real lines, a frame past the cut says the file was too large (not
// that it is unreadable), and the changed file keeps its source item, marked
// as cut.
func TestBuildUsesFilesReadInPart(t *testing.T) {
	const notRead = "(source larger than 1 MiB, not read)"
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("l" + itoa(i) + "\n")
	}
	part := b.String() // lines 1-10 complete; line 11 onwards was cut
	read := func(p string) (string, bool, error) {
		switch p {
		case "src/big.ts":
			return part, true, nil
		case "src/huge-line.ts":
			return "", true, nil // not one complete line fits
		}
		return "", false, os.ErrNotExist
	}
	fs := []Failure{{Name: "t", File: "test/a.test.ts", Line: 1, Frames: []Frame{
		{File: "src/big.ts", Line: 5}, {File: "src/big.ts", Line: 11}, {File: "src/big.ts", Line: 900},
		{File: "src/huge-line.ts", Line: 1},
	}}}
	hs := []Hunk{{File: "src/big.ts", NewStart: 2, NewCount: 1, Text: "@@ -2 +2 @@"},
		{File: "src/huge-line.ts", NewStart: 1, NewCount: 1, Text: "@@ -1 +1 @@"}}
	ev := Build(fs, hs, read, Limits{MaxExcerpt: 2000, MaxTotal: 50000})
	byTitle := map[string]Item{}
	for _, e := range ev {
		byTitle[e.Title] = e
	}
	if f := byTitle["src/big.ts:5"]; f.Excerpt != "l3\nl4\nl5\nl6\nl7" || f.LineStart != 3 || f.LineEnd != 7 {
		t.Errorf("frame inside the part: %+v", f)
	}
	for _, key := range []string{"src/big.ts:11", "src/big.ts:900", "src/huge-line.ts:1"} {
		f := byTitle[key]
		if f.Excerpt != notRead || f.LineStart != f.LineEnd || f.LineStart == 0 {
			t.Errorf("frame past the cut %s: %+v, want excerpt %q on its own line", key, f, notRead)
		}
	}
	src, ok := byTitle["Current source of src/big.ts"]
	if !ok || src.LineStart != 1 || src.LineEnd != 10 || src.Excerpt != strings.TrimSuffix(part, "\n")+truncMark {
		t.Errorf("source item of a file read in part: found=%v %+v", ok, src)
	}
	src, ok = byTitle["Current source of src/huge-line.ts"]
	if !ok || src.LineStart != 0 || src.LineEnd != 0 || src.Excerpt != notRead {
		t.Errorf("source item of a file with no complete line read: found=%v %+v", ok, src)
	}
	// A cut source longer than MaxExcerpt carries one mark, not two.
	ev = Build(nil, hs[:1], read, Limits{MaxExcerpt: 8, MaxTotal: 50000})
	if len(ev) != 2 || ev[1].Excerpt != part[:8]+truncMark {
		t.Errorf("cut source over MaxExcerpt: %+v", ev)
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

// testSHA is a 40-hex sha whose first 7 characters are i in hex.
func testSHA(i int) string { return fmt.Sprintf("%07x%033x", i, i) }

// fixtureInputs are the failures, hunks and files of the pagination fixture.
func fixtureInputs(t *testing.T) ([]Failure, []Hunk, ReadFunc) {
	t.Helper()
	src := "l1\nl2\nl3\nl4\nl5\nl6\nl7\ntest(\"first\", () => {\n  assert.deepEqual(paginate(items, 1, 3).items, [1, 2, 3]);\n});\nl11\nl12\n  assert.deepEqual(x);\nl14\n"
	read := fakeRead(map[string]string{
		"test/pagination.test.ts": src,
		"src/pagination.ts":       "export function paginate() {}\n",
		"src/format.ts":           "export function formatTitle() {}\n",
	})
	return ParseNodeTest(load(t, "node_test_fail.txt"), "C:/work/pagination-demo"), ParseDiff(load(t, "diff.txt")), read
}

func commitItems(ev []Item) []Item {
	out := []Item{}
	for _, e := range ev {
		if e.Type == "commit" {
			out = append(out, e)
		}
	}
	return out
}

func TestBuildWithoutCommitsIsUnchanged(t *testing.T) {
	f, h, r := fixtureInputs(t)
	lim := Limits{MaxExcerpt: 2000, MaxTotal: 50000}
	if got, want := BuildWithCommits(f, h, nil, r, lim), Build(f, h, r, lim); !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildWithCommits with no commits:\n%+v\nwant Build:\n%+v", got, want)
	}
}

func TestCommitItemsComeLastAndKeepOtherIDs(t *testing.T) {
	f, h, r := fixtureInputs(t)
	lim := Limits{MaxExcerpt: 2000, MaxTotal: 50000}
	base := Build(f, h, r, lim)
	commits := []CommitMessage{
		{SHA: "0123456789abcdef0123456789abcdef01234567", Message: "subject\n\nbody line  \n\n"},
		{SHA: "fedcba9876543210fedcba9876543210fedcba98", Message: "older one\t"},
	}
	ev := BuildWithCommits(f, h, commits, r, lim)
	if len(base) == 0 || len(ev) != len(base)+2 {
		t.Fatalf("got %d items, want the %d of Build plus 2", len(ev), len(base))
	}
	if !reflect.DeepEqual(ev[:len(base)], base) {
		t.Fatalf("the other items changed:\n%+v\nwant:\n%+v", ev[:len(base)], base)
	}
	want := []Item{
		{ID: "E" + itoa(len(base)+1), Type: "commit", Title: "commit 0123456: subject", Excerpt: "subject\n\nbody line"},
		{ID: "E" + itoa(len(base)+2), Type: "commit", Title: "commit fedcba9: older one", Excerpt: "older one"},
	}
	for i, w := range want {
		if got := ev[len(base)+i]; got != w {
			t.Errorf("commit item %d = %+v, want %+v", i, got, w)
		}
	}
}

func TestCommitItemsAreRedacted(t *testing.T) {
	msg := "add key sk-abcdefghijklmnopqrstu\n\ntoken: hunter22secret\n"
	ev := BuildWithCommits(nil, nil, []CommitMessage{{SHA: testSHA(1), Message: msg}}, fakeRead(nil), Limits{MaxExcerpt: 2000, MaxTotal: 50000})
	if len(ev) != 1 {
		t.Fatalf("want 1 item, got %+v", ev)
	}
	it := ev[0]
	if it.Title != "commit 0000001: add key [REDACTED]" {
		t.Errorf("title = %q", it.Title)
	}
	if it.Excerpt != "add key [REDACTED]\n\ntoken: [REDACTED]" {
		t.Errorf("excerpt = %q", it.Excerpt)
	}
	for _, leak := range []string{"sk-abcdefghijklmnopqrstu", "hunter22secret"} {
		if strings.Contains(it.Title, leak) || strings.Contains(it.Excerpt, leak) {
			t.Errorf("leaked %q: %+v", leak, it)
		}
	}
}

// A private key that starts on the first line of the message and ends on a
// later one is redacted in the whole message before the title takes that
// line, so no key bytes reach the title (contract, corrections of record to
// v4, entry 2).
func TestCommitTitleDropsASecretThatEndsOnALaterLine(t *testing.T) {
	begin := func(kind string) string { return "-----BEGIN " + kind + "PRIVATE KEY-----" }
	end := func(kind string) string { return "-----END " + kind + "PRIVATE KEY-----" }
	keyBytes := []string{"MIIEpAIBAAKCAQEAfirstKEYline", "secondKEYlineQ29uZmlkZW50aWFs", "lastKEYlineAAAA"}
	cases := []struct {
		name, msg, title, excerpt string
	}{
		{
			name: "header joined to key bytes",
			msg: begin("RSA ") + keyBytes[0] + "\n" + keyBytes[1] + "\n" + keyBytes[2] + "\n" + end("RSA ") +
				"\n\nrotate the deploy key\n",
			title:   "commit 0000001: [REDACTED]",
			excerpt: "[REDACTED]\n\nrotate the deploy key",
		},
		{
			name: "key after subject text",
			msg: "add the deploy key " + begin("OPENSSH ") + keyBytes[0] + "\r\n" + keyBytes[1] + "\r\n" + keyBytes[2] + end("OPENSSH ") +
				" to the fixtures\r\n\r\nbody line\r\n",
			title:   "commit 0000001: add the deploy key [REDACTED] to the fixtures",
			excerpt: "add the deploy key [REDACTED] to the fixtures\r\n\r\nbody line",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := BuildWithCommits(nil, nil, []CommitMessage{{SHA: testSHA(1), Message: c.msg}}, fakeRead(nil), Limits{MaxExcerpt: 2000, MaxTotal: 50000})
			if len(ev) != 1 {
				t.Fatalf("want 1 item, got %+v", ev)
			}
			it := ev[0]
			for _, leak := range append([]string{"PRIVATE KEY"}, keyBytes...) {
				if strings.Contains(it.Title, leak) || strings.Contains(it.Excerpt, leak) {
					t.Errorf("leaked %q: title %q, excerpt %q", leak, it.Title, it.Excerpt)
				}
			}
			if it.Title != c.title || it.Excerpt != c.excerpt {
				t.Errorf("title %q, excerpt %q; want %q and %q", it.Title, it.Excerpt, c.title, c.excerpt)
			}
		})
	}
}

func TestCommitTitleIsCapped(t *testing.T) {
	const mark = "…[truncated]"
	msg := strings.Repeat("s", 300) + "\n\nbody"
	ev := BuildWithCommits(nil, nil, []CommitMessage{{SHA: testSHA(2), Message: msg}}, fakeRead(nil), Limits{MaxExcerpt: 2000, MaxTotal: 50000})
	if len(ev) != 1 {
		t.Fatalf("want 1 item, got %+v", ev)
	}
	title := ev[0].Title
	if len(title) > 200+len(mark) || !strings.HasSuffix(title, mark) || !strings.HasPrefix(title, "commit 0000002: sss") {
		t.Fatalf("title %d bytes: %q", len(title), title)
	}
	if strings.Contains(title, "body") || ev[0].Excerpt != msg {
		t.Fatalf("title holds more than the first line, or the excerpt changed: %+v", ev[0])
	}
}

// The cut lands inside an "é": it backs off to the start of that character.
// Invalid bytes and escape sequences are kept as they are, and the items
// still marshal.
func TestCommitExcerptCutKeepsCharacters(t *testing.T) {
	msg := "abcdefg" + strings.Repeat("é\xff\x1b[31m", 186) + "hello"
	if len(msg) != 1500 || utf8.RuneStart(msg[1000]) {
		t.Fatalf("set-up: %d bytes, byte 1000 must be inside a character", len(msg))
	}
	// Also a run of bytes that are never a character start: the cut does not
	// walk back through all of it.
	cont := strings.Repeat("\x80", 1500)
	for _, m := range []string{msg, cont} {
		ev := BuildWithCommits(nil, nil, []CommitMessage{{SHA: testSHA(3), Message: m}}, fakeRead(nil), Limits{MaxExcerpt: 3000, MaxTotal: 40000})
		if len(ev) != 1 {
			t.Fatalf("want 1 item, got %d", len(ev))
		}
		ex := ev[0].Excerpt
		if !strings.HasSuffix(ex, truncMark) || len(ex) > 1000+len(truncMark) {
			t.Fatalf("excerpt %d bytes, ends %q", len(ex), ex[max(0, len(ex)-20):])
		}
		kept := strings.TrimSuffix(ex, truncMark)
		if !strings.HasPrefix(m, kept) || len(kept) < 1000-(utf8.UTFMax-1) {
			t.Fatalf("kept part is not the start of the message: %d bytes", len(kept))
		}
		if m == msg && !utf8.RuneStart(m[len(kept)]) {
			t.Fatalf("kept part ends inside a character: cut at byte %d", len(kept))
		}
		if _, err := json.Marshal(ev); err != nil {
			t.Fatalf("items do not marshal: %v", err)
		}
	}
}

func TestCommitItemsStopAtTheTotal(t *testing.T) {
	var cs []CommitMessage
	for i := 0; i < 6; i++ {
		cs = append(cs, CommitMessage{SHA: testSHA(i), Message: strings.Repeat(string(rune('a'+i)), 900)})
	}
	cs = append(cs, CommitMessage{SHA: testSHA(6), Message: "0123456789"})
	ev := BuildWithCommits(nil, nil, cs, fakeRead(nil), Limits{MaxExcerpt: 3000, MaxTotal: 40000})
	total := 0
	for i, e := range ev {
		if e.Type != "commit" || !strings.HasPrefix(e.Title, "commit "+testSHA(i)[:7]+": "+cs[i].Message[:10]) || e.Excerpt != cs[i].Message {
			t.Errorf("item %d = %q %q", i, e.Type, e.Title)
		}
		total += len(e.Excerpt)
	}
	if len(ev) != 4 || total != 3600 {
		t.Fatalf("got %d commit items, %d bytes; want 4 and 3600", len(ev), total)
	}
}

func TestCommitItemsAtMostTen(t *testing.T) {
	var cs []CommitMessage
	for i := 0; i < 12; i++ {
		cs = append(cs, CommitMessage{SHA: testSHA(i), Message: fmt.Sprintf("m%04d", i)})
	}
	ev := BuildWithCommits(nil, nil, cs, fakeRead(nil), Limits{MaxExcerpt: 3000, MaxTotal: 40000})
	if len(ev) != 10 || ev[0].Excerpt != "m0000" || ev[9].Excerpt != "m0009" || ev[9].ID != "E10" {
		t.Fatalf("got %d items: %+v", len(ev), ev)
	}
}

// The commit excerpts are set aside first: the source item that would fill
// the budget is dropped instead of the commits.
func TestCommitSpaceIsReservedFromTheBudget(t *testing.T) {
	lim := Limits{MaxExcerpt: 3000, MaxTotal: 5000}
	hunks := []Hunk{{File: "src/a.ts", NewStart: 1, NewCount: 1, Text: strings.Repeat("h", 2500)}}
	read := fakeRead(map[string]string{"src/a.ts": strings.Repeat("s", 2500)})
	if ev := Build(nil, hunks, read, lim); len(ev) != 2 || ev[1].Type != "source" {
		t.Fatalf("set-up: without commits the source item must fit: %d items", len(ev))
	}
	cs := []CommitMessage{{SHA: testSHA(1), Message: strings.Repeat("c", 300)}, {SHA: testSHA(2), Message: strings.Repeat("d", 300)}}
	ev := BuildWithCommits(nil, hunks, cs, read, lim)
	types := []string{}
	total := 0
	for _, e := range ev {
		types = append(types, e.Type)
		total += len(e.Excerpt)
	}
	if strings.Join(types, ",") != "diff_hunk,commit,commit" || total > 5000 {
		t.Fatalf("types %v, %d bytes; want diff_hunk,commit,commit within 5000", types, total)
	}
	if len(commitItems(ev)) != 2 || ev[1].ID != "E2" || ev[2].ID != "E3" {
		t.Fatalf("commit items: %+v", commitItems(ev))
	}
}

// Below 4000 bytes, MaxTotal is the commit limit: the commit excerpts, and
// all items together, stay within it.
func TestCommitItemsStayWithinASmallTotal(t *testing.T) {
	lim := Limits{MaxExcerpt: 3000, MaxTotal: 1500}
	hunks := []Hunk{{File: "src/a.ts", NewStart: 1, NewCount: 1, Text: strings.Repeat("h", 100)}}
	cs := []CommitMessage{{SHA: testSHA(1), Message: strings.Repeat("c", 900)}, {SHA: testSHA(2), Message: strings.Repeat("d", 900)}}
	ev := BuildWithCommits(nil, hunks, cs, fakeRead(nil), lim)
	types := []string{}
	total, commitTotal := 0, 0
	for _, e := range ev {
		types = append(types, e.Type)
		total += len(e.Excerpt)
		if e.Type == "commit" {
			commitTotal += len(e.Excerpt)
		}
	}
	if commitTotal > 1500 || total > 1500 {
		t.Fatalf("commit excerpts %d bytes, all items %d bytes; want both within 1500 (types %v)", commitTotal, total, types)
	}
	if strings.Join(types, ",") != "diff_hunk,commit" || ev[1].Excerpt != cs[0].Message {
		t.Fatalf("types %v; want diff_hunk,commit with the newest commit", types)
	}
}
