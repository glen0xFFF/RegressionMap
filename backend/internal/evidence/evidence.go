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

// Build assembles the bundle. readFile reads a repo-relative path from the
// current revision. Items are emitted in a fixed order: test failures, stack
// frames, diff hunks, then source of changed non-test files.
func Build(failures []Failure, hunks []Hunk, readFile func(string) (string, error), lim Limits) []Item {
	var items []Item
	total := 0
	add := func(it Item) {
		it.Excerpt = Redact(it.Excerpt)
		if len(it.Excerpt) > lim.MaxExcerpt {
			it.Excerpt = it.Excerpt[:lim.MaxExcerpt] + truncMark
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
			LineStart: f.Line, LineEnd: f.Line, Excerpt: f.Name + "\n" + f.Message})
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
			if src, err := readFile(fr.File); err == nil {
				lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
				if fr.Line >= 1 && fr.Line <= len(lines) {
					lo, hi := max(1, fr.Line-2), min(len(lines), fr.Line+2)
					it.LineStart, it.LineEnd = lo, hi
					it.Excerpt = strings.Join(lines[lo-1:hi], "\n")
				}
			}
			if it.Excerpt == "" {
				it.LineStart, it.LineEnd = fr.Line, fr.Line
				it.Excerpt = "(source not readable)"
			}
			add(it)
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
			LineStart: h.NewStart, LineEnd: max(end, h.NewStart), Excerpt: h.Text})
		if !isChanged[h.File] {
			isChanged[h.File] = true
			changed = append(changed, h.File)
		}
	}

	for _, p := range changed {
		if isTestFile(p) {
			continue
		}
		src, err := readFile(p)
		if err != nil {
			continue
		}
		src = strings.TrimRight(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
		n := strings.Count(src, "\n") + 1
		add(Item{Type: "source", Title: "Current source of " + p, File: p, LineStart: 1, LineEnd: n, Excerpt: src})
	}
	return items
}
