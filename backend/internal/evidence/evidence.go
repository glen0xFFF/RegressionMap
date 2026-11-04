// Package evidence turns raw test output and git diffs into a bounded,
// redacted list of evidence items with stable IDs (E1, E2, ...).
// Nothing here is guessed: line numbers come from the test runner, the diff
// header, or the file on disk, and are 0 when unknown.
package evidence

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Item is one piece of evidence. JSON shape matches docs/API_CONTRACT.md.
type Item struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	File      string `json:"file"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Excerpt   string `json:"excerpt"`
}

// Frame is a stack location inside the repository.
type Frame struct {
	File string
	Line int
}

// Failure is one failing test reported by node:test.
type Failure struct {
	Name    string
	File    string
	Line    int
	Message string
	Frames  []Frame
}

// Hunk is one diff hunk, located in the new (current) file.
type Hunk struct {
	File     string
	NewStart int
	NewCount int
	Text     string
}

// Limits bound the evidence bundle.
type Limits struct {
	MaxExcerpt int
	MaxTotal   int
}

const truncMark = "\n…[truncated]"

// maxTitle caps a title in bytes (before the mark) so a huge test name cannot
// blow the budget; titleTruncMark is appended when it is cut.
const (
	maxTitle       = 200
	titleTruncMark = "…[truncated]"
)

// capTitle truncates s to at most maxTitle bytes on a rune boundary.
func capTitle(s string) string {
	if len(s) <= maxTitle {
		return s
	}
	cut := maxTitle
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + titleTruncMark
}

// RedactPath redacts each path segment on its own, so a secret-looking
// segment becomes [REDACTED] while the rest of the path stays usable.
// Build applies it to Item.File; callers that send file names alongside the
// evidence (for example a changed-files list) must use it too, so no secret
// leaks and cited paths still match Item.File.
func RedactPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = Redact(s)
	}
	return strings.Join(segs, "/")
}

func itoa(i int) string { return strconv.Itoa(i) }

func normPath(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// relTo returns p relative to root, or "" when p is outside root.
func relTo(p, root string) string {
	p, root = normPath(p), strings.TrimSuffix(normPath(root), "/")
	if strings.HasPrefix(strings.ToLower(p), strings.ToLower(root)+"/") {
		return p[len(root)+1:]
	}
	return ""
}

var (
	testAtRe = regexp.MustCompile(`^test at (.+):(\d+):(\d+)\s*$`)
	nameRe   = regexp.MustCompile(`^✖ (.+?) \(\d+(?:\.\d+)?ms\)\s*$`)
	frameRe  = regexp.MustCompile(`^\s*at (?:.*\()?(file:///[^)]+?):(\d+):(\d+)\)?\s*$`)
	atLineRe = regexp.MustCompile(`^\s*at `)
)

// ParseNodeTest reads the "failing tests" summary node:test prints.
// repoRoot is the directory the tests ran in; frames outside it are dropped.
func ParseNodeTest(out, repoRoot string) []Failure {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "✖ failing tests:") {
			start = i + 1
		}
	}
	if start < 0 {
		return nil
	}
	var fs []Failure
	var cur *Failure
	var msg []string
	flush := func() {
		if cur != nil {
			cur.Message = strings.TrimSpace(strings.Join(msg, "\n"))
			fs = append(fs, *cur)
		}
		cur, msg = nil, nil
	}
	for _, l := range lines[start:] {
		if m := testAtRe.FindStringSubmatch(l); m != nil {
			flush()
			file := normPath(m[1])
			if r := relTo(file, repoRoot); r != "" {
				file = r
			}
			n, _ := strconv.Atoi(m[2])
			cur = &Failure{File: file, Line: n}
			continue
		}
		if cur == nil {
			continue
		}
		if cur.Name == "" {
			if m := nameRe.FindStringSubmatch(l); m != nil {
				cur.Name = m[1]
				continue
			}
		}
		if m := frameRe.FindStringSubmatch(l); m != nil {
			if u, err := url.Parse(m[1]); err == nil {
				p := strings.TrimPrefix(u.Path, "/")
				if !strings.Contains(p, ":") { // non-Windows absolute path
					p = "/" + p
				}
				if r := relTo(p, repoRoot); r != "" && !strings.Contains(r, "node_modules/") {
					n, _ := strconv.Atoi(m[2])
					cur.Frames = append(cur.Frames, Frame{File: r, Line: n})
				}
			}
			continue
		}
		if atLineRe.MatchString(l) {
			continue // node internals
		}
		msg = append(msg, strings.TrimPrefix(l, "  "))
	}
	flush()
	return fs
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ParseDiff splits unified diff text into hunks keyed by new file path.
func ParseDiff(diff string) []Hunk {
	var hs []Hunk
	var file string
	var cur *Hunk
	var body []string
	flush := func() {
		if cur != nil {
			cur.Text = strings.Join(body, "\n")
			hs = append(hs, *cur)
		}
		cur, body = nil, nil
	}
	for _, l := range strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			file = ""
		case strings.HasPrefix(l, "+++ "):
			p := strings.TrimPrefix(l, "+++ ")
			if p == "/dev/null" {
				file = ""
			} else {
				file = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "index "):
		default:
			if m := hunkRe.FindStringSubmatch(l); m != nil {
				flush()
				if file == "" {
					continue
				}
				s, _ := strconv.Atoi(m[1])
				c := 1
				if m[2] != "" {
					c, _ = strconv.Atoi(m[2])
				}
				cur = &Hunk{File: file, NewStart: s, NewCount: c}
				body = []string{l}
				continue
			}
			if cur != nil {
				body = append(body, l)
			}
		}
	}
	flush()
	return hs
}

