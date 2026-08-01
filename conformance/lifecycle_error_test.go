//go:build bazelrunfiles

package conformance_test

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
)

// The five failure signatures below are VERBATIM from a red
// //conformance:conformance_test run reproduced under concurrent load (three
// runs, three failures, code identical to master). Every one is a harness
// lifecycle failure — a blown wall-clock deadline, a torn-down connection, a
// closed transaction context — and every one was published as a cross-engine
// semantic finding: four as "NEW cross-engine divergence(s)", two as "STALE
// RFC-082 annotation(s)" inviting someone to delete a correct pin.
//
// They are constructed as literals rather than captured from a live run so the
// pins are deterministic. Changing one of these strings is not a cosmetic edit:
// it means the observed signature is no longer what the classifier matches.

// javaDeadlineExceeded is MoreAsyncUtil.getWithDeadline firing the
// AsyncLoadingCache 5s wall-clock deadline on Java's resolver-state load.
func javaDeadlineExceeded() error {
	return &plandiff.JavaError{
		Message:            "deadline exceeded",
		ExceptionClass:     "DeadlineExceededException",
		ExceptionFullClass: "com.apple.foundationdb.async.MoreAsyncUtil$DeadlineExceededException",
	}
}

// javaContextNotActive is FDBRecordContext.ensureActive finding a closed
// context. Java's own ExceptionUtil maps this to ErrorCode.TRANSACTION_INACTIVE
// — a lifecycle code, never a semantic one.
func javaContextNotActive() error {
	return &plandiff.JavaError{
		Message:            "Transaction is no longer active.",
		ExceptionClass:     "RecordContextNotActiveException",
		ExceptionFullClass: "com.apple.foundationdb.record.provider.foundationdb.RecordContextNotActiveException",
	}
}

// javaCommitUnknown is FDB 1021 commit_unknown_result, retryable by FDB's own
// definition.
func javaCommitUnknown() error {
	return &plandiff.JavaError{
		Message:        "Transaction may or may not have committed",
		ExceptionClass: "FDBException",
	}
}

// javaTransportTimeout is the conformance server failing to answer at all: the
// HTTP client's own timeout, which surfaces as a net.Error.
func javaTransportTimeout() error {
	return fmt.Errorf("plandiff: HTTP POST: %w", &net.OpError{
		Op:  "dial",
		Err: fmt.Errorf(`Post "http://127.0.0.1:36787/invoke": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`),
	})
}

// goFixtureConnClosed is the Go side losing its FDB connection midway through
// building the ephemeral fixture — the full observed wrap chain, leaf to root.
func goFixtureConnClosed(phase string) error {
	return &plandiff.FixtureError{
		Phase: phase,
		Err: api.WrapErrorf(
			fmt.Errorf("failed to load index states: %w", transport.ErrConnClosed),
			api.ErrCodeInternalError, "open catalog store"),
	}
}

