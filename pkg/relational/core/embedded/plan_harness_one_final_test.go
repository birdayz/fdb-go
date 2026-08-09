package embedded

// planAndVerifyOneFinal plans sql and returns every reference reachable at
// extraction that holds more than one PHYSICAL final expression — RFC-183
// P5's precondition (Java's getRangesOverPlan is getOnlyElement over the final
// expressions, which throws on two).
//
// READ THE COMMENT ON TestOneFinalPlanPerReference BEFORE TRUSTING WHAT THIS
// RETURNS. An empty result does not mean the property holds: the underlying
// walk terminates at any reference with an empty final set, and the property
// itself is Java's mechanism rather than a Go invariant. RFC-224 settles both.
//
// It lives in a _test.go file because it is the one function in the planning
// harness with no consumer outside this package's tests. The rest of
// plan_harness.go is genuine cross-package API — `PlanRecordQueryWithMetadata`
// alone has 110 call sites elsewhere, and `explaindiff.go` (a NON-test file in
// another package) calls `PlanPhysicalForTestWithReachability` — so the file as
// a whole is production code despite its ForTest names, and moving it wholesale
// broke the build.
//
// Safe under t.Parallel: verifyOneFinal is a per-Planner flag and the
// violations are RETURNED, so nothing is shared between callers.
func planAndVerifyOneFinal(sql, schema string) ([]string, error) {
	_, violations, err := planPhysicalForTest(sql, schema, nil, true, nil, plannerOptionsFrom(nil))
	if err != nil {
		return nil, err
	}
	return violations, nil
}
