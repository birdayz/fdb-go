package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"sort"
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

// assertNarrowedByUEmailOnBaseScan pins the precondition every index-state test
// in this file rests on: the plan is narrowed by U_EMAIL and does NOT scan it.
// If the access path ever moves onto U_EMAIL these tests keep passing while
// testing the ordinary index-scan dependency instead of the stamp, which is the
// silent way this file stops covering what it claims to.
func assertNarrowedByUEmailOnBaseScan(t *testing.T, explain string) {
	t.Helper()
	if !strings.Contains(explain, "narrowed-by:U_EMAIL") {
		t.Fatalf("the plan is not narrowed by U_EMAIL, so it has no proof-only "+
			"dependency on it and this test's claim about that dependency — that it "+
			"refuses, or that it survives — would be vacuous: %s", explain)
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

// singleReadVersionBrokenMessage names, once, what a failure of either
// invisibility pin below actually means. Both fire a REAL index-state transition
// strictly inside a statement whose plan carries a proof stamp on U_EMAIL, so a
// 40001 out of either one is not a stray error: it says the statement observed
// two different store states, which is the premise RFC-210's proof denies.
const singleReadVersionBrokenMessage = "The statement observed a mid-statement " +
	"index-state transition, so it did NOT run at one read version. That is the " +
	"property PlannerConfiguration.SingleReadVersion asserts, and it is the ONLY " +
	"reason the secondary-UNIQUE proof is admissible here: re-arming this scenario " +
	"re-arms a plan whose DISTINCT was narrowed on a uniqueness claim the store " +
	"withdrew mid-flight. Fix the read-version scoping; do not relax this test."

// drainDistinctProofRows reads a result set to completion and returns its values
// sorted, so an assertion can name the ANSWER and not only the error.
func drainDistinctProofRows(t *testing.T, rows *sql.Rows) []string {
	t.Helper()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, value)
	}
	sort.Strings(got)
	return got
}

// A transition landing between PLANNING and the FIRST PAGE is INVISIBLE inside
// an explicit transaction — by construction, not by luck.
//
// The proof is drawn only under PlannerConfiguration.SingleReadVersion, which is
// set from the connection's explicit transaction (planSelectCascades). Inside one
// transaction, planning and every page read at ONE read version: planning's
// index-state store open takes it, and every page reuses that same
// transaction-scoped store (connection.go's storeIn). A transition committed by
// another transaction after planning is therefore not in this statement's
// snapshot, and the per-page dependency revalidation — which still runs, against
// that same snapshot — has nothing to refuse.
//
// So this pins a NEGATIVE result, and the negative is what licenses the design:
// it is why "the transition arrived after we planned" is not a wrong-rows hazard
// for an in-transaction statement. It replaces an assertion that this shape
// raises 40001, which was written while the proof was drawn in AUTO-COMMIT —
// where planning and each page are separate transactions and the hazard is real.
//
// It is NOT vacuous: the plan asserted below carries `narrowed-by:U_EMAIL`, so
// the statement genuinely holds a proof-only dependency on the index being
// transitioned, and the revalidation genuinely evaluates it on every page.
func TestFDB_DistinctProof_TransitionAfterPlanningIsInvisibleInOneTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	// Fires at the end of the statement's ONLY planning call: after the plan and
	// its proof stamp exist, before the first page produces a row.
	logger := &nthPlanTransitionLogger{n: 1, fn: func() error {
		return f.makeUniqueIndexPending(ctx)
	}}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetPlanLogger(logger)
	})

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, queryErr := tx.QueryContext(ctx, distinctProofQuery)
	if transitionErr := logger.err(); transitionErr != nil {
		t.Fatalf("logger state transition: %v", transitionErr)
	}
	if queryErr != nil {
		t.Fatalf("a transition landing after planning failed an in-transaction "+
			"statement: %v\n%s", queryErr, singleReadVersionBrokenMessage)
	}
	defer func() { _ = rows.Close() }()

	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("planning events = %d, want 1", len(events))
	}
	// Without the stamp there is no proof-only dependency on U_EMAIL and this
	// test pins nothing: any statement survives a transition it never depended on.
	assertNarrowedByUEmailOnBaseScan(t, events[0].PlanExplain)

	got := drainDistinctProofRows(t, rows)
	if err := rows.Err(); err != nil {
		t.Fatalf("a transition landing after planning failed an in-transaction "+
			"statement mid-drain: %v\n%s", err, singleReadVersionBrokenMessage)
	}
	if strings.Join(got, ",") != distinctProofWantRows {
		t.Fatalf("rows = %v, want %s", got, distinctProofWantRows)
	}
}

