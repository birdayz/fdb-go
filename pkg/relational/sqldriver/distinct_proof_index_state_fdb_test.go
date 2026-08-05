package sqldriver_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// A secondary UNIQUE index licenses a DISTINCT elision (R2) or a narrowed dedup
// (R3) only while the store calls it READABLE, and the licensed plan NEVER
// READS that index — the R3 shape below is a base-record scan. So no executor
// leaf check can notice the index moving, and the only thing standing between a
// mid-statement transition and wrong rows is the proof STAMP the planner writes
// onto the plan, which the dependency walk then collects.
//
// These queries are chosen so the stamp is the SOLE source of the dependency.
// The NULL-rejecting variant that R2 fully elides moves the access path onto
// U_EMAIL, so a plan-carries-U_EMAIL assertion there would hold through the
// ordinary index-scan arm and would prove nothing about the stamp. The bare
// query stays on a base scan, so if the stamp arm is removed the dependency set
// is empty and every 40001 below disappears.
const distinctProofQuery = "SELECT DISTINCT EMAIL FROM T"

const distinctProofWantRows = "a@example,b@example,c@example"

// assertStaleIndexDependency requires the refusal to be SQLSTATE 40001 AND to
// name the index it refused for. A 40001 that does not say which dependency
// moved sends the operator to the wrong index, and a plain "something changed"
// would be satisfied by a check that invalidated on any transition anywhere —
// the self-inflicted outage the scoped dependency set exists to avoid.
func assertStaleIndexDependency(t *testing.T, err error, indexName string) {
	t.Helper()
	assertSerializationFailure(t, err)
	if !strings.Contains(err.Error(), indexName) {
		t.Fatalf("the stale-plan refusal does not name %s: %v", indexName, err)
	}
}

// assertNarrowedByUEmailOnBaseScan pins the precondition every 40001 test in
// this file rests on: the plan is narrowed by U_EMAIL and does NOT scan it. If
// the access path ever moves onto U_EMAIL these tests keep passing while
// testing the ordinary index-scan dependency instead of the stamp, which is the
// silent way this file stops covering what it claims to.
func assertNarrowedByUEmailOnBaseScan(t *testing.T, explain string) {
	t.Helper()
	if !strings.Contains(explain, "narrowed-by:U_EMAIL") {
		t.Fatalf("the plan is not narrowed by U_EMAIL, so it has no proof-only "+
			"dependency on it and this test's 40001 would be vacuous: %s", explain)
	}
	if strings.Contains(explain, "IndexScan") {
		t.Fatalf("the plan now READS an index: %s\nThe whole point of this query "+
			"is that its only dependency on U_EMAIL is the proof stamp; once the "+
			"plan scans an index the dependency arrives through the ordinary scan "+
			"arm and the stamp is no longer under test.", explain)
	}
}

// nthPlanTransitionLogger captures every planning event and fires fn exactly
// once, at the end of the nth planning call. Firing on a chosen call rather than
// on the first is what lets a test land a transition strictly between a plan
// CACHE HIT and the execution transaction that hit follows.
type nthPlanTransitionLogger struct {
	n  int
	fn func() error

	mu       sync.Mutex
	events   []embedded.PlanGenerationInfo
	fired    bool
	transErr error
}

func (l *nthPlanTransitionLogger) LogPlanGeneration(_ context.Context, info embedded.PlanGenerationInfo) {
	l.mu.Lock()
	l.events = append(l.events, info)
	due := len(l.events) == l.n && !l.fired
	if due {
		l.fired = true
	}
	l.mu.Unlock()
	if !due {
		return
	}
	err := l.fn()
	l.mu.Lock()
	l.transErr = err
	l.mu.Unlock()
}

func (l *nthPlanTransitionLogger) snapshot() []embedded.PlanGenerationInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]embedded.PlanGenerationInfo, len(l.events))
	copy(out, l.events)
	return out
}

func (l *nthPlanTransitionLogger) err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.fired {
		return nil
	}
	return l.transErr
}

