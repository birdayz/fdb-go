//go:build bazelrunfiles

package conformance_test

// Helpers of the runSql harness and the cross-engine corpus runs: the retry,
// lifecycle and conformance predicates run_sql_conformance_test.go and
// yamsql_cross_engine_conformance_test.go drive, and that the predicates'
// own unit tests (divergence_holds_test.go, entry_conforms_test.go,
// lifecycle_error_test.go, transient_fdb_error_test.go) and
// java_facts_conformance_test.go call. A file of its own so the spec can run
// in conformance_corpora_test and the Java-only probes of rfc257_parity_test
// while its callers stay in conformance_test.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
)

// seedCorpusParallelism is the number of (fresh Java server + Go runner) workers
// the SeedRunCorpus loop fans out across. Each worker drives a disjoint subset
// of the ~1620 corpus entries on its OWN pre-spawned Java server (so there is no
// concurrent spawning while queries run, and per-server load is LOWER than the
// single-server serial baseline that is already green). Speeds the loop ~Nx at
// the cost of N live JVMs for its duration. Override with
// CONFORMANCE_SEED_PARALLELISM; default 8.
func seedCorpusParallelism() int {
	if v := os.Getenv("CONFORMANCE_SEED_PARALLELISM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8
}

// maxConflictRetries bounds the backoff-retry below.
const maxConflictRetries = 16

// isTransientFDBError reports whether err is an FDB error that is retryable BY
// DESIGN and therefore says nothing about SQL semantics. Two codes reach the
// harness, both only from the Java side (the embedded Go engine retries them
// internally, which is why one engine surfaces what the other absorbs):
//
//   - 1020 "not committed due to conflict". Running the corpus in parallel makes
//     many workers create their ephemeral schema at once, and those CREATEs
//     contend on the shared relational catalog keyspace.
//   - 1007 "transaction is too old". FDB's 5s transaction limit is WALL-CLOCK,
//     not work: under CI load a corpus entry whose whole dataset is a handful of
//     rows can still exceed it while descheduled. Treating it as an engine
//     answer manufactures a phantom "Java errored but Go succeeded" divergence
//     on a query neither engine actually disagreed about.
//
// This is the class Java itself calls retriable — FDBExceptions.isRetriable
// (FDBExceptions.java:233) delegates to FDBException.isRetryable(), which is
// true for both codes. The fix is the same for both: re-run that side until it
// gets a transaction that survives, after which the cross-engine result matches
// a serial run. An error that persists across every attempt still lands in the
// divergence report, so a Java side that genuinely cannot finish stays red.
func isTransientFDBError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not committed due to conflict") ||
		strings.Contains(msg, "Transaction is too old to perform reads or be committed")
}

// javaInfraFailure reports whether a Java-side error is an INFRASTRUCTURE
// signal rather than a statement about query semantics, and returns the label
// to report it under.
//
// Two shapes qualify. An error that is not a *plandiff.JavaError never reached
// Java's SQL engine at all — the transport paths (POST failure, non-200,
// body/JSON decode) return plain wrapped errors. And a JavaError whose
// exception class is a DEADLINE/TIMEOUT is the server giving up on the clock:
// measured on this repo, running the full bazel suite concurrently with another
// Docker-spinning job starves the conformance server enough to raise
// DeadlineExceededException on an arbitrary corpus entry (the same target
// passes in isolation in roughly half the wall time). A deadline is never
// evidence about rows or plans.
//
// This does NOT downgrade the run — a sick server must not silently pass, and
// both callers still report the entry as failing. It only fixes the LABEL, so a
// timeout is never announced as "Go's behaviour no longer matches the pinned
// divergence" and nobody spends a shift hunting a semantic change that did not
// happen.
func javaInfraFailure(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	var je *plandiff.JavaError
	if !errors.As(err, &je) {
		return true, "conformance-server call failed (INFRA, not engine behaviour): " + err.Error()
	}
	switch je.ExceptionClass {
	case "DeadlineExceededException", "TimeoutException", "SocketTimeoutException":
		return true, "conformance server exceeded its deadline (INFRA, not engine behaviour; " +
			"check for a concurrent Docker/bazel job starving it): " + err.Error()
	}
	return false, ""
}

