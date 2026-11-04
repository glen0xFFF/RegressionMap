package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"regressionmap/internal/httpx"
)

// logBad holds the characters a JSON log line must never carry raw: C1
// controls (U+0080 to U+009F), format characters (Cf: U+202E, U+200B,
// U+FEFF and the tag character U+E0041) and U+2028/U+2029.
const logBad = "a\U00000080b\U00000085c\U0000009bd\U0000009fe\U0000202ef\U0000200bg\U0000feffh\U00002028i\U00002029j\U000E0041k"

// logBadEscaped is logBad as the JSON logger writes it: each of those
// characters as the six-character text \uXXXX in lowercase hex (an astral
// one as its UTF-16 surrogate pair).
const logBadEscaped = "a\\u0080b\\u0085c\\u009bd\\u009fe\\u202ef\\u200bg\\ufeffh\\u2028i\\u2029j\\udb40\\udc41k"

func assertNoRawLogBad(t *testing.T, raw string) {
	t.Helper()
	for _, r := range []rune{0x80, 0x85, 0x9b, 0x9f, 0x202e, 0x200b, 0xfeff, 0x2028, 0x2029, 0xe0041} {
		if strings.ContainsRune(raw, r) {
			t.Fatalf("JSON log carries raw %U:\n%+q", r, raw)
		}
	}
}

func decodeLogLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("log line is not JSON (%v): %q", err, line)
	}
	return rec
}

// With LOG_FORMAT=json, a string attribute, an error attribute and the
// message carry those characters as \uXXXX text, also inside a group and in
// attributes added with With. C0 controls keep the JSON handler's own
// escapes, and other text, including other non-ASCII letters and U+00A0,
// is unchanged.
func TestJSONLogEscapesC1FormatAndSeparatorCharacters(t *testing.T) {
	var buf bytes.Buffer
	lg, err := newLogger(&buf, "json")
	if err != nil {
		t.Fatal(err)
	}
	const plain = "bücher.example \U000000a0é"
	lg.Warn("message "+logBad,
		"str", logBad,
		"error", errors.New("failure "+logBad),
		"c0", "x\x1b[2Jy\nz\t",
		"plain", plain,
		"n", 3,
	)
	lg.WithGroup("g").With("pre", logBad).Info("grouped", "inner", logBad)
	raw := buf.String()
	assertNoRawLogBad(t, raw)
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d:\n%s", len(lines), raw)
	}
	rec := decodeLogLine(t, lines[0])
	for k, want := range map[string]any{
		"level": "WARN",
		"msg":   "message " + logBadEscaped,
		"str":   logBadEscaped,
		"error": "failure " + logBadEscaped,
		"c0":    "x\x1b[2Jy\nz\t",
		"plain": plain,
		"n":     float64(3),
	} {
		if rec[k] != want {
			t.Errorf("%s = %+q, want %+q", k, rec[k], want)
		}
	}
	if want := "\"c0\":\"x\\u001b[2Jy\\nz\\t\""; !strings.Contains(lines[0], want) {
		t.Errorf("C0 controls lost the JSON handler's escapes: %s", lines[0])
	}
	g, _ := decodeLogLine(t, lines[1])["g"].(map[string]any)
	if g["pre"] != logBadEscaped || g["inner"] != logBadEscaped {
		t.Errorf("group attributes %+q, want both %+q", g, logBadEscaped)
	}
}

// derefErr's Error reads its receiver, so a typed-nil *derefErr panics in
// Error() with a nil pointer dereference.
type derefErr struct{ text string }

func (e *derefErr) Error() string { return e.text }

// panicErr's Error always panics with the same value.
type panicErr struct{}

func (panicErr) Error() string { panic("panicErr: fixed panic value") }

// nilSafeErr's Error handles a nil receiver and returns text holding U+0085.
type nilSafeErr struct{}

func (e *nilSafeErr) Error() string {
	if e == nil {
		return "nil receiver a\U00000085b"
	}
	return "non-nil receiver"
}

// logErrorLine logs one record with err as its "error" attribute through lg,
// which writes to buf, and returns that line decoded, without its "time"
// key. A panic in the logging call fails the test.
func logErrorLine(t *testing.T, name string, lg *slog.Logger, buf *bytes.Buffer, err error) map[string]any {
	t.Helper()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s: logging the error panicked: %v", name, r)
			}
		}()
		lg.Error("failed", "error", err)
	}()
	rec := decodeLogLine(t, strings.TrimSuffix(buf.String(), "\n"))
	delete(rec, "time")
	return rec
}

// An error whose Error() panics, a typed-nil pointer whose Error() reads its
// receiver or a value whose Error() panics outright, is written with
// LOG_FORMAT=json exactly as the JSON handler writes it without
// escapeLogAttr ("<nil>", or "!PANIC: " and the panic value), and the
// logging call does not panic.
func TestJSONLogErrorWhoseErrorPanicsMatchesPlainHandler(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"typed-nil pointer", (*derefErr)(nil), "<nil>"},
		{"panicking Error", panicErr{}, "!PANIC: panicErr: fixed panic value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b, b2 bytes.Buffer
			lg, err := newLogger(&b, "json")
			if err != nil {
				t.Fatal(err)
			}
			got := logErrorLine(t, "newLogger json", lg, &b, tc.err)
			want := logErrorLine(t, "plain JSON handler", slog.New(slog.NewJSONHandler(&b2, nil)), &b2, tc.err)
			if want["error"] != tc.want {
				t.Fatalf("fixture: the plain JSON handler wrote error %+q, want %+q", want["error"], tc.want)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("newLogger json wrote %+q, the plain JSON handler %+q", got, want)
			}
		})
	}
}

// A typed-nil pointer whose Error() handles a nil receiver does not panic,
// so its text is escaped like that of any other error.
func TestJSONLogEscapesTextOfNilSafeError(t *testing.T) {
	var b bytes.Buffer
	lg, err := newLogger(&b, "json")
	if err != nil {
		t.Fatal(err)
	}
	rec := logErrorLine(t, "newLogger json", lg, &b, (*nilSafeErr)(nil))
	assertNoRawLogBad(t, b.String())
	if want := "nil receiver a\\u0085b"; rec["error"] != want {
		t.Fatalf("error = %+q, want %+q", rec["error"], want)
	}
}

// The access log writes the decoded request path through the same logger,
// so a request for a percent-encoded path cannot put those characters into
// it raw, with or without an API key.
func TestJSONAccessLogEscapesDecodedPath(t *testing.T) {
	var buf bytes.Buffer
	lg, err := newLogger(&buf, "json")
	if err != nil {
		t.Fatal(err)
	}
	h := httpx.AccessLog(lg)(http.NotFoundHandler())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/x%C2%85y%C2%9B2K%E2%80%AEz%E2%80%A8", nil))
	raw := buf.String()
	assertNoRawLogBad(t, raw)
	rec := decodeLogLine(t, strings.TrimSuffix(raw, "\n"))
	if want := "/api/x\\u0085y\\u009b2K\\u202ez\\u2028"; rec["msg"] != "http" || rec["path"] != want {
		t.Fatalf("access log record %+q, want path %+q", rec, want)
	}
}

// The text handler is unchanged: it quotes such values already.
func TestTextLogQuotesC1FormatAndSeparatorCharacters(t *testing.T) {
	var buf bytes.Buffer
	lg, err := newLogger(&buf, "text")
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("m", "str", "a\U00000085b\U0000202ec")
	if want := "str=\"a\\u0085b\\u202ec\""; !strings.Contains(buf.String(), want) {
		t.Fatalf("text log %+q, want it to contain %+q", buf.String(), want)
	}
}
