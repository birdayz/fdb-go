package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This test is the probe that produced RFC-210's second refutation: the arm it
// was written to record as unreachable was unreachable for a reason nobody had
// stated, and stating it is what produced R2 and R3. It is REWRITTEN rather than
// deleted now that those routes are live — three of its claims survive, one
// INVERTS, and the boundary between them is the thing worth pinning.
//
// A secondary UNIQUE index's EXEMPT SET is the entries on which the declaration
// constrains nothing or something finer than logical row equality: NULL key
// components (under NULLS DISTINCT the uniqueness check is SKIPPED, so one NULL
// prefix legitimately holds arbitrarily many entries) and raw-NaN components.
// Every proof built on a UNIQUE declaration needs that set to be EMPTY on the
// rows it will see. Three routes discharge it:
//
//	R1 — the CATALOG: every key component declared NOT NULL, none FLOAT/DOUBLE.
//	R2 — the PREDICATE: NULL rejected on every key column, on THIS stream.
//	R3 — the RESIDUAL: dedup exactly the exempt subset instead of proving it empty.
//
// What this test pins is the BOUNDARY: R1 cannot fire from SQL at all, and R2
// can. The two facts are one fact seen from two sides, and separating them is
// how the arm came to look dead when it was only inert.
func TestSecondaryUniqueProofBoundary_R1UnreachableFromSQL(t *testing.T) {
	t.Parallel()

	// ---------------------------------------------------------------------
	// (a) The ROOT FACT, unchanged: the DDL rejects a NOT NULL scalar column.
	//
	// It is MORE load-bearing now than when this test was written, not less. It
	// is the reason R1 is vacuous on the SQL surface, which is the reason R2 and
	// R3 exist at all. NOT NULL is accepted only for ARRAY column types
	// (deliberately, for Java parity), and an array key is fan-out, which the
	// base-record-cardinality clause refuses for an independent reason.
	// ---------------------------------------------------------------------
	_, err := buildSchemaTemplateFromDDL(
		"CREATE TABLE T (ID BIGINT, E STRING NOT NULL, PRIMARY KEY (ID))")
	if err == nil {
		t.Fatal("CREATE TABLE accepted a NOT NULL scalar column. R1 — the catalog " +
			"route — is now reachable from SQL for the first time and needs the " +
			"end-to-end coverage it does not have; every existing SQL-level test of " +
			"this area exercises R2 or R3 instead")
	}
	if !strings.Contains(err.Error(), "NOT NULL is only allowed for ARRAY column type") {
		t.Fatalf("NOT NULL on a scalar is rejected for an unexpected reason, so this "+
			"test is no longer pinning the fact it names: %v", err)
	}

	// ---------------------------------------------------------------------
	// (b) The consequence at the CANDIDATE level, with the claim NARROWED.
	//
	// This arm used to assert "the proof does not fire on any SQL query". That is
	// now FALSE — R2 and R3 fire. The surviving claim is the precise one:
	// SecondaryUniqueKeyGloballyEnforced, which is R1's implementation, is false
	// for EVERY SQL-expressible unique index. Same loop, same fixtures, a claim
	// about R1 rather than about the whole arm.
	// ---------------------------------------------------------------------
	for name, ddl := range map[string]string{
		"STRING":  "CREATE TABLE T (ID BIGINT, N STRING, PRIMARY KEY (ID))\nCREATE UNIQUE INDEX U ON T (N)",
		"INTEGER": "CREATE TABLE T (ID BIGINT, N INTEGER, PRIMARY KEY (ID))\nCREATE UNIQUE INDEX U ON T (N)",
		"BIGINT":  "CREATE TABLE T (ID BIGINT, N BIGINT, PRIMARY KEY (ID))\nCREATE UNIQUE INDEX U ON T (N)",
		"BOOLEAN": "CREATE TABLE T (ID BIGINT, N BOOLEAN, PRIMARY KEY (ID))\nCREATE UNIQUE INDEX U ON T (N)",
		"BYTES":   "CREATE TABLE T (ID BIGINT, N BYTES, PRIMARY KEY (ID))\nCREATE UNIQUE INDEX U ON T (N)",
		"primary-key column": "CREATE TABLE T (ID BIGINT, E STRING, PRIMARY KEY (ID))\n" +
			"CREATE UNIQUE INDEX U ON T (ID)",
		"AS SELECT form": "CREATE TABLE T (ID BIGINT, N STRING, PRIMARY KEY (ID))\n" +
			"CREATE UNIQUE INDEX U AS SELECT N FROM T ORDER BY N",
	} {
		tmpl, buildErr := buildSchemaTemplateFromDDL(ddl)
		if buildErr != nil {
			t.Fatalf("%s: schema DDL: %v", name, buildErr)
		}
		pc := &metadataPlanContext{md: tmpl.Underlying()}
		seen := false
		for _, candidate := range pc.buildMatchCandidates() {
			if !candidate.IsUnique() || candidate.CandidateName() != "U" {
				continue
			}
			typed, ok := candidate.(interface{ GetKeyComponentTypes() []values.Type })
			if !ok {
				t.Fatalf("%s: candidate U states no authoritative key component types, "+
					"so clause 8 would be judged on a declared SQL type instead", name)
			}
			seen = true
			if properties.SecondaryUniqueKeyGloballyEnforced(typed.GetKeyComponentTypes(), 1) {
				t.Fatalf("%s: unique index U has a globally enforced key (%v). R1 now "+
					"fires from SQL, which it never has; the routes below are no "+
					"longer the only ones and R1 needs coverage of its own",
					name, typed.GetKeyComponentTypes())
			}
		}
		if !seen {
			t.Fatalf("%s: no unique candidate named U was built, so this row observed nothing", name)
		}
	}
}

