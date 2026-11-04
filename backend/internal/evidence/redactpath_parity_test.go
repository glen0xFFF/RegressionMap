package evidence

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

// parityFile is the golden fixture that web/src/redactParity.test.ts also
// reads: the web client copies secretRes, Redact and RedactPath (redactPath in
// web/src/diff.ts) to match redacted hunk paths to changed files, so both sides
// are tested against the same entries and a drift on either side fails CI. It
// lives under web/ because the web Docker stage copies only web/ and
// type-checks the tests, so no web file may import anything outside web/.
const parityFile = "../../../web/src/testdata/redact-path-parity.json"

// parityRegen is the command that rewrites every "want" from the current
// RedactPath. The "want" values are never written by hand.
const parityRegen = "cd backend && go test ./internal/evidence -run TestRedactPathParity -update"

var updateParity = flag.Bool("update", false, "rewrite the RedactPath parity fixture from the current RedactPath")

type parityCase struct {
	In   string `json:"in"`
	Want string `json:"want"`
}

func TestRedactPathParity(t *testing.T) {
	raw, err := os.ReadFile(parityFile)
	if err != nil {
		t.Fatalf("read %s: %v\nThe fixture holds the inputs: restore it from git, add any new entry as {\"in\": \"...\", \"want\": \"\"}, then regenerate the wanted values with:\n  %s", parityFile, err, parityRegen)
	}
	var cases []parityCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse %s: %v\nFix the JSON, then regenerate with:\n  %s", parityFile, err, parityRegen)
	}
	if len(cases) == 0 {
		t.Fatalf("%s has no entries, so it guards nothing; restore it from git and regenerate with:\n  %s", parityFile, parityRegen)
	}

	if *updateParity {
		for i := range cases {
			cases[i].Want = RedactPath(cases[i].In)
		}
		if err := os.WriteFile(parityFile, encodeParity(cases), 0o644); err != nil {
			t.Fatalf("write %s: %v", parityFile, err)
		}
		t.Logf("rewrote %s with %d entries", parityFile, len(cases))
		return
	}

	bad := 0
	for i, c := range cases {
		if got := RedactPath(c.In); got != c.Want {
			bad++
			t.Errorf("entry %d: RedactPath(%+q) = %+q, fixture wants %+q", i, c.In, got, c.Want)
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d entries of %s differ from RedactPath. If the server change is intended, regenerate the fixture with:\n  %s\nand make redactPath in web/src/diff.ts match it (cd web && npx vitest run).", bad, len(cases), parityFile, parityRegen)
	}

	// Invisible characters must stay visible in review: the file holds only
	// printable ASCII and line breaks (CRLF is allowed, as Windows checkouts use it).
	// i is the byte offset in the file as read, CRs included.
	text := string(raw)
	for i, ch := range text {
		if ch == '\n' || (ch == '\r' && strings.HasPrefix(text[i+1:], "\n")) {
			continue
		}
		if ch < 0x20 || ch > 0x7e {
			t.Fatalf("%s holds the raw character %+q at byte %d; every non-ASCII and control character must be written as a \\u escape. Regenerate with:\n  %s", parityFile, ch, i, parityRegen)
		}
	}
}

// TestEncodeParityRoundTrips checks the writer -update uses: the output is
// printable ASCII ending in a newline and parses back to the same entries.
func TestEncodeParityRoundTrips(t *testing.T) {
	in := []parityCase{
		{In: "a\"b\\c/\t\x0b\U000000A0\U00002028\U0001F600\x7f", Want: "plain"},
		{In: "", Want: "[REDACTED]"},
	}
	out := encodeParity(in)
	if !strings.HasSuffix(string(out), "]\n") {
		t.Errorf("output does not end with a newline: %q", out)
	}
	for i, b := range out {
		if b != '\n' && (b < 0x20 || b > 0x7e) {
			t.Fatalf("byte %d is %#x, not printable ASCII: %s", i, b, out)
		}
	}
	var back []parityCase
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, out)
	}
	if fmt.Sprint(back) != fmt.Sprint(in) {
		t.Errorf("round trip changed the entries:\n got %+q\nwant %+q", back, in)
	}
}

// encodeParity writes one entry per line. Every non-ASCII and control
// character is escaped as \uXXXX (a UTF-16 surrogate pair above U+FFFF).
func encodeParity(cases []parityCase) []byte {
	var b strings.Builder
	b.WriteString("[\n")
	for i, c := range cases {
		b.WriteString(`  {"in": `)
		writeASCIIString(&b, c.In)
		b.WriteString(`, "want": `)
		writeASCIIString(&b, c.Want)
		b.WriteString("}")
		if i < len(cases)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	return []byte(b.String())
}

func writeASCIIString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
		case r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(b, "\\u%04X\\u%04X", hi, lo)
		default:
			fmt.Fprintf(b, "\\u%04X", r)
		}
	}
	b.WriteByte('"')
}
