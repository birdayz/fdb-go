package embedded

import (
	"errors"
	"strings"
	"testing"

	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/query/logical"
)

// Java permits a correlated array field as the primary source of an EXISTS
// subquery. The inner source is an Explode correlated to the outer scan, not a
// catalog table named "R.TAGS".
func TestPlanCorrelatedPrimaryUnnestExists(t *testing.T) {
	t.Parallel()

	const schema = `
CREATE TABLE t (
  id BIGINT NOT NULL,
  tags BIGINT ARRAY,
  PRIMARY KEY (id)
)`
	plan, err := PlanQueryForTest(
		`SELECT R.ID FROM T AS R
		 WHERE EXISTS (SELECT E FROM R.TAGS AS E WHERE E = 9)`,
		schema,
		nil,
	)
	if err != nil {
		t.Fatalf("correlated primary unnest must plan: %v", err)
	}
	if !strings.Contains(plan, "Explode") {
		t.Fatalf("correlated primary unnest plan lacks Explode: %s", plan)
	}
}

func TestPlanCorrelatedPrimaryUnnestExistsRejectsNonArrays(t *testing.T) {
	t.Parallel()

	const schema = `
CREATE TABLE t (
  id BIGINT NOT NULL,
  tags BIGINT ARRAY,
  PRIMARY KEY (id)
)`
	for _, tc := range []struct {
		name string
		sql  string
		code api.ErrorCode
	}{
		{
			name: "scalar",
			sql:  `SELECT R.ID FROM T AS R WHERE EXISTS (SELECT E FROM R.ID AS E WHERE E = 9)`,
			code: api.ErrCodeInvalidColumnReference,
		},
		{
			name: "missing",
			sql:  `SELECT R.ID FROM T AS R WHERE EXISTS (SELECT E FROM R.NOPE AS E WHERE E = 9)`,
			code: api.ErrCodeUndefinedColumn,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := PlanQueryForTest(tc.sql, schema, nil)
			if err == nil {
				t.Fatalf("want %s, got a plan", tc.code)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestPlanCorrelatedPrimaryUnnestKeepsTableFirstResolution(t *testing.T) {
	t.Parallel()

	const schema = `
CREATE TABLE t (
  id BIGINT NOT NULL,
  tags BIGINT ARRAY,
  PRIMARY KEY (id)
)
CREATE TABLE tags (
  id BIGINT NOT NULL,
  e BIGINT,
  PRIMARY KEY (id)
)`
	plan, err := PlanQueryForTest(
		`SELECT S.ID FROM T AS S
		 WHERE EXISTS (SELECT E.E FROM S.TAGS AS E WHERE E.E = 9)`,
		schema,
		nil,
	)
	if err != nil {
		t.Fatalf("schema-qualified table must retain table-first resolution: %v", err)
	}
	if strings.Contains(plan, "Explode") {
		t.Fatalf("schema-qualified table was misclassified as a correlated array: %s", plan)
	}
}

func TestBuildCorrelatedPrimaryUnnestResolvesDuplicateAliasPerAttribute(t *testing.T) {
	t.Parallel()

	b := metadata.NewSchemaTemplateBuilder().SetName("duplicate_alias_array")
	b.AddTable("LEFT_T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("TAGS", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"ID"})
	b.AddTable("RIGHT_T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("RIGHT_ID", api.NewLongType(false), 1),
	}, []string{"RIGHT_ID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	root, err := parseQueryFromSelect(t,
		`SELECT A.ID FROM LEFT_T AS A, RIGHT_T AS A
		 WHERE EXISTS (SELECT E FROM A.TAGS AS E WHERE E = 9)`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, err := buildLogicalPlanForQueryWithCatalog(root, tmpl.Underlying())
	if err != nil {
		t.Fatalf("only one duplicate-alias source carries TAGS and must resolve: %v", err)
	}
	var correlatedUnnest *logical.LogicalUnnest
	var visit func(logical.LogicalOperator)
	visit = func(node logical.LogicalOperator) {
		if unnest, ok := node.(*logical.LogicalUnnest); ok &&
			unnest.CorrelatedCollection != nil {
			correlatedUnnest = unnest
		}
		if filter, ok := node.(*logical.LogicalFilter); ok {
			for _, exists := range filter.ExistsSubqueries {
				visit(exists.Plan)
			}
		}
		for _, child := range node.Children() {
			visit(child)
		}
	}
	visit(op)
	if correlatedUnnest == nil {
		t.Fatalf("per-attribute correlated array resolution lacks LogicalUnnest: %s", op.Explain(""))
	}
}

func TestPlanCorrelatedPrimaryUnnestKeepsQuotedElementIdentity(t *testing.T) {
	t.Parallel()

	const schema = `
CREATE TABLE t (
  id BIGINT NOT NULL,
  tags BIGINT ARRAY,
  PRIMARY KEY (id)
)`
	_, err := PlanQueryForTest(
		`SELECT R.ID FROM T AS R
		 WHERE EXISTS (SELECT E FROM R.TAGS AS "e" WHERE "e" = 9)`,
		schema,
		nil,
	)
	if err == nil {
		t.Fatal(`unquoted SELECT E must not bind the quoted lower-case element alias "e"`)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Fatalf("quoted element mismatch error = %v, want %s", err, api.ErrCodeUndefinedColumn)
	}
}

// This is the production-reach certificate for RFC-190.4c: the SQL frontend
// must expose the correlated Explode to value-index matching, the fanout index
// must replace the existential child, and the E→ForEach cardinality repair
// must enforce primary-key distinctness above that scan.
func TestPlanCorrelatedPrimaryUnnestUsesFanOutIndexWithPKDistinct(t *testing.T) {
	t.Parallel()

	b := metadata.NewSchemaTemplateBuilder().SetName("fanout_exists")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("TAGS", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"ID"})
	b.AddFanOutIndex("T", "T_TAGS", "TAGS")
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}

	plan, err := PlanRecordQueryWithMetadata(
		`SELECT R.ID FROM T AS R
		 WHERE EXISTS (SELECT E FROM R.TAGS AS E WHERE E = 9)`,
		tmpl.Underlying(),
		properties.FixedStatistics{Cardinality: 1_000_000},
	)
	if err != nil {
		t.Fatalf("plan fanout EXISTS: %v", err)
	}
	explain := plan.Explain()
	for _, required := range []string{"IndexScan(T_TAGS", "UnorderedPrimaryKeyDistinct"} {
		if !strings.Contains(explain, required) {
			t.Fatalf("fanout EXISTS plan must contain %q: %s", required, explain)
		}
	}
	if strings.Contains(explain, "Scan(T)") || strings.Contains(explain, "Explode") {
		t.Fatalf("fanout EXISTS fell back to base scan/explode instead of the index: %s", explain)
	}
}

// Partial matches are keyed by MatchCandidate identity. A metadata context
// must therefore return the same candidate objects on every read; rebuilding
// them makes a leaf match invisible while MatchIntermediate climbs the
// multi-level fanout candidate graph.
func TestMetadataPlanContextMatchCandidateIdentityIsStable(t *testing.T) {
	t.Parallel()

	b := metadata.NewSchemaTemplateBuilder().SetName("fanout_candidate_identity")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("TAGS", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"ID"})
	b.AddFanOutIndex("T", "T_TAGS", "TAGS")
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}

	ctx := buildCascadesPlanContext(tmpl.Underlying(), cascades.DefaultPlannerConfiguration())
	first := ctx.GetMatchCandidates()
	second := ctx.GetMatchCandidates()
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("candidate counts = %d and %d, want equal non-zero counts", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf(
				"candidate %q identity changed between reads: %p != %p",
				first[i].CandidateName(),
				first[i],
				second[i],
			)
		}
	}

	// The returned slice itself is caller-owned; mutating it must not corrupt
	// the context's cached candidate list.
	first[0] = nil
	third := ctx.GetMatchCandidates()
	if third[0] == nil || third[0] != second[0] {
		t.Fatal("caller mutation leaked into cached match candidates")
	}
}

// TestStructColumnIsRejectedAtDDL pins the gate that makes a MULTI-SEGMENT
// lateral unnest (`FROM t, t.rec.arr AS x`) unreachable from SQL: a struct
// column cannot be declared, so no table can carry a nested array for such a
// path to descend.
//
// This is a load-bearing NEGATIVE result, not trivia. The lowering's
// multi-segment collection bake (unnestBakedRootCollection's suffix arm, pinned
// unit-wise by TestUnnestBakedRootCollectionFusesAMultiSegmentPath in
// pkg/relational/core/query) has no end-to-end route ONLY because of this
// rejection. When struct columns land, that arm becomes reachable and needs a
// full translateUnnestJoin case plus a rows assertion — nothing else records
// that dependency, so the day this test starts failing is the day the coverage
// gap opens.
func TestStructColumnIsRejectedAtDDL(t *testing.T) {
	t.Parallel()

	const schema = `
CREATE TYPE AS STRUCT nested_s (vals BIGINT ARRAY, label STRING)
CREATE TABLE w (
  wid BIGINT NOT NULL,
  nested nested_s,
  PRIMARY KEY (wid)
)`
	_, err := PlanQueryForTest(`SELECT x FROM w, w.nested.vals AS x`, schema, nil)
	if err == nil {
		t.Fatal("a struct column was accepted — a nested array is now declarable, so the " +
			"multi-segment lateral-unnest arm is REACHABLE and needs end-to-end coverage " +
			"through translateUnnestJoin (see TestUnnestBakedRootCollectionFusesAMultiSegmentPath)")
	}
	// The DDL wraps the column rejection in a table-scope error, so the code
	// that matters sits down the Unwrap chain rather than on the outermost
	// error. Walking to it is the point: an outer-code assertion would go on
	// passing if the inner cause changed to something unrelated.
	var found bool
	for e := err; e != nil; e = errors.Unwrap(e) {
		var apiErr *api.Error
		if errors.As(e, &apiErr) && apiErr.Code == api.ErrCodeUnsupportedOperation &&
			strings.Contains(apiErr.Message, "only primitive column types") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("error = %v, want %s \"only primitive column types are supported\" in the "+
			"cause chain — a DIFFERENT rejection means the multi-segment path is now "+
			"blocked somewhere else, and this pin has stopped naming the thing that "+
			"re-arms it", err, api.ErrCodeUnsupportedOperation)
	}
}
