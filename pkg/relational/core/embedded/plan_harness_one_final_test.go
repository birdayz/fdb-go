package embedded

import cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"

// planAndVerifyExtraction plans sql and returns RFC-224's report for the exact
// Reference path selector-based extraction dereferences. Unlike the retired
// one-final check, the report treats property-retained alternatives as legal
// and makes vacuity visible through explicit visited/dead-end counts.
//
// It lives in a _test.go file because it has no consumer outside this
// package's invariant tests. The rest of plan_harness.go is genuine
// cross-package API and remains production code.
//
// Safe under t.Parallel: both the enable flag and report belong to the one
// Planner created for this call; no package-global measurement is shared.
func planAndVerifyExtraction(sql, schema string) (cascades.ExtractionVerificationReport, error) {
	_, report, err := planPhysicalForTest(sql, schema, nil, true, nil, plannerOptionsFrom(nil))
	if err != nil {
		return cascades.ExtractionVerificationReport{}, err
	}
	if report == nil {
		return cascades.ExtractionVerificationReport{}, nil
	}
	return *report, nil
}