// maxLifecycleRetries bounds re-runs for the LIFECYCLE class. It is far lower
// than maxConflictRetries because a lifecycle re-run can cost a whole HTTP
// client timeout (30s) per attempt: sixteen of those would blow the suite's own
// deadline, and a conformance server still unreachable after three spaced
// attempts is sick rather than momentarily busy.
const maxLifecycleRetries = 3

// isLifecycleError reports whether err is a HARNESS-LIFECYCLE failure — a
// wall-clock deadline blown, a connection torn down, or a transaction context
// closed underneath the statement — rather than either engine's answer about
// SQL semantics. Under a loaded machine (eight pooled JVMs, a shared FDB
// container and the in-process Go engine all competing) these fire on queries
// that neither engine disagrees about, and the harness used to publish them as
// "NEW cross-engine divergence", sending readers after a phantom regression.
//
// Every arm is an EXACT signature, and narrowness is the whole safety property:
// this class must be unable to express a row or plan disagreement. An unknown
// error is never lifecycle, so a genuine engine defect cannot hide here.
//
// The arms, each observed verbatim in a red conformance run:
//
//   - Transport (net.Error): "plandiff: HTTP POST: … context deadline exceeded".
//     The conformance server was unreachable or too slow to answer at all, so
//     Java never rendered a verdict. Same class the rowdiff harness already
//     calls INFRA (rowdiff/run.go isInfraError). This arm is not scoped to one
//     side: a net.Error is a socket that failed, and a socket cannot express a
//     row or plan disagreement, so it is infra wherever it surfaces.
//   - java DeadlineExceededException "deadline exceeded". Java's
//     MoreAsyncUtil.getWithDeadline fired the AsyncLoadingCache deadline —
//     DEFAULT_DEADLINE_TIME_MILLIS is 5000 (AsyncLoadingCache.java:45), a
//     WALL-CLOCK bound on the resolver-state load, not on the query's work.
//     Exactly the 1007 rationale one layer up.
//   - java RecordContextNotActiveException "Transaction is no longer active."
//     FDBRecordContext.ensureActive (FDBRecordContext.java:548) found a closed
//     context. Java's own relational layer classifies this as a lifecycle
//     outcome, not a semantic one: ExceptionUtil maps it to
//     ErrorCode.TRANSACTION_INACTIVE (ExceptionUtil.java:66), alongside
//     TRANSACTION_TIMEOUT and away from every semantic code.
//   - java FDBException "Transaction may or may not have committed" — FDB 1021
//     commit_unknown_result, retryable by FDB's own definition and, like 1020
//     and 1007 above, carrying no information about SQL semantics. Safe to
//     re-run only because each attempt builds a FRESH uuid-suffixed ephemeral
//     schema, so a re-attempt is clean rather than a double-apply.
//   - Go *plandiff.FixtureError whose cause is a pure-Go client connection
//     teardown (isConnTeardown). BOTH conditions are required. The FixtureError
//     half means the failure hit the ephemeral DDL/setup that BUILDS the
//     fixture, so the Go engine never answered the query and there is nothing
//     to compare; a Go error on the query itself is not a FixtureError and
//     stays a divergence. The teardown half means the pure-Go client's
//     connection was torn down underneath it (transport/conn.go), not that Go
//     rejected the DDL — without it, a Go-only DDL gap during setup would be
//     swallowed as infra. The teardown travels in two shapes (see
//     isConnTeardown), and the arm must match both.
//
// A lifecycle error is RETRIED, not suppressed. One that survives every attempt
// still reaches the report — labelled INFRA so nobody chases an engine bug, but
// red, because a harness that cannot run is not a harness that passed.
func isLifecycleError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var fe *plandiff.FixtureError
	if errors.As(err, &fe) && isConnTeardown(err) {
		return true
	}
	var je *plandiff.JavaError
	if !errors.As(err, &je) {
		return false
	}
	switch je.ExceptionClass {
	case "DeadlineExceededException":
		return je.Message == "deadline exceeded"
	case "RecordContextNotActiveException":
		return je.Message == "Transaction is no longer active."
	case "FDBException":
		return je.Message == "Transaction may or may not have committed"
	}
	return false
}