// The same transition, one PAGE later, and invisible for the same reason: an
// explicit transaction's pages are not separate transactions, so there is no
// second read version for the transition to become visible at.
//
// This is the arm that most obviously changed meaning. In auto-commit each page
// IS a transaction, a transition between two of them is real, and the second page
// must refuse — which the sibling
// TestFDB_IndexStatePlanning_TransitionBetweenPagesFails40001 still pins, on a
// plan that SCANS U_EMAIL. Under the read-version gate a proof-carrying plan
// cannot arise in auto-commit at all, so the shape that reaches here is the
// in-transaction one, where the correct outcome is that the statement finishes
// with every value exactly once.
//
// The one-row page limit is what makes this a claim about PAGES: a leaf cursor
// gets one free record per page (key_value_cursor.go's per-cursor initial pass)
// while the record budget itself is transaction-scoped, so with the limit at 1
// every page yields exactly one row and three rows cannot come from one page. The
// sibling test named above is the sentinel for that fact — if the limit ever
// stopped forcing page boundaries it goes red, rather than this one going quietly
// vacuous.
func TestFDB_DistinctProof_TransitionBetweenPagesIsInvisibleInOneTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &syncCaptureLogger{}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).Build())
		ec.SetPlanLogger(logger)
	})

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, distinctProofQuery)
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
	// Committed by a DIFFERENT transaction, strictly between two pages of this
	// one; every later page still reads the snapshot the statement started from.
	if err := f.makeUniqueIndexPending(ctx); err != nil {
		t.Fatalf("transition after first page: %v", err)
	}

	got := append([]string{email}, drainDistinctProofRows(t, rows)...)
	if err := rows.Err(); err != nil {
		t.Fatalf("a transition landing between two pages of ONE transaction "+
			"failed the statement: %v\n%s", err, singleReadVersionBrokenMessage)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != distinctProofWantRows {
		t.Fatalf("rows across pages spanning the transition = %v, want %s\n"+
			"A short or duplicated result is the OTHER way the single-read-version "+
			"property fails: the pages disagreed about the store.",
			got, distinctProofWantRows)
	}
}

