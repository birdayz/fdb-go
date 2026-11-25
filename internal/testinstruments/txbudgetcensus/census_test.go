package txbudgetcensus_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/internal/testinstruments/txbudgetcensus"
)

// ---------------------------------------------------------------------------
// Unit pins on the INSTRUMENT.
//
// These drive the census over synthetic sources rather than over the suite,
// because the two ways a tally goes wrong are properties of the instrument and
// must stay pinned even if the corpus stops containing an example. They point
// in opposite directions and both were observed in this repo's own suite.
// ---------------------------------------------------------------------------

// TestSequentialTransactionsAreNotPooled pins the OVER-COUNT direction.
//
// Two transactions, one statement each, in one function. A per-function tally
// reports one exposed two-statement transaction; there is none, because the
// first window is closed before the second opens. This shape is real —
// TestFDB_DMLCascades_ExplicitTxRollback — and a census without this property
// would demand a retry that protects nothing.
func TestSequentialTransactionsAreNotPooled(t *testing.T) {
	t.Parallel()
	const src = `package p
import "testing"
func TestThing(t *testing.T) {
	_ = clusterFilePath
	tx, _ := db.BeginTx(ctx, nil)
	tx.ExecContext(ctx, "INSERT INTO t VALUES (1)")
	tx.Rollback()
	tx2, _ := db.BeginTx(ctx, nil)
	tx2.ExecContext(ctx, "DELETE FROM t")
	tx2.Commit()
}`
	sites, err := txbudgetcensus.ScanSource("seq_test.go", src)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sites) != 2 {
		t.Fatalf("found %d transactions, want 2 — the census is not seeing each "+
			"BeginTx as its own window", len(sites))
	}
	for _, s := range sites {
		if s.Statements() != 1 {
			t.Errorf("%s counted %d statements, want 1: statements from a SIBLING "+
				"transaction are being pooled into this one. Two sequential "+
				"one-statement transactions cannot exceed the MVCC window — each "+
				"closes before the next opens — so reporting them as exposed "+
				"demands a retry that protects nothing.", s, s.Statements())
		}
		if s.Exposed() {
			t.Errorf("%s classified EXPOSED on one statement", s)
		}
	}
}

// TestSubtestsRebindingTxAreCountedApart pins the UNDER-COUNT direction, which
// is the dangerous one: it reports FEWER exposures than exist.
//
// Four subtests each bind their own `tx`. Only the one issuing a loop of
// statements is exposed. A tally keyed on the NAME pools all four and cannot
// say which. This shape is real — TestFDB_TransactionProbe — and pooling hid
// that its five-INSERT loop was the only exposed transaction in the file.
func TestSubtestsRebindingTxAreCountedApart(t *testing.T) {
	t.Parallel()
	const src = `package p
import "testing"
func TestThing(t *testing.T) {
	_ = clusterFilePath
	t.Run("a", func(t *testing.T) {
		tx, _ := db.BeginTx(ctx, nil)
		tx.ExecContext(ctx, "INSERT INTO t VALUES (1)")
		tx.Rollback()
	})
	t.Run("b", func(t *testing.T) {
		tx, _ := db.BeginTx(ctx, nil)
		tx.ExecContext(ctx, "INSERT INTO t VALUES (2)")
		tx.Commit()
	})
	t.Run("c", func(t *testing.T) {
		tx, _ := db.BeginTx(ctx, nil)
		for i := 0; i < 5; i++ {
			tx.ExecContext(ctx, "INSERT INTO t VALUES (3)")
		}
		tx.Rollback()
	})
	t.Run("d", func(t *testing.T) {
		tx, _ := db.BeginTx(ctx, nil)
		tx.ExecContext(ctx, "INSERT INTO t VALUES (4)")
		tx.Rollback()
	})
}`
	sites, err := txbudgetcensus.ScanSource("sub_test.go", src)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sites) != 4 {
		t.Fatalf("found %d transactions, want 4 — four subtests rebinding `tx` are "+
			"collapsing into one tally, which is how an exposed transaction hides",
			len(sites))
	}
	exposed := txbudgetcensus.Exposed(sites)
	if len(exposed) != 1 {
		t.Fatalf("classified %d exposed, want exactly 1 (the loop): %v.\n"+
			"Pooling four subtests under one name makes the count meaningless in "+
			"BOTH directions — it invents statements the third subtest never issued "+
			"and hides which transaction actually carries the assumption.",
			len(exposed), exposed)
	}
}

