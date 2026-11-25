// Package txbudgetcensus finds test transactions that carry an unstated
// assumption about FDB's five-second MVCC window.
//
// THE CONDITION IT LOOKS FOR. An explicit SQL transaction pins one FDB read
// version, and that version dies five seconds after it opened in WALL CLOCK —
// whether the client was working or merely queued behind other load. The driver
// pre-empts at four with a typed 40001 carrying
// api.TransactionTimeLimitError. So a test that opens a transaction against a
// REAL FoundationDB and issues more than one statement on it is asserting,
// without saying so, that all of them finish inside four seconds. Under a
// full-parallel suite run against one containerised FDB that assertion is
// reachable: one such test was measured at 5.34s against 0.08s standalone.
//
// A transaction meeting all three of — real FDB, two or more statements, no
// time-limit handling — is EXPOSED. The fix is never to widen a timeout; it is
// to retry the WHOLE transaction on the typed marker, which is what
// api.IsTransactionTimeLimit and the retryTx helper in the sqldriver tests do.
//
// WHY AN AST WALK AND NOT A GREP. The two ways a naive tally goes wrong are
// both invisible to text matching, they point in OPPOSITE directions, and both
// were observed in this repo's own suite:
//
//   - OVER-COUNT: two SEQUENTIAL one-statement transactions in one test
//     function read as a single two-statement transaction. Counting per
//     function rather than per transaction reports an exposure that does not
//     exist, because each window closes before the next opens.
//   - UNDER-COUNT: several subtests each rebinding the same name `tx` collapse
//     into one tally, hiding which of them is actually exposed. This one is the
//     dangerous direction — it reports FEWER exposures than exist.
//
// Both are pinned as unit tests on synthetic sources, because they are
// properties of the INSTRUMENT and must not depend on the corpus continuing to
// contain an example.
package txbudgetcensus

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// stmtMethods are the database/sql calls that issue a statement on a
// transaction. Commit and Rollback are deliberately absent: they end the
// window rather than spending it, and counting them would manufacture a second
// statement for every single-statement transaction.
var stmtMethods = map[string]bool{
	"Exec": true, "ExecContext": true,
	"Query": true, "QueryContext": true,
	"QueryRow": true, "QueryRowContext": true,
	"Prepare": true, "PrepareContext": true,
}

// timeLimitMarkers are the ways a test says it has thought about the window.
// Matching the typed marker and the shared helpers, never SQLSTATE 40001: a
// genuine write conflict carries that same code and needs the opposite
// response, so a test keyed on the code alone has NOT handled this condition.
var timeLimitMarkers = []string{
	"TransactionTimeLimitError",
	"IsTransactionTimeLimit",
	"isTxBudgetExhausted",
	"retryTx",
	"runInTxWithRetry",
	"TimeLimitPreempted",
	"TimeLimitFDBTooOld",
}

// Site is one explicit transaction — one BeginTx/Begin call — with everything
// needed to classify it.
type Site struct {
	File   string
	Line   int
	Func   string
	TxVar  string
	Direct int // statements issued directly on the transaction
	Helper int // statements a helper issues, detected by the tx being passed in

	RealFDB bool
	Sim     bool
	Handles bool
}

// Statements is the lower bound on how many statements this transaction issues.
// A LOWER bound on purpose: a helper receiving the transaction may issue several
// and is counted once, which is how a converted test scored 2 while really
// issuing 3. Under-counting can only make the census miss an exposure, never
// invent one.
func (s Site) Statements() int { return s.Direct + s.Helper }

// Exposed reports whether this transaction carries the assumption.
func (s Site) Exposed() bool {
	return s.RealFDB && !s.Sim && s.Statements() >= 2 && !s.Handles
}

func (s Site) String() string {
	return fmt.Sprintf("%s:%d %s (tx=%s, statements>=%d)",
		filepath.Base(s.File), s.Line, s.Func, s.TxVar, s.Statements())
}

// ScanSource classifies every explicit transaction in one Go source file.
func ScanSource(filename, src string) ([]Site, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}

	// Real FDB versus SimFDB. SimFDB's logical clock makes the measurement
	// meaningless — its window does not advance with wall time — so a sim test
	// cannot carry this assumption however many statements it issues.
	realFDB := strings.Contains(src, "clusterFilePath") ||
		strings.Contains(src, "requireFDB") ||
		strings.Contains(src, "no Docker")
	sim := strings.Contains(src, "simfdb") ||
		strings.Contains(src, "SimFDB") ||
		strings.Contains(src, "NewSim")

	var sites []Site
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || !strings.HasPrefix(fd.Name.Name, "Test") {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, r := range as.Rhs {
				ce, ok := r.(*ast.CallExpr)
				if !ok {
					continue
				}
				se, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok || (se.Sel.Name != "BeginTx" && se.Sel.Name != "Begin") {
					continue
				}
				id, ok := as.Lhs[0].(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}
				// THE SCOPE IS THE INNERMOST BLOCK holding this BeginTx, and it
				// is what makes the count per-TRANSACTION rather than per-name.
				// A later rebinding of the same name in a SIBLING block — the
				// subtest case — is a different transaction with its own window
				// and must not pool its statements with this one.
				scope := innermostBlock(fd.Body, as.Pos())
				if scope == nil {
					continue
				}
				// HANDLING IS READ FROM THE TRANSACTION'S OWN LINE OF DESCENT —
				// its scope plus every ANCESTOR — with SIBLING closures removed.
				// Both halves are corrections of a wrong first cut, in opposite
				// directions:
				//
				//   - Reading the whole enclosing function fails OPEN: a function
				//     whose first subtest retries lends its marker to a second
				//     that does not, so the day someone adds a statement to the
				//     unretried subtest it becomes exposed invisibly.
				//   - Reading ONLY the transaction's own block fails CLOSED on the
				//     real shape of the fix: retryTx opens the transaction and
				//     hands it to a closure, so the marker is legitimately in an
				//     ancestor and the transaction is genuinely handled.
				//
				// Excluding sibling function literals is what separates the two.
				handles := false
				text := descentText(src, fset, fd, as.Pos())
				for _, m := range timeLimitMarkers {
					if strings.Contains(text, m) {
						handles = true
						break
					}
				}
				s := Site{
					File: filename, Line: fset.Position(as.Pos()).Line,
					Func: fd.Name.Name, TxVar: id.Name,
					RealFDB: realFDB, Sim: sim, Handles: handles,
				}
				s.Direct, s.Helper = countStatements(scope, id.Name, as.Pos())
				sites = append(sites, s)
			}
			return true
		})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Line < sites[j].Line })
	return sites, nil
}

