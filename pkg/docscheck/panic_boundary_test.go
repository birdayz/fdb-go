package docscheck

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/pkg/linters/norecover"
)

// RFC-134 (audit P2): the panic-boundary discipline has two halves. The recover ratchet is the
// `norecover` nogo analyzer (a new recover() outside the documented boundary allowlist fails the
// build). This test is the other half — the boundary *fuzz net*: each of the four audit-named input
// boundaries must keep a no-panic fuzz target that has a real seed corpus, so it actually exercises
// malformed input under `bazelisk test`/`go test` rather than being an empty no-op (name-presence
// alone is theater — assert the seed corpus, the concrete "it does something" signal).
// It checks three things per boundary: (a) the fuzz fn exists, (b) its body seeds a corpus (f.Add),
// and (c) the file is wired into a go_test target's srcs (f.Add in the source does not
// prove the fuzzer still compiles/runs in CI — it could be present yet dropped from go_test). A
// boundary silently losing its fuzz (rename / delete / unwire) or its seeds turns this red. The four
// fuzz files AND their BUILD.bazel are data-staged into the test's runfiles.
var panicBoundaryFuzz = []struct {
	boundary string
	fuzzFn   string
	file     string // repo-relative path to the fuzz _test.go
}{
	{"SQL parser", "FuzzParse", "pkg/relational/core/parser/fuzz_test.go"},
	{"SQL→Cascades translation", "FuzzTranslateToCascades", "pkg/relational/core/query/cascades_translator_test.go"},
	{"wire reader decode", "FuzzNewReader", "pkg/fdbgo/wire/reader_fuzz_test.go"},
	{"tuple decode", "FuzzUnpack", "pkg/fdbgo/fdb/tuple/tuple_malformed_test.go"},
}

func TestPanicBoundary_FuzzNetsWired(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range panicBoundaryFuzz {
		src := readDoc(t, root, b.file)

		// (a) the no-panic fuzz function still exists, and (b) its body seeds a corpus (f.Add) so it
		// replays real malformed inputs — not an empty fuzz that proves nothing. Scope the f.Add to
		// THIS function's body (file may hold several fuzzers) by slicing from the declaration to the
		// next top-level func.
		fnRe := regexp.MustCompile(`func\s+` + regexp.QuoteMeta(b.fuzzFn) + `\(`)
		loc := fnRe.FindStringIndex(src)
		if loc == nil {
			t.Errorf("%s boundary: fuzz target %s is gone from %s — the no-panic net for this boundary "+
				"disappeared (RFC-134)", b.boundary, b.fuzzFn, b.file)
			continue
		}
		body := src[loc[1]:]
		if next := regexp.MustCompile(`\nfunc `).FindStringIndex(body); next != nil {
			body = body[:next[0]]
		}
		if !strings.Contains(body, "f.Add(") {
			t.Errorf("%s boundary: fuzz target %s in %s has no f.Add() seed corpus — an unseeded fuzz "+
				"does not replay malformed inputs under bazelisk test (RFC-134)", b.boundary, b.fuzzFn, b.file)
		}

		// (c) the file is wired into a go_test target's srcs in its dir's BUILD.bazel — i.e. actually
		// compiled and replayed under `bazelisk test`, not merely present + exported for this test's
		// data dep (f.Add in the source is not proof the fuzzer still runs in CI).
		dir := filepath.Dir(b.file)
		build := readDoc(t, root, filepath.Join(dir, "BUILD.bazel"))
		if !goTestWiresSrc(build, filepath.Base(b.file)) {
			t.Errorf("%s boundary: %s is not in a go_test srcs in %s/BUILD.bazel — the fuzzer is no longer "+
				"compiled/replayed under bazelisk test even though the file still exists (RFC-134)",
				b.boundary, filepath.Base(b.file), dir)
		}
	}
}

// srcsAttrRe finds the `srcs =` attribute. The \b prevents matching `embedsrcs`/`data`/`x_srcs`.
var srcsAttrRe = regexp.MustCompile(`\bsrcs\s*=`)

// goTestWiresSrc reports whether any go_test(...) target in a BUILD.bazel lists srcFile in its `srcs`
// attribute — i.e. the file is actually compiled + replayed under bazelisk test. It scans only inside
// the string-aware balanced parens of each go_test( call (so a top-level exports_files([...]) — as
// these fuzz files have, to feed this test's data dep — does NOT count), and within that, only the
// `srcs` attribute's VALUE: from `srcs =` to the comma that ends the expression at the top level,
// balancing []/(){} and skipping strings. That value may be any Starlark expression — a literal
// `[...]`, a concat `common + [...]`, or `select(...) + [...]` — so the check is correct whatever the
// formatting (same-line or wrapped), while the same filename in `data`/`embedsrcs`/a comment does NOT
// count (comments are stripped first). Edge cases pinned by TestGoTestWiresSrc.
func goTestWiresSrc(build, srcFile string) bool {
	build = stripStarlarkComments(build) // a commented-out `# "x_test.go"` is NOT a real srcs entry
	needle := `"` + srcFile + `"`
	for i := 0; ; {
		j := strings.Index(build[i:], "go_test(")
		if j < 0 {
			return false
		}
		call, callEnd := scanGroup(build, i+j+len("go_test("), '(', ')')
		i = callEnd
		if v, ok := srcsValue(call); ok && strings.Contains(v, needle) {
			return true
		}
	}
}