// THE PROTECTION THAT ACTUALLY RUNS when an index transitions between two
// transactions on one connection: the second statement does NOT get the cached
// proof-carrying plan, because the readable-index view is part of the plan-cache
// key (plannerOptions.cacheKeyPart) and the transition changed it. It replans and
// gets an honest, unstamped plan.
//
// This is worth pinning precisely because it is the mechanism a reader expects
// execution-time revalidation to provide, and does not. Under the read-version
// gate a proof exists only inside an explicit transaction, where planning and
// every page share ONE read version — so the two cases are exhaustive and
// neither leaves a stale proof running:
//
//   - the transition is visible to the next statement's planning, so the cache
//     key differs and the plan is rebuilt (this test); or
//   - it is not visible to that planning, in which case it is not visible to
//     that statement's execution either, and the proof still holds at the read
//     version the whole statement runs at (the two invisibility pins above).
//
// If this test ever reports a cache HIT, that third case has appeared: a plan
// whose proof rests on an index state the store has withdrawn, served to a
// statement at a read version that can see the withdrawal. Nothing downstream
// would catch it — that is what the sibling cache-hit test below asks the
// execution-time check to do.
func TestFDB_DistinctProof_TransitionBetweenTransactionsReplansTheProofAway(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &syncCaptureLogger{}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetPlanLogger(logger)
	})

	// Warm the connection's plan cache from inside a transaction, which is the
	// only regime that draws the proof at all.
	got := queryIndexStateStrings(t, ctx, conn, distinctProofQuery)
	if strings.Join(got, ",") != distinctProofWantRows {
		t.Fatalf("warming run rows = %v, want %s", got, distinctProofWantRows)
	}
	warm := logger.snapshot()
	if len(warm) != 1 {
		t.Fatalf("planning events after warming = %d, want 1", len(warm))
	}
	assertNarrowedByUEmailOnBaseScan(t, warm[0].PlanExplain)

	// Strictly BETWEEN the two transactions: the next statement's planning reads
	// a store state that already has it.
	if err := f.makeUniqueIndexPending(ctx); err != nil {
		t.Fatalf("transition between transactions: %v", err)
	}

	got = queryIndexStateStrings(t, ctx, conn, distinctProofQuery)
	if strings.Join(got, ",") != distinctProofWantRows {
		t.Fatalf("post-transition rows = %v, want %s", got, distinctProofWantRows)
	}
	events := logger.snapshot()
	if len(events) != 2 {
		t.Fatalf("planning events = %d, want 2", len(events))
	}
	if events[1].Cache != embedded.PlanCacheMiss {
		t.Fatalf("the post-transition run was %s: the connection served the plan "+
			"cached before the transition, whose DISTINCT is narrowed on U_EMAIL's "+
			"withdrawn uniqueness claim: %s\nThe readable-index view must stay part "+
			"of the plan-cache key; without it nothing else can catch this, because "+
			"a statement inside one transaction cannot observe a transition that "+
			"preceded its own read version.", events[1].Cache, events[1].PlanExplain)
	}
	if strings.Contains(events[1].PlanExplain, "U_EMAIL") {
		t.Fatalf("the replanned statement still rests on a READABLE_UNIQUE_PENDING "+
			"index: %s", events[1].PlanExplain)
	}
	if !strings.Contains(events[1].PlanExplain, "Distinct(") {
		t.Fatalf("the replanned statement has no dedup operator, so the withdrawn "+
			"proof was replaced by nothing: %s", events[1].PlanExplain)
	}
}

// carriesUEmailProof reports whether an EXPLAIN still advertises U_EMAIL as the
// licence for a DISTINCT decision — either arm of the proof: `narrowed-by:` for
// the R3 residual dedup, `distinct-by:` for a full R2 elision. Those two strings
// are the ONLY places a plan admits that its dedup rests on a secondary UNIQUE
// index, so they are what "still stamped" has to mean here.
func carriesUEmailProof(explain string) bool {
	return strings.Contains(explain, "narrowed-by:U_EMAIL") ||
		strings.Contains(explain, "distinct-by:U_EMAIL")
}

// runDistinctProofInTx runs the proof query inside an explicit transaction and
// returns the rows and the error WITHOUT failing the test on either. A refusal
// is a legitimate outcome for the disjunction below, so the usual helper — which
// fatals on any query error — cannot express it.
func runDistinctProofInTx(t *testing.T, ctx context.Context, conn *sql.Conn) ([]string, error) {
	t.Helper()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, distinctProofQuery)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var value string
		if scanErr := rows.Scan(&value); scanErr != nil {
			t.Fatalf("scan: %v", scanErr)
		}
		got = append(got, value)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return got, rowsErr
	}
	sort.Strings(got)
	return got, nil
}