// A non-READABLE unique index licenses NOTHING, and the statement still runs.
//
// Both halves are required and they fail in opposite directions. Licensing
// something from a WRITE_ONLY or DISABLED index would elide or narrow a DISTINCT
// on a uniqueness claim the store does not stand behind. Refusing the statement
// would be the other failure: the index is merely unavailable as a PROOF, not as
// a reason to fail — nothing here reads it, so IndexNotReadableError must not
// escape.
func TestFDB_DistinctProof_NonReadableUniqueIndexLicensesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		transition func(*indexStatePlanningFixture, context.Context) error
	}{
		{"write_only", func(f *indexStatePlanningFixture, ctx context.Context) error {
			return f.setIndexState(ctx, "U_EMAIL", false)
		}},
		{"disabled", func(f *indexStatePlanningFixture, ctx context.Context) error {
			return f.setIndexDisabled(ctx, "U_EMAIL")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newIndexStatePlanningFixture(t)
			// BEFORE planning, so the state is the one the candidate filter sees.
			if err := tc.transition(f, ctx); err != nil {
				t.Fatalf("take U_EMAIL %s: %v", tc.name, err)
			}
			logger := &syncCaptureLogger{}
			conn := installLogger(t, f.db, logger)

			for _, query := range []string{
				distinctProofQuery,
				"SELECT DISTINCT EMAIL FROM T WHERE EMAIL IS NOT NULL",
			} {
				explain, got := distinctEmailRun(t, ctx, conn, logger, query)
				if strings.Join(got, ",") != distinctProofWantRows {
					t.Fatalf("%s %q rows = %v, want %s", tc.name, query, got,
						distinctProofWantRows)
				}
				if !strings.Contains(explain, "Distinct(") {
					t.Fatalf("%s: a non-readable unique index eliminated the DISTINCT "+
						"in %q: %s", tc.name, query, explain)
				}
				if strings.Contains(explain, "U_EMAIL") {
					t.Fatalf("%s: a non-readable unique index appears in the plan for "+
						"%q: %s\nIt may license neither an elision, nor a narrowing, "+
						"nor a scan.", tc.name, query, explain)
				}
			}
		})
	}
}

// The transition lands between PLANNING and the first page — two separate
// transactions — and the plan it invalidates does not name the index anywhere
// except in its proof stamp. Without the stamp collected as a dependency the
// statement would return rows deduplicated under a narrowing the store no longer
// backs.
func TestFDB_DistinctProof_TransitionBetweenPlanAndFirstPageFails40001(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &nthPlanTransitionLogger{n: 1, fn: func() error {
		return f.makeUniqueIndexPending(ctx)
	}}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetPlanLogger(logger)
	})

	rows, err := conn.QueryContext(ctx, distinctProofQuery)
	if rows != nil {
		_ = rows.Close()
	}
	if transitionErr := logger.err(); transitionErr != nil {
		t.Fatalf("logger state transition: %v", transitionErr)
	}
	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("planning events = %d, want 1", len(events))
	}
	assertNarrowedByUEmailOnBaseScan(t, events[0].PlanExplain)
	assertStaleIndexDependency(t, err, "U_EMAIL")
}

// The same transition, one page later. An auto-commit statement's pages are
// SEPARATE transactions, so a check made once per statement — a plan-cache key
// included — cannot see this one. The second page must refuse rather than
// continue narrowing under a uniqueness claim that has been withdrawn.
func TestFDB_DistinctProof_TransitionBetweenPagesFails40001(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &syncCaptureLogger{}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).Build())
		ec.SetPlanLogger(logger)
	})

	rows, err := conn.QueryContext(ctx, distinctProofQuery)
	if err != nil {
		t.Fatalf("query first page: %v", err)
	}
	defer func() { _ = rows.Close() }()
	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("planning events = %d, want 1", len(events))
	}
	assertNarrowedByUEmailOnBaseScan(t, events[0].PlanExplain)

	if !rows.Next() {
		t.Fatalf("first buffered row missing: %v", rows.Err())
	}
	var email string
	if err := rows.Scan(&email); err != nil {
		t.Fatalf("scan first buffered row: %v", err)
	}
	if err := f.makeUniqueIndexPending(ctx); err != nil {
		t.Fatalf("transition after first page: %v", err)
	}
	for rows.Next() {
		var more string
		if err := rows.Scan(&more); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Fatalf("a page after the transition returned %q from a stale plan", more)
	}
	assertStaleIndexDependency(t, rows.Err(), "U_EMAIL")
}