// srcsValue returns the `srcs` attribute's value within a go_test call body — from just after
// `srcs =` to the comma that ends the expression at the top level (depth 0), balancing []/(){} and
// skipping string literals. Neither a comma inside the list nor a later same-line attribute
// (`data =`, `embedsrcs =`) is included. ok is false when there is no srcs attribute.
func srcsValue(call string) (string, bool) {
	loc := srcsAttrRe.FindStringIndex(call)
	if loc == nil {
		return "", false
	}
	s := call[loc[1]:]
	depth, inStr := 0, false
	for k := 0; k < len(s); k++ {
		switch c := s[k]; {
		case inStr:
			if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '[', c == '(', c == '{':
			depth++
		case c == ']', c == ')', c == '}':
			depth--
		case c == ',' && depth == 0:
			return s[:k], true
		}
	}
	return s, true
}

// stripStarlarkComments removes `#`-to-end-of-line comments, but only when the `#` is outside a
// double-quoted string, so a commented-out srcs entry (or a commented-out go_test() block) cannot
// masquerade as live wiring. Filenames in BUILD srcs do not contain `#`, but the quote-awareness keeps
// it correct regardless.
func stripStarlarkComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inStr = !inStr
			b.WriteByte(c)
		case c == '#' && !inStr:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// scanGroup returns s[open:close) (exclusive of the closing delimiter) for the group whose opening
// delimiter was already consumed just before open (depth starts at 1), skipping string literals so a
// delimiter inside a string does not throw off the balance, plus the index just past the close.
func scanGroup(s string, open int, openCh, closeCh byte) (string, int) {
	depth, inStr := 1, false
	for k := open; k < len(s); k++ {
		switch c := s[k]; {
		case inStr:
			if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == openCh:
			depth++
		case c == closeCh:
			depth--
			if depth == 0 {
				return s[open:k], k + 1
			}
		}
	}
	return s[open:], len(s)
}

// TestGoTestWiresSrc pins the BUILD wiring parser against the edge cases codex #332 raised — only a
// real go_test `srcs` entry counts; data/embedsrcs/comments/exports_files/substrings do not; and any
// srcs expression (literal, concat, select) is accepted.
func TestGoTestWiresSrc(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		build string
		want  bool
	}{
		{"literal srcs", `go_test(name="t", srcs = ["a_test.go", "fuzz_test.go"], embed=[":x"])`, true},
		{"concat srcs", "go_test(\n  name=\"t\",\n  srcs = common + [\n    \"fuzz_test.go\",\n  ],\n  embed=[\":x\"],\n)", true},
		{"select srcs", "go_test(\n  srcs = select({\"//c\": [\"x.go\"]}) + [\"fuzz_test.go\"],\n  embed=[\":x\"],\n)", true},
		{"only in data", "go_test(\n  srcs = [\"a_test.go\"],\n  data = [\"fuzz_test.go\"],\n)", false},
		{"same-line data", `go_test(name="t", srcs=["a_test.go"], data=["fuzz_test.go"])`, false},
		{"trailing comma list", "go_test(\n  srcs = [\n    \"fuzz_test.go\",\n  ],\n  data = [\"x\"],\n)", true},
		{"paren in string name", `go_test(name="foo()", srcs=["fuzz_test.go"])`, true},
		{"only in embedsrcs", "go_test(\n  srcs = [\"a_test.go\"],\n  embedsrcs = [\"fuzz_test.go\"],\n)", false},
		{"commented out in srcs", "go_test(\n  srcs = [\n    \"a_test.go\",\n    # \"fuzz_test.go\",\n  ],\n)", false},
		{"only exports_files (outside go_test)", `exports_files(["fuzz_test.go"])` + "\n" + `go_test(srcs=["a_test.go"])`, false},
		{"substring not whole name", `go_test(srcs=["myfuzz_test.go"])`, false},
		{"absent", `go_test(srcs=["a_test.go", "b_test.go"])`, false},
	}
	for _, c := range cases {
		if got := goTestWiresSrc(c.build, "fuzz_test.go"); got != c.want {
			t.Errorf("%s: goTestWiresSrc = %v, want %v", c.name, got, c.want)
		}
	}
}

// docRowRe matches a §2 boundary-table row in docs/panic-audit.md: | `path.go` | N | role |
var docRowRe = regexp.MustCompile("(?m)^\\|\\s*`([^`]+\\.go)`\\s*\\|\\s*(\\d+)\\s*\\|")

// TestPanicBoundary_AllowlistMatchesDoc keeps docs/panic-audit.md §2 in lockstep with the executable
// norecover allowlist, so the doc can't silently rot (the exact failure the audit flagged — it had
// drifted to 158/11 while the tree was 155/22). The analyzer is the source of truth; this asserts the
// human doc still describes it, in both directions (no missing row, no stale row, matching counts).
func TestPanicBoundary_AllowlistMatchesDoc(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	doc := readDoc(t, root, "docs/panic-audit.md")

	docMap := map[string]int{}
	for _, m := range docRowRe.FindAllStringSubmatch(doc, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		docMap[m[1]] = n
	}
	if len(docMap) == 0 {
		t.Fatal("parsed no §2 boundary rows from docs/panic-audit.md — the table format changed; the " +
			"doc/allowlist sync guard is blind (RFC-134)")
	}

	for file, want := range norecover.Allowlist {
		switch got, ok := docMap[file]; {
		case !ok:
			t.Errorf("docs/panic-audit.md §2 is missing allowlisted boundary %q (count %d) — keep the doc "+
				"in sync with pkg/linters/norecover (RFC-134)", file, want)
		case got != want:
			t.Errorf("docs/panic-audit.md §2 lists %q with count %d but the norecover allowlist says %d", file, got, want)
		}
	}
	for file := range docMap {
		if _, ok := norecover.Allowlist[file]; !ok {
			t.Errorf("docs/panic-audit.md §2 lists %q which is not in the norecover allowlist — stale doc row (RFC-134)", file)
		}
	}
}