// isConnTeardown reports whether err carries a pure-Go client connection
// teardown, in EITHER of the two shapes it actually travels:
//
//   - pre-facade (transport/client layers): the transport's ConnClosedError,
//     matched by sentinel identity (errors.Is transport.ErrConnClosed) or by
//     its coded *wire.FDBError 1030 request_maybe_delivered.
//   - post-facade: fdb.Error{Code: 1030}. convertError (fdb/transaction.go)
//     rebuilds the error as a VALUE type carrying only the code — the wrap
//     chain, and with it the errors.Is identity to the sentinel, does NOT
//     survive the facade. The 1030 code is the only structural handle left,
//     and it is unambiguous: in the pure-Go client 1030 originates ONLY from
//     a connection teardown (request_maybe_delivered is client-side RPC
//     machinery in C++ too — fdbrpc.h waitValueOrSignal — never an in-band
//     storage-server reply). TestConvertError_ConnTeardownShape
//     (pkg/fdbgo/fdb) pins that this is the exact shape the facade emits.
//
// Matching only the sentinel identity would make the arm unsatisfiable for
// every real facade-crossing teardown — exactly the class of error this
// classifier exists to keep red-but-INFRA.
func isConnTeardown(err error) bool {
	if errors.Is(err, transport.ErrConnClosed) {
		return true
	}
	var wireErr *wire.FDBError
	if errors.As(err, &wireErr) && wireErr.Code == connTeardownCode {
		return true
	}
	var facadeErr gofdb.Error
	return errors.As(err, &facadeErr) && facadeErr.Code == connTeardownCode
}

// connTeardownCode is FDB 1030 request_maybe_delivered
// (flow/error_definitions.h:57, release-7.3) — the code the pure-Go transport
// stamps on every connection teardown.
const connTeardownCode = 1030

// lifecycleDetail returns the INFRA report line for whichever side hit a
// lifecycle failure, naming the side so a reader knows where to look. Shared by
// entryConforms and divergenceHolds: both used to translate a dead harness into
// a claim about the ENGINES — one as "Java errored but Go succeeded", the other
// as a stale RFC-082 annotation — and neither claim was evidence-backed.
func lifecycleDetail(javaErr, goErr error) (string, bool) {
	if isLifecycleError(javaErr) {
		return "Java-side harness lifecycle failure (INFRA, not engine behaviour): " + javaErr.Error(), true
	}
	if isLifecycleError(goErr) {
		return "Go-side harness lifecycle failure (INFRA, not engine behaviour): " + goErr.Error(), true
	}
	return "", false
}

// retryBudget returns how many times the harness may re-run the side that
// produced err. Zero means "this is an engine answer — report it".
func retryBudget(err error) int {
	switch {
	case isTransientFDBError(err):
		return maxConflictRetries
	case isLifecycleError(err):
		return maxLifecycleRetries
	default:
		return 0
	}
}

// runJavaRetrying runs a Java-only fixture (template, setup, statement) under
// rerunWhileRetryable, as the corpus runs do: a retryable FDB code or a
// lifecycle failure reruns it on a fresh schema rather than being reported.
func runJavaRetrying(ctx context.Context, rn plandiff.SetupRunner, schema string, setup []string, sql string) plandiff.RunResult {
	run := func() plandiff.RunResult { return rn.RunWithSetup(ctx, schema, setup, sql) }
	jr, _ := rerunWhileRetryable(run(), plandiff.RunResult{}, 0, run, func() plandiff.RunResult { return plandiff.RunResult{} })
	return jr
}

