//go:build bazelrunfiles

package conformance_test

// Committed instrument for the measurements of the LIVE Java fdb-relational
// EXECUTE CONTINUATION path (tag 4.12.11.0) that the Go continuation envelope
// is designed against, taken through the conformance server's
// `continuationProbe` step (conformance/sql_plan_steps.java). Every number the
// design rests on has to survive as an assertion here rather than as a
// transcript, so a Java upgrade that moves one of them goes red instead of
// silently invalidating the design:
//
//	A. An unknown plan_serialization_mode ("GO_V0") is resolved by
//	   PlanValidator.validateSerializedPlanSerializationMode via
//	   PlanHashMode.valueOf BEFORE the validPlanHashModes membership test —
//	   so a foreign mode string surfaces Enum.valueOf's failure under
//	   SQLSTATE XXXXX (ErrorCode.UNKNOWN), not the typed INVALID_CONTINUATION
//	   (24F00) the membership test would raise. The exact SQLSTATE is pinned
//	   because client-facing guidance about cross-engine tokens depends on it.
//
//	B. ContinuedPhysicalQueryPlan.validatePlanAgainstEnvironment catches
//	   PlanValidationException from validateContinuationConstraint, counts
//	   CONTINUATION_REJECTED, and falls through to CONTINUATION_ACCEPTED. A
//	   continuation whose plan constraint no longer holds is still executed.
//	   Two facts guard that conclusion from being a blind spot: the ORIGINAL
//	   constraint is asserted to be the real two-predicate conjunction (so
//	   the swap replaced something load-bearing), and the same doctored bytes
//	   with plan_hash perturbed by one ARE rejected with 24F00 (so the bytes
//	   demonstrably reach Java's validation chain).
//
//	C. AstNormalizer.processLiteral folds each literal into the parameter
//	   hash with Objects.hash(canonicalName, literal). For a x'..' literal
//	   that literal is a byte[], whose hashCode() is identity — so the
//	   binding hash stamped into the continuation is not stable across two
//	   executions of the same query text. The integer-literal control is
//	   stable, non-zero, and — measured across two independently spawned
//	   JVMs — identical across processes.
//
// The spec PRINTS the full JSON so the raw numbers stay readable in the test
// log. Exact hash VALUES are deliberately not pinned — only the stable/unstable
// relation, non-zeroness, and cross-process agreement, which are the
// load-bearing properties.
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

// runContinuationProbe spawns a FRESH Java conformance server (its own JVM
// process), invokes the continuationProbe step against clusterFile, and returns
// the decoded JSON. A fresh server per call is what makes the cross-process
// binding-hash comparison below an actual cross-process measurement.
func runContinuationProbe(ctx context.Context, clusterFile, label string) map[string]any {
	GinkgoHelper()

	srv, err := NewIsolatedJavaInvoker()
	Expect(err).NotTo(HaveOccurred(), "spawning JVM %s", label)
	defer func() { _ = srv.Close() }()

	raw, err := srv.Invoke(ctx, "continuationProbe", map[string]any{
		"clusterFile": clusterFile,
	})
	// A missing step comes back as
	// "No conformance step found with name: continuationProbe" — this is
	// the assertion that goes red if the Java step is deleted.
	Expect(err).NotTo(HaveOccurred(), "continuationProbe step must exist and run (JVM %s)", label)

	var decoded map[string]any
	Expect(json.Unmarshal(raw, &decoded)).To(Succeed())
	indented, mErr := json.MarshalIndent(decoded, "", "  ")
	Expect(mErr).NotTo(HaveOccurred())
	GinkgoWriter.Printf("continuationProbe RAW JSON (JVM %s):\n%s\n", label, string(indented))
	AddReportEntry("continuationProbe/"+label, string(indented))

	Expect(decoded).To(HaveKeyWithValue("seed_ok", true), "probe table must seed (JVM %s)", label)
	return decoded
}

// probeNumber returns a numeric probe field, failing if the key is absent or
// carries JSON null. recordThrowable / addNullableInt emit JsonNull whenever an
// arm did not measure what it was supposed to, and a bare map lookup would hand
// back an untyped nil that most matchers accept — so every read goes through
// here rather than indexing the map directly.
func probeNumber(probe map[string]any, key string) float64 {
	GinkgoHelper()
	v, present := probe[key]
	Expect(present).To(BeTrue(), "probe result must carry %q", key)
	f, ok := v.(float64)
	Expect(ok).To(BeTrue(), "%q must be a JSON number, got %T (%v) — a null here means the Java arm never measured it", key, v, v)
	return f
}

