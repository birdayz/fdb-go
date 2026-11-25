package executor

import (
	"embed"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// executorSources embeds every .go file in the executor package directory at
// COMPILE time, so the single-threaded invariant test below can inspect the
// production sources regardless of the test's working directory. Reading from
// the filesystem at runtime (os.ReadDir) is unreliable under Bazel's sandbox,
// which does not stage source files next to the test binary; an embedded FS is
// resolved by the compiler and travels with the binary. (rules_go surfaces the
// embed via the go_test embedsrcs attribute, populated by gazelle.)
//
//go:embed *.go
var executorSources embed.FS

// TestExecutorPackageIsSingleThreaded pins the RFC-130 §2.2 concurrency
// invariant: the executor package launches ZERO goroutines. ChargeMemory (and
// the accounted boundedBuffer/boundedSet/TempTable buffers) deliberately use a
// plain int64 counter with no mutex/atomic, which is sound ONLY because a
// single statement is executed by one goroutine. If a future change introduces
// a parallel-union (or any `go ...`) this test fails, forcing a deliberate
// revisit (move ChargeMemory to atomic, audit the buffers) instead of a silent
// data race.
//
// It parses each non-test source and walks the AST for *ast.GoStmt (the `go f()`
// launch) — an AST walk, not a text grep, so a `go ` inside a comment or string
// literal can never false-positive.
func TestExecutorPackageIsSingleThreaded(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(executorSources, ".")
	if err != nil {
		t.Fatalf("read embedded executor sources: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	sawSource := false

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Test files may legitimately spawn goroutines (e.g. the TempTable
		// concurrency test) — the invariant is about PRODUCTION executor code.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		sawSource = true

		src, rerr := executorSources.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		file, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				pos := fset.Position(g.Pos())
				offenders = append(offenders, name+":"+pos.String())
			}
			return true
		})
	}

	if !sawSource {
		t.Fatal("found no non-test .go sources embedded for the executor package — test is misconfigured")
	}
	if len(offenders) != 0 {
		t.Fatalf("RFC-130 §2.2 single-threaded invariant VIOLATED: the executor package "+
			"launches goroutine(s) at %v.\nChargeMemory uses an unsynchronized counter that "+
			"is only safe single-threaded — move it to atomic and re-audit the accounted "+
			"buffers before introducing concurrency here.", offenders)
	}
}