// rerunWhileRetryable re-runs whichever side hit a retryable HARNESS error (a
// retryable FDB code, or a lifecycle failure) until it produces an engine answer
// or its budget runs out. Each side is re-run independently and only while its
// OWN budget allows, so a cheap FDB conflict still gets its sixteen attempts
// while an expensive transport timeout gets three.
//
// Backoff rises with the attempt and is staggered per worker (wid), so two
// workers that collided on one attempt do not retry in lockstep and collide
// again. Every RunWithSetup builds a fresh uuid-suffixed ephemeral schema, so a
// re-run is a clean re-attempt rather than a replay onto dirty state.
func rerunWhileRetryable(jr, gr plandiff.RunResult, wid int,
	runJava, runGo func() plandiff.RunResult,
) (plandiff.RunResult, plandiff.RunResult) {
	for attempt := 1; attempt <= maxConflictRetries; attempt++ {
		javaBudget, goBudget := retryBudget(jr.Err), retryBudget(gr.Err)
		if attempt > javaBudget && attempt > goBudget {
			break
		}
		time.Sleep(time.Duration(attempt)*40*time.Millisecond + time.Duration(wid)*11*time.Millisecond)
		if attempt <= javaBudget {
			jr = runJava()
		}
		if attempt <= goBudget {
			gr = runGo()
		}
	}
	return jr, gr
}

// entryConforms reports whether Go's result conforms to Java's for a
// non-annotated corpus entry: a matching server-side root error message when
// Java errors, or conforming column metadata (plandiff.ConformColumns) plus
// byte-equal rows when Java succeeds. Returns a human-readable detail on
// non-conformance. This is the predicate the RFC-082 regression lock reconciles
// against rfc082KnownRed.
func entryConforms(javaResult, goResult plandiff.RunResult) (bool, string) {
	// Lifecycle first, on BOTH sides and before any engine comparison: a side
	// that was killed by a blown deadline, a torn-down connection or a closed
	// transaction context never rendered a verdict, so there is no verdict to
	// disagree with. These have already been re-run to their budget by
	// rerunWhileRetryable, so reaching here means the condition PERSISTED —
	// still red, never silently passed, but named as infra so the response is
	// decidable from the CI log alone.
	if detail, ok := lifecycleDetail(javaResult.Err, goResult.Err); ok {
		return false, detail
	}
	if javaResult.Err != nil {
		// An error that is NOT a *plandiff.JavaError never came from Java's
		// SQL engine — the transport paths (HTTP POST failure, non-200,
		// body/JSON decode) return plain wrapped errors (httpclient.go). A
		// slow/unreachable conformance server under CI load is an INFRA
		// failure, not engine behaviour: report it as such — still red
		// (a sick server must not silently pass) but never classified as a
		// cross-engine divergence, so nobody chases a phantom engine
		// regression off a timeout.
		if infra, detail := javaInfraFailure(javaResult.Err); infra {
			return false, detail
		}
		if goResult.Err == nil {
			// Java errored, Go succeeded. With the conformance server's plan
			// cache disabled (sql_plan_steps.java) the Java result is
			// deterministic: an UnableToPlanException here means Java's
			// Cascades planner genuinely has no plan for this query (it is
			// thrown only on finalExpressions.isEmpty() AFTER full
			// exploration — budget exhaustion throws
			// RecordQueryPlanComplexityException instead). That is a real,
			// reproducible Go read-side extension, NOT planner noise, so it
			// must be declared via an rfc082Divergences annotation
			// (DivergenceJavaErrorsGoCorrect), not silently swallowed here.
			// The Java error rides along so a red names the exception —
			// UnableToPlan vs ComplexityException vs anything else decides
			// the response, and the classification must be checkable from
			// the CI log alone.
			return false, "Java errored but Go succeeded: " + javaResult.Err.Error()
		}
		var je *plandiff.JavaError
		if !errors.As(javaResult.Err, &je) {
			return false, fmt.Sprintf("Java error is %T (not *plandiff.JavaError)", javaResult.Err)
		}
		var ge *api.Error
		if !errors.As(goResult.Err, &ge) {
			return false, fmt.Sprintf("Go error is %T (not *api.Error)", goResult.Err)
		}
		goRootMsg := ge.Message
		for cause := ge.Unwrap(); cause != nil; {
			var inner *api.Error
			if !errors.As(cause, &inner) {
				break
			}
			goRootMsg = inner.Message
			cause = inner.Unwrap()
		}
		if goRootMsg != je.Message {
			return false, fmt.Sprintf("error messages diverge: java=%q go=%q", je.Message, goRootMsg)
		}
		return true, ""
	}
	if goResult.Err != nil {
		return false, "Java succeeded but Go errored: " + goResult.Err.Error()
	}
	if detail, ok := plandiff.ConformColumns(goResult.Rows.Columns, javaResult.Rows.Columns); !ok {
		return false, "column metadata: " + detail
	}
	if !reflect.DeepEqual(goResult.Rows.Rows, javaResult.Rows.Rows) {
		return false, "row data diverges"
	}
	return true, ""
}

