//go:build bazelrunfiles

package conformance_test

// End-to-end integration tests for the runSql harness (Track A1 of TODO.md
// execution roadmap). Drives the Java fdb-relational engine via
// SqlPlanSteps#runSql / runWithSetup against a shared FDB testcontainer.
//
// Specs in this file:
//
//   1. Schema-less SELECT — pins the documented "No Schema specified"
//      error path on /__SYS without a schema.
//   2. SELECT against an ephemeral-schema table — pins column metadata
//      (uppercased names + JDBC type names) for an empty table.
//   3. Empty result set — pins zero-row handling.
//   4. Multi-primitive INSERT-then-SELECT round-trip — BIGINT, DOUBLE,
//      STRING, BOOLEAN with NULL preservation.
//   5. INTEGER + FLOAT round-trip — type narrowing via explicit CAST.
//   6. BYTES round-trip — base64 encoding via X'...' literal.
//   7. SeedRunCorpus driver — runs every corpus entry against Java and
//      asserts per-entry Expected RowSet (precise diagnostics on
//      divergence).
//
// What this file does NOT assert:
//
//   - Cross-engine result-set equivalence. That's Track A3 (yamsql
//     corpus) and depends on a real Go-side runner (Track C2).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

// writeClusterFileToTemp materialises the cluster file string contents
// (env.ClusterFile) to a temp file on disk and returns its path. The
// Go embedded SQL driver's DSN takes a `cluster_file=<path>` option,
// not the file contents — so the conformance test writes once per It
// block and removes it on cleanup.
func writeClusterFileToTemp(contents string) string {
	f, err := os.CreateTemp("", "fdb-conformance-*.cluster")
	Expect(err).NotTo(HaveOccurred())
	_, err = f.WriteString(contents)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())
	return f.Name()
}

