package sqldriver_test

// RFC-210's performance harness: the secondary-UNIQUE DISTINCT-elision routes,
// measured and ASSERTED rather than logged.
//
// Three routes discharge a DISTINCT over a secondary UNIQUE index. R1
// (metadata) is inert on the SQL surface, because the DDL rejects a NOT NULL
// scalar column. R2 fires when a NULL-rejecting conjunct covers every key
// column and removes the operator outright. R3 is the floor: the operator
// stays, but only rows carrying a NULL/NaN key component enter the seen-set.
//
// This file holds two tests, and the split is deliberate — they measure two
// different things and only their conjunction is the claim:
//
//   - TestFDB_DistinctUniqueElisionCostProbe measures COST through the real SQL
//     path, where the planner is the thing under test: only that path
//     establishes index states, so only that path can yield an R2/R3 plan.
//   - TestFDB_DistinctUniqueElisionRetention measures the seen-set's exact
//     CONTENT through the executor, by reading the hash-distinct's continuation.
//     That needs a plan OBJECT in hand, which the SQL path does not hand back, so
//     it plans through the metadata harness — but under the AFFIRMATIVE
//     all-readable index-state view, so the narrowing and its exempt slots are
//     the PLANNER's rather than the test's. Whether the live SQL path produces
//     that plan is the other test's assertion; whether the planner computes the
//     right SLOTS, and whether the executor then retains the right rows, is this
//     one's.
//
// The fixture carries EMAIL_PLAIN alongside EMAIL: identical values, identical
// key bytes, no unique index. `SELECT DISTINCT email_plain` is therefore the
// full-distinct control for `SELECT DISTINCT email` — same access path, same
// projection width, same dedup keys, differing only in whether a proof exists.
// Measured on master, where no route fires, the two run within 1.007x of each
// other, which is what makes the control a stand-in for pre-RFC behaviour on
// the same store instead of a cross-process comparison.
//
// EVERY MEASUREMENT AND EVERY PROOF-DEPENDENT ASSERTION RUNS INSIDE AN EXPLICIT
// TRANSACTION, and that is a consequence of the optimization's own gate rather
// than a harness preference. The secondary-UNIQUE proof is a statement about
// the store at ONE INSTANT, so it is licensed only where the whole result comes
// from one read version. In auto-commit each page takes a fresh one — a value
// can be deleted from one row and re-inserted on another between pages and be
// emitted twice — so the proof is withheld and the full operator comes back.
// Auto-commit therefore gets its own arms, asserting exactly what is true
// there: the rows are right, no stamp is rendered, and the operator survives.
// It is never timed, because with the proof withheld both sides of every pair
// are literally the same plan.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

const duecRows = 100000

// duecSmallRows is the population for the ROW-IDENTITY arm run inside an
// explicit transaction, and it is small on purpose.
//
// That arm's whole result has to be read under ONE read version — that is what
// licenses the elision, so it is not negotiable — and an explicit
// transaction's reads are bounded by the driver's 4s budget ahead of FDB's 5s
// MVCC window. At duecRows the read does not fit under `-race`: the race lane
// lost the window on all eight permitted attempts and then reported
// `40001: transaction read budget exhausted ... read version 4.15s old`. The
// retry was working; the measurement had no margin.
//
// Scale is NOT what that arm establishes. It compares the narrowed operator's
// rows against the full operator's, value for value, and asserts the NULL
// collapse — properties of the dedup, provable at any size that carries the
// same density. Scale is what the BUDGET arms establish, and they keep
// duecRows.
//
// So the fix is the population rather than the ceiling. Raising the retry count
// would buy attempts at a window that is already too small, and the 4s budget
// is a real FDB constraint rather than a knob — a probe that needed it moved
// would be measuring something it cannot measure.
const duecSmallRows = 2000

// duecReps is the pair count RFC-209 §7 settled on: a single pair on this
// harness drifts enough to fail a clean run, and every ratio below is the
// MEDIAN of the per-rep ratios rather than a ratio of medians.
//
// The pairing is per REP, not back-to-back. Each rep runs all nine shapes in a
// fixed order, so the two sides of a pair are separated by the other seven
// rather than adjacent — what divides out is a slow moment lasting a whole rep,
// which is the drift this harness actually shows. A sub-rep spike lands on one
// side only and survives into that rep's ratio; taking the MEDIAN across reps
// is what discards it.
//
// NINE rather than RFC-209's five, and the extra four are paid for by a
// measurement rather than by caution. R3's effect in this regime is ~12-14%,
// and the null hypothesis — the same plan on both sides, produced by disabling
// the narrowing in the executor — was measured as high as 0.942x on a loaded
// box. Five reps leave those two populations closer than the separation the
// criteria draw, so the extra samples buy the margin the bounds assert.
const duecReps = 9

// duecPageScanLimit forces the 100k query to span ~10 pages. Paging is now
// exercised only in AUTO-COMMIT, and only for CORRECTNESS: it is the regime the
// single-read-version gate withholds the proof from, so the rows it returns are
// produced by the full operator and must still be right.
//
// It is deliberately NOT the timing regime. Paged auto-commit runs the same
// plan on both sides of every pair (see the measurement section), and paged
// INSIDE a transaction is bounded by FDB's 5-second MVCC window long before it
// reaches this fixture's size.
const duecPageScanLimit = 10000

// duecRun is one measured execution: rows, wall clock, and the bytes this
// process cumulatively allocated during it (the seen-set churn shows up here).
type duecSample struct {
	rows  int
	dur   time.Duration
	alloc uint64
}

// duecMeasure accumulates duecReps samples per query.
type duecSeries struct {
	tag, query string
	rows       int
	nulls      int
	samples    []duecSample
}

func (s *duecSeries) medianDur() time.Duration {
	ds := make([]time.Duration, 0, len(s.samples))
	for _, x := range s.samples {
		ds = append(ds, x.dur)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func (s *duecSeries) medianAlloc() uint64 {
	as := make([]uint64, 0, len(s.samples))
	for _, x := range s.samples {
		as = append(as, x.alloc)
	}
	sort.Slice(as, func(i, j int) bool { return as[i] < as[j] })
	return as[len(as)/2]
}

// duecPerRepRatios pairs the two series REP BY REP and returns one ratio per
// rep. Both the asserted medians and the regime detector below read this, so
// they are computed over the same samples in the same pairing.
func duecPerRepRatios(num, den []duecSample, pick func(duecSample) float64) []float64 {
	n := len(num)
	if len(den) < n {
		n = len(den)
	}
	rs := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		d := pick(den[i])
		if d == 0 {
			continue
		}
		rs = append(rs, pick(num[i])/d)
	}
	return rs
}

// duecMedianRatio is the median of the PER-PAIR ratios, not the ratio of the
// medians. Only the former divides out a slow moment that hit one rep.
func duecMedianRatio(num, den *duecSeries, pick func(duecSample) float64) float64 {
	rs := duecPerRepRatios(num.samples, den.samples, pick)
	if len(rs) == 0 {
		return math.NaN()
	}
	sort.Float64s(rs)
	return rs[len(rs)/2]
}

func duecDurOf(s duecSample) float64   { return float64(s.dur) }
func duecAllocOf(s duecSample) float64 { return float64(s.alloc) }

// ---- can this run's clock carry a criterion at all? ---------------------
//
// The wall-clock criteria below compare two plans that differ only by how much
// work the dedup operator does, and R3's margin over the full operator is
// 9-18%. A criterion that fine is a statement about the MACHINE as much as
// about the code: it holds only while the box can resolve a 9-18% difference
// between two ~400 ms queries. When it cannot, the criterion does not become
// weak evidence — it becomes noise wearing an assertion's clothes, and it fails
// in BOTH directions (observed inverting to 1.035x on a loaded box, and
// observed passing at 0.992x on a tree containing no R3 at all).
//
// The precedent is duecRaceInstrumented, one file over: instrumentation
// invalidates the REGIME, not the property, so the wall-clock arms are withheld
// and everything non-temporal keeps asserting. LOAD is the same class. What
// follows is that same ruling with the trigger measured rather than declared by
// a build tag, because load — unlike -race — is not knowable from a constant.
//
// TWO detectors, because either alone is insufficient.
//
// DETECTOR A — PROOF OF INVALIDITY. If any timed run dies because its
// transaction outlived FDB's MVCC window, the measurement window was
// demonstrably not held: the query ran past the horizon on a fixture that
// completes in 215-554 ms when the box is healthy. That is not a slow sample, it
// is a truncated one, and no statistic over the surviving samples repairs it.
//
// THE WINDOW IS LOST IN TWO SPELLINGS, and taking only the obvious one is how
// this detector spent its first weeks unable to fire. FDB's own
// transaction_too_old (1007) is the spelling everyone reaches for, and inside an
// explicit transaction on this driver it is very nearly unreachable: the SQL
// layer PRE-EMPTS at four seconds (paginatingRows.preflightTxBudget, anchored on
// the read-version instant) precisely so FDB's five-second wall is never hit. So
// the carrier that actually arrives is the driver's own pre-emption, and a
// detector that knows only 1007 sees a healthy run and lets duecRunInTx fatal —
// taking every load-INDEPENDENT arm of this probe down with the timing.
//
// Both spellings are recognised TYPED. The pre-emption is matched on
// api.TransactionTimeLimitError, not on its SQLSTATE: 40001 is shared with a
// genuine read/write conflict, and a detector that withheld on any 40001 would
// read a real conflict bug as weather — the same trap the 1020/1031 near misses
// below are excluded for.
//
// It is deliberately NOT a retry. Re-running until the window holds converts a
// broken regime into a green one by resampling, which is the precise fudge this
// mechanism exists to refuse — a probe that retries until it passes asserts only
// that the box eventually had a good minute.
//
// DETECTOR B — GRADED LOAD, SELF-CALIBRATING. Detector A does not cover the
// failure that actually shows up most: a run where every query completes, no
// error is raised, and the criterion simply inverts. Reproduced under load at
// B/D′ = 1.019x and 1.035x against a 0.95 bound, with no 1007 anywhere.
//
// So the probe calibrates itself against a pair whose true ratio is 1.0 BY
// CONSTRUCTION. Rows C and A′ are the same access path — `Project(…,
// IndexScan(BY_EMAIL, [<>] COVERING))` — with NO OPERATOR ON EITHER SIDE; C is
// R2-elided and A′ never had a DISTINCT to elide. Nothing in RFC-210 can move
// their ratio. Whatever the harness reports for C/A′ that is not 1.0 is
// instrument error, measured on this box, in this run, in the same regime and
// the same rep pairing as the claim.
//
// The principle: A MACHINE THAT CANNOT MEASURE A KNOWN-IDENTICAL PAIR AS
// IDENTICAL CANNOT BE TRUSTED TO RESOLVE THE EFFECT. Its own error bar is the
// evidence, and it is evidence the run produces rather than evidence quoted from
// some other box.
//
// THE BOUND IS DERIVED FROM THE PROBE'S OWN RECORDED QUIET DATA, never from an
// absolute wall-clock constant — "each side must finish inside N ms" would rot
// the first time the hardware changed, and would red on a box that is merely
// slow rather than merely loaded. RFC-210 §2.1 records C/A′ across four
// independent quiet in-transaction runs as 0.88-1.02x, so 0.12 is the widest
// deviation from 1.0 this pair has ever shown on a machine whose measurements
// were believed. A run outside that envelope is outside every regime the
// asserted numbers were taken in.
//
// WHAT THIS DOES AND DOES NOT CERTIFY, stated plainly because the gap is easy to
// read past. C and A′ are the two SHORTEST shapes in the fixture (174-178 ms
// against B and D′'s 373-445 ms), so a fixed-size stall is a larger fraction of
// them and the null pair is a COARSER instrument than the claim it guards: its
// quiet envelope, ±0.12, is wider than the 0.05 margin the B/D′ bound draws from
// 1.0. Passing Detector B therefore does not prove the box resolved 5%. It
// proves the box was not in the state that produced the observed inversions
// (1.220x on the null pair, measured on the same run whose B/D′ inverted to
// 1.035x). That is a NECESSARY condition, honestly bounded — not a sufficient
// one dressed up as one.
const (
	// duecNullPairQuietDev is the widest deviation from 1.0 the null pair has
	// shown across RFC-210 §2.1's four recorded quiet in-transaction runs
	// (C/A′ = 0.88-1.02x). It is a RATIO, so it carries across hardware; the
	// absolute milliseconds behind it do not and are never consulted.
	duecNullPairQuietDev = 0.12

	// duecTransactionTooOld is FDB's 1007 — the SECOND of Detector A's two
	// carriers, and on this driver the rarer one. Kept rather than dropped
	// because the driver's pre-emption is a CLIENT-side ceiling: anything that
	// reads outside paginatingRows' preflight, or a future backend that reports
	// no read-version instant (the cgo escape hatch does not), still meets FDB's
	// wall directly.
	duecTransactionTooOld = 1007

	// duecWallClockEnv opts THIS INVOCATION into asserting the wall-clock
	// criteria. Unset — which is every CI lane and every plain `just test` — the
	// timing arms LOG and do not fail.
	//
	// THIS IS THE PROBE'S OWN DOCUMENTED POSITION, IMPLEMENTED. The comment above
	// the regime verdict has always said the timing arms are "a QUIET-MACHINE /
	// LOCAL instrument ... not a PR gate and must not be quoted as one" — and they
	// were asserted on every lane anyway. A local instrument used as a merge gate
	// is the defect; every downstream symptom follows from it. The regime
	// detectors narrowed the gap without closing it: they catch the states they
	// were derived for, and the run that motivated this got past all three because
	// its null pair landed at 0.995x while its per-rep ratios spanned 0.728-1.747.
	// A dispersion arm would have been the natural fourth detector and the measured
	// record REFUSES it (#641), so the remaining honest move is to stop asserting a
	// quiet-machine instrument on lanes that are not quiet.
	//
	// WHAT THIS DOES NOT WEAKEN, and the split is the whole point: everything
	// non-temporal still asserts everywhere, unconditionally — plan shapes, the
	// elision decisions, row counts, NULL counts, the nine statement-memory-budget
	// rows, and the allocation criteria. Those are counts rather than durations and
	// load does not move them; they are the PR gate for RFC-210 and this flag does
	// not touch them. The budgetArmsRan/rowArmsRan anti-silence tally below is what
	// keeps that structural claim checkable rather than merely intended.
	//
	// MEASURED BEFORE CHANGING ANYTHING, because "the assertion is noisy" is also
	// what a real regression sounds like. RE-DERIVED AT HEAD rather than inherited:
	// three runs on this tree at load average 30 gave B/D' 0.871x, 0.878x, 0.858x
	// (s1 0.825-0.882x, s50 0.905-0.916x), every one with the regime resolvable and
	// every one comfortably under the 0.95 bound. The figures this change was first
	// written against agree — 17 runs at 0c7c57bf1 measured 0.766-0.904x and 9 on a
	// later master 0.820-0.902x — so R3's margin is intact and the CI miss at 0.951x
	// was THE REGIME, NOT THE CODE. That is the fact #638 has been blocked on.
	//
	// Three runs is not seventeen, and it is stated as three.
	duecWallClockEnv = "DUEC_ASSERT_WALLCLOCK"
)

// duecRegimeVerdict reports whether this run's WALL CLOCK may carry a criterion,
// and — when it may not — the reason, which is logged rather than swallowed.
//
// It is a pure function of the run's own samples so that the detector itself can
// be driven with fabricated overload and shown to trip
// (duec_regime_detector_test.go). A detector nothing ever exercises is a
// detector nobody knows is dead.
func duecRegimeVerdict(
	raceInstrumented, sawWindowLost bool, nullNum, nullDen []duecSample,
) (ok bool, why string) {
	if raceInstrumented {
		return false, "the race detector taxes every memory access on BOTH sides of " +
			"every pair, which inflates the shared base and compresses R3's margin " +
			"toward 1.0 (measured 0.968x under -race against 0.82-0.91x without it)"
	}
	if sawWindowLost {
		return false, fmt.Sprintf(
			"a timed run LOST ITS MEASUREMENT WINDOW — either the driver's own "+
				"read-budget pre-emption (api.TransactionTimeLimitError, which fires at "+
				"4s) or FDB's transaction_too_old (%d) behind it. The query ran past the "+
				"MVCC horizon on a fixture that completes in 215-554 ms when the box is "+
				"healthy, so the window was demonstrably not held. That is a TRUNCATED "+
				"sample, not a slow one, and no statistic over the survivors repairs it. "+
				"This is never retried — resampling until the window holds would turn a "+
				"broken regime into a green one",
			duecTransactionTooOld)
	}
	rs := duecPerRepRatios(nullNum, nullDen, duecDurOf)
	if len(rs) == 0 {
		return false, "the null pair produced no usable per-rep ratios, so the run " +
			"offers no evidence that its clock resolves anything"
	}
	sort.Float64s(rs)
	med := rs[len(rs)/2]
	if dev := math.Abs(med - 1.0); dev > duecNullPairQuietDev {
		return false, fmt.Sprintf(
			"the NULL PAIR C/A′ — the same access path with no operator on either "+
				"side, whose true ratio is 1.0 by construction — measured %.3fx, a "+
				"deviation of %.3f against the %.2f this pair has ever shown quiet "+
				"(RFC-210 §2.1 records 0.88-1.02x over four runs). The box could not "+
				"measure two identical plans as identical, so it cannot resolve R3's "+
				"9-18%%. Per-rep ratios, sorted: %v",
			med, dev, duecNullPairQuietDev, duecRoundRatios(rs))
	}
	return true, ""
}

// duecUnmeasuredRatio returns the first ratio that is not a finite number, which
// is how "the regime was accepted but nothing was actually measured" looks.
//
// Extracted from the probe so it can be driven without FDB. The hazard it guards
// is silent by construction — every timing criterion is spelled `ratio > bound`
// and NaN > anything is FALSE in Go, so an unmeasured run passes all three and
// logs ASSERTED. Keeping the check inline would have made it the one arm of this
// mechanism that no test could reach, in a file whose whole subject is detectors
// nobody knows are dead.
//
// Iteration order over a map is randomised, so the NAME returned for a run with
// several bad ratios is arbitrary; the verdict is not, which is all the caller
// uses.
func duecUnmeasuredRatio(ratios map[string]float64) (name string, value float64, bad bool) {
	for n, v := range ratios {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return n, v, true
		}
	}
	return "", 0, false
}