// THE CACHE-HIT REGRESSION. Dependencies are a function of the PLAN, so a cache
// hit derives them from the cached plan — which is only possible because the
// proof lives IN the plan.
//
// A side-channel design (a proof-only list threaded out of the fresh-planning
// path) drops the dependency on every cache hit, and the cache hit is precisely
// the long-lived case the revalidation exists for: the first execution guarded,
// every later one unguarded. That shape passes every other test in this file,
// which is why this one asserts the cache actually HIT — a miss here would
// replan against the new state, run correctly, and quietly retire the test.
func TestFDB_DistinctProof_TransitionAgainstCachedPlanFails40001(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	// Fires at the end of the SECOND planning call: the cache lookup has already
	// happened and returned a hit, and the execution transaction has not opened.
	logger := &nthPlanTransitionLogger{n: 2, fn: func() error {
		return f.makeUniqueIndexPending(ctx)
	}}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetPlanLogger(logger)
	})

	// Warm the cache. No transition yet, so this one must succeed.
	got := queryIndexStateStrings(t, ctx, conn, distinctProofQuery)
	if strings.Join(got, ",") != distinctProofWantRows {
		t.Fatalf("warming run rows = %v, want %s", got, distinctProofWantRows)
	}

	rows, err := conn.QueryContext(ctx, distinctProofQuery)
	if rows != nil {
		_ = rows.Close()
	}
	if transitionErr := logger.err(); transitionErr != nil {
		t.Fatalf("logger state transition: %v", transitionErr)
	}

	events := logger.snapshot()
	if len(events) != 2 {
		t.Fatalf("planning events = %d, want 2", len(events))
	}
	assertNarrowedByUEmailOnBaseScan(t, events[0].PlanExplain)
	if events[0].Cache != embedded.PlanCacheMiss {
		t.Fatalf("the warming run was %s, want miss: nothing was cached, so the "+
			"second run cannot be a hit", events[0].Cache)
	}
	if events[1].Cache != embedded.PlanCacheHit {
		t.Fatalf("the second run was %s, want hit: this test only covers the "+
			"cache-hit dependency path when the cache actually hits", events[1].Cache)
	}
	assertNarrowedByUEmailOnBaseScan(t, events[1].PlanExplain)
	assertStaleIndexDependency(t, err, "U_EMAIL")
}

