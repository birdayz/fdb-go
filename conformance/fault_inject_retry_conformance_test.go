package conformance_test

// Regression test for RFC-090: the Java conformance server retries RETRYABLE-
// AND-NOT-COMMITTED FDB errors (the canonical 1007 transaction_too_old class
// that A3 hits when CI-box saturation starves the JVM thread past FDB's 5s
// transaction window) instead of surfacing them as false cross-engine
// failures — but NEVER retries a maybe-committed (1021) or non-retryable error,
// so a write is never silently replayed.
//
// The behaviour is otherwise only reachable under real load, so we drive it
// deterministically through the test-only `runWithSetupInjectingFaults` step
// (SqlPlanSteps): it runs the setup + query through the production
// `withFdbRetry`, but injects N genuine FDBExceptions (of a chosen code) before
// the SELECT executes. Genuine FDBExceptions mean the production predicate
// (FDBException#isRetryableNotCommitted, native) classifies them exactly as it
// would a live error. The injection countdown is a method-local AtomicInteger,
// so it is isolated per request even on the shared/pooled server.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// FDB error codes exercised below.
const (
	fdbTransactionTooOld    = 1007 // retryable, not-committed → retried
	fdbNotCommitted         = 1020 // retryable, not-committed → retried
	fdbCommitUnknownResult  = 1021 // retryable but MAYBE-committed → NOT retried
	fdbNonRetryableOpFailed = 1000 // not retryable → NOT retried
)

// fault-inject step constants: a 3-row table, query selects the single id whose
// n == 10 (id 2).
const (
	fiSchema = "CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))"
	fiQuery  = "SELECT id FROM t WHERE 10 = n"
)

var fiSetup = []string{"INSERT INTO t VALUES (1, 5), (2, 10), (3, 15)"}

// rowSetJSON mirrors SqlPlanSteps#resultSetToJson output.
type rowSetJSON struct {
	Columns []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"columns"`
	Rows [][]any `json:"rows"`
}

var _ = Describe("Conformance server FDB retry (RFC-090)", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tenantName := fmt.Sprintf("fault_inject_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	invoke := func(faultCount, faultCode int) (rowSetJSON, error) {
		raw, err := java.Invoke(ctx, "runWithSetupInjectingFaults", map[string]any{
			"clusterFile":    env.ClusterFile,
			"schemaTemplate": fiSchema,
			"setupSqls":      fiSetup,
			"querySql":       fiQuery,
			"faultCount":     faultCount,
			"faultCode":      faultCode,
		})
		if err != nil {
			return rowSetJSON{}, err
		}
		var rs rowSetJSON
		Expect(json.Unmarshal(raw, &rs)).To(Succeed())
		return rs, nil
	}

	It("recovers from a burst of transaction_too_old (1007) and returns the correct row", func() {
		// Two injected 1007s are within the attempt budget (MAX_FDB_RETRIES=6),
		// so the third attempt runs the real query and returns id=2.
		rs, err := invoke(2, fdbTransactionTooOld)
		Expect(err).NotTo(HaveOccurred(), "retryable not-committed error must be absorbed")
		Expect(rs.Rows).To(HaveLen(1))
		Expect(rs.Rows[0][0].(float64)).To(Equal(float64(2)), "WHERE 10 = n selects id 2")
	})

	It("recovers from not_committed (1020) — the other not-committed retryable code", func() {
		rs, err := invoke(3, fdbNotCommitted)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Rows).To(HaveLen(1))
		Expect(rs.Rows[0][0].(float64)).To(Equal(float64(2)))
	})

	It("recovers at the exact budget boundary (5 faults, MAX_FDB_RETRIES=6)", func() {
		// 5 faults = the most that can be absorbed: attempts 1..5 inject, attempt
		// 6 (the last) runs the real query. One more would exhaust. Pins the
		// recoverable edge of the budget.
		rs, err := invoke(5, fdbTransactionTooOld)
		Expect(err).NotTo(HaveOccurred(), "5 faults must still recover on the 6th (final) attempt")
		Expect(rs.Rows).To(HaveLen(1))
		Expect(rs.Rows[0][0].(float64)).To(Equal(float64(2)))
	})

	It("surfaces at the exact budget boundary (6 faults = every attempt fails)", func() {
		// 6 faults = MAX_FDB_RETRIES: all six attempts inject, none reaches the
		// real query, so it must surface. The minimal exhausting count — one less
		// recovers (above), so this pins the exhausted edge precisely.
		_, err := invoke(6, fdbTransactionTooOld)
		Expect(err).To(HaveOccurred(), "6 faults exhaust the budget and must fail loudly")
		var je *JavaError
		Expect(errors.As(err, &je)).To(BeTrue(), "expected a typed Java FDBException, got %v", err)
		Expect(je.ExceptionClass).To(Equal("FDBException"))
	})

	It("surfaces the error once the retry budget is exhausted", func() {
		// 7 > MAX_FDB_RETRIES (6): every attempt is injected, so the last one
		// throws and the error must surface rather than hang or pass silently.
		_, err := invoke(7, fdbTransactionTooOld)
		Expect(err).To(HaveOccurred(), "an indefinitely-failing box must fail loudly, not spin forever")
		var je *JavaError
		Expect(errors.As(err, &je)).To(BeTrue(), "expected a typed Java FDBException, got %v", err)
		Expect(je.ExceptionClass).To(Equal("FDBException"))
	})

	It("does NOT retry commit_unknown_result (1021) — a maybe-committed write must never be replayed", func() {
		// A single 1021 with budget to spare: if 1021 were (wrongly) retried,
		// the second attempt would succeed and return the row. It must instead
		// surface immediately — this is the reviewer-caught hole the
		// isRetryableNotCommitted predicate closes.
		_, err := invoke(1, fdbCommitUnknownResult)
		Expect(err).To(HaveOccurred(), "1021 commit_unknown_result must NOT be retried")
		var je *JavaError
		Expect(errors.As(err, &je)).To(BeTrue(), "expected the injected FDBException to surface, got %v", err)
		Expect(je.ExceptionClass).To(Equal("FDBException"))
	})

	It("does NOT retry a non-retryable error (1000)", func() {
		_, err := invoke(1, fdbNonRetryableOpFailed)
		Expect(err).To(HaveOccurred(), "a non-retryable FDB error must surface immediately")
		var je *JavaError
		Expect(errors.As(err, &je)).To(BeTrue(), "expected the injected FDBException to surface, got %v", err)
		Expect(je.ExceptionClass).To(Equal("FDBException"))
	})
})
