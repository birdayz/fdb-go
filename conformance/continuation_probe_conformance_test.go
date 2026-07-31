//go:build bazelrunfiles

package conformance_test

// Committed instrument for three measurements of the LIVE Java fdb-relational
// EXECUTE CONTINUATION path (tag 4.12.11.0), taken through the conformance
// server's `continuationProbe` step (conformance/sql_plan_steps.java). The Go
// continuation envelope is designed against these numbers, so the measurement
// has to survive as a test rather than as a transcript:
//
//	A. An unknown plan_serialization_mode ("GO_V0") is resolved by
//	   PlanValidator.validateSerializedPlanSerializationMode via
//	   PlanHashMode.valueOf BEFORE the validPlanHashModes membership test —
//	   so a foreign mode string surfaces Enum.valueOf's failure, not the
//	   typed INVALID_CONTINUATION the membership test would raise.
//
//	B. ContinuedPhysicalQueryPlan.validatePlanAgainstEnvironment catches
//	   PlanValidationException from validateContinuationConstraint, counts
//	   CONTINUATION_REJECTED, and falls through to CONTINUATION_ACCEPTED. A
//	   continuation whose plan constraint no longer holds is still executed.
//
//	C. AstNormalizer.processLiteral folds each literal into the parameter
//	   hash with Objects.hash(canonicalName, literal). For a x'..' literal
//	   that literal is a byte[], whose hashCode() is identity — so the
//	   binding hash stamped into the continuation is not stable across two
//	   executions of the same query text.
//
// The spec PRINTS the full JSON so the raw numbers stay readable in the test
// log, and pins each of the three conclusions in the direction that makes it a
// conformance oracle: if a Java upgrade starts rejecting the unknown mode with
// a typed INVALID_CONTINUATION, closes the constraint fail-open, or stabilises
// the bytes-literal binding hash, this goes red and the envelope design gets
// revisited. Exact hash VALUES are deliberately not pinned — only the
// stable/unstable relation, which is the load-bearing property.
//
// Ginkgo, not a plain go test, because the FDB container and Java server in
// this package live on the suite (BeforeSuite), exactly as
// plan_diff_conformance_test.go and duplicate_alias_java_probe_test.go use
// them; a t.Parallel() go test cannot reach either.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ContinuationProbe", func() {
	It("measures Java's EXECUTE CONTINUATION mode / constraint / binding-hash behaviour", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("contprobe_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()

		raw, err := srv.Invoke(ctx, "continuationProbe", map[string]any{
			"clusterFile": env.ClusterFile,
		})
		// A missing step comes back as
		// "No conformance step found with name: continuationProbe" — this is
		// the assertion that goes red if the Java step is deleted.
		Expect(err).NotTo(HaveOccurred(), "continuationProbe step must exist and run")

		var pretty map[string]any
		Expect(json.Unmarshal(raw, &pretty)).To(Succeed())
		indented, mErr := json.MarshalIndent(pretty, "", "  ")
		Expect(mErr).NotTo(HaveOccurred())
		GinkgoWriter.Printf("continuationProbe RAW JSON:\n%s\n", string(indented))
		AddReportEntry("continuationProbe", string(indented))

		Expect(pretty).To(HaveKeyWithValue("seed_ok", true), "probe table must seed")

		// --- Probe A: the mode string never reaches a typed rejection. ---
		GinkgoWriter.Printf("[A] threw=%v class=%v sqlState=%v\n    message=%v\n    causeChain=%v\n",
			pretty["probeA_threw"], pretty["probeA_exceptionClass"],
			pretty["probeA_sqlState"], pretty["probeA_message"], pretty["probeA_causeChain"])
		Expect(pretty).To(HaveKeyWithValue("probeA_threw", true))
		// Enum.valueOf runs before the validPlanHashModes membership test, so
		// an unrecognised mode is an IllegalArgumentException, not the
		// PlanValidationException the membership test would raise. The
		// SQLSTATE proves it: INVALID_CONTINUATION is 24F00, and this is not
		// that.
		Expect(pretty["probeA_message"]).To(ContainSubstring("No enum constant"),
			"Java stopped resolving the mode with Enum.valueOf first — re-check PlanValidator.validateSerializedPlanSerializationMode")
		Expect(pretty["probeA_causeChain"]).To(ContainSubstring("java.lang.IllegalArgumentException"))
		Expect(pretty["probeA_sqlState"]).NotTo(Equal("24F00"),
			"an unknown plan_serialization_mode now surfaces as a typed INVALID_CONTINUATION — the escape hatch closed")

		// --- Probe B: the fail-open. ---
		GinkgoWriter.Printf("[B] control rows=%v | doctored threw=%v rows=%v class=%v sqlState=%v\n    message=%v\n",
			pretty["probeB_control_rowsReturned"], pretty["probeB_threw"],
			pretty["probeB_rowsReturned"], pretty["probeB_exceptionClass"],
			pretty["probeB_sqlState"], pretty["probeB_message"])
		// The doctored constraint must actually be constant-FALSE, or "rows
		// came back" proves nothing about the fail-open.
		Expect(pretty["probeB_falsePlanConstraintProto"]).To(ContainSubstring("constant_predicate"))
		Expect(pretty["probeB_falsePlanConstraintProto"]).To(ContainSubstring("value: false"))
		Expect(pretty).To(HaveKeyWithValue("probeB_control_rowsReturned", float64(3)),
			"the untouched continuation must resume the remaining 3 rows; otherwise the doctored run has no baseline")
		Expect(pretty).To(HaveKeyWithValue("probeB_threw", false),
			"Java now REJECTS a continuation whose plan constraint evaluates false — the fail-open at ContinuedPhysicalQueryPlan.validatePlanAgainstEnvironment closed")
		Expect(pretty).To(HaveKeyWithValue("probeB_rowsReturned", float64(3)),
			"a false plan constraint must still yield every remaining row for the fail-open to be what it looks like")

		// --- Probe C: binding-hash stability. ---
		GinkgoWriter.Printf("[C] bytes: %v vs %v (equal=%v) | int: %v vs %v (equal=%v)\n",
			pretty["probeC_bytes_hash1"], pretty["probeC_bytes_hash2"], pretty["probeC_bytes_equal"],
			pretty["probeC_int_hash1"], pretty["probeC_int_hash2"], pretty["probeC_int_equal"])
		Expect(pretty).To(HaveKeyWithValue("probeC_int_equal", true),
			"the integer-literal control went unstable too — the instability is no longer byte[]-specific and this probe no longer isolates it")
		Expect(pretty).To(HaveKeyWithValue("probeC_bytes_equal", false),
			"Java's binding hash for a bytes literal is now stable across two parses — Objects.hash over the byte[] identity hashCode must have been fixed")
	})
})
