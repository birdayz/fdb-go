package sqldriver_test

// The DISTINCT cost probe's Detector A, driven by REAL FDB and a REAL clock —
// the end of the wire neither unit test can reach.
//
// WHY THIS EXISTS. duec_regime_detector_test.go pins that
// duecMeasurementWindowLost recognises an api.TransactionTimeLimitError the TEST
// constructed. sim_sql_tx_budget_test.go pins that the DRIVER mints one, under a
// SIMULATED clock against SimFDB — which models no MVCC aging at all, so nothing
// there could have told us which of the two possible carriers a real store
// produces. Neither pins the join, and the join is exactly where the original
// defect lived: Detector A matched FDB's transaction_too_old (1007), the driver
// produced its own pre-emption instead, and the probe fatalled on a loaded CI box
// instead of withholding its wall-clock criteria. Both ends were green throughout.
//
// WHAT THIS DECIDES THAT NOTHING ELSE DOES: WHICH CARRIER WINS THE RACE. Two
// ceilings sit on the same transaction — the driver's 4 s read budget
// (paginatingRows.preflightTxBudget) and FDB's own 5 s MVCC wall. Only a real
// store has the second one. If the driver pre-empts first, the probe sees
// api.TransactionTimeLimitError and 1007 is very nearly unreachable here; if it
// did not, FDB's 1007 would arrive instead. Detector A has to recognise whichever
// one shows up, and the assertions below are written in that order: the union
// first, because that is what the probe actually needs, then the identity, so a
// silent swap between the two carriers is reported rather than absorbed.
//
// THE WINDOW IS AGED, NOT OUTWORKED, and that is deliberate. Spending the budget
// by doing four seconds of work would make this test a race against the box's
// speed — the same dependence that made the cost probe flaky, reproduced inside
// the test written to pin it. Idling past the budget is one-directional: no
// machine is fast enough to make six wall-clock seconds fit inside a four-second
// budget, so this cannot flake in the passing direction and cannot be made to
// pass by a quiet box. It costs the six seconds it asserts.
//
// A PAGED VARIANT WAS TRIED FIRST AND ABANDONED, and the record of why is worth
// more than the variant was.
//
// An OptExecutionScannedRowsLimit of 50, 10 and 1 over the same 20 000 rows all
// completed in 1.62-1.65 s inside a transaction — the same total across a 50x
// span of page count — so the intended pressure never arrived and the cost
// probe's "~140 ms per page" figure does not hold in this environment.
//
// I READ THAT AS "THE OPTION DOES NOT PAGE THIS PATH", WROTE IT AS AN ASSERTION,
// AND IT WAS REFUTED ON THE FIRST FULL SUITE RUN: beside the other FDB targets
// the same scan died on the budget at 4 s having returned 5 289 rows. Partial
// rows followed by the pre-emption is proof that it pages — an unpaged scan
// offers no mid-query point for the per-page preflight to fire at. Page-size
// invariance shows the per-page OVERHEAD is negligible; it says nothing about
// whether pages exist, and I inferred the second from the first.
//
// The negative result was therefore an artefact of a quiet box, and the only
// reason that is known is that it was pinned instead of written down. What is
// pinned now is the half that does not depend on the machine —
// TestFDB_DuecScannedRowsLimitDoesNotSilentlyTruncate — because whether 20 000
// paged rows fit inside 4 s on a given box is exactly the load-dependent timing
// question this whole mechanism exists to stop asserting.
//
// THE ORIGINAL CI FAILURE is committed beside this file at
// testdata/duec_ci_window_loss.txt, verbatim. It is the substrate for the claim
// that reverting the marker reproduces the OBSERVED defect rather than a cousin
// of it; without a copy in the tree that identity would rest on a commit message.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_DuecMeasurementWindowLoss_IsSeenByDetectorA(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_duecwin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_duecwin")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE duecwin "+
			"CREATE TABLE rows1 (id BIGINT, v STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_duecwin/s WITH TEMPLATE duecwin")
	dsn := fmt.Sprintf("fdbsql:///testdb_duecwin?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx,
		"INSERT INTO rows1 (id, v) VALUES (1, 'a'), (2, 'b')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// The FIRST read is what opens the MVCC window — the budget is anchored on
	// the read-version instant, not on BeginTx
	// (TestSimTxBudget_IdleBeforeFirstReadIsFree pins that distinction), so
	// without this the idle below would be free and prove nothing.
	var v string
	if err := tx.QueryRowContext(ctx, "SELECT v FROM rows1 WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("first in-tx read: %v", err)
	}

	// Past the driver's 4s budget AND past FDB's 5s wall, so the condition is
	// reached whichever ceiling is the one being enforced.
	const idle = 6 * time.Second
	time.Sleep(idle)

	start := time.Now()
	qerr := tx.QueryRowContext(ctx, "SELECT v FROM rows1 WHERE id = 2").Scan(&v)
	t.Logf("read %v after the window opened: err=%v (%T), returned in %v",
		idle, qerr, qerr, time.Since(start).Round(time.Millisecond))

	if qerr == nil {
		t.Fatalf("an in-transaction read %v past its read version SUCCEEDED. FDB binds "+
			"an explicit transaction's reads to a 5-second MVCC window and the driver "+
			"pre-empts at 4 (paginatingRows.preflightTxBudget); neither ceiling held. "+
			"The cost probe's Detector A then guards a condition that cannot occur, "+
			"which means it can never fire and the probe has no protection against a "+
			"loaded box at all.", idle)
	}

	// THE ASSERTION THE COST PROBE DEPENDS ON. Not "an error happened" — the
	// probe's detector must SEE it. A window loss the detector cannot recognise
	// is fatal to the probe: duecRunInTx fatals, and every load-INDEPENDENT arm
	// (plan shapes, row counts, NULL counts, the nine memory-budget rows — none
	// of which load can move by one byte) dies with the timing. That is the CI
	// red this reproduces.
	if !duecMeasurementWindowLost(qerr) {
		t.Fatalf("the in-transaction read lost its measurement window, but the cost "+
			"probe's Detector A does not recognise the error: %v (%T)\n"+
			"duecMeasurementWindowLost is what lets TestFDB_DistinctUniqueElisionCostProbe "+
			"WITHHOLD its wall-clock criteria instead of fatalling. Blind to this "+
			"carrier, the probe reds on a loaded box and takes its shape, row-count and "+
			"memory-budget arms down with the timing.", qerr, qerr)
	}

	// WHICH carrier — asserted rather than assumed, because the answer is the
	// whole reason Detector A was blind. On this driver the pre-emption wins: it
	// fires a full second before FDB's wall. If this ever becomes a raw 1007, the
	// pre-emption stopped working and preflightTxBudget is what to look at — the
	// probe still survives (Detector A takes both carriers), but the driver has
	// regressed from a clean typed stop to FDB's raw retryable error.
	var fdbErr fdb.Error
	sawRaw1007 := errors.As(qerr, &fdbErr) && fdbErr.Code == duecTransactionTooOld
	if !api.IsTransactionTimeLimit(qerr) {
		t.Fatalf("the window loss arrived as %v (%T), not as the driver's typed "+
			"read-budget pre-emption (raw FDB 1007 present: %v).\n"+
			"The driver is meant to pre-empt at 4s so callers get a typed, "+
			"non-retryable stop naming the remedy, rather than FDB's 1007 at 5s which "+
			"retry logic will spin on forever.", qerr, qerr, sawRaw1007)
	}
	var ttl *api.TransactionTimeLimitError
	if errors.As(qerr, &ttl); ttl.Elapsed < idle {
		t.Fatalf("the pre-emption reports elapsed=%v after a %v idle: the budget is "+
			"anchored somewhere later than the read-version instant, so a transaction "+
			"can outlive FDB's window while the driver still believes it is fresh",
			ttl.Elapsed, idle)
	}
}