// duecRoundRatios renders per-rep ratios at three digits, so a withholding log
// line shows the shape of the dispersion and not just its median.
func duecRoundRatios(rs []float64) []float64 {
	out := make([]float64, len(rs))
	for i, r := range rs {
		out[i] = math.Round(r*1000) / 1000
	}
	return out
}

// duecMeasurementWindowLost reports whether err says this run's transaction
// outlived FDB's MVCC window — in EITHER spelling. See Detector A above for why
// both are required and why the driver's pre-emption is the one that actually
// arrives.
//
// Typed throughout, never a string match on a rendered message. api.Error's own
// doc forbids parsing its wording, and both the 1007 path and the pre-emption
// path arrive wrapped: the relational layer turns 1007 into
// api.Error{Code: 40001} keeping the cause (connection.go's translateFDBCode),
// and the pre-emption is an api.Error{Code: 40001} whose cause is the marker.
// errors.As reaches through both.
//
// Both FDB carriers are checked for the same reason connection.go checks both:
// the pure-Go client surfaces a value-typed fdb.Error and the wire reader a
// pointer-typed *wire.FDBError, and a run that hit the other one would be read as
// healthy.
func duecMeasurementWindowLost(err error) bool {
	if err == nil {
		return false
	}
	// ONE question, asked of the product. api.IsTransactionTimeLimit is true for
	// BOTH producers because both attach the marker — the driver's pre-emption at
	// 4s and translateFDBCode's 1007 arm at 5s. This test deliberately does not
	// re-enumerate them: a list kept here is a list that drifts from the guards,
	// which is precisely how the 1007-only version of this function shipped while
	// the condition arrived as something else.
	if api.IsTransactionTimeLimit(err) {
		return true
	}
	// The belt for an error that never reached translateFDBError — a raw 1007
	// straight off the client, with no relational wrapping. The SQL path funnels
	// everything through that translator (its own comment records that the typed
	// lanes are exhaustive and there is no string fallback), so this arm should be
	// unreachable from here; it is kept because "should be unreachable" is what
	// the previous version of this detector believed about 1007 arriving at all.
	var valErr fdb.Error
	if errors.As(err, &valErr) && valErr.Code == duecTransactionTooOld {
		return true
	}
	var wireErr *wire.FDBError
	if errors.As(err, &wireErr) && wireErr.Code == duecTransactionTooOld {
		return true
	}
	return false
}

func TestFDB_DistinctUniqueElisionCostProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_duec")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_duec")
	// Three tables differing only in NULL density. EMAIL_PLAIN mirrors EMAIL
	// value for value, NULL for NULL, and carries no index.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE duec "+
			"CREATE TABLE users (id BIGINT, email STRING, email_plain STRING, payload STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email ON users (email) "+
			"CREATE TABLE users1 (id BIGINT, email STRING, email_plain STRING, payload STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email1 ON users1 (email) "+
			"CREATE TABLE users50 (id BIGINT, email STRING, email_plain STRING, payload STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email50 ON users50 (email) "+
			// The same three densities at duecSmallRows, for the arm that has to
			// read its whole result inside ONE read version. See duecSmallRows.
			"CREATE TABLE users_s (id BIGINT, email STRING, email_plain STRING, payload STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email_s ON users_s (email) "+
			"CREATE TABLE users1_s (id BIGINT, email STRING, email_plain STRING, payload STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email1_s ON users1_s (email) "+
			"CREATE TABLE users50_s (id BIGINT, email STRING, email_plain STRING, payload STRING, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX by_email50_s ON users50_s (email)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_duec/s WITH TEMPLATE duec")
	dsn := fmt.Sprintf("fdbsql:///testdb_duec?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(16)

	duecLoad(t, ctx, db, "users", 0)
	duecLoad(t, ctx, db, "users1", 100)
	duecLoad(t, ctx, db, "users50", 2)
	duecLoadN(t, ctx, db, "users_s", 0, duecSmallRows)
	duecLoadN(t, ctx, db, "users1_s", 100, duecSmallRows)
	duecLoadN(t, ctx, db, "users50_s", 2, duecSmallRows)

	// ---- plan shapes ---------------------------------------------------
	// Every measurement below is interpretable only against the plan it was
	// taken on, so the shapes are ASSERTED. Each failure message names the fact
	// of RFC-210 that changed.
	// EXPLAIN runs inside an EXPLICIT TRANSACTION throughout this probe.
	//
	// The secondary-UNIQUE proof is licensed only where the whole result comes
	// from ONE read version, and an explicit transaction is what provides it —
	// in auto-commit each page takes a fresh read version, so a value can move
	// between pages and be emitted twice, and the proof is withheld
	// (rule_implement_distinct_final.go, and the paging reproducer beside this
	// file). Every plan shape this probe reasons about is therefore a
	// single-read-version shape, and reading it in auto-commit would assert the
	// pre-fix behaviour.
	ex := func(q string) string { return duecExplainInTx(t, ctx, db, q) }
	const (
		qA  = "SELECT email FROM users"
		qA2 = "SELECT email FROM users WHERE email IS NOT NULL"
		qB  = "SELECT DISTINCT email FROM users"
		qC  = "SELECT DISTINCT email FROM users WHERE email IS NOT NULL"
		qD  = "SELECT DISTINCT email_plain FROM users"
	)
	explains := map[string]string{}
	for _, q := range []string{
		qA, qA2, qB, qC, qD,
		"SELECT DISTINCT email FROM users1", "SELECT DISTINCT email_plain FROM users1",
		"SELECT DISTINCT email FROM users50", "SELECT DISTINCT email_plain FROM users50",
		"SELECT DISTINCT email FROM users ORDER BY email",
		"SELECT email FROM users ORDER BY email",
		"SELECT DISTINCT email FROM users ORDER BY email LIMIT 10",
		"SELECT email FROM users ORDER BY email LIMIT 10",
		"SELECT DISTINCT email FROM users LIMIT 10",
		"SELECT email FROM users LIMIT 10",
	} {
		explains[q] = ex(q)
		t.Logf("EXPLAIN %-56s => %s", q, explains[q])
	}

	// Row A. The baseline the delivered-delta table compares against: a plain
	// base-record scan with no operator on top. If the access path moves, the
	// comparison measures something else.
	if strings.Contains(explains[qA], "Distinct(") ||
		!strings.Contains(explains[qA], "Scan(USERS)") ||
		strings.Contains(explains[qA], "IndexScan") {
		t.Fatalf("row A is no longer a plain base-record scan: %s", explains[qA])
	}
	// Row A'. The control row C is actually "the same plan modulo the DISTINCT":
	// adding the NULL-rejecting filter moves the access path to the covering
	// index REGARDLESS of the DISTINCT, so A is NOT that control and a C-vs-A
	// ratio partly measures the access path. Both bounds are asserted below;
	// this one is the one that isolates the operator.
	if strings.Contains(explains[qA2], "Distinct(") ||
		!strings.Contains(explains[qA2], "IndexScan(BY_EMAIL,") {
		t.Fatalf("row A' is no longer a covering index scan without a distinct: %s\n"+
			"It is the only control that isolates R2's elision from the access-path "+
			"change the filter causes on its own.", explains[qA2])
	}
	// Row B — R3. The operator SURVIVES and carries the narrowing stamp. Both
	// halves matter: a plan with no Distinct at all would mean R2 fired on an
	// unfiltered query (unsound over a nullable key), and a Distinct without the
	// stamp would mean R3 stopped firing and the query pays the full seen-set.
	if !strings.Contains(explains[qB], "Distinct(") {
		t.Fatalf("row B lost its distinct operator entirely: %s\n"+
			"R2 must NOT fire on an unfiltered query over a NULLABLE unique key: "+
			"two NULL emails are two index entries and one output row.", explains[qB])
	}
	if !strings.Contains(explains[qB], "narrowed-by:BY_EMAIL") {
		t.Fatalf("row B is no longer NARROWED by BY_EMAIL: %s\n"+
			"R3 (rule_implement_distinct_final.go, WithNarrowedDedup) stopped firing; "+
			"the bare SELECT DISTINCT is back to the full seen-set and every timing "+
			"and churn bound below is measuring the pre-RFC operator.", explains[qB])
	}
	// Row C — R2. No operator at all.
	if strings.Contains(explains[qC], "Distinct(") {
		t.Fatalf("row C still carries a physical distinct: %s\n"+
			"R2's full elision (a NULL-rejecting conjunct on every key column) "+
			"stopped firing.", explains[qC])
	}
	if !strings.Contains(explains[qC], "distinct-by:BY_EMAIL") {
		t.Fatalf("row C carries no elision proof stamp: %s\n"+
			"An elision without the stamp is an elision whose dependency on the "+
			"index's state was never recorded (RFC-210 §5.2).", explains[qC])
	}
	// Row D' — the full-distinct control. Same shape as B, no stamp.
	if !strings.Contains(explains[qD], "Distinct(") ||
		strings.Contains(explains[qD], "narrowed-by") ||
		!strings.Contains(explains[qD], "Scan(USERS)") ||
		strings.Contains(explains[qD], "IndexScan") {
		t.Fatalf("the full-distinct control is no longer an unstamped distinct over a "+
			"base scan: %s\nEMAIL_PLAIN must stay unindexed; if it acquires a proof "+
			"the control becomes a copy of row B and every R3 bound below is vacuous.",
			explains[qD])
	}
	// The sweep's two operators, at both densities.
	for _, tbl := range []string{"users1", "users50"} {
		r3, full := "SELECT DISTINCT email FROM "+tbl, "SELECT DISTINCT email_plain FROM "+tbl
		if !strings.Contains(explains[r3], "narrowed-by:BY_EMAIL") {
			t.Fatalf("sweep %s: the R3 side is not narrowed: %s", tbl, explains[r3])
		}
		if !strings.Contains(explains[full], "Distinct(") ||
			strings.Contains(explains[full], "narrowed-by") {
			t.Fatalf("sweep %s: the full side is not an unstamped distinct: %s", tbl, explains[full])
		}
	}

	// ---- AUTO-COMMIT: correctness and plan shape, never timing ----------
	// The regime the gate withholds the proof from, asserted directly. Two
	// halves, and both are load-bearing: no stamp may appear, AND the full
	// operator must come back. Withholding a proof that left the query with no
	// deduplication at all would be worse than drawing it.
	//
	// This is also why nothing below this line is TIMED in auto-commit. With
	// the proof withheld, `SELECT DISTINCT email` and `SELECT DISTINCT
	// email_plain` are the SAME PLAN over the same data, so their ratio is 1.0
	// plus whatever the box was doing — measured at 0.858x on one run and
	// 1.012x on another, with a median ALLOCATION ratio of exactly 1.000x,
	// which is the signature of one plan run twice.
	acconn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, int64(duecPageScanLimit)).Build())
	})
	for _, ac := range []struct{ tag, query string }{
		{"B (R3)", qB},
		{"C (R2)", qC},
		{"S1-R3", "SELECT DISTINCT email FROM users1"},
		{"S50-R3", "SELECT DISTINCT email FROM users50"},
		{"R2 half-NULL", "SELECT DISTINCT email FROM users50 WHERE email IS NOT NULL"},
	} {
		plan := explainPlan(t, ctx, db, ac.query)
		t.Logf("AUTO-COMMIT EXPLAIN %-13s %-52s => %s", ac.tag, ac.query, plan)
		if strings.Contains(plan, "narrowed-by:") || strings.Contains(plan, "distinct-by:") {
			t.Fatalf("%s drew a secondary-UNIQUE proof in AUTO-COMMIT: %s\n"+
				"Each page runs its own transaction at its own read version, so a value "+
				"can be deleted from one row and re-inserted on another between pages "+
				"and be emitted twice. The proof is licensed only under a single read "+
				"version.", ac.tag, plan)
		}
		if !strings.Contains(plan, "Distinct(") {
			t.Fatalf("%s has NO dedup operator in AUTO-COMMIT: %s\n"+
				"Withholding the proof must restore the full operator, not leave the "+
				"query undeduplicated.", ac.tag, plan)
		}
	}

	// ---- measurement: UNPAGED, INSIDE AN EXPLICIT TRANSACTION -----------
	// The regime and both of its exclusions are measured facts, not choices of
	// convenience.
	//
	// TRANSACTION, because it is the only regime in which the proof fires at
	// all — see the auto-commit assertions immediately above, which are what
	// make a timed auto-commit pair meaningless rather than merely noisy.
	//
	// UNPAGED, because a paged transactional scan is bounded by the same budget
	// with none of the headroom. The CONCLUSION recorded here has always been
	// right; its stated MECHANISM was wrong, and the correction is kept rather
	// than the sentence deleted, because the wrong mechanism is re-derivable and
	// someone will otherwise re-derive it.
	//
	// What it used to say: "a page costs ~140 ms of fixed overhead there, so ten
	// pages burn the window at roughly 25 000 rows." The FIGURE does not hold
	// here. An in-transaction scan of 20 000 rows under
	// OptExecutionScannedRowsLimit took 1.619 s, 1.630 s and 1.647 s at limits of
	// 50, 10 and 1 — the same total across a 50x span of page COUNT (400 to
	// 20 000 pages), so in that range the cost is dominated by per-ROW work and is
	// not a fixed charge per page.
	//
	// The OUTCOME holds exactly, and is now observed rather than estimated: run
	// beside the rest of the suite, that same 20 000-row paged scan died on the
	// budget at 4 s having returned 5 289 rows. Partial rows followed by the
	// pre-emption is itself the proof that it pages — an unpaged scan has no
	// mid-query point at which the per-page preflight could fire. So paging is
	// available, it is simply not worth having: 20 000 paged rows cost more than
	// 100 000 unpaged ones (160-413 ms), and both answer to the same 4 s ceiling.
	//
	// AN EARLIER REVISION OF THIS COMMENT CLAIMED THE OPTION DOES NOT PAGE THIS
	// PATH AT ALL. That was inferred from the page-size invariance above and it is
	// false: invariance shows the per-page OVERHEAD is negligible, which is not
	// the same as no pages. The claim was written as a test, and the test refuted
	// it on the first suite run. What survives as a pin is the load-INDEPENDENT
	// half — TestFDB_DuecScannedRowsLimitDoesNotSilentlyTruncate — because whether
	// a given box finishes 20 000 paged rows inside 4 s is precisely the
	// load-dependent timing question this whole mechanism exists to stop asserting.
	//
	// The paged transactional pair was separately measured at 0.906-1.045x even
	// at sizes it survives, so there is no win there to assert either way.
	//
	// UNPAGED IS SUBJECT TO THE SAME CEILING — it buys HEADROOM, not exemption,
	// and an earlier revision of this comment claimed "no such ceiling", which is
	// false and cost a CI red. Every shape here runs inside one transaction whose
	// reads are pre-empted at 4 s (txPageTimeLimit, anchored on the read-version
	// instant). Unpaged merely avoids the per-page overhead: the shapes measure
	// 160-413 ms on a healthy box, so the margin is roughly 10x and load spends
	// it. CI has spent it — `SELECT DISTINCT email_plain FROM users1` (S1-full,
	// 411 ms here) died on the pre-emption in a lane whose loads ran 7-10x slow.
	//
	// That is the regime failing, not the property, so it is Detector A's input
	// rather than a fatal: see duecMeasurementWindowLost. Shrinking the fixture
	// would buy margin and is REFUSED — the null pair's ±0.12 envelope and R3's
	// 0.82-0.91x effect are both recorded at 100 000 rows in RFC-210 §2.1, so a
	// smaller fixture would leave every bound below asserting against numbers
	// taken on a population it no longer runs.
	//
	// The former PAGED AUTO-COMMIT timings are gone rather than relaxed. A
	// bound over two runs of one plan is not a weak criterion, it is a
	// criterion about nothing, and leaving it in place at a looser number
	// would report R3 as measured on a regime where R3 does not exist.
	tconn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	series := map[string]*duecSeries{}
	order := []struct{ tag, query string }{
		{"A", qA},
		{"A'", qA2},
		{"B", qB},
		{"C", qC},
		{"D'", qD},
		{"S1-full", "SELECT DISTINCT email_plain FROM users1"},
		{"S1-R3", "SELECT DISTINCT email FROM users1"},
		{"S50-full", "SELECT DISTINCT email_plain FROM users50"},
		{"S50-R3", "SELECT DISTINCT email FROM users50"},
	}
	for _, o := range order {
		series[o.tag] = &duecSeries{tag: o.tag, query: o.query}
	}
	// sawWindowLost is Detector A's whole state: whether ANY timed run in ANY rep
	// lost its measurement window. It is sticky rather than per-shape because a
	// window that failed once failed during a period this run was also timing
	// everything else.
	sawWindowLost := false
	for rep := 0; rep < duecReps; rep++ {
		for _, o := range order {
			s := series[o.tag]
			sample, nulls, windowLost := duecRunInTx(t, ctx, tconn, o.query)
			if windowLost {
				sawWindowLost = true
				t.Logf("REGIME rep=%d %s: MEASUREMENT WINDOW LOST after %d rows in %v",
					rep, o.tag, sample.rows, sample.dur.Round(time.Millisecond))
			}
			s.samples = append(s.samples, sample)
			s.rows, s.nulls = sample.rows, nulls
		}
	}
	for _, o := range order {
		s := series[o.tag]
		t.Logf("INTX %-9s rows=%-7d nullRows=%-3d medDur=%-10v medAllocMiB=%-8.1f durs=%v",
			s.tag, s.rows, s.nulls, s.medianDur().Round(time.Millisecond),
			float64(s.medianAlloc())/(1<<20),
			func() []time.Duration {
				out := make([]time.Duration, 0, len(s.samples))
				for _, x := range s.samples {
					out = append(out, x.dur.Round(time.Millisecond))
				}
				return out
			}())
	}
	ratio := func(num, den string) (timeR, allocR float64) {
		timeR = duecMedianRatio(series[num], series[den], duecDurOf)
		allocR = duecMedianRatio(series[num], series[den], duecAllocOf)
		t.Logf("RATIO %-9s / %-9s medTime=%.3fx medAlloc=%.3fx", num, den, timeR, allocR)
		return timeR, allocR
	}
	// R2's pair and R2's decomposition. LOGGED, NEVER ASSERTED — see the
	// paragraph below, which records why this harness cannot measure R2 at all.
	//
	// A'/A is the load-bearing one and it carries NO OPERATOR ON EITHER SIDE: it
	// is `IS NOT NULL` moving the access path from the base scan onto the
	// covering index, with no DISTINCT anywhere. Whatever it measures is the
	// confound in C/A, and it is logged here so the claim that C/A is almost
	// entirely access path rests on a measurement this test produces rather
	// than on one taken elsewhere and quoted.
	ratio("A'", "A")
	ratio("C", "A")
	// C/A' is also the NULL PAIR the regime detector calibrates on — no operator
	// on either side, true ratio 1.0 by construction. See duecRegimeVerdict.
	nullT, _ := ratio("C", "A'")
	// R3's pair, and the decomposition that says the win is the OPERATOR rather
	// than anything else that differs between the two columns. A is the same
	// base scan with no operator on top, so D'/A is what the full operator
	// costs and B/A is what survives the narrowing.
	bvdT, _ := ratio("B", "D'")
	ratio("D'", "A")
	ratio("B", "A")
	s1T, _ := ratio("S1-R3", "S1-full")
	s50T, _ := ratio("S50-R3", "S50-full")

	// ALLOCATION IS LOGGED, NEVER ASSERTED, and the reason is a property of the
	// instrument rather than a tolerance question. duecRunPaged reads
	// runtime.MemStats.TotalAlloc, which is a PROCESS-GLOBAL counter, while this
	// test calls t.Parallel() inside a binary that runs dozens of other FDB
	// tests concurrently. Their allocations land inside whichever measurement
	// window happens to be open, so the number attributed to a query is that
	// query's churn plus an arbitrary share of the suite's.
	//
	// That is not noise that averages out. It is unequal across the two sides of
	// a pair, so it moves the RATIO: measured alone, C/A' is 1.000x; under the
	// full suite the same pair measured 1.156x against a 1.15x band, with both
	// sides inflated (+35% and +60%) by traffic neither of them caused. A
	// criterion whose value moves 16% for reasons outside the code under test
	// cannot separate 1.00x from master's 1.27x.
	//
	// The claim the churn bound was reaching for — that the elided plan retains
	// NO dedup keys, and that R3 retains only the exempt ones — is instead
	// asserted below against the statement memory budget, and in
	// TestFDB_DistinctUniqueElisionRetention as exact counts. Both are
	// deterministic: they are properties of what the operator stored, not of how
	// long anything took or of what else was running.

	// R2 IS NOT TIMED HERE, AND CANNOT BE. This is a property of the fixture,
	// not a tolerance that was relaxed, so the assertion is removed rather than
	// widened.
	//
	// Two independent reasons, either of which is sufficient:
	//
	//  1. THE CONTROL RUNS A DIFFERENT ACCESS PATH. EMAIL is indexed, so `email
	//     IS NOT NULL` is SARGable and the plan becomes
	//     `IndexScan(BY_EMAIL, [<>] COVERING)`. EMAIL_PLAIN is unindexed, so the
	//     same predicate stays a residual over a base scan. Measured with the
	//     DISTINCT stripped from BOTH sides, that pair already runs at
	//     0.46-0.50x — nearly the whole apparent win is the access path, and
	//     none of it is the elision.
	//  2. THE TWO OPERATORS ARE DIFFERENT OPERATORS. R2 removes a STREAMING
	//     distinct over index-ordered input, which holds no seen-set at all;
	//     the control times a HASH distinct over a base scan. The estimator is
	//     wrong in kind and no band over it means anything.
	//
	// No query on this branch produces `Distinct(Project(IndexScan(BY_EMAIL,
	// [<>] COVERING)))` — the plan R2 actually replaces — so R2's benefit is
	// not measurable in this harness by construction. Its discriminators are
	// the plan shape asserted above and the row counts asserted below, both of
	// which are exact.

	// R3's timing, in the regime where R3 exists. Ratios survive a concurrent
	// suite and not by luck: both sides of each pair are the SAME access path
	// over the same store, so shared load scales both and divides out, and the
	// reported figure is the median of 5 per-rep ratios rather than of 5
	// absolute times.
	//
	// "Strictly faster" cannot be spelled `< 1.0`. With R3 absent — on master,
	// or in auto-commit on this branch — B and D' are the SAME plan over the
	// same data, so their ratio is 1.0 plus noise and a `< 1.0` test is a coin
	// flip; it measured 0.992x on a tree containing no R3 at all, where it
	// would have passed. The bound is therefore a MARGIN drawn from the
	// measured effect size: unpaged in-transaction at 100k rows, R3 runs at
	// 0.82-0.91x of the full operator across four independent runs, and the
	// null hypothesis — the same measurement with the executor's exempt test
	// forced to admit every row, so the stamp survives and the narrowing
	// delivers nothing — measured 0.94-1.04x. Anything at or above 0.95x is not
	// R3 working, it is R3 missing.
	//
	// The DECOMPOSITION is what says the win is the OPERATOR rather than
	// anything else that distinguishes the two columns: against A, the same
	// base scan with no operator on top, the full distinct costs 1.13-1.24x
	// while the narrowed one costs 1.00-1.07x. R3 takes the operator's overhead
	// to roughly nothing, which is exactly what removing the retention should
	// do and is not something a difference in column position could produce.
	//
	// The bound is NOT tuned to the measurement. 0.95 sits clear of both the
	// effect and the null, which is what makes a failure here informative in
	// either direction.
	const r3Margin = 0.95
	// WALL-CLOCK criteria only, and only when the clock means something. The
	// two detectors are defined above duecRegimeVerdict; this is where their
	// verdict is spent.
	//
	// NOTE WHAT IS AND IS NOT WITHHELD, because the split is the entire ruling.
	// Withheld: the three strict-win TIMING bounds, and only those. Still
	// asserted, on every run, loaded or not: every plan-shape arm, the
	// auto-commit proof-withholding arms, all nine statement-memory-budget rows,
	// every row count and NULL count in both regimes, R2's half-NULL row count,
	// the ordered/LIMIT shape arms, and the executor-variant arms. Those are
	// COUNTS and SHAPES — properties of what the operator stored and what the
	// planner emitted — and no amount of load moves them by even one byte. A
	// loaded box loses the ability to say R3 is FASTER; it never loses the
	// ability to say R3 fired, retained what it should, and returned the right
	// rows. The budget rows in particular are the load-independent form of the
	// same claim, and they red on master.
	//
	// NO CI LANE GUARANTEES A QUIET MEASUREMENT, and that is checked rather than
	// hoped. `.github/workflows/ci.yml` sets `concurrency: group:
	// ci-${{ github.ref }}`, which cancels only SAME-REF runs, so two different
	// PRs never contend for cancellation — they contend for hardware. All four
	// of its jobs (`ci`, `race`, `wire-oracle`, `govulncheck`) pin to
	// `hetzner-fdb-vm`, a fleet of FIVE boxes serving one job each, so a single
	// PR can occupy four fifths of the fleet and a second PR overlaps it by
	// construction. And even a job alone on a box is not quiet: `.bazelrc` sets
	// `--local_test_jobs=4`, so this probe runs beside up to three other FDB
	// testcontainer targets inside its own invocation, on a 4-vCPU box. That
	// last one is the floor, and no workflow edit removes it.
	//
	// nightly-coverage.yml is NOT the quiet lane either — it is strictly
	// noisier. It runs `bazelisk coverage //...` (the whole suite, under
	// coverage instrumentation), then two race steps, then benchmarks, in ONE
	// job on that same fleet, and its own comments record that GitHub dispatches
	// it whenever a box frees up, hours off its cron hour, alongside whatever
	// else is queued. It guarantees coverage and a heartbeat; it guarantees
	// nothing about the clock.
	//
	// So the honest status of the timing arms is: a QUIET-MACHINE / LOCAL
	// instrument that still reds for a genuine R3 regression when the box can
	// resolve one, and abstains — loudly, in the log — when it cannot. They are
	// not a PR gate and must not be quoted as one. The PR gate for RFC-210 is
	// the shape-and-count set listed above, which is load-independent by
	// construction.
	timingResolvable, withheldWhy := duecRegimeVerdict(
		duecRaceInstrumented, sawWindowLost, series["C"].samples, series["A'"].samples)
	// The opt-in is checked SEPARATELY from the regime verdict, and BOTH still
	// run, so an opted-out lane still reports whether its box could have carried
	// the criteria. That keeps the log useful as the record it has always been —
	// silencing the detectors along with the assertions would have thrown away the
	// very data that settled the dispersion question in #641.
	wallClockAsserted := timingResolvable && duecAssertWallClock()
	if !duecAssertWallClock() {
		t.Logf("REGIME: wall-clock criteria NOT ASSERTED — %s is unset, so the timing "+
			"arms LOG only (B/D'=%.3fx s1=%.3fx s50=%.3fx; regime resolvable=%t). "+
			"Everything non-temporal still asserts. Set %s=1 on a quiet box to arm them.",
			duecWallClockEnv, bvdT, s1T, s50T, timingResolvable, duecWallClockEnv)
	}
	if !timingResolvable {
		t.Logf("REGIME: wall-clock criteria WITHHELD (B/D'=%.3fx s1=%.3fx s50=%.3fx). %s.\n"+
			"Everything non-temporal still runs and still asserts — shapes, row counts, "+
			"NULL counts and the nine statement-memory-budget rows are counts rather "+
			"than durations, and load does not move them.",
			bvdT, s1T, s50T, withheldWhy)
	} else {
		// AN ACCEPTED REGIME MUST ACTUALLY HAVE MEASURED SOMETHING. Every timing
		// criterion below is spelled `ratio > bound`, and NaN > anything is FALSE
		// in Go — so a run that produced no usable per-rep ratios would sail
		// through all three while reporting the regime as ASSERTED. That is the
		// exact shape this whole mechanism exists to refuse (a criterion that
		// abstains while reading as enforced), arriving through the ACCEPTING door
		// rather than the withholding one, and nothing else on this path would
		// notice it.
		// nullT is deliberately NOT in this set, and the reason is a dependency
		// two functions away that nothing else states. It gates the accept/withhold
		// decision itself rather than a criterion — duecRegimeVerdict recomputes
		// the null pair internally and returns FALSE on an empty ratio slice, so a
		// NaN there withholds instead of sailing through. That safety rests
		// entirely on duecPerRepRatios skipping zero DENOMINATORS (`if d == 0 {
		// continue }`), which is what keeps a 0/0 out of the median. If that skip
		// is ever removed, nullT becomes NaN-able, math.Abs(NaN-1.0) > bound is
		// FALSE, and the detector ACCEPTS every run it can no longer measure —
		// so this comment is the pin on that `continue`.
		if name, v, bad := duecUnmeasuredRatio(map[string]float64{
			"B/D'": bvdT, "S1-R3/S1-full": s1T, "S50-R3/S50-full": s50T,
		}); bad {
			t.Fatalf("the regime was ACCEPTED but %s is %v: the criteria below compare "+
				"it with `>`, and a non-finite value passes every such comparison "+
				"silently. An accepted run that measured nothing is indistinguishable "+
				"from a green one, which is worse than a withheld run.", name, v)
		}
		// The per-rep ratios are logged on the ACCEPTING path too, not only when
		// withholding. Detector B reads ONE statistic — the median's deviation
		// from 1.0. A DISPERSION arm would be sharper in principle (it would catch
		// a run whose reps swing while the median lands near 1.0 by luck), and this
		// line existed to accumulate the record that would let one be derived.
		//
		// THE RECORD NOW EXISTS, AND IT REFUSES THE ARM. It was gathered because a
		// CI run on #638 produced exactly the anticipated blind spot: null pair
		// C/A′ = 0.995x — dead centred, comfortably inside the envelope, no window
		// loss, so wall-clock criteria were ASSERTED — over per-rep ratios
		//
		//	[0.728 1.271 0.791 1.747 0.979 1.153 0.87 1.082 0.995]
		//
		// whose max/min is 2.400. Against that, SEVENTEEN runs on a quiet local box
		// (this instrument's own named lane; every one of them ACCEPTED, medians
		// 0.927-0.989):
		//
		//	max/min  1.113 1.133 1.159 1.245 1.255 1.256 1.373 1.391 1.396
		//	         1.406 1.439 1.445 1.520 1.633 1.673 1.682 2.084
		//
		// The quiet band runs to 2.084 — ONE observation short of the 2.400 that a
		// bound would have to exclude, a separation of 1.15x. There is no gap to
		// place a bound in: anything below 2.084 fires on a run measured as quiet
		// and accepted, and anything in (2.084, 2.400) rests on a single failure
		// observation while the quiet tail is visibly not exhausted — run 16 was
		// the first above 1.7 and it took fifteen runs to appear.
		//
		// The two other natural statistics are worse, not better. q3/q1 INVERTS
		// (quiet reaches 1.416 against the failure's 1.325 — it would rank the
		// failure as calmer than a quiet run), and the coefficient of variation
		// separates by 1.067x, which is nothing.
		//
		// So the original refusal stands, and now it stands on data instead of on
		// the absence of it: a bound here would be invented, and its only measured
		// effect would be to convert one red B/D′ assertion into a withheld one.
		// An instrument change whose sole demonstrated consequence is suppressing a
		// failure needs a HIGHER evidentiary bar than a normal one, not a lower
		// one. What would actually earn the arm is a statistic that separates the
		// two populations with real margin — not a threshold slid into a 15% gap.
		t.Logf("REGIME: null pair C/A' = %.3fx, inside the %.2f quiet envelope, and no "+
			"timed run lost its window — wall-clock criteria ASSERTED. Per-rep "+
			"ratios %v (logged so a quiet box's DISPERSION becomes recorded data; "+
			"nothing asserts it yet).",
			nullT, duecNullPairQuietDev,
			duecRoundRatios(duecPerRepRatios(series["C"].samples, series["A'"].samples, duecDurOf)))
	}
	if wallClockAsserted && bvdT > r3Margin {
		t.Fatalf("R3 timing: B/D' = %.3fx is not strictly faster than the full "+
			"distinct (B=%v D'=%v). R3's exempt test must run on the row's RAW "+
			"SLOTS before the dedup key is packed; an implementation that packs "+
			"first and tests after is correct and delivers almost none of this.",
			bvdT, series["B"].medianDur(), series["D'"].medianDur())
	}
	// The sweep's timing criterion, on the same unpaged transactional regime. At
	// 1% the seen-set is 1 key against 99 001, so R3 must be faster: measured
	// 0.81-0.91x. At 50% it is the near-worst case and the criterion is "no
	// worse than full", never "faster" — that asymmetry IS the content of
	// "strictly dominates" — though it in fact measures 0.87-0.96x, because
	// half the rows still skip the tuple encoder entirely.
	if wallClockAsserted && s1T > r3Margin {
		t.Fatalf("sweep 1%%: R3/full = %.3fx is not faster (bound %.2fx, for the same "+
			"reason as B/D': without the route the two are one plan and the ratio is "+
			"1.0 plus noise)", s1T, r3Margin)
	}
	if wallClockAsserted && s50T > 1.0 {
		t.Fatalf("sweep 50%%: R3/full = %.3fx is WORSE than the full distinct. R3's "+
			"seen-set is a subset of the full one on every input, so there is no "+
			"density at which it may cost more.", s50T)
	}

	// ---- the deterministic discriminator: the memory budget -------------
	// What both routes actually change is WHAT THE OPERATOR RETAINS, and the
	// statement memory budget reads that back categorically. The hash
	// distinct's seen-set is charged against MAX_STATEMENT_MEMORY_BYTES and a
	// breach fails LOUDLY (TestFDB_SelectDistinct_BudgetLoudFail pins that
	// mechanism), so a budget far below one page of keys but far above zero
	// splits the plans in two:
	//
	//   survives  <=>  the plan retains (almost) nothing
	//   breaches  <=>  the plan retains a key per distinct value
	//
	// This is what discharges R3, and it is strictly stronger than the
	// allocation band it replaces: without the route, all three R3 rows below
	// BREACH — measured on master as "54F01: statement memory budget exceeded:
	// 65550 bytes buffered exceeds limit 65536 bytes" — while with it they
	// complete. It is also immune to concurrency, because the budget is charged
	// per statement by the operator itself, so nothing another test allocates
	// can move it.
	//
	// It does NOT discharge R2, and that has to be said plainly rather than
	// left for someone to infer from a passing row. Row C survives this budget
	// on master too. The reason is that master's C is
	// `Distinct(Project(IndexScan(BY_EMAIL, [<>] COVERING)))` — the filter moves
	// the input to index order, so the STREAMING distinct fires, and the
	// streaming variant compares each row against the previous one and holds no
	// seen-set at all. So row C here is a NON-REGRESSION guard (an "elision"
	// that reintroduced retention would breach), never evidence that R2 fired.
	// R2's discriminator is the plan-shape assertion above, which does red on
	// master with "row C still carries a physical distinct".
	//
	// The same fact explains the shape of the churn numbers: master's C
	// allocates 140 MiB against D''s 432 MiB, because it is paying for a
	// streaming operator rather than a hash one — which is why the C/A' churn
	// ratio it produces is 1.27x and not something far larger.
	//
	// These rows run UNPAGED INSIDE A TRANSACTION, for the same two reasons the
	// timings do: the proof is licensed nowhere else, and paging inside a
	// transaction cannot reach this fixture's size. The budget does not need
	// paging to bite — the seen-set is charged as it grows, not as it is
	// serialized — so the discriminator survives the move intact.
	// budgetArmsRan and rowArmsRan are the ANTI-SILENCE tally, and they are
	// counted rather than assumed for a specific reason.
	//
	// Detector A now makes a loaded box WITHHOLD the three wall-clock criteria
	// where it previously fatalled. That trade is only sound while the
	// load-independent arms keep asserting on exactly the runs that withhold —
	// otherwise a mechanism sold as "abstain from the timing" has quietly become
	// "abstain from everything", and a green run would mean nothing at all. The
	// arms below are deliberately NOT gated on the verdict; these two counters are
	// what make that structural fact checkable instead of merely intended, so a
	// future edit that tucks one of them behind `if timingResolvable` reds here
	// rather than silently halving the probe.
	//
	// They are compared against a FLOOR, never an exact count: an exact number
	// reds every time someone adds a legitimate case, which trains people to
	// update the constant without reading it.
	budgetArmsRan, rowArmsRan := 0, 0

	const duecBudgetBytes = 65536
	bconn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptMaxStatementMemoryBytes, duecBudgetBytes).Build())
	})
	for _, c := range []struct {
		tag, query string
		wantBreach bool
		why        string
	}{
		{"A", qA, false, "no operator at all"},
		{"A'", qA2, false, "no operator at all"},
		{"C", qC, false, "GUARD ONLY: R2 removed the operator, but master survives this too (streaming variant)"},
		{"B", qB, false, "DISCRIMINATOR: R3 retains only exempt rows, and this store has none"},
		{"D'", qD, true, "the full distinct retains one key per distinct value"},
		{"S1-R3", "SELECT DISTINCT email FROM users1", false, "DISCRIMINATOR: R3 retains the single NULL key"},
		{"S1-full", "SELECT DISTINCT email_plain FROM users1", true, "full: 99 001 keys"},
		{"S50-R3", "SELECT DISTINCT email FROM users50", false, "DISCRIMINATOR: R3 retains the single NULL key"},
		{"S50-full", "SELECT DISTINCT email_plain FROM users50", true, "full: 50 001 keys"},
	} {
		budgetArmsRan++
		err := duecBudgetRun(t, ctx, bconn, c.query)
		t.Logf("BUDGET %-9s breached=%-5v want=%-5v (%s)", c.tag, err != nil, c.wantBreach, c.why)
		if c.wantBreach && err == nil {
			t.Fatalf("BUDGET %s: %q completed inside a %d-byte statement budget, but it "+
				"should retain a dedup key per distinct value and breach. Either the "+
				"seen-set stopped being charged against the budget — which would make "+
				"this whole discriminator blind — or this control acquired a proof it "+
				"must not have.", c.tag, c.query, duecBudgetBytes)
		}
		if !c.wantBreach && err != nil {
			t.Fatalf("BUDGET %s: %q breached a %d-byte statement budget: %v\n"+
				"This is the load-independent form of RFC-210's claim (%s). A breach "+
				"here means the plan is retaining a key per distinct value, whatever "+
				"EXPLAIN says — an elision or narrowing that did not reach the "+
				"executor.", c.tag, c.query, duecBudgetBytes, err, c.why)
		}
	}

	// ---- rows are identical, whatever the operator ----------------------
	// The correctness half of the sweep. 50% NULLs must yield exactly ONE NULL
	// row, and the narrowed operator must agree with the full one value for
	// value at every density.
	//
	// Run in BOTH regimes, because they exercise different code and each has a
	// way to be wrong the other cannot see. In-transaction the narrowed
	// operator is the one producing the rows, so this is R3's correctness. In
	// paged auto-commit the proof is withheld and the FALLBACK produces them,
	// so this is the gate's correctness — a gate that withheld the proof but
	// left the query mis-deduplicated would pass every in-transaction row here.
	// The two regimes read DIFFERENT-SIZED tables, and the asymmetry is the
	// point rather than an oversight.
	//
	// In-transaction the whole result must come from one read version, which is
	// what licenses the elision — so the read is bounded by the driver's 4s
	// budget and at duecRows it does not fit under `-race` (see duecSmallRows).
	// Auto-commit pages, so each page takes its own read version and no single
	// read is bounded that way; that regime keeps the full population.
	//
	// Both still cover all three NULL densities, which is what these arms
	// actually interrogate. The small tables are the same generator at a
	// smaller count, so `users50_s` is half NULL exactly as `users50` is.
	for _, regime := range []struct {
		name    string
		tables  []duecIdentityTable
		collect func(*testing.T, context.Context, string) []string
	}{
		{"in-tx", duecSmallIdentityTables(), func(t *testing.T, ctx context.Context, q string) []string {
			return duecCollectInTx(t, ctx, tconn, q)
		}},
		{"auto-commit", []duecIdentityTable{
			{table: "users", wantRows: duecRows, wantNulls: 0},
			{table: "users1", wantRows: duecRows - duecRows/100 + 1, wantNulls: 1},
			{table: "users50", wantRows: duecRows/2 + 1, wantNulls: 1},
		}, func(t *testing.T, ctx context.Context, q string) []string {
			return duecCollect(t, ctx, acconn, q)
		}},
	} {
		for _, c := range regime.tables {
			r3 := regime.collect(t, ctx, "SELECT DISTINCT email FROM "+c.table)
			full := regime.collect(t, ctx, "SELECT DISTINCT email_plain FROM "+c.table)
			if len(r3) != c.wantRows {
				t.Fatalf("%s/%s: narrowed DISTINCT returned %d rows, want %d",
					regime.name, c.table, len(r3), c.wantRows)
			}
			if len(full) != c.wantRows {
				t.Fatalf("%s/%s: full DISTINCT returned %d rows, want %d",
					regime.name, c.table, len(full), c.wantRows)
			}
			nulls := 0
			for i := range r3 {
				if r3[i] != full[i] {
					t.Fatalf("%s/%s: narrowed and full DISTINCT disagree at %d: %q vs %q",
						regime.name, c.table, i, r3[i], full[i])
				}
				if r3[i] == "NULL" {
					nulls++
				}
			}
			wantNulls := c.wantNulls
			if nulls != wantNulls {
				t.Fatalf("%s/%s: narrowed DISTINCT returned %d NULL rows, want %d — a NULL "+
					"key component is EXEMPT, which is exactly why it still has to be "+
					"deduplicated down to one row", regime.name, c.table, nulls, wantNulls)
			}
			rowArmsRan++
			t.Logf("ROWS %-11s %-8s rows=%d nullRows=%d identical=true",
				regime.name, c.table, len(r3), nulls)
		}
	}

	// ---- R2's fourth assertion, on a fixture that can fail it -----------
	// §7.1 requires that an R2 plan RETAIN the predicate that made the exempt
	// set empty: an implementation that proved the set empty and then dropped
	// the predicate returns exactly the NULL rows it licensed itself to ignore.
	//
	// Every other R2 fixture in this file is the ZERO-NULL table, where that
	// criterion cannot fail — drop the predicate there and the same 100 000 rows
	// come back, because there were no NULLs to leak. USERS50 is half NULL, so
	// the criterion becomes falsifiable for the first time: 50 000 rows if the
	// predicate survives, 50 001 if it was dropped after the proof was drawn.
	//
	// The predicate survives as a SCAN RANGE rather than as a filter node — it
	// is SARGable, so the planner pushes it into the range and leaves no
	// residual — which is why the plan assertion below reads the range and not a
	// PredicatesFilter. The row count is the assertion that does not care which
	// form it takes: whichever way the NULL-rejection is carried, losing it
	// shows up here.
	const r2Filtered = "SELECT DISTINCT email FROM users50 WHERE email IS NOT NULL"
	r2Explain := ex(r2Filtered)
	t.Logf("EXPLAIN %-56s => %s", r2Filtered, r2Explain)
	if strings.Contains(r2Explain, "Distinct(") {
		t.Fatalf("R2 did not fully elide on the half-NULL table: %s", r2Explain)
	}
	if !strings.Contains(r2Explain, "distinct-by:BY_EMAIL50") {
		t.Fatalf("the elided plan names no proving index: %s", r2Explain)
	}
	if !strings.Contains(r2Explain, "IndexScan(BY_EMAIL50, [<>]") {
		t.Fatalf("the R2 plan no longer carries the NULL-rejecting range: %s\n"+
			"A full [*] range over BY_EMAIL50 reaches the 50 000 NULL entries the "+
			"proof assumed were unreachable, and the DISTINCT that would have "+
			"collapsed them is gone.", r2Explain)
	}
	//
	// Collected INSIDE A TRANSACTION, and that is the substance of the check
	// rather than a detail. In auto-commit the proof is withheld, the operator
	// survives, and the query returns 50 000 rows whether or not the predicate
	// would have been dropped from the elided plan — so an auto-commit count
	// here would pass on an implementation that leaks every NULL.
	if rows := duecCollectInTx(t, ctx, tconn, r2Filtered); len(rows) != 50000 {
		t.Fatalf("R2 over the half-NULL table returned %d rows, want exactly 50000.\n"+
			"USERS50 holds 50 000 distinct non-NULL emails and 50 000 NULLs. Any "+
			"count above 50 000 means the NULL-rejecting predicate was dropped after "+
			"licensing the elision, and the rows it was holding back are now flowing "+
			"past an operator that is no longer there.", len(rows))
	}

	// ---- ordered and LIMIT shapes: shape only, never timed --------------
	// The STREAMING variant is REFUSED the narrowing, and this is the SQL-level
	// half of that refusal. The ordered shapes plan a streaming distinct, whose
	// dedup compares each row against the LAST EMITTED one and holds no seen-set
	// at all — so there is nothing for a narrowing to shrink, and rendering
	// `narrowed-by` on it would advertise an optimization the executor does not
	// perform. An acceptance criterion reading that rendering would report R3 as
	// firing on a shape where it does not.
	//
	// The plan-level and executor-level halves are pinned in
	// plans/distinct_streaming_refuses_narrowing_test.go and the executor's
	// narrowedDedupFor test; neither observes what the PLANNER emits for real
	// SQL, which is the gap this closes.
	for _, ordered := range []string{
		"SELECT DISTINCT email FROM users ORDER BY email",
		"SELECT DISTINCT email FROM users ORDER BY email LIMIT 10",
	} {
		if !strings.Contains(explains[ordered], "Distinct(") {
			t.Fatalf("the ordered shape lost its distinct operator: %s\n%s",
				ordered, explains[ordered])
		}
		if strings.Contains(explains[ordered], "narrowed-by") {
			t.Fatalf("the ordered shape renders a NARROWING: %s\n%s\n"+
				"The streaming executor never consults the flag — it returns before "+
				"narrowedDedupFor is reached — so this rendering claims a residual "+
				"dedup that no code performs.", ordered, explains[ordered])
		}
	}

	// RFC-210 §2.1 could not resolve a delta on these — the spreads overlap
	// heavily — and a bound on an unresolvable delta measures the harness. They
	// are still pinned for SHAPE, because which executor variant fires decides
	// which cost the operator pays and EXPLAIN does not render it.
	duecAssertVariants(t, explains)

	// THE ANTI-SILENCE CHECK, run unconditionally — on the withheld path as much
	// as the asserted one, which is the only path where it says anything.
	//
	// The floors are what the fixture structurally produces: nine memory-budget
	// rows (three shapes x three tables) and six row-identity rows (three tables x
	// two regimes). Falling below either means arms that load cannot move — counts
	// and shapes, not durations — stopped running, and the probe is reporting
	// green for a smaller claim than it advertises.
	//
	// Before Detector A learned the driver's pre-emption, a loaded box reached
	// NEITHER of these: it fatalled inside the timing loop, which runs first, so
	// every budget row and every row-identity comparison was skipped. The counters
	// are what stop that from being reintroduced as an abstention instead of a
	// failure.
	const (
		wantBudgetArms = 9
		wantRowArms    = 6
	)
	if budgetArmsRan < wantBudgetArms || rowArmsRan < wantRowArms {
		t.Fatalf("the LOAD-INDEPENDENT arms did not all run: %d/%d statement-memory-budget "+
			"rows, %d/%d row-identity rows (wall-clock criteria resolvable=%v).\n"+
			"These are the arms that gate RFC-210 in CI, and they are counts and shapes "+
			"rather than durations — no amount of load moves them by one byte, so nothing "+
			"about the machine may excuse skipping them. If a regime detector now gates "+
			"one of them, that is the bug: withholding must cost the three timing bounds "+
			"and nothing else.",
			budgetArmsRan, wantBudgetArms, rowArmsRan, wantRowArms, timingResolvable)
	}
}