// TestIsLifecycleError_ObservedSignatures pins that each verbatim signature is
// recognised as lifecycle, and — the load-bearing half — that the class cannot
// stretch to cover an engine answer.
func TestIsLifecycleError_ObservedSignatures(t *testing.T) {
	t.Parallel()

	t.Run("observed lifecycle failures classify as lifecycle", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"java DeadlineExceededException", javaDeadlineExceeded()},
			{"java RecordContextNotActiveException", javaContextNotActive()},
			{"java FDBException commit_unknown_result", javaCommitUnknown()},
			{"conformance-server transport timeout", javaTransportTimeout()},
			{"go fixture CREATE DATABASE, connection closed", goFixtureConnClosed("CREATE DATABASE")},
			{"go fixture setup DML, connection closed", goFixtureConnClosed(`setup "INSERT INTO T_S12 VALUES (2, 'banana')"`)},
			{"go fixture CREATE SCHEMA TEMPLATE, connection closed", goFixtureConnClosed("CREATE SCHEMA TEMPLATE")},
		} {
			if !isLifecycleError(tc.err) {
				t.Errorf("%s must classify as lifecycle — it is a harness failure, not an "+
					"engine answer, and reporting it as a cross-engine divergence sends "+
					"readers after a phantom regression. Got not-lifecycle for: %v", tc.name, tc.err)
			}
		}
	})

	// Widening any arm of the classifier must break this block. It is the only
	// thing standing between the INFRA class and a suppression hole.
	t.Run("an engine answer is never lifecycle", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			err  error
		}{{
			name: "planner exception",
			err: &plandiff.JavaError{
				Message: "Cascades planner could not plan query", ExceptionClass: "UnableToPlanException",
			},
		}, {
			name: "complexity exception",
			err: &plandiff.JavaError{
				Message: "plan complexity exceeded", ExceptionClass: "RecordQueryPlanComplexityException",
			},
		}, {
			name: "semantic error",
			err: &plandiff.JavaError{
				Message: "Unknown column FOO", ExceptionClass: "RelationalException",
			},
		}, {
			// The FDBException arm is keyed to ONE exact message. A classifier
			// that matched the class alone would eat every FDB-surfaced engine
			// error, including a uniqueness violation.
			name: "FDBException that is not commit_unknown_result",
			err: &plandiff.JavaError{
				Message: "Duplicate entry for unique index IDX_A", ExceptionClass: "FDBException",
			},
		}, {
			// The DeadlineExceededException arm is keyed to its exact message
			// too — same reason.
			name: "DeadlineExceededException with a different message",
			err: &plandiff.JavaError{
				Message: "rows do not match: expected 3, got 2", ExceptionClass: "DeadlineExceededException",
			},
		}, {
			// A Go-only DDL gap during fixture setup is a REAL divergence: Go
			// rejected a schema Java accepts. Classifying every FixtureError as
			// infra — dropping the ErrConnClosed conjunct — would swallow it.
			name: "go fixture failure that is NOT a connection teardown",
			err: &plandiff.FixtureError{
				Phase: "CREATE SCHEMA TEMPLATE",
				Err:   api.NewError(api.ErrCodeInternalError, "unsupported column type ARRAY<STRUCT>"),
			},
		}, {
			// The converse conjunct. A connection teardown while running the
			// QUERY is not a FixtureError, and must stay visible: the pure-Go
			// client surfacing an uncoded transport teardown to its caller is a
			// client defect, and the conformance suite must not launder it.
			name: "connection teardown on the query under test, not the fixture",
			err: fmt.Errorf("plandiff/go: query: %w",
				api.WrapErrorf(fmt.Errorf("scan: %w", transport.ErrConnClosed),
					api.ErrCodeInternalError, "execute")),
		}, {
			name: "plain row disagreement",
			err:  errors.New("row data diverges"),
		}} {
			if isLifecycleError(tc.err) {
				t.Errorf("%s must NOT classify as lifecycle — the INFRA class must be "+
					"unable to express an engine answer, or a real divergence gets "+
					"eaten by it: %v", tc.name, tc.err)
			}
		}
	})

	t.Run("nil is not lifecycle", func(t *testing.T) {
		t.Parallel()
		if isLifecycleError(nil) {
			t.Fatal("a successful run must never be classified as a harness failure")
		}
	})
}