// BOTH LICENSES HOLD → NO STAMP, and therefore NO 40001.
//
// This is the criterion that fails the conservative-looking implementation —
// stamp whenever a qualifying unique index exists — which every other test in
// this file passes. When an UNCONDITIONAL license already justifies the elision,
// recording a dependency on a mutable index state makes the plan's correctness
// rest on something it does not rest on, and transitioning that index then fails
// a statement that would have been correct regardless. That is the same
// over-scoping as an unscoped signature, arrived at from the other direction:
// an ordinary index build turning into statement failures.
//
// Every arm projects EMAIL, so U_EMAIL qualifies as a proof in all of them, and
// every arm projects more than EMAIL so the access path stays a base-record scan
// — moving onto the covering U_EMAIL would give the plan a REAL dependency and
// make the assertion untestable rather than false.
//
// The NULL-rejecting conjunct is not decoration and the test is worthless
// without it. EMAIL is nullable, so on an unfiltered stream the secondary-UNIQUE
// proof is only the R3 RESIDUAL, and an implementation that stamps whenever a
// unique index qualifies still yields an unstamped plan here — the residual
// never reaches the stamp because an unconditional license returns first. The
// conjunct empties the exempt set, which is what makes U_EMAIL a FULL-ELISION
// candidate and puts the two licenses in genuine competition. Both query shapes
// are kept per arm because they catch different over-eager orderings: one that
// consults the full elision first, and one that consults any qualifying index
// first.
//
// What differs across arms is the license that must arrive FIRST: whole-record
// distinctness for SELECT DISTINCT *, and primary-key coverage once ID is
// projected. Neither can be withdrawn by a state transition, so neither may
// stamp.
//
// Stated so a later reader does not over-credit an arm: only the PRIMARY-KEY
// arms discriminate the over-eager implementation, and that is structural rather
// than an accident of these queries. The record-distinctness license and the
// secondary-UNIQUE proof cannot compete on the SQL path at all — a SQL
// projection sets the distinct-records property to false by construction
// (plan_properties.go, visitProjectionPlan), and the secondary-UNIQUE proof is
// gathered only from a logical PROJECTION member, so a projection-less SELECT
// DISTINCT * computes no proof for an over-eager ordering to prefer. The
// record-distinctness arms are kept because the elision and its survival across
// a transition are real properties worth pinning, not because they can catch a
// mis-ordering.
func TestFDB_DistinctProof_UnconditionalLicenseYieldsUnstampedPlan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		query   string
		license string
	}{
		{"whole_record", "SELECT DISTINCT * FROM T", "record-level distinctness"},
		{
			"whole_record_null_rejected",
			"SELECT DISTINCT * FROM T WHERE EMAIL IS NOT NULL",
			"record-level distinctness",
		},
		{"primary_key", "SELECT DISTINCT ID, EMAIL, PAD FROM T", "primary-key coverage"},
		{
			"primary_key_null_rejected",
			"SELECT DISTINCT ID, EMAIL, PAD FROM T WHERE EMAIL IS NOT NULL",
			"primary-key coverage",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newIndexStatePlanningFixture(t)
			logger := &syncCaptureLogger{}
			conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
				ec.SetOptions(api.NewOptionsBuilder().
					Set(api.OptExecutionScannedRowsLimit, 1).Build())
				ec.SetPlanLogger(logger)
			})

			rows, err := conn.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer func() { _ = rows.Close() }()

			events := logger.snapshot()
			if len(events) != 1 {
				t.Fatalf("planning events = %d, want 1", len(events))
			}
			explain := events[0].PlanExplain
			if strings.Contains(explain, "IndexScan") {
				t.Fatalf("the access path moved onto an index: %s\nA U_EMAIL "+
					"dependency would then be REAL rather than a stamp, and this "+
					"arm could no longer tell the two apart.", explain)
			}
			if strings.Contains(explain, "U_EMAIL") {
				t.Fatalf("%s already licenses this elision, yet the plan records a "+
					"dependency on U_EMAIL: %s\nStamping here makes an ordinary "+
					"index build fail a statement whose correctness never rested on "+
					"that index.", tc.license, explain)
			}
			if strings.Contains(explain, "Distinct(") {
				t.Fatalf("%s did not license the elision, so this arm no longer "+
					"exercises two licenses holding at once: %s", tc.license, explain)
			}

			if err := f.setIndexState(ctx, "U_EMAIL", false); err != nil {
				t.Fatalf("take U_EMAIL WRITE_ONLY mid-statement: %v", err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				// Naming WHICH failure was caught: a 40001 is the bug this test
				// exists for, anything else is a broken fixture, and collapsing
				// them would report the fixture breaking as the bug.
				var apiErr *api.Error
				if errors.As(err, &apiErr) &&
					apiErr.Code == api.ErrCodeSerializationFailure {
					t.Fatalf("transitioning U_EMAIL invalidated a plan licensed by "+
						"%s, which no index state can withdraw: %v", tc.license, err)
				}
				t.Fatalf("statement failed after an unrelated transition: %v", err)
			}
			if n != 3 {
				t.Fatalf("rows = %d, want 3 across separate pages", n)
			}
		})
	}
}