// TestDuecCIGoldenStillMatchesTheDriversError ties the committed CI transcript to
// the error the driver produces TODAY, so the golden is load-bearing rather than
// decorative.
//
// The strongest available evidence that this fix addresses the OBSERVED defect
// rather than a cousin of it is that reverting the typed marker makes the driver
// emit the CI failure's string again. That identity is worth nothing if the
// transcript lives only in a commit message and the string can drift out from
// under it. This is the check that keeps the two in contact.
//
// It needs no FDB and no fixture: the pre-emption's wording is a pure function of
// its constructor. What it costs is that a DELIBERATE rewording of the remedy now
// reds here — which is the intent. Rewording the error is exactly the moment to
// re-confirm that the reworded error is still the same condition, and to refresh
// the transcript rather than let it rot.
func TestDuecCIGoldenStillMatchesTheDriversError(t *testing.T) {
	t.Parallel()
	const golden = "testdata/duec_ci_window_loss.txt"
	raw, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading the committed CI transcript %s: %v\n"+
			"It is the only substrate in the tree for the claim that reverting the "+
			"marker reproduces the failure CI actually saw. Deleting it does not make "+
			"that claim false, it makes it uncheckable.", golden, err)
	}
	transcript := string(raw)

	// The failing line from the transcript, as the driver renders it now. Built
	// from the constructor rather than copied, so the two sources cannot drift
	// apart silently.
	current := api.NewTransactionTimeLimitError(6*time.Second, 4*time.Second).Error()
	const remedy = "transaction read budget exhausted: the explicit transaction's reads " +
		"are bound to FDB's 5-second MVCC window, which opened at the transaction's " +
		"first read; commit sooner or decompose the work into smaller transactions"

	if !strings.Contains(current, remedy) {
		t.Fatalf("the driver's read-budget pre-emption no longer renders the message CI "+
			"recorded.\n  now:      %s\n  expected: %s\n"+
			"If this rewording is deliberate, update %s in the same commit and say why "+
			"the reworded error is still the same condition — the transcript is what "+
			"makes 'the fix addresses the observed defect' checkable.",
			current, remedy, golden)
	}
	if !strings.Contains(transcript, remedy) {
		t.Fatalf("the committed CI transcript no longer contains the failure message it "+
			"exists to preserve. %s was edited into something that is no longer the "+
			"observed defect.", golden)
	}
	// The transcript must also still name the SQLSTATE and the shape, because
	// "40001 on S1-full" is what identifies WHICH failure this was.
	for _, want := range []string{
		"40001",
		"SELECT DISTINCT email_plain FROM users1",
		"TestFDB_DistinctUniqueElisionCostProbe",
	} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("the committed CI transcript no longer mentions %q; it has stopped "+
				"identifying which failure it is a record of", want)
		}
	}
}