// probeString is probeNumber's counterpart for string fields. It is the guard
// against a vacuously-green assertion on a SQLSTATE: recordThrowable writes
// JsonNull for any non-SQLException, so a null SQLSTATE must fail loudly rather
// than satisfy a matcher.
func probeString(probe map[string]any, key string) string {
	GinkgoHelper()
	v, present := probe[key]
	Expect(present).To(BeTrue(), "probe result must carry %q", key)
	s, ok := v.(string)
	Expect(ok).To(BeTrue(), "%q must be a JSON string, got %T (%v) — a null here means the throwable was not a SQLException", key, v, v)
	return s
}

var _ = Describe("ContinuationProbe", func() {
	It("measures Java's EXECUTE CONTINUATION mode / constraint / binding-hash behaviour", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("contprobe_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		pretty := runContinuationProbe(ctx, env.ClusterFile, "1")

		// --- Probe A: the mode string never reaches a typed rejection. ---
		GinkgoWriter.Printf("[A] threw=%v class=%v sqlState=%v\n    message=%v\n    causeChain=%v\n",
			pretty["probeA_threw"], pretty["probeA_exceptionClass"],
			pretty["probeA_sqlState"], pretty["probeA_message"], pretty["probeA_causeChain"])
		Expect(pretty).To(HaveKeyWithValue("probeA_threw", true))
		// Enum.valueOf runs before the validPlanHashModes membership test, so
		// an unrecognised mode is an IllegalArgumentException, not the
		// PlanValidationException the membership test would raise.
		Expect(probeString(pretty, "probeA_message")).To(ContainSubstring("No enum constant"),
			"Java stopped resolving the mode with Enum.valueOf first — re-check PlanValidator.validateSerializedPlanSerializationMode")
		Expect(probeString(pretty, "probeA_causeChain")).To(ContainSubstring("java.lang.IllegalArgumentException"))
		// Pinned POSITIVELY, not as "!= 24F00": INVALID_CONTINUATION is 24F00
		// and this path yields XXXXX (ErrorCode.UNKNOWN). Any client guidance
		// promising a 24F00 for a foreign continuation would be wrong, and a
		// negative assertion here would also pass on a null SQLSTATE.
		Expect(probeString(pretty, "probeA_sqlState")).To(Equal("XXXXX"),
			"the SQLSTATE for an unknown plan_serialization_mode moved; if it is now 24F00 the escape hatch closed and the envelope's Path-1 fence must be revisited")

		// --- Probe B: the fail-open. ---
		GinkgoWriter.Printf("[B] control rows=%v | doctored threw=%v rows=%v class=%v sqlState=%v\n    message=%v\n",
			pretty["probeB_control_rowsReturned"], pretty["probeB_threw"],
			pretty["probeB_rowsReturned"], pretty["probeB_exceptionClass"],
			pretty["probeB_sqlState"], pretty["probeB_message"])
		GinkgoWriter.Printf("[B] orig constraint present=%v proto=%v\n",
			pretty["probeB_origHasPlanConstraint"], pretty["probeB_origPlanConstraintProto"])

		// The constraint the swap REPLACED. Java's real continuation carries
		// the conjunction of the compatible-type-evolution and
		// database-object-dependencies predicates; those two are the design
		// input for the Go constraint port, so the shape is asserted rather
		// than merely reported. If Java starts emitting a different (or empty)
		// constraint, the port's target changed.
		Expect(pretty).To(HaveKeyWithValue("probeB_origHasPlanConstraint", true),
			"the untouched continuation carries no plan constraint at all — the swap replaced nothing and probe B proves nothing")
		origConstraint := probeString(pretty, "probeB_origPlanConstraintProto")
		Expect(origConstraint).To(ContainSubstring("compatible_type_evolution"),
			"Java's real continuation constraint no longer includes the compatible-type-evolution predicate")
		Expect(origConstraint).To(ContainSubstring("database_object_dependencies"),
			"Java's real continuation constraint no longer includes the database-object-dependencies predicate")

		// The doctored constraint must actually be constant-FALSE, or "rows
		// came back" proves nothing about the fail-open.
		Expect(probeString(pretty, "probeB_falsePlanConstraintProto")).To(ContainSubstring("constant_predicate"))
		Expect(probeString(pretty, "probeB_falsePlanConstraintProto")).To(ContainSubstring("value: false"))
		Expect(pretty).To(HaveKeyWithValue("probeB_control_rowsReturned", float64(3)),
			"the untouched continuation must resume the remaining 3 rows; otherwise the doctored run has no baseline")
		Expect(pretty).To(HaveKeyWithValue("probeB_threw", false),
			"Java now REJECTS a continuation whose plan constraint evaluates false — the fail-open at ContinuedPhysicalQueryPlan.validatePlanAgainstEnvironment closed")
		Expect(pretty).To(HaveKeyWithValue("probeB_rowsReturned", float64(3)),
			"a false plan constraint must still yield every remaining row for the fail-open to be what it looks like")

		// --- Probe B contrast arm: the same doctored bytes, plan_hash+1. ---
		// Without this, "Java accepted the doctored continuation" is
		// indistinguishable from "the probe never got Java to look at the
		// doctored continuation". PlanGenerator's plan-hash comparison is
		// unguarded by any catch, so it rejects; the constraint gate is
		// therefore the odd one out, not general leniency.
		GinkgoWriter.Printf("[B/hash] origPlanHash=%v perturbed=%v | threw=%v sqlState=%v\n    message=%v\n",
			pretty["probeB_origPlanHash"], pretty["probeB_perturbedPlanHash"],
			pretty["probeB_hashPerturbed_threw"], pretty["probeB_hashPerturbed_sqlState"],
			pretty["probeB_hashPerturbed_message"])
		Expect(pretty).To(HaveKeyWithValue("probeB_hashPerturbed_threw", true),
			"Java ACCEPTED a continuation whose plan_hash does not match the serialized plan — the probe can no longer demonstrate that doctored bytes reach validation, so probe B's acceptance may be a blind spot")
		Expect(probeString(pretty, "probeB_hashPerturbed_sqlState")).To(Equal("24F00"),
			"the plan-hash mismatch no longer surfaces as INVALID_CONTINUATION")
		Expect(probeString(pretty, "probeB_hashPerturbed_message")).To(
			ContainSubstring("cannot continue query due to mismatch between serialized and actual plan hash"),
			"PlanGenerator's plan-hash rejection message moved")

		// --- Probe C: binding-hash stability. ---
		GinkgoWriter.Printf("[C] bytes: %v vs %v (equal=%v) | int: %v vs %v (equal=%v)\n",
			pretty["probeC_bytes_hash1"], pretty["probeC_bytes_hash2"], pretty["probeC_bytes_equal"],
			pretty["probeC_int_hash1"], pretty["probeC_int_hash2"], pretty["probeC_int_equal"])
		Expect(pretty).To(HaveKeyWithValue("probeC_int_equal", true),
			"the integer-literal control went unstable too — the instability is no longer byte[]-specific and this probe no longer isolates it")
		// Equal-and-zero would also satisfy the equality above and would mean
		// the control measured nothing (an unset int32 field decodes as 0).
		intHash1 := probeNumber(pretty, "probeC_int_hash1")
		intHash2 := probeNumber(pretty, "probeC_int_hash2")
		Expect(intHash1).NotTo(BeZero(),
			"the integer-literal binding hash is 0 — the control agrees only because nothing was measured")
		Expect(intHash2).NotTo(BeZero(),
			"the integer-literal binding hash is 0 — the control agrees only because nothing was measured")
		Expect(pretty).To(HaveKeyWithValue("probeC_bytes_equal", false),
			"Java's binding hash for a bytes literal is now stable across two parses — Objects.hash over the byte[] identity hashCode must have been fixed")

		// --- Probe C, cross-process. ---
		// The design claim is that the integer binding hash is stable across
		// PROCESSES, not merely within one JVM; a single-JVM run cannot see the
		// difference, because an identity hashCode is also stable within a
		// process once the object is interned. A second independently spawned
		// conformance server (its own JVM, its own heap, its own identity hash
		// seed) is what separates "deterministic function of the query text"
		// from "stable reference for the lifetime of this heap".
		second := runContinuationProbe(ctx, env.ClusterFile, "2")
		secondIntHash := probeNumber(second, "probeC_int_hash1")
		secondBytesHash := probeNumber(second, "probeC_bytes_hash1")
		GinkgoWriter.Printf("[C/cross-JVM] int: %v (JVM 1) vs %v (JVM 2) | bytes: %v vs %v\n",
			intHash1, secondIntHash, pretty["probeC_bytes_hash1"], secondBytesHash)
		Expect(secondIntHash).To(Equal(intHash1),
			"the integer-literal binding hash disagreed across two separate JVM launches — it is not a deterministic function of the query text, so Go cannot reproduce it from the text alone")
		Expect(second).To(HaveKeyWithValue("probeC_bytes_equal", false),
			"the bytes-literal binding hash stabilised in the second JVM — the instability finding no longer reproduces")
	})
})