// TestEntryConforms_LifecycleIsInfra pins the ROUTING: every observed signature
// must reach entryConforms' infra arm rather than one of its two cross-engine
// verdicts. Before this, a Java-side lifecycle error read as "Java errored but
// Go succeeded" and a Go-side one as "Java succeeded but Go errored" — both
// claims about the ENGINES, neither backed by a side that actually ran.
func TestEntryConforms_LifecycleIsInfra(t *testing.T) {
	t.Parallel()

	ok := plandiff.RunResult{Engine: "go"}

	for _, tc := range []struct {
		name       string
		java, gosl plandiff.RunResult
	}{
		{"java deadline exceeded", plandiff.RunResult{Engine: "java", Err: javaDeadlineExceeded()}, ok},
		{"java context not active", plandiff.RunResult{Engine: "java", Err: javaContextNotActive()}, ok},
		{"java commit unknown", plandiff.RunResult{Engine: "java", Err: javaCommitUnknown()}, ok},
		{"java transport timeout", plandiff.RunResult{Engine: "java", Err: javaTransportTimeout()}, ok},
		{
			"go fixture connection closed",
			plandiff.RunResult{Engine: "java"},
			plandiff.RunResult{Engine: "go", Err: goFixtureConnClosed("CREATE DATABASE")},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conforms, detail := entryConforms(tc.java, tc.gosl)
			if conforms {
				t.Fatal("a harness that could not run must stay RED — infra is a label, never a pass")
			}
			if !strings.Contains(detail, "INFRA") {
				t.Fatalf("detail must name the failure as infra so nobody chases an "+
					"engine bug, got: %s", detail)
			}
			for _, forbidden := range []string{"Java errored but Go succeeded", "Java succeeded but Go errored"} {
				if strings.Contains(detail, forbidden) {
					t.Fatalf("a lifecycle failure must never be reported as a cross-engine "+
						"divergence (%q), got: %s", forbidden, detail)
				}
			}
		})
	}
}

// TestDivergenceHolds_LifecycleIsInfra pins the same routing on the annotated
// path — the arm that produced two of the three reproduced reds. A Java server
// that timed out is not Java refuting an RFC-082 annotation, and reporting it as
// STALE invites deleting a pin that is still correct.
func TestDivergenceHolds_LifecycleIsInfra(t *testing.T) {
	t.Parallel()

	for _, dir := range []plandiff.DivergenceDirection{
		plandiff.DivergenceJavaErrorsGoCorrect,
		plandiff.DivergenceJavaWrongRowsGoCorrect,
	} {
		dir := dir
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"transport timeout", javaTransportTimeout()},
			{"commit unknown", javaCommitUnknown()},
			{"deadline exceeded", javaDeadlineExceeded()},
			{"context not active", javaContextNotActive()},
		} {
			tc := tc
			t.Run(string(dir)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				div := &plandiff.Divergence{Direction: dir}
				held, detail := divergenceHolds(div,
					plandiff.RunResult{Engine: "java", Err: tc.err},
					plandiff.RunResult{Engine: "go"})
				if held {
					t.Fatal("a harness that could not run must stay RED")
				}
				if !strings.Contains(detail, "INFRA") {
					t.Fatalf("detail must name the failure as infra, got: %s", detail)
				}
				if strings.Contains(detail, "annotation says") {
					t.Fatalf("a lifecycle failure carries NO evidence about the annotation, "+
						"so it must never be reported as a stale one, got: %s", detail)
				}
			})
		}
	}
}

// TestRetryBudget pins the re-run policy: a lifecycle failure is retried, not
// suppressed, and an engine answer is never retried away.
func TestRetryBudget(t *testing.T) {
	t.Parallel()

	if got := retryBudget(javaDeadlineExceeded()); got != maxLifecycleRetries {
		t.Errorf("a lifecycle failure must be re-run (budget %d), got %d", maxLifecycleRetries, got)
	}
	if got := retryBudget(goFixtureConnClosed("CREATE DATABASE")); got != maxLifecycleRetries {
		t.Errorf("a Go fixture teardown must be re-run (budget %d), got %d", maxLifecycleRetries, got)
	}
	if got := retryBudget(javaTransportTimeout()); got != maxLifecycleRetries {
		t.Errorf("a transport timeout must be re-run (budget %d), got %d", maxLifecycleRetries, got)
	}
	// The transport class gets a SMALLER budget than the FDB-code class on
	// purpose: each of its attempts can cost a full 30s client timeout, and
	// sixteen of those would blow the suite's own deadline.
	if maxLifecycleRetries >= maxConflictRetries {
		t.Errorf("the lifecycle budget (%d) must stay below the FDB-conflict budget (%d) — "+
			"a lifecycle re-run can cost a whole HTTP timeout",
			maxLifecycleRetries, maxConflictRetries)
	}
	transient := &plandiff.JavaError{
		Message: "Transaction is too old to perform reads or be committed", ExceptionClass: "FDBException",
	}
	if got := retryBudget(transient); got != maxConflictRetries {
		t.Errorf("a retryable FDB code keeps its full budget %d, got %d", maxConflictRetries, got)
	}
	semantic := &plandiff.JavaError{
		Message: "Cascades planner could not plan query", ExceptionClass: "UnableToPlanException",
	}
	if got := retryBudget(semantic); got != 0 {
		t.Errorf("an engine answer must never be retried away, got budget %d", got)
	}
	if got := retryBudget(nil); got != 0 {
		t.Errorf("a successful run must not enter the retry loop, got budget %d", got)
	}
}