var secretRes = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]{8,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd)\s*[:=]\s*["']?)[^\s"']{4,}`),
}

// Redact replaces likely secrets with [REDACTED].
func Redact(s string) string {
	for i, re := range secretRes {
		if i == 1 || i == len(secretRes)-1 {
			s = re.ReplaceAllString(s, "${1}[REDACTED]")
		} else {
			s = re.ReplaceAllString(s, "[REDACTED]")
		}
	}
	return s
}

func isTestFile(p string) bool {
	return strings.Contains(p, ".test.") || strings.Contains(p, ".spec.") || strings.HasPrefix(p, "test/") || strings.Contains(p, "/test/")
}

// ReadFunc reads a repo-relative path from the current revision. truncated
// means src holds only the start of the file, cut after a complete line.
type ReadFunc func(path string) (src string, truncated bool, err error)

// notRead is the excerpt of a stack frame past the part of a file that was
// read. The 1 MiB is gitx's read cap (gitx.MaxOutputBytes); the text is part
// of the API contract.
const notRead = "(source larger than 1 MiB, not read)"

// CommitMessage is one commit for the commit evidence items.
type CommitMessage struct {
	SHA     string // the title uses its first 7 characters
	Message string
}

// Bounds of the commit items (docs/API_CONTRACT.md, v4): at most maxCommits
// commits, each excerpt cut at maxCommitExcerpt bytes before the mark, and at
// most maxCommitTotal bytes for all their excerpts, marks included.
const (
	maxCommits       = 10
	maxCommitExcerpt = 1000
	maxCommitTotal   = 4000
)

// cutExcerpt cuts s to at most n bytes and adds truncMark. It backs off to
// the start of the character the cut falls in; bytes that cannot belong to
// a character (more than UTFMax-1 continuation bytes) are cut where n says.
func cutExcerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > n-(utf8.UTFMax-1) && cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if !utf8.RuneStart(s[cut]) {
		cut = n
	}
	return s[:cut] + truncMark
}

// buildCommitItems turns commits, newest first, into commit items without
// ids, and returns them with the bytes of their excerpts. It stops at the
// first commit whose excerpt would bring the excerpts above limit, and
// leaves that commit and every older one out.
func buildCommitItems(commits []CommitMessage, limit int) ([]Item, int) {
	items := []Item{}
	total := 0
	for i, c := range commits {
		if i == maxCommits {
			break
		}
		msg := strings.TrimRightFunc(c.Message, unicode.IsSpace)
		subject, _, _ := strings.Cut(msg, "\n")
		subject = strings.TrimSuffix(subject, "\r")
		excerpt := cutExcerpt(Redact(msg), maxCommitExcerpt)
		if total+len(excerpt) > limit {
			break
		}
		total += len(excerpt)
		items = append(items, Item{Type: "commit", Title: capTitle(Redact("commit " + c.SHA[:min(7, len(c.SHA))] + ": " + subject)), Excerpt: excerpt})
	}
	return items, total
}

// Build assembles the bundle with no commit items: it is BuildWithCommits
// with no commits.
func Build(failures []Failure, hunks []Hunk, readFile ReadFunc, lim Limits) []Item {
	return BuildWithCommits(failures, hunks, nil, readFile, lim)
}

