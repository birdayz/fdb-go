// Package norecover is a nogo analyzer that enforces the panic→error boundary
// discipline (RFC-134, audit P2). `recover()` is the mechanism by which a panic
// becomes a returned error at a public/goroutine boundary; an *undisciplined* new
// recover() is how a real error path gets silently swallowed (or rows dropped via a
// keep=false default arm). This analyzer makes adding one a conscious act: every
// recover() must live in a file on the allowlist below — the deliberate boundary
// layer classified in docs/panic-audit.md §2 — with a permitted per-file count.
//
// A recover() in any other file, or one MORE than a file's allowance, is a nogo
// build error. Removing a recover never reddens the build (the gate fires on *more*,
// never *fewer*), so legitimately deleting a boundary needs no allowlist edit; adding
// one does. Test files are exempt — tests recover freely.
package norecover

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// allowlist maps a repo-relative file path (matched as a suffix of the compiled
// file's path) to the number of recover() calls permitted in it. These are the
// deliberate panic→error boundary sites documented in docs/panic-audit.md §2. Keep
// the two in sync: this map is the executable allowlist; the doc is its rationale.
// Counts are the analyzer's own AST count of builtin recover() calls per file (NOT a
// grep of the text — a `recover` in a comment/string is not a call and is not counted),
// verified by building each package with an empty allowlist and reading the diagnostics.
// Exported so the docs/panic-audit.md §2 table can be gated against it (a sync test fails
// CI if the doc and this allowlist diverge — RFC-134 "keep panic-audit.md current").
var Allowlist = map[string]int{
	// client / fdb facade: Transact closure + goroutine + Must* backstops
	"pkg/fdbgo/client/panicbackstop.go": 1,
	"pkg/fdbgo/client/database.go":      2,
	"pkg/fdbgo/client/grv.go":           1,
	"pkg/fdbgo/transport/conn.go":       2,
	"pkg/fdbgo/fdb/panic.go":            1,
	"pkg/fdbgo/fdb/transaction.go":      1,
	// libfdbc cgo backend — analyzed only under `cgo && libfdbc`; 1 real recover (backend.go:354).
	"pkg/fdbgo/libfdbc/backend.go": 1,
	// SQL engine: parse / connection / executor boundaries
	"pkg/relational/core/parser/parser.go":               3,
	"pkg/relational/core/embedded/connection.go":         2,
	"pkg/relational/core/embedded/cascades_generator.go": 1,
	// record layer: tuple.Pack on user-derived comparison keys
	"pkg/recordlayer/merge_cursor.go": 1,
	// binding-tester harness binary (cgo-dependent build)
	"cmd/fdb-stacktester/directory_ops.go": 1,
}

// Analyzer is the nogo entry point, bound to the production allowlist above.
var Analyzer = newAnalyzer(Allowlist)

// newAnalyzer builds an analyzer over a given allowlist. Production uses the package
// allowlist; tests inject their own so the per-file-count logic can be exercised
// against testdata paths without mutating global state (keeping the tests parallel-safe).
func newAnalyzer(allow map[string]int) *analysis.Analyzer {
	r := &runner{allow: allow}
	return &analysis.Analyzer{
		Name: "norecover",
		Doc:  "flags recover() outside the documented panic→error boundary allowlist (RFC-134)",
		Run:  r.run,
	}
}

type runner struct{ allow map[string]int }

func (r *runner) run(pass *analysis.Pass) (any, error) {
	// Collect every builtin recover() position, grouped by file.
	perFile := map[string][]*ast.Ident{}
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "recover" {
				return true
			}
			// Confirm it resolves to the universe builtin, not a shadowing local
			// named "recover" (which would not be a panic→error boundary at all).
			if b, ok := pass.TypesInfo.Uses[ident].(*types.Builtin); !ok || b.Name() != "recover" {
				return true
			}
			fname := pass.Fset.Position(ident.Pos()).Filename
			if strings.HasSuffix(filepath.ToSlash(fname), "_test.go") {
				return true // tests recover freely
			}
			perFile[fname] = append(perFile[fname], ident)
			return true
		})
	}

	for fname, idents := range perFile {
		allowed := r.allowanceFor(fname)
		if len(idents) <= allowed {
			continue
		}
		// Report the calls beyond the allowance (source order), so an unchanged
		// allowlisted boundary stays quiet and only the surplus is flagged.
		sort.Slice(idents, func(i, j int) bool { return idents[i].Pos() < idents[j].Pos() })
		for _, ident := range idents[allowed:] {
			pass.Report(analysis.Diagnostic{
				Pos: ident.Pos(),
				End: ident.End(),
				Message: "recover() is not in the panic→error boundary allowlist (RFC-134). A new " +
					"recover is an undisciplined panic-swallow that can hide a real error path. If this " +
					"is a deliberate boundary, add the file (with its count) to pkg/linters/norecover " +
					"and docs/panic-audit.md §2; otherwise return an error instead of recovering.",
			})
		}
	}
	return nil, nil
}

// allowanceFor returns the permitted recover() count for a compiled file path by
// matching it against the repo-relative suffixes in the allowlist. Non-allowlisted
// files get 0.
func (r *runner) allowanceFor(fname string) int {
	slash := filepath.ToSlash(fname)
	for suffix, n := range r.allow {
		if strings.HasSuffix(slash, suffix) {
			return n
		}
	}
	return 0
}