// duecLoad fills one table with duecRows rows; every nullEvery-th row gets a
// NULL email (and the same NULL in the unindexed mirror column). nullEvery <= 0
// means none.
func duecLoad(t *testing.T, ctx context.Context, db *sql.DB, table string, nullEvery int) {
	t.Helper()
	duecLoadN(t, ctx, db, table, nullEvery, duecRows)
}

// duecLoadN is duecLoad at an explicit row count, for the tables whose arm has
// to fit inside a read version rather than exercise scale. See duecSmallRows.
func duecLoadN(t *testing.T, ctx context.Context, db *sql.DB, table string, nullEvery, rows int) {
	t.Helper()
	const batch = 250
	const workers = 8
	start := time.Now()
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	per := rows / workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lo := w * per
			for base := lo; base < lo+per; base += batch {
				var sb strings.Builder
				fmt.Fprintf(&sb, "INSERT INTO %s (id, email, email_plain, payload) VALUES ", table)
				for i := 0; i < batch; i++ {
					id := base + i
					if i > 0 {
						sb.WriteString(",")
					}
					if nullEvery > 0 && id%nullEvery == 0 {
						fmt.Fprintf(&sb, "(%d, NULL, NULL, 'pad-%07d-xxxxxxxxxxxxxxxxxxxx')", id, id)
						continue
					}
					fmt.Fprintf(&sb,
						"(%d, 'user%07d@example.com', 'user%07d@example.com', 'pad-%07d-xxxxxxxxxxxxxxxxxxxx')",
						id, id, id, id)
				}
				if _, e := db.ExecContext(ctx, sb.String()); e != nil {
					errCh <- fmt.Errorf("insert %s at %d: %w", table, base, e)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("load: %v", e)
	}
	var n, nulls int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+table+" WHERE email IS NULL").Scan(&nulls); err != nil {
		t.Fatalf("null count %s: %v", table, err)
	}
	if n != int64(rows) {
		t.Fatalf("%s loaded %d rows, want %d", table, n, rows)
	}
	wantNulls := int64(0)
	if nullEvery > 0 {
		wantNulls = int64(rows / nullEvery)
	}
	if nulls != wantNulls {
		t.Fatalf("%s holds %d NULL emails, want %d — the NULL density IS the sweep's "+
			"independent variable", table, nulls, wantNulls)
	}
	t.Logf("LOAD %-8s rows=%d nullEmails=%d in %v",
		table, n, nulls, time.Since(start).Round(time.Millisecond))
}