// BuildWithCommits assembles the bundle. Items are emitted in a fixed order:
// test failures, stack frames, diff hunks, source of changed non-test files,
// then one "commit" item per commit, newest first. The commit excerpts are
// set aside from MaxTotal before the other items are added, so a large diff
// cannot push them out; coming last, they leave the other ids unchanged.
func BuildWithCommits(failures []Failure, hunks []Hunk, commits []CommitMessage, readFile ReadFunc, lim Limits) []Item {
	cItems, reserved := buildCommitItems(commits, min(maxCommitTotal, lim.MaxTotal))
	lim.MaxTotal -= reserved
	var items []Item
	total := 0
	// add appends it; cut marks an excerpt that is already incomplete, so it
	// ends with truncMark even when it fits MaxExcerpt.
	add := func(it Item, cut bool) {
		// Title and File reach the model too. A path embedded in the title is
		// swapped for its segment-redacted form so both stay consistent and
		// usable; everything else in the title goes through Redact.
		const ph = "\x00"
		title := it.Title
		if it.File != "" {
			title = strings.ReplaceAll(title, it.File, ph)
		}
		it.File = RedactPath(it.File)
		it.Title = capTitle(strings.ReplaceAll(Redact(title), ph, it.File))
		it.Excerpt = Redact(it.Excerpt)
		if len(it.Excerpt) > lim.MaxExcerpt {
			it.Excerpt = it.Excerpt[:lim.MaxExcerpt] + truncMark
		} else if cut {
			it.Excerpt += truncMark
		}
		if total+len(it.Excerpt) > lim.MaxTotal {
			return
		}
		total += len(it.Excerpt)
		it.ID = "E" + itoa(len(items)+1)
		items = append(items, it)
	}

	for _, f := range failures {
		add(Item{Type: "test_failure", Title: "Failing test: " + f.Name, File: f.File,
			LineStart: f.Line, LineEnd: f.Line, Excerpt: f.Name + "\n" + f.Message}, false)
	}

	seen := map[string]bool{}
	for _, f := range failures {
		for _, fr := range f.Frames {
			key := fr.File + ":" + itoa(fr.Line)
			if seen[key] {
				continue
			}
			seen[key] = true
			it := Item{Type: "stack_frame", Title: key, File: fr.File}
			if src, truncated, err := readFile(fr.File); err == nil {
				lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
				if truncated {
					// src ends after a complete line: what follows the last
					// line break belongs to the line that was cut.
					lines = lines[:len(lines)-1]
				}
				switch {
				case fr.Line >= 1 && fr.Line <= len(lines):
					lo, hi := max(1, fr.Line-2), min(len(lines), fr.Line+2)
					it.LineStart, it.LineEnd = lo, hi
					it.Excerpt = strings.Join(lines[lo-1:hi], "\n")
				case truncated && fr.Line > len(lines):
					it.LineStart, it.LineEnd = fr.Line, fr.Line
					it.Excerpt = notRead
				}
			}
			if it.Excerpt == "" {
				it.LineStart, it.LineEnd = fr.Line, fr.Line
				it.Excerpt = "(source not readable)"
			}
			add(it, false)
		}
	}

	changed := []string{}
	isChanged := map[string]bool{}
	for _, h := range hunks {
		end := h.NewStart + h.NewCount - 1
		if h.NewCount == 0 {
			end = h.NewStart
		}
		add(Item{Type: "diff_hunk", Title: "Change in " + h.File, File: h.File,
			LineStart: h.NewStart, LineEnd: max(end, h.NewStart), Excerpt: h.Text}, false)
		if !isChanged[h.File] {
			isChanged[h.File] = true
			changed = append(changed, h.File)
		}
	}

	for _, p := range changed {
		if isTestFile(p) {
			continue
		}
		src, truncated, err := readFile(p)
		if err != nil {
			continue
		}
		src = strings.TrimRight(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
		it := Item{Type: "source", Title: "Current source of " + p, File: p, LineStart: 1, LineEnd: strings.Count(src, "\n") + 1, Excerpt: src}
		if truncated && src == "" {
			// Not one complete line fitted the read cap.
			it.LineStart, it.LineEnd, it.Excerpt, truncated = 0, 0, notRead, false
		}
		add(it, truncated)
	}

	for _, it := range cItems {
		it.ID = "E" + itoa(len(items)+1)
		items = append(items, it)
	}
	return items
}
