package docscheck

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/linters/norecover"
)

// RFC-134 (audit P2): the panic-boundary discipline has two halves. The recover ratchet is the
// `norecover` nogo analyzer (a new recover() outside the documented boundary allowlist fails the
// build). This test is the other half — the boundary *fuzz net*: each of the four audit-named input
// boundaries must keep a no-panic fuzz target that has a real seed corpus, so it actually exercises
// malformed input under `bazelisk test`/`go test` rather than being an empty no-op (Torvalds:
// name-presence alone is theater — assert the seed corpus, the concrete "it does something" signal).
// It checks three things per boundary: (a) the fuzz fn exists, (b) its body seeds a corpus (f.Add),
// and (c) the file is wired into a go_test target's srcs (codex #332: f.Add in the source does not
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
		// data dep (codex #332: f.Add in the source is not proof the fuzzer still runs in CI).
		dir := filepath.Dir(b.file)
		build := readDoc(t, root, filepath.Join(dir, "BUILD.bazel"))
		if !goTestWiresSrc(build, filepath.Base(b.file)) {
			t.Errorf("%s boundary: %s is not in a go_test srcs in %s/BUILD.bazel — the fuzzer is no longer "+
				"compiled/replayed under bazelisk test even though the file still exists (RFC-134)",
				b.boundary, filepath.Base(b.file), dir)
		}
	}
}

// goTestWiresSrc reports whether any go_test(...) target in a BUILD.bazel lists srcFile in its srcs.
// It scans only inside the balanced parens of each go_test( call, so a file that appears merely in a
// top-level exports_files([...]) (as these fuzz files do, to feed this test's data dep) does NOT
// count — only genuine go_test membership, which is what makes the fuzzer run under Bazel.
func goTestWiresSrc(build, srcFile string) bool {
	needle := `"` + srcFile + `"`
	for i := 0; ; {
		j := strings.Index(build[i:], "go_test(")
		if j < 0 {
			return false
		}
		start := i + j + len("go_test(")
		depth, k := 1, start
		for k < len(build) && depth > 0 {
			switch build[k] {
			case '(':
				depth++
			case ')':
				depth--
			}
			k++
		}
		if strings.Contains(build[start:k], needle) {
			return true
		}
		i = k
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