// duecRunInTx executes one query inside an explicit transaction on conn and
// rolls back. The transaction is what licenses the secondary-UNIQUE proof, so
// it is the only regime in which a timing here is about RFC-210 at all; the
// timing covers QueryContext plus the drain, never BeginTx.
//
// A LOST MEASUREMENT WINDOW is RETURNED rather than fatal, and that asymmetry is
// the whole of Detector A. It is not a bug in the code under test — it is the box
// telling this run that its MVCC window did not hold — so it must reach the
// caller as EVIDENCE ABOUT THE REGIME instead of killing the test and taking
// every non-temporal assertion down with it. Every OTHER error still fatals: a
// genuine failure must stay loud, and reading an unrelated break as "the box was
// busy" is exactly how a real defect would hide here. That is why the recogniser
// is duecMeasurementWindowLost rather than "the SQLSTATE was 40001" — the code
// this arrives under is shared with a genuine conflict.
//
// The partial sample is returned as measured rather than zeroed, so the ratios
// the withholding path logs are finite and show what the run actually saw.
func duecRunInTx(
	t *testing.T, ctx context.Context, c *sql.Conn, q string,
) (sample duecSample, nulls int, windowLost bool) {
	t.Helper()
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin for %q: %v", q, err)
	}
	defer func() { _ = tx.Rollback() }()
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	start := time.Now()
	finish := func(n, nul int, too bool) (duecSample, int, bool) {
		d := time.Since(start)
		var m1 runtime.MemStats
		runtime.ReadMemStats(&m1)
		return duecSample{rows: n, dur: d, alloc: m1.TotalAlloc - m0.TotalAlloc}, nul, too
	}
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		if duecMeasurementWindowLost(err) {
			return finish(0, 0, true)
		}
		t.Fatalf("query %q: %v", q, err)
	}
	n := 0
	var s sql.NullString
	for rows.Next() {
		if err := rows.Scan(&s); err != nil {
			if duecMeasurementWindowLost(err) {
				rows.Close()
				return finish(n, nulls, true)
			}
			t.Fatalf("scan: %v", err)
		}
		n++
		if !s.Valid {
			nulls++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		if duecMeasurementWindowLost(err) {
			return finish(n, nulls, true)
		}
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	rows.Close()
	return finish(n, nulls, false)
}

// duecWindowAttempts bounds every lost-window retry in this file. It is a real
// bound: if the drain stops fitting FDB's window altogether, the last attempt
// must FAIL saying so rather than spin.
const duecWindowAttempts = 8

// duecWithHeldWindow runs try until it reports that the MVCC window HELD, and
// is the single place this file's retry semantics are stated.
//
// THE ASYMMETRY WITH duecRunInTx IS THE POINT. duecRunInTx is TIMING: resampling
// there until the window happens to hold turns a broken regime into a green
// measurement, which is why it returns its truncated sample as evidence
// instead. Everything routed through here reads ROWS or observes a MEMORY-BUDGET
// breach, and neither has that hazard — the data is committed, so a fresh
// transaction sees the same rows and the same budget behaviour, and a retry buys
// nothing except a window that held.
//
// It is needed because the fixture is 100k rows against FDB's 5s MVCC window
// (the driver pre-empts at 4s). Unloaded that drains in 215-554 ms; under the
// race detector it measured 4.16-4.29s — MARGINALLY over, which is also why
// retrying works at all. duecRetained's db.Run already leaned on exactly this,
// retrying the same shape until an attempt fit.
//
// try receives `fatal` on the final attempt so it can report the window loss as
// the failure it then is.
func duecWithHeldWindow[T any](t *testing.T, what string, try func(fatal bool) (T, bool)) T {
	t.Helper()
	for attempt := 1; ; attempt++ {
		out, lost := try(attempt == duecWindowAttempts)
		if !lost {
			return out
		}
		t.Logf("%s: measurement window lost on attempt %d/%d — retrying against "+
			"committed data in a fresh transaction", what, attempt, duecWindowAttempts)
	}
}

// duecBudgetRun executes q inside an explicit transaction and returns the error
// the statement memory budget produced, or nil if it completed. The
// transaction is required: outside one the proof is withheld and every row
// below would be measuring the full operator. The driver may surface a breach
// at query time or on the first scan, so both are drained before deciding.
func duecBudgetRun(t *testing.T, ctx context.Context, c *sql.Conn, q string) error {
	t.Helper()
	return duecWithHeldWindow(t, "BUDGET "+q, func(fatal bool) (error, bool) {
		return duecTryBudgetRun(t, ctx, c, q, fatal)
	})
}

func duecTryBudgetRun(
	t *testing.T, ctx context.Context, c *sql.Conn, q string, fatal bool,
) (breach error, windowLost bool) {
	t.Helper()
	tx, berr := c.BeginTx(ctx, nil)
	if berr != nil {
		t.Fatalf("begin for %q: %v", q, berr)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, q)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	if err == nil {
		return nil, false
	}
	// A LOST WINDOW IS NOT EVIDENCE EITHER WAY, so it is neither a breach nor a
	// failure — it is a run that did not get to observe the budget at all. This
	// arm asks whether the statement memory budget was breached; a transaction
	// that ran past FDB's MVCC horizon answers no part of that question, and
	// reading it as "not breached" would silently disarm the discriminator.
	// Tested before the budget recogniser below because the read-budget message
	// says neither "limit" nor "memory" and would otherwise reach the Fatalf.
	if duecMeasurementWindowLost(err) && !fatal {
		return nil, true
	}
	// A breach must be the BUDGET, never some unrelated failure quietly read as
	// evidence for the claim.
	msg := err.Error()
	if !strings.Contains(msg, "limit") && !strings.Contains(msg, "memory") &&
		!strings.Contains(msg, string(api.ErrCodeExecutionLimitReached)) {
		t.Fatalf("query %q failed for a reason that is not the memory budget: %v", q, err)
	}
	return err, false
}

// duecCollectInTx is duecCollect inside an explicit transaction, where the
// rows are produced by the PROVEN plan rather than by the fallback.
//
// It retries a lost window (duecWithHeldWindow states why that is legitimate
// here and forbidden for duecRunInTx). Without it the race lane reported
// `40001: transaction read budget exhausted` and took the whole non-temporal
// sweep down with it — which withholding could not have covered for, since the
// anti-silence tally demands all six row-identity arms and says withholding
// must cost the three timing bounds and nothing else.
func duecCollectInTx(t *testing.T, ctx context.Context, c *sql.Conn, q string) []string {
	t.Helper()
	return duecWithHeldWindow(t, "COLLECT in-tx "+q, func(fatal bool) ([]string, bool) {
		return duecTryCollectInTx(t, ctx, c, q, fatal)
	})
}

// duecTryCollectInTx is one attempt. fatal makes the last attempt report the
// window loss as the failure it then is, so the retry above cannot become an
// unbounded spin that reports nothing.
func duecTryCollectInTx(
	t *testing.T, ctx context.Context, c *sql.Conn, q string, fatal bool,
) (out []string, windowLost bool) {
	t.Helper()
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin for %q: %v", q, err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, qerr := tx.QueryContext(ctx, q)
	if qerr != nil {
		if duecMeasurementWindowLost(qerr) && !fatal {
			return nil, true
		}
		t.Fatalf("collect %q: %v", q, qerr)
	}
	defer rows.Close()
	var s sql.NullString
	for rows.Next() {
		if scanErr := rows.Scan(&s); scanErr != nil {
			if duecMeasurementWindowLost(scanErr) && !fatal {
				return nil, true
			}
			t.Fatalf("scan %q: %v", q, scanErr)
		}
		// The "v:" prefix is duecDrain's encoding and must match it exactly:
		// without it a row whose email is the literal string "NULL" and a row
		// whose email IS SQL NULL collapse to one value, and the sweep's
		// null-count assertion is then counting the wrong thing.
		if s.Valid {
			out = append(out, "v:"+s.String)
		} else {
			out = append(out, "NULL")
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		if duecMeasurementWindowLost(rowsErr) && !fatal {
			return nil, true
		}
		t.Fatalf("rows.Err %q: %v", q, rowsErr)
	}
	sort.Strings(out)
	return out, false
}

// duecCollect returns the query's values sorted, with SQL NULL as "NULL", so
// two operators' outputs can be compared as multisets.
func duecCollect(t *testing.T, ctx context.Context, c *sql.Conn, q string) []string {
	t.Helper()
	rows, qerr := c.QueryContext(ctx, q)
	return duecDrain(t, rows, qerr)
}

func duecDrain(t *testing.T, rows *sql.Rows, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	defer rows.Close()
	var out []string
	var s sql.NullString
	for rows.Next() {
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if s.Valid {
			out = append(out, "v:"+s.String)
		} else {
			out = append(out, "NULL")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(out)
	return out
}

// duecAssertVariants pins which executor variant each shape gets, through the
// metadata-only planning harness.
//
// That harness NEVER yields R2 or R3, and the reason is load-bearing rather
// than incidental: secondaryUniqueEliminationProof demands AFFIRMATIVE evidence
// that index states were established, and only the SELECT/DML generator paths
// supply it. A metadata-only run may still plan an index scan, but it may not
// prove anything from an index's declared uniqueness. So this function asserts
// the UNPROVEN shapes, and the SQL EXPLAINs above assert the proven ones; the
// pairing is what makes "only a run that asked the store gets the proof"
// testable rather than merely commented.
func duecAssertVariants(t *testing.T, explains map[string]string) {
	t.Helper()
	md := duecMetaData(t)
	shapes := []struct {
		name, query    string
		wantDistinct   bool
		wantStreaming  bool
		wantNarrowable bool
	}{
		{"distinct", "SELECT DISTINCT email FROM users", true, false, false},
		{"no_distinct", "SELECT email FROM users", false, false, false},
		{"distinct_order", "SELECT DISTINCT email FROM users ORDER BY email", true, true, false},
		{"no_distinct_order", "SELECT email FROM users ORDER BY email", false, false, false},
		{"distinct_order_limit", "SELECT DISTINCT email FROM users ORDER BY email LIMIT 10", true, true, false},
		{"distinct_limit", "SELECT DISTINCT email FROM users LIMIT 10", true, false, false},
	}
	for _, s := range shapes {
		p, e := embedded.PlanRecordQueryWithMetadata(s.query, md, nil)
		if e != nil {
			t.Fatalf("VARIANT %s: plan error: %v", s.name, e)
		}
		found, streaming, narrowed := false, false, false
		plans.Walk(p, func(node plans.RecordQueryPlan) bool {
			if d, ok := node.(*plans.RecordQueryDistinctPlan); ok {
				found = true
				streaming = d.Streaming
				narrowed = narrowed || d.IsNarrowedDedup()
			}
			return true
		})
		t.Logf("VARIANT %-22s distinct=%v streaming=%v narrowed=%v plan=%s",
			s.name, found, streaming, narrowed, p.Explain())
		if found != s.wantDistinct {
			t.Fatalf("VARIANT %s: distinctPresent=%v, want %v (%s)",
				s.name, found, s.wantDistinct, p.Explain())
		}
		if found && streaming != s.wantStreaming {
			// The unordered shape gets the HASH set, whose seen-key set rides
			// every continuation page; the ordered shape gets the streaming
			// variant, which carries no seen-key set at all — which is why
			// RFC-210 sets no timing criterion on the ordered shape.
			t.Fatalf("VARIANT %s: streaming=%v, want %v (%s)",
				s.name, streaming, s.wantStreaming, p.Explain())
		}
		if narrowed != s.wantNarrowable {
			t.Fatalf("VARIANT %s: narrowed=%v on the METADATA-ONLY harness, want %v.\n"+
				"That harness establishes no index states, so no secondary-UNIQUE "+
				"proof may be drawn on it. If this now narrows, the proof is being "+
				"drawn without affirmative evidence that the index is readable — "+
				"which is the exact failure the ReadableIndexes gate exists to "+
				"prevent. If index states were deliberately threaded here, this "+
				"expectation is what needs updating.", s.name, narrowed, s.wantNarrowable)
		}
	}
	// The ordered SQL shape reaches the index; asserted because the streaming
	// variant above is only available on an ordered input.
	if !strings.Contains(explains["SELECT DISTINCT email FROM users ORDER BY email"], "IndexScan(BY_EMAIL") {
		t.Fatalf("ORDER BY email no longer reaches BY_EMAIL: %s",
			explains["SELECT DISTINCT email FROM users ORDER BY email"])
	}
}

// TestFDB_DistinctUniqueElisionRetention measures the EXACT content of the
// hash-distinct's seen-set at three NULL densities, by reading it back off the
// continuation the operator serializes.
//
// The row limit is set to the number of rows the query produces, so the cursor
// stops at LIMIT rather than at exhaustion and its live continuation therefore
// carries the COMPLETE set. An exhausted cursor returns an end continuation,
// which carries nothing.
//
// Two projections are measured because they answer two different questions.
// `SELECT DISTINCT email` gives the KEYS retained — the memory and
// continuation-bytes cost. `SELECT DISTINCT email, payload` gives the ROWS that
// ENTERED: PAYLOAD is unique per row, so every admitted row packs a different
// key and the key count IS the row count. Without the second projection the
// 1%-and-50% rows are indistinguishable, since a thousand NULL emails collapse
// to one key.
func TestFDB_DistinctUniqueElisionRetention(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	md := duecMetaData(t)
	desc := md.GetRecordType("USERS").Descriptor

	for _, d := range []struct {
		label     string
		nullEvery int
		outRows   int
		wantNulls int
	}{
		{"0%", 0, 100000, 0},
		{"1%", 100, 99001, 1000},
		{"50%", 2, 50001, 50000},
	} {
		ks := subspace.FromBytes(tuple.Tuple{t.Name(), d.label}.Pack())
		duecDirectLoad(t, ctx, db, md, desc, ks, d.nullEvery)

		// The exempt SLOTS, as the PLANNER computed them, on a shape where the
		// answer is not 0. exemptSlotsFor resolves the index's key columns
		// against the dedup key's slot order, so moving EMAIL out of first
		// position must move the slot with it. A mapping stuck at 0 would test
		// PAYLOAD for exemptness — a column that is never NULL — and the
		// operator would retain NOTHING at 50% NULL density while claiming to
		// narrow, which is the wrong-slot failure this asserts against.
		for _, s := range []struct {
			query string
			want  []int
		}{
			{"SELECT DISTINCT email, payload FROM USERS", []int{0}},
			{"SELECT DISTINCT payload, email FROM USERS", []int{1}},
		} {
			got := duecDistinctIn(duecPlan(t, s.query, md, true)).GetNarrowedExemptSlots()
			if len(got) != len(s.want) || (len(got) == 1 && got[0] != s.want[0]) {
				t.Fatalf("density %s: the planner computed exempt slots %v for %q, want %v.\n"+
					"These are positions in the DEDUP KEY's slot order, and the index "+
					"keys EMAIL alone. Testing any other position retains rows the "+
					"index already proves unique and misses the ones it does not.",
					d.label, got, s.query, s.want)
			}
		}
		// And the mapping is measured, not only asserted: at 50% NULL the
		// payload-FIRST projection must still admit exactly the NULL-email rows.
		// Every row packs a distinct key here (PAYLOAD is unique per row), so
		// the admitted count is the row count. A slot stuck at 0 admits 0.
		if d.wantNulls > 0 {
			r3PayloadFirst := duecRetained(t, ctx, db, md, ks,
				"SELECT DISTINCT payload, email FROM USERS", duecRows, true)
			if r3PayloadFirst.keys != d.wantNulls {
				t.Fatalf("density %s: with EMAIL projected SECOND, R3 admitted %d rows, "+
					"want exactly %d.\nThe exempt test is reading the wrong slot: 0 "+
					"admitted means it is testing PAYLOAD, which is never NULL.",
					d.label, r3PayloadFirst.keys, d.wantNulls)
			}
		}

		// Keys retained: what the seen-set holds and what rides the continuation.
		fullKeys := duecRetained(t, ctx, db, md, ks, "SELECT DISTINCT email FROM USERS", d.outRows, false)
		r3Keys := duecRetained(t, ctx, db, md, ks, "SELECT DISTINCT email FROM USERS", d.outRows, true)
		// Rows entered: the structural claim of RFC-210 §2.1's sweep table.
		fullRows := duecRetained(t, ctx, db, md, ks, "SELECT DISTINCT email, payload FROM USERS", duecRows, false)
		r3Rows := duecRetained(t, ctx, db, md, ks, "SELECT DISTINCT email, payload FROM USERS", duecRows, true)
		t.Logf("RETAINED %-4s keys full=%-6d r3=%-6d | rows-entered full=%-6d r3=%-6d",
			d.label, fullKeys.keys, r3Keys.keys, fullRows.keys, r3Rows.keys)

		// The full distinct admits EVERY row, at every density. That is the
		// column the sweep table compares R3 against, and it is flat by
		// construction: the operator has no exempt set.
		if fullRows.keys != duecRows {
			t.Fatalf("density %s: full distinct admitted %d rows, want %d",
				d.label, fullRows.keys, duecRows)
		}
		// R3 admits EXACTLY the exempt rows — the NULL-keyed ones. Not "about",
		// not "at most": the narrowed seen-set is a subset of the full one on
		// every input and degenerates to EMPTY on an ordinary table, and an
		// off-by-anything here means the exempt test is looking at the wrong
		// slot or running after the key is packed.
		if r3Rows.keys != d.wantNulls {
			t.Fatalf("density %s: R3 admitted %d rows, want exactly %d (the rows with "+
				"a NULL email). R3's seen-set must be exactly the exempt subset.",
				d.label, r3Rows.keys, d.wantNulls)
		}
		// And the keys those rows collapse to: all NULL emails share one key, so
		// R3 holds 0 keys on an ordinary table and 1 wherever a NULL exists —
		// against the full operator's one key per distinct value.
		wantR3Keys := 0
		if d.wantNulls > 0 {
			wantR3Keys = 1
		}
		if r3Keys.keys != wantR3Keys {
			t.Fatalf("density %s: R3 retained %d keys, want %d", d.label, r3Keys.keys, wantR3Keys)
		}
		if fullKeys.keys != d.outRows {
			t.Fatalf("density %s: full distinct retained %d keys, want %d (one per "+
				"output row)", d.label, fullKeys.keys, d.outRows)
		}
		if !r3Keys.narrowed || fullKeys.narrowed {
			t.Fatalf("density %s: narrowing flags are wrong (r3=%v full=%v)",
				d.label, r3Keys.narrowed, fullKeys.narrowed)
		}
		if fullKeys.rows != d.outRows || r3Keys.rows != d.outRows {
			t.Fatalf("density %s: row counts differ between the operators: full=%d r3=%d, want %d",
				d.label, fullKeys.rows, r3Keys.rows, d.outRows)
		}
	}
}

type duecRetention struct {
	rows     int
	keys     int
	narrowed bool
}

// duecPlan plans query through one of the two index-state views. narrow=true
// asks for the affirmative all-readable view, under which the secondary-UNIQUE
// proof is available and R3 narrows; narrow=false is the plain UNKNOWN-state
// harness, which refuses the proof and yields the full operator.
func duecPlan(
	t *testing.T, query string, md *recordlayer.RecordMetaData, narrow bool,
) plans.RecordQueryPlan {
	t.Helper()
	planner := embedded.PlanRecordQueryWithMetadata
	if narrow {
		planner = embedded.PlanRecordQueryAssertingAllIndexesReadable
	}
	plan, err := planner(query, md, nil)
	if err != nil {
		t.Fatalf("plan %q (narrow=%v): %v", query, narrow, err)
	}
	if !narrow {
		return plan
	}
	if d := duecDistinctIn(plan); d == nil {
		t.Fatalf("plan %q under the all-readable view carries no distinct at all: %s",
			query, plan.Explain())
	} else if !d.IsNarrowedDedup() {
		t.Fatalf("the PLANNER did not narrow %q under the affirmative all-readable "+
			"view: %s\nThis harness now asks the planner for the narrowing rather "+
			"than applying one itself, so a rule that stopped firing makes every "+
			"retention measurement below silently measure the FULL operator instead "+
			"of failing.", query, plan.Explain())
	}
	return plan
}

func duecDistinctIn(plan plans.RecordQueryPlan) *plans.RecordQueryDistinctPlan {
	var found *plans.RecordQueryDistinctPlan
	plans.Walk(plan, func(n plans.RecordQueryPlan) bool {
		if d, ok := n.(*plans.RecordQueryDistinctPlan); ok && found == nil {
			found = d
		}
		return true
	})
	return found
}

// duecRetained plans a query, executes it against ks with a returned-row limit,
// and reports the seen-set size read off the resulting continuation.
//
// narrow selects WHICH PLANNER VIEW plans the query, and the narrowing is the
// planner's own rather than the test's. The plain harness leaves index state
// UNKNOWN and therefore refuses to draw a secondary-UNIQUE proof at all (see
// duecAssertVariants, which pins that refusal); the asserting entry mints the
// affirmative all-readable view the live generator mints after fetching a
// snapshot, which is a claim this fixture is entitled to make — the store is
// built here, and none of its indexes is ever transitioned.
//
// This used to hand-write the narrowing as WithNarrowedDedup("BY_EMAIL",
// []int{0}), which measured the executor over a plan the planner had no part
// in. The slot list is the piece that was going unasserted: exemptSlotsFor maps
// the index's key columns onto positions in the DEDUP KEY's slot order, and a
// literal [0] agrees with a broken mapping on every fixture whose key column
// happens to be projected first. TestFDB_DistinctUniqueElisionRetention asserts
// the mapping directly — reading GetNarrowedExemptSlots off the planner's own
// plan for `SELECT DISTINCT payload, email`, a shape where the key column is
// NOT first — and then measures it, since at 50% NULL density a slot stuck at 0
// would test PAYLOAD and admit nothing.
func duecRetained(
	t *testing.T,
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	md *recordlayer.RecordMetaData,
	ks subspace.Subspace,
	query string,
	rowLimit int,
	narrow bool,
) duecRetention {
	t.Helper()
	plan := duecPlan(t, query, md, narrow)
	var out duecRetention
	plans.Walk(plan, func(n plans.RecordQueryPlan) bool {
		if d, ok := n.(*plans.RecordQueryDistinctPlan); ok && d.IsNarrowedDedup() {
			out.narrowed = true
		}
		return true
	})
	_, rerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		// RESET PER ATTEMPT. db.Run RETRIES its closure, and `out` lives in the
		// enclosing scope, so a counter incremented here without this reset
		// reports the SUM OVER ATTEMPTS rather than the surviving attempt. The
		// fixture normally finishes inside FDB's 5s MVCC window and retries
		// never happen, which is why the sum equalled the truth — until the
		// race detector taxed every memory access and the 100k-row drain
		// started tripping the read budget. It then reported
		// `full=384256 … want 100000`, an impossible count off a table with
		// 100000 rows: ~3.84 attempts added together.
		//
		// `narrowed` is deliberately NOT reset: it is derived from the PLAN
		// above, before any transaction, and is retry-independent.
		out.rows, out.keys = 0, 0
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		cursor, cErr := executor.ExecutePlan(ctx, plan, store,
			executor.EmptyEvaluationContext(), nil,
			recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(rowLimit))
		if cErr != nil {
			return nil, cErr
		}
		defer cursor.Close()
		for {
			next, nErr := cursor.OnNext(ctx)
			if nErr != nil {
				return nil, nErr
			}
			if next.HasNext() {
				out.rows++
				continue
			}
			cont := next.GetContinuation()
			if cont == nil || cont.IsEnd() {
				return nil, fmt.Errorf(
					"cursor stopped without a live continuation (reason %v after %d rows); "+
						"the row limit must stop it BEFORE exhaustion or the seen-set is unreadable",
					next.GetNoNextReason(), out.rows)
			}
			encoded, eErr := cont.ToBytes()
			if eErr != nil {
				return nil, eErr
			}
			var dc gen.DistinctHashContinuation
			if uErr := dc.UnmarshalVT(encoded); uErr != nil {
				return nil, fmt.Errorf("not a DistinctHashContinuation: %w", uErr)
			}
			out.keys = len(dc.GetSeenKeys())
			return nil, nil
		}
	})
	if rerr != nil {
		t.Fatalf("retained %q (narrow=%v): %v", query, narrow, rerr)
	}
	return out
}

// duecMetaData is the fixture's schema as record-layer metadata: the same
// shape the SQL DDL above creates, so a plan built here is the plan the SQL
// path builds modulo the proof the SQL path is allowed to draw.
func duecMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(`
		CREATE TABLE USERS (id bigint, email string, email_plain string, payload string, PRIMARY KEY(id))
		CREATE UNIQUE INDEX by_email ON USERS (email)`)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	idx := md.GetIndex("BY_EMAIL")
	if idx == nil || !idx.IsUnique() {
		t.Fatal("BY_EMAIL missing or not unique — the whole fixture rests on it")
	}
	return md
}

// duecDirectLoad writes the fixture through the record layer, so the retention
// test owns a store it can execute a hand-built plan against.
func duecDirectLoad(
	t *testing.T,
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	md *recordlayer.RecordMetaData,
	desc protoreflect.MessageDescriptor,
	ks subspace.Subspace,
	nullEvery int,
) {
	t.Helper()
	const workers = 8
	const perTx = 200
	per := duecRows / workers
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lo := w * per
			for base := lo; base < lo+perTx*((per+perTx-1)/perTx); base += perTx {
				if base >= lo+per {
					break
				}
				_, e := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, sErr := recordlayer.NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
					if sErr != nil {
						return nil, sErr
					}
					for i := 0; i < perTx; i++ {
						id := int64(base + i)
						m := dynamicpb.NewMessage(desc)
						m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
						if nullEvery <= 0 || int(id)%nullEvery != 0 {
							v := fmt.Sprintf("user%07d@example.com", id)
							m.Set(desc.Fields().ByName("EMAIL"), protoreflect.ValueOfString(v))
							m.Set(desc.Fields().ByName("EMAIL_PLAIN"), protoreflect.ValueOfString(v))
						}
						m.Set(desc.Fields().ByName("PAYLOAD"), protoreflect.ValueOfString(
							fmt.Sprintf("pad-%07d-xxxxxxxxxxxxxxxxxxxx", id)))
						if _, se := store.SaveRecord(proto.Message(m)); se != nil {
							return nil, se
						}
					}
					return nil, nil
				})
				if e != nil {
					errCh <- fmt.Errorf("direct load at %d: %w", base, e)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("%v", e)
	}
}