// divergenceHolds reports whether a corpus entry's RFC-082 Divergence annotation
// still describes reality. The conformance server runs with its plan cache
// disabled (sql_plan_steps.java) and the cross-engine corpus runs on a dedicated
// isolated server, so Java's behaviour is deterministic — the gate asserts BOTH
// the annotation's Java premise AND Go's pinned behaviour. A drift on either side
// returns false so the lock reports it rather than letting a stale annotation rot.
func divergenceHolds(div *plandiff.Divergence, query string, javaResult, goResult plandiff.RunResult) (bool, string) {
	// Same gate as entryConforms, for the same reason. An annotation's premise
	// ("Java errors here", "Java succeeds with wrong rows") can only be
	// confirmed or refuted by a side that actually ran; a Java server that timed
	// out is not Java disagreeing with the annotation. Without this, a loaded CI
	// run reported live annotations as STALE and invited someone to delete a
	// correct pin.
	if detail, ok := lifecycleDetail(javaResult.Err, goResult.Err); ok {
		return false, detail
	}
	// And the transport arm entryConforms also carries: an error that is not a
	// *plandiff.JavaError never reached Java's SQL engine at all, so it can
	// neither confirm nor refute the pinned divergence. The lifecycle gate above
	// only matches its observed signature classes; a plain wrapped transport
	// failure (POST failure, non-200, body decode) needs this arm or it reads
	// as the annotation going stale.
	if infra, detail := javaInfraFailure(javaResult.Err); infra {
		return false, detail
	}
	switch div.Direction {
	case plandiff.DivergenceJavaErrorsGoCorrect:
		// Java must (deterministically) error; Go must succeed with pinned rows.
		if javaResult.Err == nil {
			return false, "annotation says Java errors, but Java succeeded — divergence gone, reclassify"
		}
		if goResult.Err != nil {
			return false, "requires Go to succeed but Go errored: " + goResult.Err.Error()
		}
		if !reflect.DeepEqual(goResult.Rows.Rows, div.GoExpectedRows) {
			return false, fmt.Sprintf("Go rows changed from the annotation: %v", goResult.Rows.Rows)
		}
		return true, ""
	case plandiff.DivergenceJavaWrongRowsGoCorrect:
		// Both engines succeed; Java's rows are wrong (Java's bug). Go must
		// succeed with the pinned correct rows AND Java must still be wrong.
		if javaResult.Err != nil {
			return false, "annotation says Java succeeds with wrong rows, but Java errored: " + javaResult.Err.Error()
		}
		if goResult.Err != nil {
			return false, "requires Go to succeed but Go errored: " + goResult.Err.Error()
		}
		if !reflect.DeepEqual(goResult.Rows.Rows, div.GoExpectedRows) {
			return false, fmt.Sprintf("Go rows changed from the annotation: %v", goResult.Rows.Rows)
		}
		if reflect.DeepEqual(javaResult.Rows.Rows, div.GoExpectedRows) {
			return false, "annotation says Java's rows are wrong, but Java now matches Go's correct rows — divergence fixed, reclassify/delete"
		}
		return true, ""
	case plandiff.DivergenceJavaIntermittentGoCorrect:
		// The ONE direction whose Java side can't be pinned to exact rows:
		// documented Java NONDETERMINISM (UNION ALL + outer ORDER BY — Java
		// sometimes sorts, sometimes returns interleaved branch order). Only the
		// ROW ORDER is intermittent — Java still SUCCEEDS every time — so we
		// still assert Java does not error (a deterministic Java throw here is a
		// NEW, worse divergence that must not be masked just because Go's rows
		// match), and that Go succeeds with the pinned (sorted) rows. We do not
		// pin Java's exact rows/order. TODO: re-verify under the plan-cache-
		// disabled server — if Java is now order-deterministic, reclassify to
		// JavaWrongRowsGoCorrect or delete (Java sorts correctly).
		if javaResult.Err != nil {
			return false, "annotation says Java succeeds (only its row order is intermittent), but Java errored: " + javaResult.Err.Error()
		}
		if goResult.Err != nil {
			return false, "requires Go to succeed but Go errored: " + goResult.Err.Error()
		}
		if !reflect.DeepEqual(goResult.Rows.Rows, div.GoExpectedRows) {
			return false, fmt.Sprintf("Go rows changed from the annotation: %v", goResult.Rows.Rows)
		}
		return true, ""
	case plandiff.DivergenceUnorderedRowOrderDiffers:
		// NEITHER engine is wrong here: both succeed, the query has no ORDER BY,
		// and the multisets agree. What is asserted is (a) Go still produces the
		// pinned order, (b) Java is still a PERMUTATION of it, and (c) the two are
		// still different. (b) is the guard that stops this direction absorbing a
		// real wrong-rows bug; (c) is the stale-annotation guard, so a fixed
		// tie-break reports here instead of leaving a pin nobody revisits.
		// THE PREMISE, CHECKED RATHER THAN ASSERTED IN PROSE. This direction is
		// only defensible because SQL guarantees nothing about the sequence of a
		// query with no ORDER BY. On a query that HAS one, order is part of the
		// answer, and "the rows are the same multiset in a different order" is the
		// exact signature of a dropped or ignored sort — which this annotation
		// would then pin green forever.
		//
		// Matching on the corpus's own literal SQL is a lint over test data we
		// author, not feature detection inside the engine; it never reaches a
		// planning decision. It errs toward REJECTING (a stray "order by" inside a
		// string literal refuses the annotation), which is the safe direction: it
		// costs an author an explanation, where the opposite silently licenses a
		// real bug.
		if orderByPattern.MatchString(query) {
			return false, "this direction excuses ROW ORDER, and it is only sound where SQL leaves order undefined — " +
				"but the query has an ORDER BY, where order IS the answer. Same-multiset-different-order is what a " +
				"dropped sort looks like. Fix the ordering or classify it as a real divergence."
		}
		if javaResult.Err != nil {
			return false, "annotation says both engines succeed with the same multiset, but Java errored: " + javaResult.Err.Error()
		}
		if goResult.Err != nil {
			return false, "requires Go to succeed but Go errored: " + goResult.Err.Error()
		}
		if !reflect.DeepEqual(goResult.Rows.Rows, div.GoExpectedRows) {
			return false, fmt.Sprintf("Go rows changed from the annotation: %v", goResult.Rows.Rows)
		}
		if !sameRowMultiset(javaResult.Rows.Rows, goResult.Rows.Rows) {
			return false, fmt.Sprintf("annotation says ONLY the order differs, but the engines return different MULTISETS — java=%v go=%v. "+
				"That is a row-level divergence wearing an ordering annotation; reclassify it.",
				javaResult.Rows.Rows, goResult.Rows.Rows)
		}
		if reflect.DeepEqual(javaResult.Rows.Rows, goResult.Rows.Rows) {
			return false, "annotation says the row ORDER differs, but the engines now agree exactly — the divergence is gone, delete the annotation"
		}
		return true, ""
	case plandiff.DivergenceBothErrorMessagesDrift:
		// Both engines error with drifting messages. Java must error; Go must
		// reject with the pinned (cause-specific) substring.
		if javaResult.Err == nil {
			return false, "annotation says both engines error, but Java succeeded"
		}
		if goResult.Err == nil {
			return false, "requires Go to error but Go succeeded"
		}
		if !strings.Contains(goResult.Err.Error(), div.GoErrorContains) {
			return false, "Go error wording changed: " + goResult.Err.Error()
		}
		return true, ""
	case plandiff.DivergenceJavaSucceedsGoRejects:
		// Go is the more restrictive side: Java succeeds, Go rejects.
		if javaResult.Err != nil {
			return false, "annotation says Java succeeds, but Java errored: " + javaResult.Err.Error()
		}
		if goResult.Err == nil {
			return false, "requires Go to error but Go succeeded"
		}
		if !strings.Contains(goResult.Err.Error(), div.GoErrorContains) {
			return false, "Go error wording changed: " + goResult.Err.Error()
		}
		return true, ""
	default:
		return false, "unknown divergence direction " + string(div.Direction)
	}
}