// TestLoopCarriedStatementsCountMoreThanOnce pins the sub-property that made
// the under-count possible: a single AST call site inside a loop runs many
// times. Without it the five-INSERT transaction above scores 1 and reads safe.
func TestLoopCarriedStatementsCountMoreThanOnce(t *testing.T) {
	t.Parallel()
	const src = `package p
import "testing"
func TestThing(t *testing.T) {
	_ = clusterFilePath
	tx, _ := db.BeginTx(ctx, nil)
	for i := 0; i < 5; i++ {
		tx.ExecContext(ctx, "INSERT INTO t VALUES (1)")
	}
	tx.Rollback()
}`
	sites, err := txbudgetcensus.ScanSource("loop_test.go", src)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("found %d transactions, want 1", len(sites))
	}
	if got := sites[0].Statements(); got < 2 {
		t.Fatalf("a loop of statements counted as %d, want >=2: one AST site inside "+
			"a for-loop executes many times at runtime, and counting it once "+
			"classifies the most exposed shape in the suite as safe", got)
	}
	if !sites[0].Exposed() {
		t.Fatalf("a real-FDB transaction issuing a loop of statements with no " +
			"time-limit handling is not classified exposed")
	}
}

// TestHandlingIsNotBorrowedFromASibling pins the fail-OPEN this census had
// until its populations were measured, and which cost it five spurious
// "handled" classifications.
//
// Subtest "a" retries; subtest "b" does not and issues two statements. Reading
// the marker from the enclosing FUNCTION lends a's retry to b, so b reads
// handled and its exposure is invisible. Attribution follows the line of
// descent instead, with sibling closures removed.
func TestHandlingIsNotBorrowedFromASibling(t *testing.T) {
	t.Parallel()
	const src = `package p
import "testing"
func TestThing(t *testing.T) {
	_ = clusterFilePath
	t.Run("a", func(t *testing.T) {
		retryTx(t, db, opts, func(x txAttempt) error { return nil })
	})
	t.Run("b", func(t *testing.T) {
		tx, _ := db.BeginTx(ctx, nil)
		tx.ExecContext(ctx, "A")
		tx.QueryContext(ctx, "B")
	})
}`
	sites, err := txbudgetcensus.ScanSource("sib_test.go", src)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("found %d transactions, want 1", len(sites))
	}
	if !sites[0].Exposed() {
		t.Fatalf("%s reads HANDLED, but its own subtest has no retry — it borrowed "+
			"the marker from a SIBLING subtest. That is the direction this guard "+
			"fails open in: the exposed transaction is classified safe and never "+
			"reported.", sites[0])
	}
}

// TestHandlingIsInheritedFromAnAncestor pins the opposite direction, which a
// too-tight fix introduces: retryTx opens the transaction and hands it to a
// closure, so the marker is legitimately in an ANCESTOR. Refusing to look up
// the chain would report every correctly-retried transaction as exposed and
// make the guard unusable.
func TestHandlingIsInheritedFromAnAncestor(t *testing.T) {
	t.Parallel()
	const src = `package p
import "testing"
func TestThing(t *testing.T) {
	_ = clusterFilePath
	retryTx(t, db, opts, func(a txAttempt) error {
		tx, _ := db.BeginTx(ctx, nil)
		tx.ExecContext(ctx, "A")
		tx.QueryContext(ctx, "B")
		return nil
	})
}`
	sites, err := txbudgetcensus.ScanSource("anc_test.go", src)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("found %d transactions, want 1", len(sites))
	}
	if sites[0].Exposed() {
		t.Fatalf("%s reads EXPOSED although an ancestor scope retries it. Attribution "+
			"that refuses to look up the chain reports every correctly-retried "+
			"transaction as a defect.", sites[0])
	}
}