// A WITHDRAWN PROOF IS NEVER SILENTLY SERVED. This pins the disjunction the
// system actually guarantees, which is weaker than either single mechanism and
// is the thing that is actually true.
//
// After an index-state transition that withdraws the proof, landing strictly
// BETWEEN two transactions on one connection, the next statement must either
//
//	(a) REPLAN — the readable-index view is part of the plan-cache key
//	    (plannerOptions.cacheKeyPart), so the transition changed the key, the
//	    lookup misses, and the rebuilt plan carries no proof on the transitioned
//	    index; or
//	(b) REFUSE — SQLSTATE 40001 naming the stale dependency, from the
//	    execution-time dependency revalidation.
//
// What it must NEVER do is the third thing: return rows from a plan still
// stamped with the withdrawn index.
//
// So the FORBIDDEN outcome is the assertion, not either arm. Asserting a
// specific arm is what made the predecessor of this test unreachable: it demanded
// a cache HIT and a 40001 together, and there is no window in which both occur.
// A transition visible to the next planning call changes the cache key and forces
// a replan; a transition NOT visible to that planning call is equally invisible to
// that statement's execution, since the two share one read version. There is no
// third window, so "hit AND 40001" was asserting a state the design excludes.
//
// (a) is what a healthy tree does today. (b) is the backstop that fires if the
// cache key ever stops including the readable-index set — which is exactly the
// mutation that proves these are two independent layers rather than one dressed
// as two. The sibling
// TestFDB_DistinctProof_TransitionBetweenTransactionsReplansTheProofAway pins (a)
// concretely, so the specific mechanism is not left unpinned by this test's
// deliberate agnosticism about which arm fires.
func TestFDB_DistinctProof_WithdrawnProofIsNeverSilentlyServed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &syncCaptureLogger{}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetPlanLogger(logger)
	})

	// Warm the connection's plan cache from inside a transaction, the only regime
	// that draws the proof at all under the read-version gate.
	got, err := runDistinctProofInTx(t, ctx, conn)
	if err != nil {
		t.Fatalf("warming run: %v", err)
	}
	if strings.Join(got, ",") != distinctProofWantRows {
		t.Fatalf("warming run rows = %v, want %s", got, distinctProofWantRows)
	}
	warm := logger.snapshot()
	if len(warm) != 1 {
		t.Fatalf("planning events after warming = %d, want 1", len(warm))
	}
	if warm[0].Cache != embedded.PlanCacheMiss {
		t.Fatalf("the warming run was %s, want miss: nothing was cached before it, "+
			"so a later HIT could not be attributed to this plan", warm[0].Cache)
	}
	// Non-vacuity: without a stamped plan in the cache there is no proof for the
	// transition to withdraw, and the disjunction below holds for a statement that
	// never depended on U_EMAIL in the first place.
	assertNarrowedByUEmailOnBaseScan(t, warm[0].PlanExplain)

	// Strictly BETWEEN the two transactions, so the next statement's planning and
	// its execution both run at a read version that can see the withdrawal.
	if err := f.makeUniqueIndexPending(ctx); err != nil {
		t.Fatalf("transition between transactions: %v", err)
	}

	got, err = runDistinctProofInTx(t, ctx, conn)
	events := logger.snapshot()
	if len(events) != 2 {
		t.Fatalf("planning events = %d, want 2", len(events))
	}
	second := events[1]

	if carriesUEmailProof(second.PlanExplain) && err == nil && len(got) > 0 {
		t.Fatalf("SILENT WRONG ROWS: a plan still stamped with the WITHDRAWN "+
			"U_EMAIL proof returned %d rows with no error.\n"+
			"cache=%s plan=%s\n"+
			"U_EMAIL is READABLE_UNIQUE_PENDING — the store no longer stands behind "+
			"the uniqueness this plan's DISTINCT decision rests on, so these rows may "+
			"contain duplicates the query asked to be rid of.\n"+
			"This is precisely the case BOTH layers exist to prevent, and reaching it "+
			"means BOTH failed: the readable-index section of "+
			"plannerOptions.cacheKeyPart did not force a replan, AND the "+
			"execution-time dependency revalidation did not refuse. Either one alone "+
			"would have caught it. Fix whichever regressed; do not relax this test.",
			len(got), second.Cache, second.PlanExplain)
	}

	// Beyond the forbidden outcome, whichever arm fired must have fired properly.
	if err != nil {
		// Arm (b): a refusal has to name the dependency that moved, or the operator
		// is sent to the wrong index.
		assertStaleIndexDependency(t, err, "U_EMAIL")
		return
	}
	// Arm (a): replanned, so the answer must still be the correct one.
	if strings.Join(got, ",") != distinctProofWantRows {
		t.Fatalf("the replanned statement returned %v, want %s: %s",
			got, distinctProofWantRows, second.PlanExplain)
	}
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