// TestFDB_DuecScannedRowsLimitDoesNotSilentlyTruncate pins the half of the paging
// question that does not depend on how fast the box is.
//
// WHAT IS NOT PINNED, and why. Whether a 20 000-row paged in-transaction scan
// FINISHES inside the 4 s budget is a pure timing question: quiet it takes
// ~1.6 s, beside the rest of the suite it died at 4 s with 5 289 rows. Asserting
// either outcome would put a load-dependent claim back into the tree, which is
// the defect this whole change removed. So neither direction is asserted.
//
// WHAT IS PINNED is the SAFETY property that holds in both regimes: a scan under
// a scanned-rows limit inside a transaction either returns every row or fails
// LOUDLY — it never quietly returns a prefix and reports success. That is
// load-independent because it is about what accompanies the rows, not about how
// many arrived, and it is the property the cost probe's row counts actually rest
// on. A silent truncation would make every paged row count in that probe agree
// with a plan that had stopped early, which is the failure a count-based
// discriminator cannot see by construction.
//
// Both observed outcomes are ACCEPTED here, and the assertion is on their
// pairing: complete-and-nil, or short-and-window-lost. The forbidden cell is
// short-and-nil.
func TestFDB_DuecScannedRowsLimitDoesNotSilentlyTruncate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_duecpage")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_duecpage")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE duecpage "+
			"CREATE TABLE rows2 (id BIGINT, v STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_duecpage/s WITH TEMPLATE duecpage")
	dsn := fmt.Sprintf("fdbsql:///testdb_duecpage?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	const total = 20000
	for base := 0; base < total; base += 500 {
		var sb strings.Builder
		sb.WriteString("INSERT INTO rows2 (id, v) VALUES ")
		for i := 0; i < 500; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "(%d, 'v%07d')", base+i, base+i)
		}
		if _, e := db.ExecContext(ctx, sb.String()); e != nil {
			t.Fatalf("insert at %d: %v", base, e)
		}
	}

	for _, perPage := range []int64{50, 1} {
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, perPage).Build())
		})
		tx, berr := conn.BeginTx(ctx, nil)
		if berr != nil {
			t.Fatalf("begin (perPage=%d): %v", perPage, berr)
		}
		seen := 0
		rows, qerr := tx.QueryContext(ctx, "SELECT DISTINCT v FROM rows2")
		if qerr == nil {
			var s sql.NullString
			for rows.Next() {
				if e := rows.Scan(&s); e != nil {
					qerr = e
					break
				}
				seen++
			}
			if qerr == nil {
				qerr = rows.Err()
			}
			rows.Close()
		}
		_ = tx.Rollback()
		t.Logf("in-tx scan, scanned-rows limit %d: %d/%d rows, windowLost=%v, err=%v",
			perPage, seen, total, duecMeasurementWindowLost(qerr), qerr)

		switch {
		case qerr == nil && seen != total:
			t.Fatalf("SILENT TRUNCATION: a scanned-rows limit of %d returned %d of %d "+
				"rows and reported NO error.\nA prefix presented as a complete result is "+
				"the one outcome no count-based check can catch: every row returned is "+
				"correct, so the cost probe's row-count arms would agree with a plan that "+
				"stopped early. Either the limit must carry the scan to completion or it "+
				"must fail loudly; reporting success for a partial answer is neither.",
				perPage, seen, total)
		case qerr != nil && !duecMeasurementWindowLost(qerr):
			t.Fatalf("a scanned-rows limit of %d failed for a reason that is NOT a lost "+
				"measurement window: %v (%T)\nThe budget pre-emption is the only failure "+
				"this shape is expected to be able to produce; anything else is a real "+
				"defect being absorbed by a test that was looking past it.",
				perPage, qerr, qerr)
		}
	}
}