// TestClassificationArms drives every arm of the exposure decision, because the
// corpus reaches only the arms it happens to contain. Each arm is one reason a
// transaction is NOT exposed, and each is a way the census could fail open.
func TestClassificationArms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, src string
		wantExpos bool
	}{
		{
			name: "real fdb, two statements, unhandled -> EXPOSED",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	_ = clusterFilePath
	tx, _ := db.BeginTx(ctx, nil)
	tx.ExecContext(ctx, "A")
	tx.QueryContext(ctx, "B")
}`,
			wantExpos: true,
		},
		{
			name: "SimFDB is not exposed: its logical clock does not advance with wall time",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	_ = clusterFilePath
	_ = simfdb.New()
	tx, _ := db.BeginTx(ctx, nil)
	tx.ExecContext(ctx, "A")
	tx.QueryContext(ctx, "B")
}`,
		},
		{
			name: "no real backend is not exposed",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	tx, _ := db.BeginTx(ctx, nil)
	tx.ExecContext(ctx, "A")
	tx.QueryContext(ctx, "B")
}`,
		},
		{
			name: "one statement cannot outlive its own window",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	_ = clusterFilePath
	tx, _ := db.BeginTx(ctx, nil)
	tx.ExecContext(ctx, "A")
	tx.Commit()
}`,
		},
		{
			name: "a retried transaction is handled",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	_ = clusterFilePath
	retryTx(t, db, opts, func(a txAttempt) error {
		tx, _ := db.BeginTx(ctx, nil)
		tx.ExecContext(ctx, "A")
		tx.QueryContext(ctx, "B")
		return nil
	})
}`,
		},
		{
			name: "a helper receiving the tx counts as a statement",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	_ = clusterFilePath
	tx, _ := db.BeginTx(ctx, nil)
	explainInTx(ctx, tx, "A")
	drainTx(ctx, tx, "B")
}`,
			wantExpos: true,
		},
		{
			name: "Commit and Rollback are not statements",
			src: `package p
import "testing"
func TestX(t *testing.T) {
	_ = clusterFilePath
	tx, _ := db.BeginTx(ctx, nil)
	tx.ExecContext(ctx, "A")
	tx.Commit()
	tx.Rollback()
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sites, err := txbudgetcensus.ScanSource("arm_test.go", tc.src)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(sites) != 1 {
				t.Fatalf("found %d transactions, want 1", len(sites))
			}
			if got := sites[0].Exposed(); got != tc.wantExpos {
				t.Fatalf("Exposed()=%v, want %v for %s", got, tc.wantExpos, sites[0])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The guard over the real corpus.
// ---------------------------------------------------------------------------

type logicalRoot struct {
	name       string
	candidates []string
}

// corpusDirRoots are the logical packages where an explicit SQL transaction
// against a real FoundationDB can live. Each resolves across legacy and future
// paths and is declared as Bazel `data` on this target; see the floors below for
// why that declaration is load-bearing.
var corpusDirRoots = []logicalRoot{
	{
		name:       "sqldriver",
		candidates: []string{"pkg/relational/sqldriver", "relational"},
	},
	{
		name:       "embedded",
		candidates: []string{"pkg/relational/core/embedded", "internal/relational/embedded"},
	},
}

// corpusTreeRoot is scanned recursively: every package of the end-to-end SQL
// suite (//pkg/relational/sqltest:corpus or //tests/sql:corpus).
var corpusTreeRoot = logicalRoot{
	name:       "sqltest",
	candidates: []string{"pkg/relational/sqltest", "tests/sql"},
}

// Floors that make a zero MEAN something. A census over an empty runfiles tree
// reports a perfect zero, and so does a census whose BeginTx detection broke —
// both indistinguishable from a clean suite unless the populations are checked.
// Measured at introduction: 665 files, 30 transactions, 4 of them handled, 0
// exposed. The floors sit well below those so ordinary churn does not trip
// them, and far enough above zero that a dead instrument cannot slip past.
const (
	minFilesRead = 300 // measured 665; a collapse means the corpus was not declared
	minTxSites   = 20  // measured 30; a collapse means BeginTx detection broke
	minHandled   = 3   // measured 4; a collapse means the markers stopped matching
)

func resolveLogicalRoots(root string, lr logicalRoot) ([]string, error) {
	var found []string
	for _, rel := range lr.candidates {
		p := filepath.Join(root, rel)
		if dirExists(p) {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("could not resolve %s root under %s: none of %v exists on disk", lr.name, root, lr.candidates)
	}
	return found, nil
}

func resolveCorpusDirs(root string) ([]string, error) {
	var dirs []string
	for _, lr := range corpusDirRoots {
		resolved, err := resolveLogicalRoots(root, lr)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, resolved...)
	}
	treeDirs, err := resolveLogicalRoots(root, corpusTreeRoot)
	if err != nil {
		return nil, err
	}
	for _, treeDir := range treeDirs {
		if err := filepath.WalkDir(treeDir, func(p string, e fs.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				dirs = append(dirs, p)
			}
			return err
		}); err != nil {
			return nil, fmt.Errorf("walking %s: %w", treeDir, err)
		}
	}
	return dirs, nil
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// Under Bazel the runfiles tree root is TEST_SRCDIR/TEST_WORKSPACE and holds
	// only declared inputs. Under `go test` the cwd is the package directory, so
	// walk up to the module root. Both are tried, and a miss is a LOUD failure
	// rather than an empty scan.
	if sd, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); sd != "" && ws != "" {
		if p := filepath.Join(sd, ws); func() bool {
			_, err := resolveLogicalRoots(p, corpusDirRoots[0])
			return err == nil
		}() {
			return p
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			if _, err := resolveLogicalRoots(dir, corpusDirRoots[0]); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate any of %v from TEST_SRCDIR=%q TEST_WORKSPACE=%q or cwd.\n"+
		"THE INSTRUMENT IS DEAD, NOT THE SUITE CLEAN. Under Bazel a test sees only "+
		"its declared inputs, so if the corpus filegroups were dropped from this "+
		"target's `data` the scan below would read zero files and report zero "+
		"exposed transactions — a perfect green over an empty set.",
		corpusDirRoots[0].candidates, os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"))
	return ""
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func TestResolveLogicalRoots(t *testing.T) {
	t.Parallel()

	t.Run("legacy layout", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		for _, rel := range []string{
			"pkg/relational/sqldriver",
			"pkg/relational/core/embedded",
			"pkg/relational/sqltest/core",
		} {
			if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		dirs, err := resolveCorpusDirs(root)
		if err != nil {
			t.Fatalf("resolveCorpusDirs(legacy): %v", err)
		}
		want := []string{
			filepath.Join(root, "pkg/relational/sqldriver"),
			filepath.Join(root, "pkg/relational/core/embedded"),
			filepath.Join(root, "pkg/relational/sqltest"),
			filepath.Join(root, "pkg/relational/sqltest/core"),
		}
		if len(dirs) != len(want) {
			t.Fatalf("dirs = %v, want %v", dirs, want)
		}
		for i := range want {
			if dirs[i] != want[i] {
				t.Errorf("dirs[%d] = %q, want %q", i, dirs[i], want[i])
			}
		}
	})

	t.Run("future layout", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		for _, rel := range []string{
			"relational",
			"internal/relational/embedded",
			"tests/sql/core",
		} {
			if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		dirs, err := resolveCorpusDirs(root)
		if err != nil {
			t.Fatalf("resolveCorpusDirs(future): %v", err)
		}
		want := []string{
			filepath.Join(root, "relational"),
			filepath.Join(root, "internal/relational/embedded"),
			filepath.Join(root, "tests/sql"),
			filepath.Join(root, "tests/sql/core"),
		}
		if len(dirs) != len(want) {
			t.Fatalf("dirs = %v, want %v", dirs, want)
		}
		for i := range want {
			if dirs[i] != want[i] {
				t.Errorf("dirs[%d] = %q, want %q", i, dirs[i], want[i])
			}
		}
	})

	t.Run("both layouts present returns union", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		for _, rel := range []string{
			"pkg/relational/sqldriver",
			"relational",
			"pkg/relational/core/embedded",
			"internal/relational/embedded",
			"pkg/relational/sqltest/core",
			"tests/sql/core",
		} {
			if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		dirs, err := resolveCorpusDirs(root)
		if err != nil {
			t.Fatalf("resolveCorpusDirs(both): %v", err)
		}
		want := []string{
			filepath.Join(root, "pkg/relational/sqldriver"),
			filepath.Join(root, "relational"),
			filepath.Join(root, "pkg/relational/core/embedded"),
			filepath.Join(root, "internal/relational/embedded"),
			filepath.Join(root, "pkg/relational/sqltest"),
			filepath.Join(root, "pkg/relational/sqltest/core"),
			filepath.Join(root, "tests/sql"),
			filepath.Join(root, "tests/sql/core"),
		}
		if len(dirs) != len(want) {
			t.Fatalf("dirs = %v, want %v", dirs, want)
		}
		for i := range want {
			if dirs[i] != want[i] {
				t.Errorf("dirs[%d] = %q, want %q", i, dirs[i], want[i])
			}
		}
	})

	t.Run("missing root fails loudly", func(t *testing.T) {
		t.Parallel()
		empty := t.TempDir()
		for _, lr := range append(append([]logicalRoot{}, corpusDirRoots...), corpusTreeRoot) {
			if got, err := resolveLogicalRoots(empty, lr); err == nil {
				t.Errorf("resolveLogicalRoots(%s) = %q, want error when no candidate exists", lr.name, got)
			}
		}
		if _, err := resolveCorpusDirs(empty); err == nil {
			t.Fatal("resolveCorpusDirs(empty) succeeded, want loud error")
		}

		// File instead of directory must also fail loudly.
		fileRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(fileRoot, "relational"), []byte("not a dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := resolveLogicalRoots(fileRoot, corpusDirRoots[0]); err == nil {
			t.Errorf("resolveLogicalRoots(file) = %q, want error when candidate is a file", got)
		}

		// Missing just one logical root (sqltest) must fail loudly rather than skipping.
		partial := t.TempDir()
		for _, rel := range []string{"relational", "internal/relational/embedded"} {
			if err := os.MkdirAll(filepath.Join(partial, rel), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := resolveCorpusDirs(partial); err == nil {
			t.Fatal("resolveCorpusDirs(partial) succeeded without sqltest root, want loud error")
		}
	})
}

// TestNoExposedTransactionsRemain is the guard.
//
// The expected exposed count is now ZERO, and that inversion is the whole point
// of the floors above: a zero used to mean "nobody has looked", and it now means
// "every exposed transaction retries". Because zero is the steady state, the
// danger has flipped from collapse to GROWTH — a non-zero here means a NEW
// transaction was written carrying the assumption, not that an old one
// regressed.
func TestNoExposedTransactionsRemain(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	dirs, err := resolveCorpusDirs(root)
	if err != nil {
		t.Fatal(err)
	}

	sites, filesRead, err := txbudgetcensus.ScanDirs(dirs...)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Non-vacuity, checked BEFORE the verdict so a dead instrument cannot pass.
	if filesRead < minFilesRead {
		t.Fatalf("the census read %d test files, want >=%d. THE INSTRUMENT IS DEAD, "+
			"NOT THE SUITE CLEAN: the verdict below is computed over whatever this "+
			"scan reached, so a shrunken corpus reports zero exposed transactions "+
			"for the same reason a clean one does. Check that the corpus filegroups "+
			"are still in this target's `data`.", filesRead, minFilesRead)
	}
	if len(sites) < minTxSites {
		t.Fatalf("the census found %d explicit transactions in %d files, want >=%d. "+
			"THE INSTRUMENT IS DEAD: BeginTx detection has broken, so every "+
			"transaction in the suite is now invisible to this guard and the zero "+
			"below is vacuous.", len(sites), filesRead, minTxSites)
	}
	handled := txbudgetcensus.Handled(sites)
	if len(handled) < minHandled {
		t.Fatalf("only %d transactions read as handling the time limit, want >=%d. "+
			"THE INSTRUMENT IS DEAD, or the retries were removed: handled "+
			"transactions are recognised by their typed marker and shared retry "+
			"helper, so a drop here means either the markers stopped matching or "+
			"the fix was reverted.", len(handled), minHandled)
	}
	// The marker must DISCRIMINATE. A marker list that matched everything would
	// mark every exposed transaction handled and the verdict below would pass
	// silently — the one way this guard fails OPEN rather than closed, and the
	// direction that ships bugs.
	if len(handled) == len(sites) {
		t.Fatalf("all %d transactions read as handling the time limit. The marker "+
			"match is no longer discriminating, so EVERY exposed transaction would "+
			"be classified handled and the zero below would be vacuous.", len(sites))
	}

	exposed := txbudgetcensus.Exposed(sites)
	if len(exposed) == 0 {
		return
	}
	msg := "the following explicit transactions run against a REAL FoundationDB, " +
		"issue two or more statements, and do not handle the whole-transaction " +
		"time limit:\n"
	for _, s := range exposed {
		msg += "  " + s.String() + "\n"
	}
	t.Fatalf("%s\n"+
		"Each pins one FDB read version across all its statements, and that version "+
		"dies five seconds after it opened in WALL CLOCK — under a loaded parallel "+
		"suite, not only in theory: one such test was measured at 5.34s against "+
		"0.08s standalone.\n\n"+
		"THE FIX IS NOT A LONGER TIMEOUT. Retry the WHOLE transaction on the typed "+
		"marker, so the work is re-established inside a transaction with a FRESH "+
		"read version — see retryTx in pkg/relational/sqldriver. Match "+
		"api.IsTransactionTimeLimit, never SQLSTATE 40001: a genuine write conflict "+
		"carries the same code and needs the opposite response.", msg)
}