var _ = Describe("RunSql Harness", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tenantName := fmt.Sprintf("runsql_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	It("runs a schema-less SELECT and returns the literal", func() {
		// Schema-less is the only end-to-end path that fdb-relational
		// genuinely refuses (executeInternal demands conn.getSchema()
		// to be non-null — see AbstractEmbeddedStatement). EXPLAIN
		// bypasses that check; runSql does not. So a typed Java
		// "No Schema specified" surface IS the correct behaviour
		// here, and we pin it explicitly rather than tolerating
		// silently. Transport-level errors remain a real harness bug.
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile)
		got := runner.Run(ctx, plandiff.Query{
			Name: "select_literal",
			SQL:  "SELECT 1",
		})
		Expect(got.Err).To(HaveOccurred(), "fdb-relational rejects schema-less executeQuery")
		Expect(got.Err.Error()).To(ContainSubstring("No Schema specified"))
		Expect(got.Err.Error()).NotTo(ContainSubstring("HTTP "), "transport-level failure")
		Expect(got.Err.Error()).NotTo(ContainSubstring("dial tcp"), "Java server not reachable")
	})

	It("runs a SELECT against a table in the ephemeral schema", func() {
		// Pins the schema-template branch end-to-end: CREATE TEMPLATE
		// / CREATE DATABASE / CREATE SCHEMA, JDBC executeQuery,
		// RelationalResultSet → JSON encoding. The table is empty
		// since each runSql call uses a fresh ephemeral schema, but
		// column metadata + JDBC type mapping are pinned. Multi-row +
		// NULL preservation are covered by the httptest unit tests
		// (full wire-shape control there).
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile)
		// fdb-relational reserves NOT NULL for ARRAY column types in
		// CREATE TABLE syntax. Primary-key columns are implicitly
		// NOT NULL — no explicit annotation needed.
		got := runner.Run(ctx, plandiff.Query{
			Name:           "select_table_columns",
			SQL:            "SELECT id, name FROM Item",
			SchemaTemplate: "CREATE TABLE Item (id BIGINT, name STRING, PRIMARY KEY (id))",
		})
		Expect(got.Err).NotTo(HaveOccurred(), "schema-template branch must succeed")
		Expect(got.Rows.Columns).To(HaveLen(2), "expected 2 columns (id, name)")
		Expect(got.Rows.Columns[0].Name).To(Equal("ID"), "fdb-relational uppercases column names")
		Expect(got.Rows.Columns[0].Type).To(Equal("BIGINT"))
		Expect(got.Rows.Columns[1].Name).To(Equal("NAME"))
		// Pin whatever fdb-relational reports for STRING — surfacing
		// any future cross-engine type-name divergence.
		Expect(got.Rows.Columns[1].Type).NotTo(BeEmpty())
		Expect(got.Rows.Rows).To(BeEmpty(), "ephemeral schema is fresh — Item is empty")
	})

	It("round-trips a row with multiple primitive types via runWithSetup", func() {
		// Pins type encoding end-to-end: INSERT a row with values across
		// fdb-relational's primitive type set, SELECT it back via the
		// shared ephemeral schema, verify each column's JSON-encoded
		// representation. Surfaces any encoder gap in
		// SqlPlanSteps#resultSetToJson — JDBC types that fall through
		// to the {"__unsupported__": ...} marker would fail the asserts.
		//
		// Coverage: BIGINT (long), DOUBLE, STRING (varchar), BOOLEAN.
		// Skipped (not supported by fdb-relational CREATE TABLE in
		// 4.12.11.0): BYTES NOT NULL, DATE, TIMESTAMP — these wait
		// on a follow-up shift.
		runner, ok := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile).(plandiff.SetupRunner)
		Expect(ok).To(BeTrue(), "javaRunner must satisfy SetupRunner")

		got := runner.RunWithSetup(ctx,
			"CREATE TABLE T (id BIGINT, score DOUBLE, label STRING, flag BOOLEAN, PRIMARY KEY (id))",
			[]string{
				"INSERT INTO T VALUES (1, 3.5, 'alice', TRUE)",
				"INSERT INTO T VALUES (2, -7.25, 'bob', FALSE)",
				"INSERT INTO T VALUES (3, 0.0, NULL, NULL)",
			},
			"SELECT id, score, label, flag FROM T ORDER BY id",
		)
		Expect(got.Err).NotTo(HaveOccurred(), "INSERT-then-SELECT must succeed")
		Expect(got.Rows.Columns).To(HaveLen(4))
		Expect(got.Rows.Rows).To(HaveLen(3))

		// Row 0: (1, 3.5, "alice", TRUE)
		Expect(got.Rows.Rows[0][0].(float64)).To(Equal(float64(1)))
		Expect(got.Rows.Rows[0][1].(float64)).To(Equal(3.5))
		Expect(got.Rows.Rows[0][2].(string)).To(Equal("alice"))
		Expect(got.Rows.Rows[0][3].(bool)).To(BeTrue())

		// Row 1: (2, -7.25, "bob", FALSE)
		Expect(got.Rows.Rows[1][0].(float64)).To(Equal(float64(2)))
		Expect(got.Rows.Rows[1][1].(float64)).To(Equal(-7.25))
		Expect(got.Rows.Rows[1][3].(bool)).To(BeFalse())

		// Row 2: (3, 0.0, NULL, NULL) — null preservation across two
		// nullable columns (one STRING, one BOOLEAN).
		Expect(got.Rows.Rows[2][1].(float64)).To(Equal(float64(0)))
		Expect(got.Rows.Rows[2][2]).To(BeNil(), "label NULL must round-trip")
		Expect(got.Rows.Rows[2][3]).To(BeNil(), "flag NULL must round-trip")
	})

	It("round-trips INTEGER and FLOAT columns", func() {
		// fdb-relational distinguishes INTEGER (32-bit) from BIGINT
		// (64-bit) and FLOAT (32-bit) from DOUBLE (64-bit) in the
		// grammar (RelationalParser.g4 columnType). Both narrow types
		// arrive over JDBC as Number, so the JSON encoder treats them
		// uniformly — this test pins that behaviour.
		// fdb-relational doesn't auto-promote BIGINT literals to INTEGER
		// or DOUBLE literals to FLOAT — explicit CAST is required at
		// INSERT time. (`A value cannot be assigned to a variable
		// because the type of the value does not match the type of the
		// variable and cannot be promoted to the type of the variable`).
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile).(plandiff.SetupRunner)
		got := runner.RunWithSetup(ctx,
			"CREATE TABLE Numeric_T (id BIGINT, i INTEGER, f FLOAT, PRIMARY KEY (id))",
			[]string{"INSERT INTO Numeric_T VALUES (1, CAST(42 AS INTEGER), CAST(1.5 AS FLOAT))"},
			"SELECT id, i, f FROM Numeric_T",
		)
		Expect(got.Err).NotTo(HaveOccurred())
		Expect(got.Rows.Rows).To(HaveLen(1))
		Expect(got.Rows.Rows[0][1].(float64)).To(Equal(float64(42)))
		Expect(got.Rows.Rows[0][2].(float64)).To(BeNumerically("~", 1.5, 1e-6))
		// Pin the JDBC type names — divergence here would surface a
		// cross-engine type-name mismatch when Go-side runners land.
		Expect(got.Rows.Columns[1].Type).NotTo(BeEmpty())
		Expect(got.Rows.Columns[2].Type).NotTo(BeEmpty())
	})

	It("round-trips BYTES columns as base64", func() {
		// SqlPlanSteps#encodeValue base64-encodes byte[] values. This
		// pins that encoding path against real fdb-relational data.
		// The string "hi" → base64 "aGk=".
		// `blob` is a reserved keyword in fdb-relational's grammar
		// (RelationalLexer.g4#BLOB). Use `payload` for the column name.
		// Hex literal syntax: X'...' (uppercase) per
		// RelationalLexer.g4#HEXADECIMAL_LITERAL.
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile).(plandiff.SetupRunner)
		got := runner.RunWithSetup(ctx,
			"CREATE TABLE Bin_T (id BIGINT, payload BYTES, PRIMARY KEY (id))",
			[]string{"INSERT INTO Bin_T VALUES (1, X'6869')"},
			"SELECT id, payload FROM Bin_T",
		)
		Expect(got.Err).NotTo(HaveOccurred())
		Expect(got.Rows.Rows).To(HaveLen(1))
		// "hi" (0x68 0x69) → base64 "aGk=".
		Expect(got.Rows.Rows[0][1].(string)).To(Equal("aGk="))
	})
	It("runs the SeedRunCorpus through BOTH engines and asserts cross-engine equivalence", func() {
		// Generic plumbing: every SeedRunCorpus entry is driven through
		// Java (via the conformance HTTP server) AND Go (via the
		// embedded fdbsql driver against the same FDB container). The
		// harness asserts both engines succeed AND produce byte-equal
		// column metadata + row values, OR (for negative entries with
		// ExpectErrorContains set) both engines fail with matching
		// error substrings.
		//
		// Adding a new test case is just appending {Name, Schema,
		// Setup, Query[, ExpectErrorContains]} to SeedRunCorpus().
		// No baseline RowSet to capture, no conformance-test wiring.
		//
		// The corpus is driven in PARALLEL across a small pool of freshly-spawned
		// Java servers + Go runners (see the fan-out below) — fresh servers, not
		// the suite-shared one, to avoid pollution from prior conformance specs.
		corpus := plandiff.SeedRunCorpus()
		// Apply RFC-082 cross-engine divergence annotations (Go-only extensions,
		// tracked Go capability gaps, and both-reject message-drift) so the
		// harness asserts Go's documented behaviour without pinning Java's.
		plandiff.ApplyRFC082Divergences(corpus)

		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)

		// Fan the corpus out across N workers, each driving a disjoint subset on
		// its OWN pre-spawned Java server + Go runner against the one shared FDB
		// cluster. Every entry runs in its own uuid-isolated ephemeral schema, so
		// workers never collide; pre-spawning all servers up front preserves the
		// no-spawn-during-query rule; and per-server load is 1/N of the
		// single-server serial baseline that is already green (so error-path
		// state accumulation, if any, is strictly lower). Java is the spec: per
		// entry, whatever Java does becomes the behaviour Go must match — drift on
		// either side surfaces. Workers only COMPUTE the (java, go) result pair;
		// Gomega's Expect is not goroutine-safe, so every assertion runs serially
		// after the join.
		n := seedCorpusParallelism()
		if n > len(corpus) {
			n = len(corpus)
		}
		type corpusRunner struct {
			java plandiff.SetupRunner
			gor  plandiff.SetupRunner
			srv  *JavaInvoker
		}
		runners := make([]corpusRunner, n)
		for i := range runners {
			srv, err := NewIsolatedJavaInvoker()
			Expect(err).NotTo(HaveOccurred(), "failed to spawn Java conformance server %d/%d", i+1, n)
			runners[i] = corpusRunner{
				java: plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner),
				gor:  plandiff.NewGoSQLSetupRunner(clusterFilePath),
				srv:  srv,
			}
		}
		defer func() {
			for _, r := range runners {
				_ = r.srv.Close()
			}
		}()

		type enginePair struct{ java, golang plandiff.RunResult }
		results := make([]enginePair, len(corpus))
		idxCh := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < n; w++ {
			wg.Add(1)
			go func(r corpusRunner, wid int) {
				defer wg.Done()
				for idx := range idxCh {
					rq := corpus[idx]
					jr := r.java.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
					gr := r.gor.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
					// Re-run whichever side hit a retryable HARNESS error — a
					// retryable FDB code (catalog conflict, transaction starved
					// past 5s) or a lifecycle failure (blown deadline, torn-down
					// connection, closed context). See rerunWhileRetryable.
					jr, gr = rerunWhileRetryable(jr, gr, wid,
						func() plandiff.RunResult {
							return r.java.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
						},
						func() plandiff.RunResult {
							return r.gor.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
						})
					results[idx] = enginePair{java: jr, golang: gr}
				}
			}(runners[w], w)
		}
		for i := range corpus {
			idxCh <- i
		}
		close(idxCh)
		wg.Wait()

		// RFC-082 regression LOCK, asserted serially over the computed results so
		// one run reports the full delta: non-annotated entries that diverge but
		// are NOT in rfc082KnownRed are regressions; known-red entries that now
		// pass must be removed from the lock (it only shrinks). Annotated entries
		// assert Go's pinned behaviour without pinning Java's.
		var regressions, fixedNowGreen, staleAnnotations []string
		for idx := range corpus {
			rq := corpus[idx]
			javaResult, goResult := results[idx].java, results[idx].golang
			if rq.Divergence != nil {
				if ok, detail := divergenceHolds(rq.Divergence, rq.Query, javaResult, goResult); !ok {
					staleAnnotations = append(staleAnnotations, fmt.Sprintf("%s: %s", rq.Name, detail))
				}
				continue
			}
			ok, detail := entryConforms(javaResult, goResult)
			known := plandiff.IsKnownRed(rq.Name)
			if !ok && !known {
				regressions = append(regressions, fmt.Sprintf("%s: %s", rq.Name, detail))
			} else if ok && known {
				fixedNowGreen = append(fixedNowGreen, rq.Name)
			}
		}
		Expect(staleAnnotations).To(BeEmpty(),
			"STALE RFC-082 annotation(s) — Go's behaviour no longer matches the pinned divergence; update/remove rfc082Divergences:\n  %s",
			strings.Join(staleAnnotations, "\n  "))
		Expect(regressions).To(BeEmpty(),
			"NEW cross-engine divergence(s) — a regression vs the locked known-red set (RFC-082); fix Go or, if intended, annotate:\n  %s",
			strings.Join(regressions, "\n  "))
		Expect(fixedNowGreen).To(BeEmpty(),
			"known-red corpus entries now PASS — remove them from rfc082KnownRed so the lock shrinks (RFC-082):\n  %s",
			strings.Join(fixedNowGreen, "\n  "))
	})

	It("returns an empty result set for SELECT with no matching rows", func() {
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile)
		// Pin zero-row handling: an empty table SELECT returns
		// columns with no rows. Avoids fdb-relational's VALUES-clause
		// syntax restrictions.
		got := runner.Run(ctx, plandiff.Query{
			Name:           "empty_select",
			SQL:            "SELECT id FROM Dummy",
			SchemaTemplate: "CREATE TABLE Dummy (id BIGINT, PRIMARY KEY (id))",
		})
		Expect(got.Err).NotTo(HaveOccurred())
		Expect(got.Rows.Columns).To(HaveLen(1))
		Expect(got.Rows.Rows).To(BeEmpty())
	})
})

// sameRowMultiset reports whether two result sets contain the same rows
// disregarding SEQUENCE — the guard under DivergenceUnorderedRowOrderDiffers.
//
// The key carries each element's TYPE as well as its value, and separates
// elements unambiguously, so it is exactly as strict as the reflect.DeepEqual the
// rest of the harness uses on non-annotated entries — only insensitive to the
// order of ROWS. A weaker key would matter: rendering a row with fmt.Sprint alone
// equates float64(5) with the string "5", and makes ["a b","c"] and ["a","b c"]
// the same row. Both runners already normalise numerics to float64
// (go_runner.go), so nothing is gained by being loose here and a real
// wrong-value divergence would be absorbed.
//
// A dropped row, a duplicated row or a changed value all break the comparison;
// only order does not, which is exactly the scope the annotation may excuse.
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
		fmt.Fprintf(&b, "%T\x1f%v\x1e", cell, cell)
	}
	return b.String()
}

// orderByPattern matches an ORDER BY in a corpus query. Used only to REFUSE the
// unordered-row-order annotation on a query whose order is defined.
var orderByPattern = regexp.MustCompile(`(?i)\border\s+by\b`)