// TestRerunWhileRetryable pins the loop itself: it re-runs a lifecycle failure,
// stops the moment a side produces an engine answer, never re-runs a side that
// already answered, and — the property that keeps retry from becoming
// suppression — gives up after the budget so a persistent failure still reaches
// the report.
func TestRerunWhileRetryable(t *testing.T) {
	t.Parallel()

	t.Run("clears a transient lifecycle failure", func(t *testing.T) {
		t.Parallel()
		javaCalls := 0
		jr, gr := rerunWhileRetryable(
			plandiff.RunResult{Engine: "java", Err: javaDeadlineExceeded()},
			plandiff.RunResult{Engine: "go"}, 0,
			func() plandiff.RunResult {
				javaCalls++
				return plandiff.RunResult{Engine: "java"}
			},
			func() plandiff.RunResult {
				t.Error("the Go side already answered — it must not be re-run")
				return plandiff.RunResult{}
			})
		if javaCalls != 1 {
			t.Errorf("the loop must stop as soon as the side answers; java re-runs = %d", javaCalls)
		}
		if jr.Err != nil || gr.Err != nil {
			t.Errorf("both sides should now be clean: java=%v go=%v", jr.Err, gr.Err)
		}
	})

	t.Run("a persistent lifecycle failure survives to the report", func(t *testing.T) {
		t.Parallel()
		calls := 0
		jr, _ := rerunWhileRetryable(
			plandiff.RunResult{Engine: "java", Err: javaTransportTimeout()},
			plandiff.RunResult{Engine: "go"}, 0,
			func() plandiff.RunResult {
				calls++
				return plandiff.RunResult{Engine: "java", Err: javaTransportTimeout()}
			},
			func() plandiff.RunResult { return plandiff.RunResult{Engine: "go"} })
		if calls != maxLifecycleRetries {
			t.Errorf("the budget must be spent exactly once through: got %d re-runs, want %d",
				calls, maxLifecycleRetries)
		}
		if jr.Err == nil {
			t.Fatal("a sick server must NOT be retried into a pass — the error must survive")
		}
		if conforms, _ := entryConforms(jr, plandiff.RunResult{Engine: "go"}); conforms {
			t.Fatal("a persistent lifecycle failure must still make the entry non-conforming")
		}
	})

	t.Run("an engine answer is never re-run", func(t *testing.T) {
		t.Parallel()
		semantic := &plandiff.JavaError{
			Message: "Cascades planner could not plan query", ExceptionClass: "UnableToPlanException",
		}
		jr, _ := rerunWhileRetryable(
			plandiff.RunResult{Engine: "java", Err: semantic},
			plandiff.RunResult{Engine: "go"}, 0,
			func() plandiff.RunResult {
				t.Error("a planner exception is Java's ANSWER — re-running it would retry a real regression away")
				return plandiff.RunResult{Engine: "java"}
			},
			func() plandiff.RunResult { return plandiff.RunResult{Engine: "go"} })
		var je *plandiff.JavaError
		if !errors.As(jr.Err, &je) || je.ExceptionClass != "UnableToPlanException" {
			t.Fatalf("the engine answer must be preserved verbatim, got: %v", jr.Err)
		}
	})
}
