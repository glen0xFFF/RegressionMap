// Package fixture builds the DEMO FIXTURE: a tiny, original TypeScript
// repository whose baseline commit passes its tests and whose later commit
// introduces an off-by-one pagination bug. It exists only for demos and
// integration tests.
package fixture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Shas are the commit ids Build created.
type Shas struct {
	Baseline string
	Refactor string
	Bug      string
}

const packageJSON = `{
  "name": "pagination-demo",
  "private": true,
  "type": "module",
  "description": "RegressionMap demo fixture. Not a real product.",
  "scripts": { "test": "node --test" }
}
`

const paginationGood = `export interface Page<T> {
  items: T[];
  page: number;
  totalPages: number;
}

export function paginate<T>(items: T[], page: number, pageSize: number): Page<T> {
  const totalPages = Math.max(1, Math.ceil(items.length / pageSize));
  const start = (page - 1) * pageSize;
  const end = start + pageSize;
  return { items: items.slice(start, end), page, totalPages };
}
`

const paginationBug = `export interface Page<T> {
  items: T[];
  page: number;
  totalPages: number;
}

export function paginate<T>(items: T[], page: number, pageSize: number): Page<T> {
  const totalPages = Math.max(1, Math.ceil(items.length / pageSize));
  const start = (page - 1) * pageSize;
  // inclusive end index for readability
  const end = start + pageSize - 1;
  return { items: items.slice(start, end), page, totalPages };
}
`

const formatV1 = `export function formatTitle(title: string): string {
  return title.trim();
}
`

const formatV2 = `export function formatTitle(title: string): string {
  return title.trim().replace(/\s+/g, " ");
}
`

const testFile = `import { test } from "node:test";
import assert from "node:assert/strict";
import { paginate } from "../src/pagination.ts";
import { formatTitle } from "../src/format.ts";

const items = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10];

test("first page returns pageSize items", () => {
  assert.deepEqual(paginate(items, 1, 3).items, [1, 2, 3]);
});

test("middle page returns the next items", () => {
  assert.deepEqual(paginate(items, 2, 3).items, [4, 5, 6]);
});

test("last page returns the remainder", () => {
  assert.deepEqual(paginate(items, 4, 3).items, [10]);
});

test("totalPages rounds up", () => {
  assert.equal(paginate(items, 1, 3).totalPages, 4);
});

test("formatTitle collapses whitespace", () => {
  assert.equal(formatTitle("  a   b "), "a b");
});
`

const readme = "# pagination-demo\n\nDemo fixture for RegressionMap. Commit 3 introduces an intentional off-by-one bug.\n"

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.name=RegressionMap Fixture", "-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-10-27T10:00:00Z", "GIT_COMMITTER_DATE=2026-10-27T10:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func write(dir string, files map[string]string) error {
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func commit(dir, msg string, files map[string]string) (string, error) {
	if err := write(dir, files); err != nil {
		return "", err
	}
	if _, err := run(dir, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := run(dir, "commit", "-q", "-m", msg); err != nil {
		return "", err
	}
	return run(dir, "rev-parse", "HEAD")
}

// Build creates the fixture repository at dir, which must not exist yet.
func Build(dir string) (Shas, error) {
	var s Shas
	if _, err := os.Stat(dir); err == nil {
		return s, fmt.Errorf("%s already exists", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return s, err
	}
	if _, err := run(dir, "init", "-q", "-b", "main"); err != nil {
		return s, err
	}
	var err error
	if s.Baseline, err = commit(dir, "feat: paginate helper with tests", map[string]string{
		"package.json": packageJSON, "README.md": readme,
		"src/pagination.ts": paginationGood, "src/format.ts": formatV1,
		"test/pagination.test.ts": strings.Replace(testFile, `"  a   b "), "a b"`, `" a b "), "a b"`, 1),
	}); err != nil {
		return s, err
	}
	if s.Refactor, err = commit(dir, "feat: collapse whitespace in titles", map[string]string{
		"src/format.ts": formatV2, "test/pagination.test.ts": testFile,
	}); err != nil {
		return s, err
	}
	if s.Bug, err = commit(dir, "refactor: clarify page end index", map[string]string{
		"src/pagination.ts": paginationBug,
	}); err != nil {
		return s, err
	}
	return s, nil
}

// ApplyFix writes the known fix into the working tree (uncommitted), as a
// developer would after reading the hypotheses.
func ApplyFix(dir string) error {
	return write(dir, map[string]string{"src/pagination.ts": paginationGood})
}