// sameRowMultiset reports whether two result sets contain the same rows
// disregarding SEQUENCE — the guard under DivergenceUnorderedRowOrderDiffers.
//
// The key carries each element's TYPE as well as its value and separates elements
// unambiguously, so a dropped row, a duplicated row or a changed value all break
// the comparison and only ROW ORDER does not — which is exactly the scope the
// annotation may excuse.
//
// It renders RECURSIVELY, and that is not decoration. `%v` flattens a composite
// cell, so a top-level-only key collides `[["a","b c"]]` with `[["a b","c"]]` —
// the UNSAFE direction, on the one helper whose whole justification is that it
// cannot absorb a wrong-value bug. Not reachable from the two entries annotated
// today (both scalar BIGINT), but the Java side emits nested JsonObject and
// JsonArray, so a STRUCT or ARRAY column decodes to exactly such a cell.
//
// It is NOT claimed to be equivalent to reflect.DeepEqual, and the difference
// runs the safe way: `%v` renders -0.0 and 0.0 differently, so the key separates
// two values DeepEqual calls equal. Over-strict refuses an annotation; the
// converse would license one.
//
// Counting is by MULTISET, not by set: `[1 1 2]` and `[1 2 2]` are different
// answers and a set comparison would call them equal.
func sameRowMultiset(a, b [][]any) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, row := range a {
		counts[rowMultisetKey(row)]++
	}
	for _, row := range b {
		key := rowMultisetKey(row)
		counts[key]--
		if counts[key] < 0 {
			return false
		}
	}
	// Every decrement matched an increment and the lengths agree, so no
	// residue can remain; the loop above already returned on any shortfall.
	return true
}