// duecExplainInTx runs EXPLAIN inside an explicit transaction, the regime in
// which the secondary-UNIQUE proof is licensed.
func duecExplainInTx(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var plan string
	if err := tx.QueryRowContext(ctx, "EXPLAIN "+query).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q in a transaction: %v", query, err)
	}
	return plan
}

// duecIdentityTable is one row-identity arm: the table, the number of rows a
// correct DISTINCT returns over it, and how many of those are the NULL row.
//
// The counts are DATA rather than derived from the table's name. They used to
// be `if c.table != "users" { wantNulls = 1 }`, which is a decision keyed on a
// string — it silently gives the wrong answer for any table added later whose
// name is not "users" but whose density is zero, which is exactly what
// duecSmallRows' no-NULL fixture is.
type duecIdentityTable struct {
	table     string
	wantRows  int
	wantNulls int
}

// duecSmallIdentityTables is the three NULL densities at duecSmallRows, for the
// in-transaction arm whose whole result has to fit inside one read version.
//
// The expected counts are COMPUTED from the density rather than written down,
// so changing duecSmallRows cannot leave a stale literal behind that the arm
// would then report as a dedup failure:
//
//   - density 0:   every email distinct          -> N rows, no NULL row
//   - density 100: N/100 rows collapse to ONE    -> N - N/100 + 1, one NULL
//   - density 2:   N/2 rows collapse to ONE      -> N/2 + 1, one NULL
func duecSmallIdentityTables() []duecIdentityTable {
	const n = duecSmallRows
	return []duecIdentityTable{
		{table: "users_s", wantRows: n, wantNulls: 0},
		{table: "users1_s", wantRows: n - n/100 + 1, wantNulls: 1},
		{table: "users50_s", wantRows: n/2 + 1, wantNulls: 1},
	}
}