// TestSecondaryUniqueProof_StrictOrderingReachability is arm (c), and it is the
// one that INVERTED.
//
// It used to assert that a SQL-planned unique index scan is never strictlySorted
// — the blanket unreachability the whole test was written to record. RFC-210 §5.7
// makes that false on a NULL-rejecting stream, and deliberately so: over a UNIQUE
// index on a nullable column the claim is FALSE in general (the index legitimately
// holds (NULL,pk=1),(NULL,pk=2),(NULL,pk=3), three entries whose claimed sort key
// is identical — the upstream bug Java ships at RemoveSortRule.java:153), and TRUE
// exactly when the scan cannot reach those entries.
//
// So the arm keeps both sides and pins the BOUNDARY rather than one edge of it.
// Each row below states which route decides it and why.
func TestSecondaryUniqueProof_StrictOrderingReachability(t *testing.T) {
	t.Parallel()

	const ddl = "CREATE TABLE T (ID BIGINT, N STRING, PRIMARY KEY (ID))\n" +
		"CREATE UNIQUE INDEX U ON T (N)"

	for _, tc := range []struct {
		name           string
		query          string
		wantStrictSort bool
		because        string
	}{
		{
			name:           "unfiltered_declines",
			query:          "SELECT N FROM T ORDER BY N",
			wantStrictSort: false,
			because: "the scan reaches every entry the index holds, NULLs included, " +
				"so its sort key has genuine ties. R1 is false for this index (arm (b)) " +
				"and R2 has nothing to read. This is the negative half of the boundary " +
				"and the shape Java claims strictlySorted on",
		},
		{
			name:           "is_not_null_licenses",
			query:          "SELECT N FROM T WHERE N IS NOT NULL ORDER BY N",
			wantStrictSort: true,
			because: "R2 via the SCAN RANGE. IS NOT NULL is admitted by " +
				"isSargableComparisonForMatch, so the planner pushes it INTO the range " +
				"rather than leaving a residual filter — which is why the scan-range " +
				"route is the one that matters here and not the filter route",
		},
		{
			name:           "ordered_bound_licenses",
			query:          "SELECT N FROM T WHERE N > 'a' ORDER BY N",
			wantStrictSort: true,
			because: "R2 again: a lower bound at a non-NULL comparand sits above the " +
				"NULL boundary, so the NULL entries are outside the range",
		},
		{
			name:           "residual_filter_declines",
			query:          "SELECT N FROM T WHERE N <> 'z' ORDER BY N",
			wantStrictSort: false,
			because: "the SQL-path twin of the refusal in " +
				"cascades/null_rejecting_scan_range_test.go. `N <> 'z'` DOES reject " +
				"NULL and NOT_EQUALS IS on R2's allow-list, but it is not SARGable, so " +
				"it survives as PredicatesFilter over a FULL scan. Crediting it would " +
				"mark the scan BELOW the filter, and that scan still emits every NULL " +
				"entry — its own RichOrdering would then claim distinctness over a " +
				"stream with ties, readable by any consumer that never sees the filter",
		},
		{
			name:           "is_null_declines",
			query:          "SELECT N FROM T WHERE N IS NULL ORDER BY N",
			wantStrictSort: false,
			because: "the trap. IS NULL is a scan-range EQUALITY type exactly as " +
				"ordinary equality is, and it seeks the NULL entries — which on this " +
				"stream are ALL that is left. Refused by the allow-list",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(tc.query, ddl, nil)
			if err != nil {
				t.Fatalf("plan %q: %v", tc.query, err)
			}
			scanned := false
			plans.Walk(plan, func(node plans.RecordQueryPlan) bool {
				indexPlan, ok := node.(*plans.RecordQueryIndexPlan)
				if !ok || !indexPlan.IsUnique() {
					return true
				}
				scanned = true
				if got := indexPlan.IsStrictlySorted(); got != tc.wantStrictSort {
					t.Fatalf("%s\nstrictlySorted = %v, want %v — because %s\nplan: %s",
						tc.query, got, tc.wantStrictSort, tc.because, plan.Explain())
				}
				return true
			})
			if !scanned {
				t.Fatalf("%s planned no unique index scan, so this row observed "+
					"nothing: %s", tc.query, plan.Explain())
			}
		})
	}
}