// rowMultisetKey renders one row so that two rows share a key only if
// reflect.DeepEqual would call them equal: every element contributes its TYPE
// and its value, and the separator cannot be produced by either, so no
// regrouping of element boundaries can collide.
func rowMultisetKey(row []any) string {
	var b strings.Builder
	for _, cell := range row {
		writeMultisetCell(&b, cell)
		b.WriteByte(0x1e)
	}
	return b.String()
}

// writeMultisetCell renders one cell, descending into the composite shapes the
// two runners actually produce: Java decodes a JsonArray to []any and a
// JsonObject to map[string]any, and Go's driver mirrors that. Map keys are sorted
// so an equal map cannot key two ways.
func writeMultisetCell(b *strings.Builder, cell any) {
	switch v := cell.(type) {
	case []any:
		b.WriteString("[")
		for _, elem := range v {
			writeMultisetCell(b, elem)
			b.WriteByte(0x1d)
		}
		b.WriteString("]")
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("{")
		for _, k := range keys {
			fmt.Fprintf(b, "%q\x1c", k)
			writeMultisetCell(b, v[k])
			b.WriteByte(0x1d)
		}
		b.WriteString("}")
	default:
		fmt.Fprintf(b, "%T\x1f%v", cell, cell)
	}
}

// orderByPattern matches an ORDER BY in a corpus query. Used only to REFUSE the
// unordered-row-order annotation on a query whose order is defined.
var orderByPattern = regexp.MustCompile(`(?i)\border\s+by\b`)