// descentText returns the source of fd with every function literal that does
// NOT contain pos blanked out.
//
// What survives is exactly the code on pos's line of descent: its own block,
// every ancestor, and the ancestors' straight-line statements. What is removed
// is every sibling closure — the subtests that are separate transactions with
// separate windows, and whose handling says nothing about this one.
func descentText(src string, fset *token.FileSet, fd *ast.FuncDecl, pos token.Pos) string {
	b := []byte(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
	base := fset.Position(fd.Pos()).Offset
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		fl, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		if pos >= fl.Pos() && pos <= fl.End() {
			return true // an ancestor of pos: keep it, and descend
		}
		lo := fset.Position(fl.Pos()).Offset - base
		hi := fset.Position(fl.End()).Offset - base
		if lo < 0 || hi > len(b) || lo >= hi {
			return false
		}
		for i := lo; i < hi; i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
		return false // siblings inside a removed sibling are already gone
	})
	return string(b)
}

// innermostBlock returns the tightest *ast.BlockStmt containing pos.
func innermostBlock(root ast.Node, pos token.Pos) *ast.BlockStmt {
	var best *ast.BlockStmt
	ast.Inspect(root, func(n ast.Node) bool {
		b, ok := n.(*ast.BlockStmt)
		if !ok || pos < b.Pos() || pos > b.End() {
			return true
		}
		if best == nil || b.Pos() > best.Pos() {
			best = b
		}
		return true
	})
	return best
}

// countStatements tallies statements issued on name within scope, after the
// transaction was opened.
//
// A call inside a for/range counts as TWO, because a loop-carried statement
// executes more than once at runtime while occupying a single AST site. Without
// that, the most exposed transaction in this repo's suite — five INSERTs in a
// loop — scored 1 and was classified safe.
func countStatements(scope ast.Node, name string, after token.Pos) (direct, helper int) {
	var loops []ast.Node
	ast.Inspect(scope, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			loops = append(loops, n)
		}
		return true
	})
	inLoop := func(p token.Pos) bool {
		for _, l := range loops {
			if p >= l.Pos() && p <= l.End() {
				return true
			}
		}
		return false
	}

	ast.Inspect(scope, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok || ce.Pos() < after {
			return true
		}
		w := 1
		if inLoop(ce.Pos()) {
			w = 2
		}
		if se, ok := ce.Fun.(*ast.SelectorExpr); ok {
			if x, ok := se.X.(*ast.Ident); ok && x.Name == name && stmtMethods[se.Sel.Name] {
				direct += w
				return true
			}
		}
		// A helper receiving the transaction issues at least one statement on it.
		for _, a := range ce.Args {
			if id, ok := a.(*ast.Ident); ok && id.Name == name {
				helper += w
				break
			}
		}
		return true
	})
	return direct, helper
}

// ScanDirs classifies every explicit transaction in the *_test.go files of the
// given directories, and reports how many files it actually read.
//
// The file count is returned rather than discarded because it is the only thing
// that distinguishes "no exposed transactions" from "no files" — and under
// Bazel a test runs in a runfiles tree containing only its declared inputs, so
// an undeclared corpus produces a perfect, meaningless zero.
func ScanDirs(dirs ...string) (sites []Site, filesRead int, err error) {
	for _, dir := range dirs {
		entries, derr := os.ReadDir(dir)
		if derr != nil {
			return nil, 0, fmt.Errorf("read dir %s: %w", dir, derr)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil, 0, fmt.Errorf("read %s: %w", p, rerr)
			}
			filesRead++
			s, serr := ScanSource(p, string(b))
			if serr != nil {
				return nil, 0, serr
			}
			sites = append(sites, s...)
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites, filesRead, nil
}

// Exposed filters to the transactions carrying the assumption.
func Exposed(sites []Site) []Site {
	var out []Site
	for _, s := range sites {
		if s.Exposed() {
			out = append(out, s)
		}
	}
	return out
}

// Handled filters to transactions that DO handle the time limit. Watched for
// collapse: if this population empties while the corpus still holds
// transactions, the marker detection has broken and every converted test would
// silently re-enter the exposed set.
func Handled(sites []Site) []Site {
	var out []Site
	for _, s := range sites {
		if s.Handles {
			out = append(out, s)
		}
	}
	return out
}
