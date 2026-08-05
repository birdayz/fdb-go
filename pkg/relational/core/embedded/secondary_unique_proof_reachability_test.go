package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// properties.SecondaryUniqueKeyGloballyEnforced is the shared gate for the two
// things a secondary UNIQUE index is allowed to prove: that a DISTINCT is
// redundant, and that an index scan is strictly sorted. Both require every
// indexed key component to be NOT NULL, and the requirement is not negotiable —
// under NULLS DISTINCT the uniqueness check is SKIPPED when a component is NULL,
// so a UNIQUE index on a nullable column legitimately holds (NULL), (NULL),
// (NULL), distinguished only by their appended primary keys.
//
// This test pins a NEGATIVE result, and it is load-bearing rather than
// decorative: it is what makes "the proof does not fire on any SQL query" a
// measured fact rather than an assumption, and it names exactly what gets
// re-armed if the fact changes.
//
// The SQL layer cannot express a NOT NULL scalar column at all — CREATE rejects
// it, deliberately, for Java parity (NOT NULL is accepted only for ARRAY column
// types). So every scalar column is proto OPTIONAL, every SQL-expressible
// secondary unique index has a nullable key, and the gate is false for all of
// them. An ARRAY column can be NOT NULL, but an array key is fan-out, which the
// base-record-cardinality clause refuses for an independent reason.
//
// WHAT RE-ARMS IF THIS TEST GOES RED: a scalar NOT NULL column becoming
// expressible makes BOTH proofs live on the SQL surface for the first time.
// Neither has end-to-end coverage today because neither can fire, and both then
// need it — the DISTINCT elision especially, because its failure mode is
// duplicate rows rather than a missed optimization.
func TestSecondaryUniqueProofIsUnreachableWhileScalarNotNullIsInexpressible(t *testing.T) {
	t.Parallel()

	// (a) The DDL rejects a NOT NULL scalar. This is the root fact; everything
	// below is its consequence.
	_, err := buildSchemaTemplateFromDDL(
		"CREATE TABLE T (ID BIGINT, E STRING NOT NULL, PRIMARY KEY (ID))")
	if err == nil {
		t.Fatal("CREATE TABLE accepted a NOT NULL scalar column. Both secondary-UNIQUE " +
			"proofs (DISTINCT elision, strict-ordering) are now reachable from SQL for " +
			"the first time and require end-to-end coverage")
	}
	if !strings.Contains(err.Error(), "NOT NULL is only allowed for ARRAY column type") {
		t.Fatalf("NOT NULL on a scalar is rejected for an unexpected reason, so this "+
			"test is no longer pinning the fact it names: %v", err)
	}

	// (b) Consequence at the CANDIDATE level: every SQL-expressible secondary
	// unique index key is nullable, whatever the column type and whichever
	// CREATE INDEX form declares it — including a key over a PRIMARY KEY column,
	// which is non-null in storage but not in the type the planner reads.
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
				t.Fatalf("%s: unique index U has a globally enforced key (%v). The "+
					"secondary-UNIQUE DISTINCT elision can now fire on a SQL query and "+
					"needs the end-to-end coverage it does not have",
					name, typed.GetKeyComponentTypes())
			}
		}
		if !seen {
			t.Fatalf("%s: no unique candidate named U was built, so this row observed nothing", name)
		}
	}

	// (c) Consequence at the PLAN level, for the OTHER consumer of the same
	// gate. #617's strict-ordering claim (indexHasGloballyEnforcedUniqueKey) is
	// dead on the SQL surface for exactly this reason, which is why nothing here
	// changed when the DISTINCT arm was added.
	plan, err := PlanPhysicalForTest(
		"SELECT N FROM T ORDER BY N",
		"CREATE TABLE T (ID BIGINT, N STRING, PRIMARY KEY (ID))\nCREATE UNIQUE INDEX U ON T (N)",
		nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	scanned := false
	plans.Walk(plan, func(node plans.RecordQueryPlan) bool {
		indexPlan, ok := node.(*plans.RecordQueryIndexPlan)
		if !ok || !indexPlan.IsUnique() {
			return true
		}
		scanned = true
		if indexPlan.IsStrictlySorted() {
			t.Fatalf("a SQL-planned unique index scan is strictlySorted (%s). The "+
				"strict-ordering half of the gate is live on the SQL surface now",
				plan.Explain())
		}
		return true
	})
	if !scanned {
		t.Fatalf("the ORDER BY query planned no unique index scan, so (c) observed "+
			"nothing: %s", plan.Explain())
	}
}
