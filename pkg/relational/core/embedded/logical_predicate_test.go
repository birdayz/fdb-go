package embedded

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"github.com/antlr4-go/antlr/v4"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

// parseQueryFromSelect parses SQL and returns the IQueryContext from
// the first SELECT statement. Used by Query-level builder tests.
func parseQueryFromSelect(t *testing.T, sql string) (antlrgen.IQueryContext, error) {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	stmt := root.Statements().AllStatement()[0]
	sel := stmt.SelectStatement()
	if sel == nil {
		t.Fatalf("not a SELECT statement: %q", sql)
	}
	return sel.Query(), nil
}

// buildTestMetaData returns a minimal RecordMetaData with the demo
// record types registered. Used by the catalog-aware builder tests
// to exercise rlcatalog lookups without a live FDB.
func buildTestMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return md
}

// TestSingleSourceOnOnlyCTEScopeBindsEveryClauseToTheProjectedRow pins the
// block-local admission for a complete join-bodied CTE schema. The nested CTE
// deliberately shadows the real Order table: its row contains only ORDER_ID,
// while the catalog table contains many columns. WHERE, projection, and ORDER
// BY must therefore all resolve against the same exact one-field CTE QOV. A
// catalog fallback would give ORDER a wider base-table type and later collide
// with the one-field runtime edge.
func TestSingleSourceOnOnlyCTEScopeBindsEveryClauseToTheProjectedRow(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	queryCtx, err := parseQueryFromSelect(t, `WITH c2(x) AS (
		WITH Order AS (
			SELECT a.order_id AS order_id
			FROM Order AS a, Order AS b
			WHERE a.order_id = b.order_id
		)
		SELECT order_id FROM Order WHERE order_id > 0 ORDER BY order_id
	) SELECT x FROM c2 ORDER BY x`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, err := NewPlanVisitor(md).VisitQuery(queryCtx)
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}

	wantCorrelation := values.NamedCorrelationIdentifier("ORDER")
	wantType := values.NewRecordType("", false, []values.Field{{
		Name: "ORDER_ID", FieldType: values.NullableLong,
	}})
	found := map[string]bool{}
	inspect := func(site string, value values.Value) {
		if value == nil {
			return
		}
		values.WalkValue(value, func(candidate values.Value) bool {
			field, ok := values.AsFieldValue(candidate)
			if !ok || field.Path() == nil {
				return true
			}
			root, ok := values.AsQuantifiedObjectValue(field.ChildValue())
			if !ok || root.Correlation() != wantCorrelation {
				return true
			}
			if !root.FlowedType().Equals(wantType) {
				t.Fatalf("%s ORDER root type = %s, want exact projected CTE row %s",
					site, root.FlowedType(), wantType)
			}
			ordinals := field.Path().Ordinals()
			if len(ordinals) != 1 || ordinals[0] != 0 {
				t.Fatalf("%s ORDER_ID path = %v, want [0]", site, ordinals)
			}
			found[site] = true
			return false
		})
	}
	var walk func(logical.LogicalOperator)
	walk = func(candidate logical.LogicalOperator) {
		if candidate == nil {
			return
		}
		switch node := candidate.(type) {
		case *logical.LogicalFilter:
			if node.Predicate != nil {
				_, transformErr := predicates.TransformEmbeddedValuesChecked(
					node.Predicate, func(value values.Value) (values.Value, error) {
						inspect("where", value)
						return value, nil
					})
				if transformErr != nil {
					t.Fatalf("inspect WHERE predicate: %v", transformErr)
				}
			}
		case *logical.LogicalProject:
			for _, value := range node.ProjectedValues {
				inspect("projection", value)
			}
		case *logical.LogicalSort:
			for _, key := range node.Keys {
				inspect("sort", key.Value)
			}
		}
		for _, child := range candidate.Children() {
			walk(child)
		}
	}
	walk(op)
	for _, site := range []string{"where", "projection", "sort"} {
		if !found[site] {
			t.Fatalf("no exact projected ORDER root found at %s", site)
		}
	}
}

// TestSingleSourceQueryBlockCTEScopes_DoesNotPromoteAcrossMultiLeg is the
// flatten-evasion mutation control. A complete ON-only schema is eligible for
// one sole-source block, but remains invisible to a comma/join block where
// advertising it as a general CTE source can rebind another leg's bare column.
func TestSingleSourceQueryBlockCTEScopes_DoesNotPromoteAcrossMultiLeg(t *testing.T) {
	t.Parallel()
	onlySource := semantic.ScopeSource{Table: &semantic.StaticTable{
		TableName: semantic.FromSegments([]string{"C"}, false),
		TableColumns: []semantic.Column{{
			Id: semantic.FromNormalized("ID"), Type: "BIGINT",
		}},
	}}
	onOnly := map[string]semantic.ScopeSource{"C": onlySource}
	query := &selectQuery{
		tableName: "C",
		joins:     []joinClause{{tableName: "ORDER", alias: "O"}},
	}
	got := singleSourceQueryBlockCTEScopes(query, nil, onOnly)
	if _, promoted := got["C"]; promoted {
		t.Fatal("multi-leg block promoted an ON-only CTE into general resolution")
	}
	if _, stillRegistered := onOnly["C"]; !stillRegistered {
		t.Fatal("multi-leg decline mutated the ON-only registry")
	}
}

func TestBoundExistsTruthAfterPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sql       string
		wantTruth predicates.TriBool
		wantErr   bool
	}{
		{
			name:      "non_grouped_aggregate_without_pagination",
			sql:       "SELECT COUNT(*) FROM t i WHERE i.id = o.id",
			wantTruth: predicates.TriTrue,
		},
		{
			name:      "non_grouped_aggregate_positive_limit",
			sql:       "SELECT MAX(id) FROM t i WHERE i.id = o.id LIMIT 5",
			wantTruth: predicates.TriTrue,
		},
		{
			name:      "non_grouped_aggregate_limit_zero",
			sql:       "SELECT COUNT(*) FROM t i WHERE i.id = o.id LIMIT 0",
			wantTruth: predicates.TriFalse,
		},
		{
			name:      "non_grouped_aggregate_offset_one",
			sql:       "SELECT COUNT(*) FROM t i WHERE i.id = o.id LIMIT 1 OFFSET 1",
			wantTruth: predicates.TriFalse,
		},
		{
			name:      "non_grouped_aggregate_offset_past_one",
			sql:       "SELECT SUM(id) FROM t i WHERE i.id = o.id LIMIT 5 OFFSET 2",
			wantTruth: predicates.TriFalse,
		},
		{
			name: "plain_limit_preserves_existence",
			sql:  "SELECT id FROM t i WHERE i.id = o.id LIMIT 1",
		},
		{
			name:      "plain_limit_zero_is_empty",
			sql:       "SELECT id FROM t i WHERE i.id = o.id LIMIT 0",
			wantTruth: predicates.TriFalse,
		},
		{
			name:    "plain_offset_is_data_dependent",
			sql:     "SELECT id FROM t i WHERE i.id = o.id LIMIT 1 OFFSET 1",
			wantErr: true,
		},
		{
			name: "grouped_limit_preserves_existence",
			sql:  "SELECT COUNT(*) FROM t i WHERE i.id = o.id GROUP BY id LIMIT 1",
		},
		{
			name: "group_key_only_is_not_exactly_one",
			sql:  "SELECT id FROM t i WHERE i.id = o.id GROUP BY id",
		},
		{
			name:    "windowed_aggregate_is_rejected_before_cardinality_classification",
			sql:     "SELECT COUNT(*) OVER () FROM t i WHERE i.id = o.id",
			wantErr: true,
		},
		{
			name:    "having_can_remove_the_global_group",
			wantErr: true,
			sql:     "SELECT COUNT(*) FROM t i WHERE i.id = o.id HAVING COUNT(*) > 0",
		},
		{
			name:    "qualify_can_remove_the_global_group",
			wantErr: true,
			sql:     "SELECT COUNT(*) FROM t i WHERE i.id = o.id QUALIFY 1 = 1",
		},
		{
			name:    "false_qualify_cannot_be_folded_into_global_group_truth",
			sql:     "SELECT COUNT(*) FROM t i WHERE i.id = o.id QUALIFY 1 = 0",
			wantErr: true,
		},
		{
			name:    "plain_correlated_qualify_retains_admission_boundary",
			sql:     "SELECT id FROM t i WHERE i.id = o.id QUALIFY id > 0",
			wantErr: true,
		},
		{
			name:    "qualify_without_where_retains_admission_boundary",
			sql:     "SELECT o.id FROM t i QUALIFY id > 0",
			wantErr: true,
		},
		{
			name:    "cte_envelope_retains_qualify_boundary",
			sql:     "WITH c AS (SELECT id FROM t) SELECT COUNT(*) FROM c WHERE id = o.id QUALIFY 1 = 0",
			wantErr: true,
		},
		{
			name: "derived_qualify_does_not_change_owning_block_admission",
			sql:  "SELECT d.id FROM (SELECT id FROM t QUALIFY id > 0) d WHERE d.id = o.id",
		},
		{
			name: "independent_qualify_is_preserved",
			sql:  "SELECT id FROM t QUALIFY id > 0",
		},
		{
			name:      "grouped_limit_zero_is_empty",
			sql:       "SELECT COUNT(*) FROM t i WHERE i.id = o.id GROUP BY id LIMIT 0",
			wantTruth: predicates.TriFalse,
		},
		{
			name:    "grouped_offset_is_data_dependent",
			sql:     "SELECT COUNT(*) FROM t i WHERE i.id = o.id GROUP BY id LIMIT 1 OFFSET 1",
			wantErr: true,
		},
		{
			name:    "planning_time_unresolved_limit_is_unknown",
			sql:     "SELECT COUNT(*) FROM t i WHERE i.id = o.id LIMIT ?",
			wantErr: true,
		},
		{
			name:    "planning_time_unresolved_offset_is_unknown",
			sql:     "SELECT COUNT(*) FROM t i WHERE i.id = o.id LIMIT 1 OFFSET ?",
			wantErr: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			query, err := parseQueryFromSelect(t, test.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			owner, _ := clauseTestOwner(t)
			bound, err := owner.bindQuery(query)
			var lowered loweredExists
			if err == nil {
				lowered, err = lowerBoundExists(bound)
			}
			got := lowered.truth
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected typed unsupported error, got truth %v", got)
				}
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
					t.Fatalf("error = %v, want unsupported-query API error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if got != test.wantTruth {
				t.Fatalf("truth = %v, want %v", got, test.wantTruth)
			}
		})
	}
}

// TestBuildLogicalPlanWithCatalog_WhereWalked pins the happy path:
// a WHERE shape the walker supports becomes a QueryPredicate tree on
// LogicalFilter, and Explain renders from the tree.
func TestBuildLogicalPlanWithCatalog_WhereWalked(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE price > 5")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	if op == nil {
		t.Fatal("expected non-nil LogicalOperator")
	}
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected top-level LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatalf("expected Predicate to be set, got nil (text=%q)", filter.PredicateText)
	}
	// Explain should route through the predicate tree now, not
	// PredicateText. The walker normalises column casing to upper
	// (rlcatalog is case-insensitive); ExplainValue renders literals
	// unquoted via valueLiteralString.
	got := op.Explain("")
	if want := "Filter(ORDER.price#2 > 5)\n  Scan(ORDER)"; got != want {
		t.Fatalf("Explain: got %q, want %q", got, want)
	}
}

// The correlated fallback used to return an UNKNOWN ScalarSubqueryValue even
// though its catalog-backed inner plan and selected column were exact.  It also
// built its private ORDER BY as a name-only key after generic name rebaking had
// been retired.  Pin both authorities at the logical boundary, before Cascades
// translation can hide either defect behind a generic unsupported error.
func TestCorrelatedScalarLogicalCarrierIsExact(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	q, err := parseQueryFromSelect(t,
		`SELECT (SELECT o.price FROM "Order" o WHERE o.order_id = c.customer_id ORDER BY o.price DESC LIMIT 1) FROM "Customer" c`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, err := buildLogicalPlanForQueryWithCatalog(q, md)
	if err != nil {
		t.Fatalf("build logical plan: %v", err)
	}

	var carrier *logical.LogicalProject
	var findCarrier func(logical.LogicalOperator)
	findCarrier = func(candidate logical.LogicalOperator) {
		if candidate == nil || carrier != nil {
			return
		}
		if project, ok := candidate.(*logical.LogicalProject); ok && len(project.CorrelatedScalarSubqueries) == 1 {
			carrier = project
			return
		}
		for _, child := range candidate.Children() {
			findCarrier(child)
		}
	}
	findCarrier(op)
	if carrier == nil {
		t.Fatal("logical plan has no correlated-scalar projection carrier")
	}
	if len(carrier.ProjectedValues) != 1 || carrier.ProjectedValues[0] == nil {
		t.Fatalf("correlated projection values = %v, want one ScalarSubqueryValue", carrier.ProjectedValues)
	}
	if got := carrier.ProjectedValues[0].Type(); got == nil || got.Code() != values.TypeCodeInt || !got.IsNullable() {
		t.Fatalf("correlated scalar carrier type = %v, want nullable INTEGER from Order.PRICE", got)
	}

	inner := carrier.CorrelatedScalarSubqueries[0].InnerPlan
	limit, ok := inner.(*logical.LogicalLimit)
	if !ok || limit.Limit != 1 || limit.Offset != 0 {
		t.Fatalf("full scalar query lost its final LIMIT 1: %#v", inner)
	}
	materialized, ok := limit.Input.(*logical.LogicalProject)
	if !ok {
		t.Fatalf("correlated scalar inner = %T, want one-column LogicalProject materializer", inner)
	}
	if len(materialized.ProjectedValues) != 1 || materialized.ProjectedValues[0] == nil {
		t.Fatalf("scalar materializer values = %v, want the exact selected PRICE field", materialized.ProjectedValues)
	}
	selected, ok := values.AsFieldValue(materialized.ProjectedValues[0])
	if !ok || selected.Path() == nil {
		t.Fatalf("scalar materializer value = %T, want exact FieldValue", materialized.ProjectedValues[0])
	}
	ordinals := selected.Path().Ordinals()
	if len(ordinals) != 1 || ordinals[0] != 2 {
		t.Fatalf("scalar materializer path = %v, want Order.PRICE ordinal [2], not source ordinal zero", ordinals)
	}
	if len(materialized.Projections) != 1 {
		t.Fatalf("scalar materializer declares %d columns, want exactly one", len(materialized.Projections))
	}
	var sort *logical.LogicalSort
	var findSort func(logical.LogicalOperator)
	findSort = func(candidate logical.LogicalOperator) {
		if candidate == nil || sort != nil {
			return
		}
		if found, ok := candidate.(*logical.LogicalSort); ok {
			sort = found
			return
		}
		for _, child := range candidate.Children() {
			findSort(child)
		}
	}
	findSort(inner)
	if sort == nil || len(sort.Keys) != 1 || sort.Keys[0].Value == nil {
		t.Fatalf("correlated scalar ORDER BY = %#v, want one exact Value-backed key", sort)
	}
	if got := sort.Keys[0].Value.Type(); got == nil || got.Code() != values.TypeCodeInt {
		t.Fatalf("correlated scalar ORDER BY key type = %v, want INTEGER", got)
	}
}

// An outer WITH wrapper carries CTE definitions in Body and the scalar query
// itself in Main.  The wrapper must be transparent to scalar result typing: a
// missing arm here turned an exact MIN(LONG) native slot into an UNKNOWN
// ScalarSubqueryValue and made the enclosing projection unadmittable.
func TestScalarSubqueryOutputTypeTraversesCTEMain(t *testing.T) {
	t.Parallel()

	operand := &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
	aggregate := &logical.LogicalAggregate{
		Calls:             []logical.AggregateCall{{Func: "MIN", Operand: "V", BareColumn: true}},
		AggregateOperands: []values.Value{operand},
	}
	project := &logical.LogicalProject{
		Input:                   aggregate,
		Projections:             []string{"MIN(V)"},
		AggregateOutputOrdinals: []int{0},
	}
	wrapped := &logical.LogicalCTE{
		Main:        project,
		CTEProducer: logical.NewCTE("HIGH", nil, nil, false).CTEProducer,
	}

	got, err := scalarSubqueryOutputTypeChecked(wrapped)
	if err != nil {
		t.Fatalf("scalarSubqueryOutputTypeChecked(CTE(Main=MIN(LONG))): %v", err)
	}
	if got == nil || !got.Equals(values.NullableLong) {
		t.Fatalf("scalarSubqueryOutputType(CTE(Main=MIN(LONG))) = %v, want %v", got, values.NullableLong)
	}
}

func TestBuildScalarValidatesSemanticOutputBeforeRegistration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		wantCode api.ErrorCode
		wantType values.Type
	}{
		// The raw protobuf fixture stores mixed-case table names. Java's
		// SemanticAnalyzer.getTable resolves the normalized name exactly, so
		// quotes are required to reach those tables before output validation.
		{
			name:     "unquoted_table_is_not_the_mixed_case_stored_table",
			sql:      "SELECT MIN(price) FROM Order",
			wantCode: api.ErrCodeUndefinedTable,
		},
		{
			name:     "multiple_columns_are_syntax_error",
			sql:      `SELECT order_id, price FROM "Order" WHERE order_id = 1`,
			wantCode: api.ErrCodeSyntaxError,
		},
		{
			name:     "string_min_is_unsupported_operation",
			sql:      `SELECT MIN(name) FROM "Customer"`,
			wantCode: api.ErrCodeUnsupportedOperation,
		},
		{
			name:     "array_min_is_unsupported_operation",
			sql:      `SELECT MIN(tags) FROM "Order"`,
			wantCode: api.ErrCodeUnsupportedOperation,
		},
		{
			name:     "numeric_min_remains_exact_and_admitted",
			sql:      `SELECT MIN(price) FROM "Order"`,
			wantType: values.NullableInt,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := parseQueryFromSelect(t, tc.sql)
			if err != nil {
				t.Fatalf("parse query: %v", err)
			}
			planner := &existsSubqueryPlanner{md: buildTestMetaData(t)}
			clause := planner.newClause()
			alias, gotType, gotErr := clause.BuildScalar(q)
			if gotErr == nil {
				if len(planner.scalarSubqueries) != 0 || len(planner.correlatedScalarSubqueries) != 0 {
					t.Fatal("construction published before clause admission")
				}
				gotErr = clause.admitValues([]values.Value{values.NewScalarSubqueryValue(alias, gotType)})
			}
			if tc.wantCode != "" {
				var apiErr *api.Error
				if !errors.As(gotErr, &apiErr) || apiErr.Code != tc.wantCode {
					t.Fatalf("BuildScalar error = %v, want SQLSTATE %s", gotErr, tc.wantCode)
				}
				if len(planner.scalarSubqueries) != 0 {
					t.Fatalf("rejected scalar registered %d plans, want 0", len(planner.scalarSubqueries))
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("BuildScalar: %v", gotErr)
			}
			if gotType == nil || !gotType.Equals(tc.wantType) {
				t.Fatalf("BuildScalar type = %v, want %v", gotType, tc.wantType)
			}
			if len(planner.scalarSubqueries) != 1 {
				t.Fatalf("accepted scalar registered %d plans, want 1", len(planner.scalarSubqueries))
			}
		})
	}
}

// Walker success on an AND of comparisons — both leaves resolved,
// connective reconstructed. Pins that multi-leaf predicates compose
// through the catalog-aware path.
func TestBuildLogicalPlanWithCatalog_WhereAnd(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE price > 5 AND order_id = 1")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate on AND shape")
	}
	got := filter.Predicate.Explain()
	if want := "(ORDER.price#2 > 5 AND ORDER.order_id#0 = 1)"; got != want {
		t.Fatalf("Predicate.Explain: got %q, want %q", got, want)
	}
}

// Passing md=nil must behave identically to buildLogicalPlanForSelect
// — no predicate attached, Explain renders from text. Guarantees the
// catalog-aware builder is a strict superset of the text builder.
func TestBuildLogicalPlanWithCatalog_NilMetaData(t *testing.T) {
	t.Parallel()
	sq := parseSelect(t, "SELECT * FROM t WHERE id > 5")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, nil, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate != nil {
		t.Fatal("expected Predicate nil when md is nil")
	}
	if want := "Filter(id > 5)\n  Scan(T)"; op.Explain("") != want {
		t.Fatalf("Explain: got %q, want %q", op.Explain(""), want)
	}
}

// A checked star scope must preserve the SQL table-resolution error, not
// leak the semantic package's internal error or build a text-only fallback.
func TestBuildLogicalPlanWithCatalog_UnknownTable(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM NoSuchTable WHERE id > 5")
	op, err := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	var sqlErr *api.Error
	if op != nil || !errors.As(err, &sqlErr) || sqlErr.Code != api.ErrCodeUndefinedTable {
		t.Fatalf("plan=%v error=%v, want no plan and 42F01", op, err)
	}
	var cause *semantic.TableNotFoundError
	if !errors.As(err, &cause) || cause.Name.Name() != "NOSUCHTABLE" {
		t.Fatalf("table-resolution cause lost: %v", err)
	}
}

// A WHERE shape outside the walker's scope returns
// UnsupportedExpressionShapeError; the builder must fall back to
// PredicateText so Explain still renders. FROBNICATE() is a
// deliberate non-existent scalar function — walkScalarFunction
// declines on names not in the seed catalogue.
func TestBuildLogicalPlanWithCatalog_UnsupportedShape(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE FROBNICATE(price) = 1")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate != nil {
		t.Fatal("expected Predicate nil — walker should have declined")
	}
	if filter.PredicateText == "" {
		t.Fatal("expected text fallback populated")
	}
}

// TestBuildLogicalPlanWithCatalog_RHSArithmeticFolded pins the
// SimplifyPredicateValues wire-in: a constant arithmetic RHS
// (`PRICE = 1+2`) folds at plan time so EXPLAIN renders `PRICE = 3`
// rather than `PRICE = 1 + 2`. Same applies to nested arithmetic and
// scalar-function RHS (`name = UPPER('hi')` → `NAME = "HI"`).
func TestBuildLogicalPlanWithCatalog_RHSArithmeticFolded(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE price = 1+2")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate non-nil")
	}
	if got := filter.Predicate.Explain(); got != "ORDER.price#2 = 3" {
		t.Fatalf("Predicate.Explain: got %q, want ORDER.price#2 = 3", got)
	}
}

// TestBuildLogicalPlanWithCatalog_RHSScalarFunctionFolded pins the
// scalar-function arm: `name = UPPER('hi')` reaches EXPLAIN as
// `NAME = "HI"`.
func TestBuildLogicalPlanWithCatalog_RHSScalarFunctionFolded(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Customer WHERE name = UPPER('hi')")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate non-nil")
	}
	got := filter.Predicate.Explain()
	if !strings.Contains(got, "HI") || strings.Contains(got, "UPPER") {
		t.Fatalf("Predicate.Explain: got %q, want folded HI without UPPER", got)
	}
}

// UPPER (and the rest of the seed scalar function set — LOWER,
// LENGTH, CHAR_LENGTH, OCTET_LENGTH) IS now handled by
// walkScalarFunction. The catalog-aware builder attaches a real
// Predicate carrying the ScalarFunctionValue. Pins the new path
// so a future walker change that breaks scalar dispatch is caught
// immediately rather than silently regressing to text.
func TestBuildLogicalPlanWithCatalog_ScalarFunctionWalked(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t, "SELECT * FROM Order WHERE UPPER(price) = 'X'")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	filter, ok := op.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("expected LogicalFilter, got %T", op)
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate non-nil — walker should accept UPPER")
	}
}

// DELETE WHERE uses the catalog-aware path and emits a real
// QueryPredicate. Same structural shape as SELECT: LogicalDelete
// wraps Scan → Filter; the Filter carries the walked predicate.
func TestBuildLogicalPlanWithCatalog_DeleteWhere(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	del := parseDelete(t, "DELETE FROM Order WHERE price > 5")
	op, err := buildLogicalPlanForDeleteWithCatalog(del, md, defaultEmbeddedSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate on DELETE WHERE")
	}
	if got := filter.Predicate.Explain(); got != "ORDER.price#2 > 5" {
		t.Fatalf("Predicate.Explain: got %q, want ORDER.price#2 > 5", got)
	}
}

// UPDATE WHERE — mirror of DELETE: catalog-aware variant attaches a
// predicate to the LogicalFilter nested under LogicalUpdate.
func TestBuildLogicalPlanWithCatalog_UpdateWhere(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	upd := parseUpdate(t, "UPDATE Order SET price = 10 WHERE order_id = 1")
	op, err := buildLogicalPlanForUpdateWithCatalog(upd, md, defaultEmbeddedSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter in UPDATE plan, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatal("expected Predicate on UPDATE WHERE")
	}
	if got := filter.Predicate.Explain(); got != "ORDER.order_id#0 = 1" {
		t.Fatalf("Predicate.Explain: got %q, want ORDER.order_id#0 = 1", got)
	}
}

// buildLogicalPlanForQueryWithCatalog threads metadata through the
// top-level Query / QueryBody / Union recursion. UNION-of-SELECTs
// each get their own catalog-aware Filter when the WHERE walks
// cleanly. Pins that the recursion doesn't drop md somewhere.
func TestBuildLogicalPlanWithCatalog_UnionThreadsMd(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"SELECT order_id FROM Order WHERE price > 5 UNION ALL SELECT order_id FROM Order WHERE price < 100")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	union, ok := op.(*logical.LogicalUnion)
	if !ok {
		t.Fatalf("expected LogicalUnion, got %T", op)
	}
	if len(union.Inputs) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(union.Inputs))
	}
	for i, branch := range union.Inputs {
		var filter *logical.LogicalFilter
		for cur := branch; cur != nil; {
			if f, ok := cur.(*logical.LogicalFilter); ok {
				filter = f
				break
			}
			ch := cur.Children()
			if len(ch) != 1 {
				break
			}
			cur = ch[0]
		}
		if filter == nil {
			t.Fatalf("union branch %d missing Filter:\n%s", i, branch.Explain(""))
		}
		if filter.Predicate == nil {
			t.Fatalf("union branch %d missing Predicate (md not threaded?)", i)
		}
	}
}

// CTE bodies thread md too — WHERE inside `WITH c AS (SELECT ... WHERE ...)`
// also walks through the catalog-aware path.
func TestBuildLogicalPlanWithCatalog_CTEThreadsMd(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH c AS (SELECT order_id FROM Order WHERE price > 5) SELECT * FROM c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	cte, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected LogicalCTE root, got %T", op)
	}
	// The CTE body's filter should carry a Predicate.
	var bodyFilter *logical.LogicalFilter
	for cur := cte.Body(); cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			bodyFilter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if bodyFilter == nil {
		t.Fatalf("CTE body missing Filter:\n%s", cte.Body().Explain(""))
	}
	if bodyFilter.Predicate == nil {
		t.Fatal("CTE body Filter missing Predicate (md not threaded?)")
	}
}

func TestBuildLogicalPlanWithCatalog_CTEOuterWhereGetsRealPredicate(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH c AS (SELECT order_id, price FROM Order) SELECT order_id FROM c WHERE price > 10")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	cte, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected LogicalCTE root, got %T", op)
	}
	// The MAIN query's filter (on the CTE reference) should carry a Predicate.
	var mainFilter *logical.LogicalFilter
	for cur := cte.Main; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			mainFilter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if mainFilter == nil {
		t.Fatalf("main query missing Filter:\n%s", cte.Main.Explain(""))
	}
	if mainFilter.Predicate == nil {
		t.Fatal("outer WHERE on CTE reference should have a real Predicate (CTE schema derived from body)")
	}
}

func TestBuildLogicalPlanWithCatalog_CTEChainedSchemaDerivation(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH a AS (SELECT order_id, price FROM Order), "+
			"b AS (SELECT order_id FROM a WHERE price > 5) "+
			"SELECT order_id FROM b WHERE order_id > 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	if op == nil {
		t.Fatal("expected non-nil plan for chained CTE query")
	}
	// Walk to the outermost CTE (A wraps B wraps main).
	cteA, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected LogicalCTE root, got %T", op)
	}
	cteB, ok := cteA.Main.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected inner LogicalCTE (B), got %T", cteA.Main)
	}
	// Main query's filter on CTE B reference should have a real Predicate.
	var mainFilter *logical.LogicalFilter
	for cur := cteB.Main; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			mainFilter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if mainFilter == nil {
		t.Fatalf("main query missing Filter:\n%s", cteB.Main.Explain(""))
	}
	if mainFilter.Predicate == nil {
		t.Fatal("outer WHERE on chained CTE should have a real Predicate")
	}
}

func TestBuildLogicalPlanWithCatalog_CTESelectStarSchemaDerivation(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH c AS (SELECT * FROM Order) SELECT order_id FROM c WHERE price > 10")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	cte, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("expected LogicalCTE, got %T", op)
	}
	var mainFilter *logical.LogicalFilter
	for cur := cte.Main; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			mainFilter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if mainFilter == nil {
		t.Fatalf("main query missing Filter:\n%s", cte.Main.Explain(""))
	}
	if mainFilter.Predicate == nil {
		t.Fatal("outer WHERE on CTE SELECT * should have a real Predicate")
	}
}

func TestBuildLogicalPlanWithCatalog_CTENoPredNeeded(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"WITH c AS (SELECT order_id FROM Order) SELECT order_id FROM c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, _ := buildLogicalPlanForQueryWithCatalog(root, md)
	if op == nil {
		t.Fatal("expected non-nil plan for CTE without WHERE")
	}
}

func TestBuildLogicalPlanWithCatalog_JoinOnPredicateUpgrade(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	// Use a column pair that EXISTS in the demo schema (Order.order_id,
	// Customer.customer_id) so the ON predicate resolves and upgrades. (The
	// earlier query used Order.customer_id, which Order does not have — the
	// resolver correctly rejected it, but that masked the upgrade mechanism
	// under best-effort tolerance.)
	root, err := parseQueryFromSelect(t,
		"SELECT Order.order_id FROM Order INNER JOIN Customer ON Order.order_id = Customer.customer_id WHERE Order.price > 5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, err := buildLogicalPlanForQueryWithCatalog(root, md)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	// Verify the plan contains a LogicalJoin with OnText set.
	var join *logical.LogicalJoin
	for cur := op; cur != nil; {
		if j, ok := cur.(*logical.LogicalJoin); ok {
			join = j
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if join == nil {
		t.Fatalf("expected LogicalJoin in plan:\n%s", op.Explain(""))
	}
	if join.OnText == "" {
		t.Fatal("JOIN OnText should be non-empty")
	}
	// A resolvable equi-join ON MUST upgrade to a structured OnPredicate — the
	// resolver no longer silently drops it (that drop was the cross-product bug).
	if join.OnPredicate == nil {
		t.Fatalf("JOIN ON predicate must upgrade to a structured predicate, got nil (OnText=%q)", join.OnText)
	}
}

// TestBuildLogicalPlanWithCatalog_JoinOnUndefinedColumn pins that an ON predicate
// referencing a column that does not exist is rejected cleanly (42703) rather than
// silently dropped → cross product. (Order has no customer_id in the demo schema.)
func TestBuildLogicalPlanWithCatalog_JoinOnUndefinedColumn(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	root, err := parseQueryFromSelect(t,
		"SELECT Order.order_id FROM Order INNER JOIN Customer ON Order.customer_id = Customer.customer_id")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = buildLogicalPlanForQueryWithCatalog(root, md)
	if err == nil {
		t.Fatal("ON referencing a nonexistent column must be rejected, got no error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *api.Error: %T %v", err, err)
	}
	if apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Fatalf("error code = %s, want %s (42703)", apiErr.Code, api.ErrCodeUndefinedColumn)
	}
}

// INSERT … SELECT routes the inner SELECT's WHERE through the
// catalog-aware path. INSERT VALUES (no nested SELECT) is identical
// to the text builder's output.
func TestBuildLogicalPlanWithCatalog_InsertSelectThreadsMd(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	ins := parseInsert(t,
		"INSERT INTO Customer (customer_id, name) SELECT order_id, 'x' FROM Order WHERE price > 5")
	op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, defaultEmbeddedSchema)
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok {
		t.Fatalf("expected LogicalInsert, got %T", op)
	}
	if insertOp.Source == nil {
		t.Fatal("expected non-nil Source on INSERT … SELECT")
	}
	// The inner SELECT's filter should carry a Predicate.
	var filter *logical.LogicalFilter
	for cur := insertOp.Source; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("inner SELECT missing Filter:\n%s", insertOp.Source.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatal("inner SELECT Filter missing Predicate (md not threaded?)")
	}
}

// INSERT … SELECT without WHERE: catalog-aware path rebuilds Source
// but there's no Filter to upgrade. Plan should still produce a
// valid LogicalInsert with non-nil Source (the inner Scan).
func TestBuildLogicalPlanWithCatalog_InsertSelect_NoWhere(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	ins := parseInsert(t,
		"INSERT INTO Customer (customer_id, name) SELECT order_id, 'x' FROM Order")
	op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, defaultEmbeddedSchema)
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok {
		t.Fatalf("expected LogicalInsert, got %T", op)
	}
	if insertOp.Source == nil {
		t.Fatal("expected non-nil Source on INSERT … SELECT (no WHERE)")
	}
	// Inner SELECT has no WHERE — Source should be a Project / Scan
	// chain with no Filter on the spine.
	for cur := insertOp.Source; cur != nil; {
		if _, ok := cur.(*logical.LogicalFilter); ok {
			t.Fatalf("did not expect Filter when inner SELECT has no WHERE; tree:\n%s", insertOp.Source.Explain(""))
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
}

// INSERT … SELECT with a JOIN inside the SELECT — the catalog-aware
// path threads metadata down to the inner SELECT including its
// multi-source scope. Pins that the JOIN scope feature composes
// with the INSERT … SELECT path.
func TestBuildLogicalPlanWithCatalog_InsertSelectJoin(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	ins := parseInsert(t,
		"INSERT INTO Customer (customer_id, name) "+
			"SELECT order_id, 'x' FROM Order o JOIN Customer c ON o.order_id = c.customer_id WHERE o.price > 5")
	op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, defaultEmbeddedSchema)
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok {
		t.Fatalf("expected LogicalInsert, got %T", op)
	}
	if insertOp.Source == nil {
		t.Fatal("expected non-nil Source on INSERT … SELECT JOIN")
	}
	// Inner SELECT's filter should carry a Predicate with the
	// qualified column resolved.
	var filter *logical.LogicalFilter
	for cur := insertOp.Source; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter inside INSERT … SELECT JOIN, got tree:\n%s", insertOp.Source.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatalf("expected resolved Predicate on JOIN-WHERE, PredicateText=%q", filter.PredicateText)
	}
	if got := filter.Predicate.Explain(); !(strings.Contains(got, "price") && strings.Contains(got, "> 5")) {
		// The resolved predicate renders the column baked + qualified (e.g.
		// "O.price#2 > 5") — the predicate still resolves; the display
		// carries the plan-time ordinal + source qualifier.
		t.Fatalf("expected a resolved price > 5 predicate, got %q", got)
	}
}

// INSERT VALUES has no nested SELECT — the catalog-aware path
// returns the same shape as the text builder (Source is nil).
func TestBuildLogicalPlanWithCatalog_InsertValuesNoOp(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	ins := parseInsert(t, "INSERT INTO Customer (customer_id, name) VALUES (1, 'x')")
	op, _ := buildLogicalPlanForInsertWithCatalog(ins, md, defaultEmbeddedSchema)
	insertOp, ok := op.(*logical.LogicalInsert)
	if !ok {
		t.Fatalf("expected LogicalInsert, got %T", op)
	}
	if insertOp.Source != nil {
		t.Fatalf("VALUES form should leave Source nil, got %T", insertOp.Source)
	}
}

// upgradeFirstFilter returns true exactly when a LogicalFilter was
// found on the unary spine. Pins the invariant the catalog-aware
// builders rely on: the text builder always emits a Filter for any
// WHERE-carrying shape. If a future builder change drops the
// Filter, this test fires — and the catalog-aware builders would
// silently swallow predicates without it.
func TestUpgradeFirstFilter_Invariants(t *testing.T) {
	t.Parallel()
	// Every WHERE-carrying SELECT / DELETE / UPDATE shape the text
	// builder emits today. Extend this list whenever a new shape
	// lands that carries a WHERE through the logical builder.
	// LIMIT-carrying shape is rejected at parse time
	// (fdb-relational 4.11.1.0 / Go aligned), so the LIMIT spine is
	// unreachable from SQL. ORDER BY without LIMIT still exercises the
	// same Filter-on-spine invariant.
	cases := []string{
		"SELECT * FROM t WHERE id > 5",
		"SELECT id FROM t WHERE id > 5 ORDER BY id",
		"SELECT id, COUNT(*) FROM t WHERE id > 5 GROUP BY id",
		"SELECT id FROM t WHERE id > 5 AND name = 'x'",
	}
	dummyPred := predicates.NewConstantPredicate(predicates.TriTrue)
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			op := buildLogicalPlanForSelect(parseSelect(t, sql))
			if op == nil {
				t.Fatalf("builder returned nil for %q", sql)
			}
			if !upgradeFirstFilter(op, dummyPred) {
				t.Fatalf("expected Filter on unary spine for %q, got tree:\n%s", sql, op.Explain(""))
			}
		})
	}

	// DELETE + UPDATE: also have Filter on the spine (under
	// LogicalDelete / LogicalUpdate).
	del := parseDelete(t, "DELETE FROM t WHERE id > 5")
	if op := buildLogicalPlanForDelete(del); op == nil || !upgradeFirstFilter(op, dummyPred) {
		t.Fatal("DELETE WHERE missing Filter on spine")
	}
	upd := parseUpdate(t, "UPDATE t SET v = 1 WHERE id > 5")
	if op := buildLogicalPlanForUpdate(upd); op == nil || !upgradeFirstFilter(op, dummyPred) {
		t.Fatal("UPDATE WHERE missing Filter on spine")
	}

	// A WHERE-less shape has no Filter — upgradeFirstFilter returns
	// false. This is the shape the catalog-aware builders pre-guard
	// against via their sq.whereExpr==nil / del.WhereExpr()==nil gates.
	op := buildLogicalPlanForSelect(parseSelect(t, "SELECT * FROM t"))
	if upgradeFirstFilter(op, dummyPred) {
		t.Fatal("expected false on WHERE-less shape (no Filter to upgrade)")
	}
}

// JOIN with qualified-column WHERE — multi-source scope picks up
// both tables, and the walker resolves `Order.price` against the
// JOIN's primary source. Predicate tree carried on LogicalFilter.
func TestBuildLogicalPlanWithCatalog_JoinQualifiedColumn(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Customer ON Order.order_id = Customer.customer_id WHERE Order.price > 5")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	// Walk down to the Filter — the Join sits below.
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter in plan, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatalf("expected Predicate on JOIN shape; PredicateText=%q", filter.PredicateText)
	}
	if got := filter.Predicate.Explain(); !(strings.Contains(got, "price") && strings.Contains(got, "> 5")) {
		t.Fatalf("expected a resolved price > 5 JOIN predicate, got %q", got)
	}
}

// JOIN with bare column unique to one side — walker resolves
// without ambiguity. `quantity` exists in Order only, not Customer.
func TestBuildLogicalPlanWithCatalog_JoinUniqueBareColumn(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Customer ON Order.order_id = Customer.customer_id WHERE quantity > 0")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil || filter.Predicate == nil {
		t.Fatalf("expected resolved Predicate on JOIN with bare-column WHERE")
	}
	if got := filter.Predicate.Explain(); !strings.Contains(got, "quantity") {
		t.Fatalf("expected quantity in resolved predicate, got %q", got)
	}
}

// Self-join without explicit alias — `Order JOIN Order ON ...` produces two
// sources both named ORDER. The duplicate sources REGISTER (per-leg binding
// ids) and the ON reference resolves per-ATTRIBUTE — both ORDER legs carry
// order_id, so the loud error is Java's exact 42702 (`Ambiguous reference
// ORDER.ORDER_ID`). Still fail-closed — better a precise error than wrong
// rows; a silent ON-clause drop that produced a cross product stays
// impossible.
func TestBuildLogicalPlanWithCatalog_SelfJoinWithoutAlias_FailsClosed(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Order ON Order.order_id = Order.order_id WHERE price > 5")
	op, err := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	if err == nil {
		t.Fatalf("expected a loud error for the unaliased self-join ON (silent drop = cross product), got op:\n%v", op)
	}
	if !strings.Contains(err.Error(), "Ambiguous reference ORDER.ORDER_ID") {
		t.Errorf("error should be Java's per-attribute 42702, got: %v", err)
	}
}

// 3-way JOIN — sq.joins carries two entries; buildWherePredicateForJoins
// must add all three sources (primary + 2 joins). Walker resolves
// qualified refs from any of the three.
func TestBuildLogicalPlanWithCatalog_ThreeWayJoin(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order o "+
			"JOIN Customer c ON o.order_id = c.customer_id "+
			"JOIN TypedRecord t ON o.order_id = t.id "+
			"WHERE o.price > 5 AND t.id > 0")
	op, _ := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	if op == nil {
		t.Fatal("expected non-nil plan")
	}
	var filter *logical.LogicalFilter
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			filter = f
			break
		}
		ch := cur.Children()
		if len(ch) != 1 {
			break
		}
		cur = ch[0]
	}
	if filter == nil {
		t.Fatalf("expected LogicalFilter, got tree:\n%s", op.Explain(""))
	}
	if filter.Predicate == nil {
		t.Fatalf("expected resolved Predicate on 3-way JOIN; PredicateText=%q", filter.PredicateText)
	}
	got := filter.Predicate.Explain()
	// Both branches of the AND should resolve.
	if !(strings.Contains(got, "price") && strings.Contains(got, "> 5")) {
		t.Errorf("expected price > 5, got %q", got)
	}
	if !(strings.Contains(got, "id") && strings.Contains(got, "> 0")) {
		t.Errorf("expected id > 0 (from TypedRecord.id), got %q", got)
	}
}

// JOIN with ambiguous bare column — `price` exists in both Order
// and Customer. Walker correctly fails on AmbiguousColumnError;
// builder falls back to text.
func TestBuildLogicalPlanWithCatalog_JoinAmbiguousColumn_ErrorsProperly(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	sq := parseSelect(t,
		"SELECT * FROM Order JOIN Customer ON Order.order_id = Customer.customer_id WHERE price > 5")
	_, err := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
	if err == nil {
		t.Fatal("expected ambiguous column error for unqualified 'price' in JOIN (exists in both Order and Customer)")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeAmbiguousColumn {
		t.Fatalf("expected ErrCodeAmbiguousColumn, got: %v", err)
	}
}

// buildDerivedTableSourceFromJoinBody must derive a body leg's nullability from
// the JOIN ALGEBRA, not by copying the catalog.
//
// A LEFT JOIN pads its right leg for every unmatched left row, so the right
// leg's columns are nullable in the body's output regardless of what the base
// table declares. The derivation read `semantic.NonEphemeral(tbl.Columns())`
// straight from the catalog and never looked at `joinClause.joinType`, so a NOT
// NULL column stayed NOT NULL through an outer join. The physical side already
// applies the opposite rule (ordinal_seed.go wraps a null-supplying leg's column
// types with WithNullability(true), citing Java's
// pullUpResultColumnsWithNullability); this is the semantic side agreeing.
//
// WHY THIS IS PINNED HERE AND NOT THROUGH SQL, stated because the difference is
// the finding. The defect is currently LATENT and the measurement is what says
// so, not an argument:
//
//   - `Order.tags` is REPEATED, which rlcatalog.isNullable reports as NOT NULL.
//     That is the ONLY column shape that can carry a false NOT NULL here — the
//     SQL DDL refuses `NOT NULL` on a scalar column outright ("0A000: NOT NULL
//     is only allowed for ARRAY column type") and every scalar catalog column,
//     PRIMARY KEY columns included, arrives nullable.
//   - Driven end-to-end against real FDB over a `LEFT JOIN`-bodied derived
//     table with an `ARRAY NOT NULL` column on each leg, NOTHING DOWNSTREAM
//     OBSERVED THE DIFFERENCE: the driver's column metadata reports an ARRAY
//     column nullable either way (it does so for the BASE table too), and
//     `IS NULL`, `IS NOT NULL` and the both-operands AND fold over the derived
//     column returned identical rows with the fix present and absent.
//
// So the wrong type is real and its consumers are, today, all blind to it. That
// makes this test the only thing that can fail when the derivation goes back to
// copying the catalog — and it is why the fix is not left out: a derivation that
// states a false NOT NULL is a loaded gun aimed at the next consumer to start
// reading it, and the SQL-level probes that would catch it cannot be written.
func TestDerivedJoinBodyNullability_IsDerivedFromTheJoinAlgebra(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)

	// FIXTURE GUARD. The whole test is "a NOT NULL column becomes nullable", so
	// the column has to be NOT NULL in the catalog first. If Order.TAGS ever
	// stops reporting NOT NULL, every assertion below is satisfied by a column
	// that was already nullable and the test proves nothing.
	body := func(kind logical.JoinKind) logical.LogicalOperator {
		return logical.NewJoin(logical.NewScan("Order", "A"), logical.NewScan("Customer", "B"), kind, "")
	}
	innerSrc, err := boundDerivedSource(md, "D", "D", body(logical.JoinInner), nil)
	if err != nil {
		t.Fatal("the INNER control body did not derive at all — nothing below is comparable")
	}
	tags, found := columnNamed(innerSrc.Table.Columns(), "tags")
	if !found {
		t.Fatalf("fixture: Order must contribute a tags column, got %v",
			columnNames(innerSrc.Table.Columns()))
	}
	if tags.Nullable {
		t.Fatal("fixture: Order.TAGS reports nullable in the catalog. It is a REPEATED " +
			"field, which rlcatalog.isNullable reports as NOT NULL, and it is the only " +
			"column shape this derivation can carry a false NOT NULL for. With a nullable " +
			"fixture column the assertions below hold before the fix and after it.")
	}

	for _, tc := range []struct {
		name string
		// jt is the flavour of the ONE join clause; the body is
		// `FROM Order AS A <jt> JOIN Customer AS B`.
		jt logical.JoinKind
		// wantNullable per leg: [left (Order.TAGS), right (Customer.*)].
		wantTagsNullable bool
		why              string
	}{
		{
			"INNER preserves both legs", logical.JoinInner, false,
			"an inner join pads nothing, so the catalog's NOT NULL survives. This is " +
				"the direction a blanket \"mark every leg nullable\" fix destroys",
		},
		{
			"LEFT preserves the LEFT leg", logical.JoinLeft, false,
			"a LEFT JOIN pads only the RIGHT side; the preserved leg keeps its type",
		},
		{
			"RIGHT pads the LEFT leg", logical.JoinRight, true,
			"a RIGHT JOIN pads everything to its left, and the left leg is where TAGS " +
				"lives — this is the arm a fix that only handles LEFT would miss",
		},
		{
			"FULL pads both legs", logical.JoinFull, true,
			"a FULL OUTER JOIN pads both sides",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src, err := boundDerivedSource(md, "D", "D", body(tc.jt), nil)
			if err != nil {
				t.Fatalf("the body did not derive — the case tests nothing")
			}
			got, ok := columnNamed(src.Table.Columns(), "tags")
			if !ok {
				t.Fatalf("no tags column in %v", columnNames(src.Table.Columns()))
			}
			if got.Nullable != tc.wantTagsNullable {
				t.Fatalf("D.TAGS nullable=%v, want %v — %s.\n"+
					"  Nullability here must come from the join algebra. Reading it off the "+
					"catalog states a NOT NULL for a column the body serves NULL in, and the "+
					"semantic row is what adjudicates every outer reference against the row "+
					"the executor actually produces.",
					got.Nullable, tc.wantTagsNullable, tc.why)
			}
		})
	}
}

func columnNamed(cols []semantic.Column, name string) (semantic.Column, bool) {
	for _, c := range cols {
		if c.Id.Name() == name {
			return c, true
		}
	}
	return semantic.Column{}, false
}

func columnNames(cols []semantic.Column) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Id.Name())
	}
	return out
}

// Computed virtual columns are typed from the body's resolved Values. There is
// no catalog column to copy for either V*2 or EXISTS, and publishing UNKNOWN at
// this boundary would make the enclosing QOV inexact under RFC-232.
func TestComputedVirtualScopesUseExactProjectedValues(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t,
		"CREATE TABLE b_exact (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		&captureLogger{})

	t.Run("CTE arithmetic and EXISTS", func(t *testing.T) {
		root := parseQuery(t, "WITH d AS ("+
			"SELECT v * 2 AS doubled, "+
			"EXISTS (SELECT 1 FROM b_exact AS c WHERE c.id = b_exact.id) AS present "+
			"FROM b_exact) SELECT doubled FROM d")
		named := root.Ctes().AllNamedQuery()
		if len(named) != 1 {
			t.Fatalf("named CTE count = %d, want 1", len(named))
		}
		src, ok, err := buildCTEColumnSource(md, "D", named[0].Query(), nil)
		if err != nil {
			t.Fatalf("computed CTE body did not build: %v", err)
		}
		if !ok || src.Table == nil {
			t.Fatal("computed CTE did not publish an exact virtual source")
		}
		doubled, found := columnNamed(src.Table.Columns(), "DOUBLED")
		if !found || doubled.Type != "BIGINT" || !doubled.Nullable {
			t.Fatalf("DOUBLED = %+v, found=%v; want nullable BIGINT", doubled, found)
		}
		present, found := columnNamed(src.Table.Columns(), "PRESENT")
		if !found || present.Type != "BOOL" || present.Nullable {
			t.Fatalf("PRESENT = %+v, found=%v; want non-null BOOL", present, found)
		}
	})

	t.Run("derived arithmetic", func(t *testing.T) {
		outer := parseSelect(t,
			"SELECT d.doubled FROM (SELECT v * 2 AS doubled FROM b_exact) AS d")
		src, ok := buildDerivedTableSource(md, "D", outer.derivedQuery)
		if !ok || src.Table == nil {
			t.Fatal("computed derived table did not publish an exact virtual source")
		}
		doubled, found := columnNamed(src.Table.Columns(), "DOUBLED")
		if !found || doubled.Type != "BIGINT" || !doubled.Nullable {
			t.Fatalf("DOUBLED = %+v, found=%v; want nullable BIGINT", doubled, found)
		}
	})

	t.Run("derived post-aggregate arithmetic", func(t *testing.T) {
		const sql = `SELECT sub.dept_id, sub.avg_sal
			FROM (SELECT id AS dept_id, SUM(v) / COUNT(*) AS avg_sal
			      FROM b_exact GROUP BY id) AS sub
			ORDER BY sub.dept_id`
		outer := parseSelect(t, sql)
		before := outer.derivedQuery.GetText()
		src, ok := buildDerivedTableSource(md, "SUB", outer.derivedQuery)
		if !ok || src.Table == nil {
			t.Fatal("post-aggregate arithmetic derived table did not publish an exact virtual source")
		}
		columns := src.Table.Columns()
		if len(columns) != 2 || columns[0].Id.Name() != "DEPT_ID" ||
			columns[0].Type != "BIGINT" || !columns[0].Nullable ||
			columns[1].Id.Name() != "AVG_SAL" || columns[1].Type != "BIGINT" ||
			!columns[1].Nullable {
			t.Fatalf("post-aggregate derived columns = %+v, want DEPT_ID/AVG_SAL nullable BIGINT", columns)
		}
		if after := outer.derivedQuery.GetText(); after != before {
			t.Fatalf("scope derivation mutated aggregate body: before %q after %q", before, after)
		}

		queryCtx, err := parseQueryFromSelect(t, sql)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		op, err := NewPlanVisitor(md).VisitQuery(queryCtx)
		if err != nil {
			t.Fatalf("VisitQuery: %v", err)
		}
		sort := findSort(op)
		if sort == nil || len(sort.Keys) != 1 || sort.Keys[0].Value == nil {
			t.Fatalf("qualified derived ORDER BY has no exact Value: tree=%s", op.Explain(""))
		}
		project := findProjection(op)
		if project == nil || len(project.ProjectedValues) != 2 ||
			project.ProjectedValues[0] == nil || project.ProjectedValues[1] == nil {
			t.Fatalf("derived projection did not resolve both exact fields: tree=%s", op.Explain(""))
		}
	})

	t.Run("ordinary aggregate keeps manual derivation", func(t *testing.T) {
		outer := parseSelect(t,
			`SELECT sub.dept_id FROM (`+
				`SELECT id AS dept_id, SUM(v) AS total FROM b_exact GROUP BY id`+
				`) AS sub`)
		src, ok := buildDerivedTableSource(md, "SUB", outer.derivedQuery)
		if !ok || src.Table == nil {
			t.Fatal("ordinary aggregate derived source unexpectedly declined")
		}
		columns := src.Table.Columns()
		if len(columns) != 2 || columns[0].Id.Name() != "DEPT_ID" ||
			columns[0].Type != "BIGINT" || columns[1].Id.Name() != "TOTAL" ||
			columns[1].Type != "BIGINT" {
			t.Fatalf("ordinary aggregate columns changed: %+v", columns)
		}
	})
}

func TestDerivedInlineValuesScopeUsesExactLogicalRow(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)

	assertPredicate := func(t *testing.T, sql string) *logical.LogicalFilter {
		t.Helper()
		queryCtx, err := parseQueryFromSelect(t, sql)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		op, err := NewPlanVisitor(md).VisitQuery(queryCtx)
		if err != nil {
			t.Fatalf("VisitQuery: %v", err)
		}
		filter, ok := op.(*logical.LogicalFilter)
		if !ok {
			t.Fatalf("logical root = %T, want LogicalFilter\n%s", op, op.Explain(""))
		}
		if filter.Predicate == nil {
			t.Fatalf("derived-inline WHERE stayed text-only: %q", filter.PredicateText)
		}
		return filter
	}

	t.Run("flat star body", func(t *testing.T) {
		const sql = `SELECT * FROM (SELECT * FROM VALUES (1), (2) AS A(B)) AS U WHERE B < 2`
		filter := assertPredicate(t, sql)
		if got := filter.Predicate.Explain(); !strings.Contains(got, "B") || !strings.Contains(got, "< 2") {
			t.Fatalf("resolved predicate = %q, want exact B < 2", got)
		}

		outer := parseSelect(t, sql)
		before := outer.derivedQuery.GetText()
		src, ok := buildDerivedTableSource(md, "U", outer.derivedQuery)
		if !ok || src.Table == nil {
			t.Fatal("flat inline derived body did not publish an exact virtual source")
		}
		cols := src.Table.Columns()
		if len(cols) != 1 || cols[0].Id.Name() != "B" || cols[0].Type != "INT" || cols[0].Nullable {
			t.Fatalf("flat derived columns = %+v, want B INT NOT NULL", cols)
		}
		if after := outer.derivedQuery.GetText(); after != before {
			t.Fatalf("scope derivation mutated parse body: before %q after %q", before, after)
		}
	})

	t.Run("projected nested record and array", func(t *testing.T) {
		const sql = `SELECT * FROM (` +
			`SELECT A.B, A.W, A.Z FROM VALUES (1, (2, 'x'), [3, 4]) AS A(B, W(X, Y), Z)` +
			`) AS U WHERE B < 8`
		assertPredicate(t, sql)
		outer := parseSelect(t, sql)
		before := outer.derivedQuery.GetText()
		src, ok := buildDerivedTableSource(md, "U", outer.derivedQuery)
		if !ok || src.Table == nil {
			t.Fatal("nested inline derived body did not publish an exact virtual source")
		}
		cols := src.Table.Columns()
		if len(cols) != 3 || cols[0].Id.Name() != "B" || cols[0].Type != "INT" {
			t.Fatalf("nested derived columns = %+v, want B/W/Z", cols)
		}
		if cols[1].Id.Name() != "W" || cols[1].Type != "RECORD" || cols[1].Nullable ||
			len(cols[1].StructFields) != 2 || cols[1].StructFields[0].Id.Name() != "X" ||
			cols[1].StructFields[0].Type != "INT" || cols[1].StructFields[1].Id.Name() != "Y" ||
			cols[1].StructFields[1].Type != "STRING" {
			t.Fatalf("nested W metadata = %+v, want exact RECORD<X INT,Y STRING>", cols[1])
		}
		if cols[2].Id.Name() != "Z" || !cols[2].IsArray || cols[2].Type != "INT" || cols[2].Nullable {
			t.Fatalf("nested Z metadata = %+v, want ARRAY<INT NOT NULL> NOT NULL", cols[2])
		}
		if after := outer.derivedQuery.GetText(); after != before {
			t.Fatalf("nested scope derivation mutated parse body: before %q after %q", before, after)
		}
	})

	t.Run("malformed common row declines", func(t *testing.T) {
		outer := parseSelect(t,
			`SELECT * FROM (SELECT A.B FROM VALUES (1), ('x') AS A(B)) AS U WHERE B < 8`)
		before := outer.derivedQuery.GetText()
		if src, ok := buildDerivedTableSource(md, "U", outer.derivedQuery); ok || src.Table != nil {
			t.Fatalf("inexact inline body published a virtual source: %+v", src)
		}
		if after := outer.derivedQuery.GetText(); after != before {
			t.Fatalf("declined scope derivation mutated parse body: before %q after %q", before, after)
		}
	})
}

func TestSemanticColumnFromExactTypeDeclinesUnrepresentableArrayElementNullability(t *testing.T) {
	t.Parallel()

	representable := values.NewArrayType(true, values.NotNullLong)
	column, ok := semanticColumnFromExactType("XS", representable)
	if !ok || !column.IsArray || column.Type != "BIGINT" || !column.Nullable {
		t.Fatalf("representable array = %+v, ok=%v; want nullable BIGINT ARRAY", column, ok)
	}

	unrepresentable := values.NewArrayType(false, values.NullableLong)
	if column, ok := semanticColumnFromExactType("XS", unrepresentable); ok {
		t.Fatalf("nullable-element array was published as exact semantic column: %+v", column)
	}
}

func TestSemanticColumnFromExactTypeCarriesRecordName(t *testing.T) {
	t.Parallel()
	fields := []values.Field{{Name: "V", FieldType: values.NotNullLong}}

	// A record whose NAME happens to be RECORD is a named record: the kind is
	// never a name, so nothing mints this shape, and one that arrives keeps it.
	namedRecord := values.NewRecordType("RECORD", false, fields)
	column, ok := semanticColumnFromExactType("S", namedRecord)
	if !ok || column.Type != "RECORD" || column.StructTypeName != "RECORD" || len(column.StructFields) != 1 {
		t.Fatalf("record named RECORD = %+v, ok=%v, want Type RECORD carrying the name RECORD", column, ok)
	}

	// A nominal record (a declared STRUCT's type) is published with its name
	// in StructTypeName, the carrier the forward bridge reads first, so the
	// round trip mints a record of the same name. Declining it instead left a
	// projected STRUCT-typed nested field with no exact row to publish.
	named := values.NewRecordType("DECLARED_STRUCT", false, fields)
	column, ok = semanticColumnFromExactType("S", named)
	if !ok || column.Type != "RECORD" || column.StructTypeName != "DECLARED_STRUCT" || len(column.StructFields) != 1 {
		t.Fatalf("nominal record = %+v, ok=%v, want Type RECORD carrying DECLARED_STRUCT", column, ok)
	}
	row := expr.SourceRowType(semantic.ScopeSource{Table: &semantic.StaticTable{TableColumns: []semantic.Column{column}}})
	if row == nil || len(row.Fields) != 1 {
		t.Fatalf("row over the published column = %v, want one field", row)
	}
	rebuilt, isRecord := row.Fields[0].FieldType.(*values.RecordType)
	if !isRecord || rebuilt.RecordName != "DECLARED_STRUCT" || !rebuilt.Equals(named) {
		t.Fatalf("round trip of the nominal record = %v, want %v under the same name", row.Fields[0].FieldType, named)
	}

	fieldless := values.NewRecordType("DECLARED_STRUCT", false, nil)
	if column, ok := semanticColumnFromExactType("S", fieldless); ok {
		t.Fatalf("fieldless record was published as exact: %+v", column)
	}

	// An anonymous record — a record constructor's row — stays anonymous on the
	// round trip, so two different anonymous shapes in one row get two synthetic
	// descriptor names instead of both claiming the literal "RECORD".
	unnamed := values.NewRecordType("", false, fields)
	column, ok = semanticColumnFromExactType("S", unnamed)
	if !ok || column.Type != "RECORD" || column.StructTypeName != "" {
		t.Fatalf("anonymous record = %+v, ok=%v, want Type RECORD with no StructTypeName", column, ok)
	}
	row = expr.SourceRowType(semantic.ScopeSource{Table: &semantic.StaticTable{TableColumns: []semantic.Column{column}}})
	rebuilt, isRecord = row.Fields[0].FieldType.(*values.RecordType)
	if !isRecord || rebuilt.RecordName != "" || !rebuilt.Equals(unnamed) {
		t.Fatalf("round trip of the anonymous record = %v, want it anonymous (no record name)", row.Fields[0].FieldType)
	}
}

func TestSemanticColumnFromExactTypeCarriesEnum(t *testing.T) {
	t.Parallel()
	enum := values.NewEnumType("COLOR", false, []values.EnumValue{{Name: "RED", Number: 9}, {Name: "BLUE", Number: 2}})
	for _, typ := range []values.Type{
		enum, values.WithNullability(enum, true),
		values.NewRecordType("PAINT", false, []values.Field{{Name: "C", FieldType: enum}}),
		values.NewArrayType(true, enum),
	} {
		column, ok := semanticColumnFromExactType("C", typ)
		if !ok {
			t.Fatalf("enum-bearing type declined: %v", typ)
		}
		row := expr.SourceRowType(semantic.ScopeSource{Table: &semantic.StaticTable{TableColumns: []semantic.Column{column}}})
		if row == nil || len(row.Fields) != 1 || !row.Fields[0].FieldType.Equals(typ) {
			t.Fatalf("enum-bearing round trip = %v, want %v", row, typ)
		}
	}
	column, ok := semanticColumnFromExactType("C", enum)
	if !ok || column.EnumTypeName != "COLOR" || len(column.EnumMembers) != 2 {
		t.Fatalf("enum declaration lost: %+v", column)
	}
	enum.Values[0].Name = "CHANGED"
	if column.EnumMembers[0].Name != "RED" {
		t.Fatal("semantic enum aliases its source declaration")
	}
}

// enumHomonymMetaData places a nested enum beside a top-level STRING of the
// same name. Binding by the leaf name instead of the resolved path changes the
// selected slot and erases the enum declaration.
func enumHomonymMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	longKind := descriptorpb.FieldDescriptorProto_TYPE_INT64
	stringKind := descriptorpb.FieldDescriptorProto_TYPE_STRING
	enumKind := descriptorpb.FieldDescriptorProto_TYPE_ENUM
	messageKind := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("enum_homonym_test.proto"),
		Package: proto.String("enumhomonymtest"),
		Syntax:  proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name:  proto.String("Color"),
			Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("RED"), Number: proto.Int32(1)}},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			// Paint is NESTED so the metadata builder sees one record type, T.
			{Name: proto.String("T"), NestedType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Paint"), Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("color"), Number: proto.Int32(1), Label: &label, Type: &enumKind,
					TypeName: proto.String(".enumhomonymtest.Color"),
				}},
			}}, Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Label: &label, Type: &longKind},
				{Name: proto.String("color"), Number: proto.Int32(2), Label: &label, Type: &stringKind},
				{
					Name: proto.String("p"), Number: proto.Int32(3), Label: &label, Type: &messageKind,
					TypeName: proto.String(".enumhomonymtest.T.Paint"),
				},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(file)
	builder.GetRecordType("T").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

func TestDerivedNestedEnumFieldKeepsExactTypeAndHomonym(t *testing.T) {
	t.Parallel()
	md := enumHomonymMetaData(t)
	for _, sql := range []string{
		`SELECT x.color FROM (SELECT t.p.color FROM t) x`,
		`WITH x AS (SELECT t.p.color FROM t) SELECT x.color FROM x`,
		`SELECT x.color FROM (SELECT t.color FROM t) x`,
	} {
		plan, _, err := PlanRecordQueryWithSubqueries(sql, md, nil)
		if err != nil || plan == nil {
			t.Fatalf("%s: plan %v, err %v", sql, plan, err)
		}
	}
	q, err := parseQueryFromSelect(t, `SELECT t.p.color FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	body, err := NewPlanVisitor(md).VisitQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	projection := findProjection(body)
	if projection == nil || len(projection.ProjectedValues) != 1 {
		t.Fatal("nested enum body lost its bound projection")
	}
	field, exact := values.AsFieldValue(projection.ProjectedValues[0])
	if !exact || field.Path().Len() != 2 || field.Path().Ordinals()[0] != 2 || field.Path().Ordinals()[1] != 0 {
		t.Fatalf("t.p.color did not resolve the nested enum ordinals [2 0]: %v", projection.ProjectedValues[0])
	}
	src, ok := exactVirtualScopeSource("X", body, md, nil, nil)
	if !ok {
		t.Fatal("the exact derivation declined the nested enum field")
	}
	cols := src.Table.Columns()
	if len(cols) != 1 || cols[0].Id.Name() != "COLOR" || cols[0].Type != "ENUM" || cols[0].EnumTypeName != "enumhomonymtest.Color" {
		t.Fatalf("exact row over the enum field = %+v, want the nested ENUM column COLOR", cols)
	}
}

func TestDerivedBindingMintReservesLexicalEnvironment(t *testing.T) {
	t.Parallel()
	inner, err := parseQueryFromSelect(t, "SELECT order_id FROM Order")
	if err != nil {
		t.Fatal(err)
	}
	table := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{"ORDER"}, false),
		TableColumns: []semantic.Column{{Id: semantic.FromNormalized("ORDER_ID"), Type: "BIGINT", Nullable: true}},
	}
	grand := semantic.NewScope(nil)
	if err := grand.AddSource(semantic.ScopeSource{
		Table: table,
		Alias: semantic.FromNormalized("Q$BOUND0"), CorrelationName: "Q$BOUND1",
		AdditionalQualifiers: []semantic.Identifier{semantic.FromNormalized("Q$BOUND2")},
	}); err != nil {
		t.Fatal(err)
	}
	parent := semantic.NewScope(grand)
	if err := parent.AddSource(semantic.ScopeSource{Table: table, Alias: semantic.FromNormalized("D"), CorrelationName: "Q$DUP1"}); err != nil {
		t.Fatal(err)
	}
	visitor := NewPlanVisitor(buildTestMetaData(t))
	visitor.enclosingScope = parent
	visitor.cteScopes = map[string]semantic.ScopeSource{"Q$BOUND3": {}}
	visitor.cteOnScopes = map[string]semantic.ScopeSource{"Q$BOUND4": {}}
	visitor.cteProducers = testCTERegistry(map[string]logical.LogicalOperator{"Q$BOUND5": logical.NewScan("Order", "")})
	for round := range 2 {
		fs := &fromSource{
			tableName: "D", tableAlias: "D", derivedQuery: inner, enclosingScope: parent,
			joins: []joinClause{
				{alias: "D", derivedQuery: inner, bindingID: "Q$DUP1"},
				{alias: "D", derivedQuery: inner, bindingID: "Q$DUP2"},
				{alias: "Q$BOUND6", tableName: "Order", bindingID: "Q$BOUND7"},
				{alias: "S.Q$BOUND8", tableName: "S.Q$BOUND8", segments: []string{"S", "Q$BOUND8"}},
			},
		}
		visitor.assignDerivedSourceBindings(fs)
		wantPrimary := fmt.Sprintf("Q$BOUND%d", 9+5*round)
		wantFirstJoin := fmt.Sprintf("Q$BOUND%d", 10+5*round)
		if fs.bindingID != wantPrimary {
			t.Fatalf("mint must reserve all lexical names and prior sibling IDs: primary=%q, want %q", fs.bindingID, wantPrimary)
		}
		for i, join := range fs.joins {
			if want := fmt.Sprintf("Q$BOUND%d", 10+5*round+i); join.bindingID != want {
				t.Fatalf("join %d identity = %q, want %q", i, join.bindingID, want)
			}
		}
		sq := selectQueryFromClassification(&selectClassification{}, fs)
		if sq.bindingID != wantPrimary || sq.enclosingScope != parent || sq.tableAlias != "D" {
			t.Fatalf("FROM bridge lost runtime/lexical identity: %+v", sq)
		}
		visitor.assignDerivedSourceBindings(fs)
		if fs.bindingID != wantPrimary || fs.joins[0].bindingID != wantFirstJoin {
			t.Fatal("repeated identity pass changed the carried bindings")
		}
	}
}

func TestDerivedBindingCarrierAndScopeRebuilds(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	outerQ, err := parseQueryFromSelect(t, "SELECT * FROM Customer d")
	if err != nil {
		t.Fatal(err)
	}
	outerSQ, err := extractFromQueryTerm(outerQ.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext))
	if err != nil {
		t.Fatal(err)
	}
	outer, err := buildSelectScopeChecked(outerSQ, md, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	q, err := parseQueryFromSelect(t, "SELECT d.price, j.order_id FROM (SELECT order_id, price FROM Order) AS d JOIN (SELECT order_id FROM Order) AS j USING (order_id)")
	if err != nil {
		t.Fatal(err)
	}
	simple := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext).QueryTerm().(*antlrgen.SimpleTableContext)
	fs, err := parseFromSource(simple)
	if err != nil {
		t.Fatal(err)
	}
	fs.enclosingScope = outer.Scope()
	visitor := NewPlanVisitor(md)
	visitor.enclosingScope = outer.Scope()
	visitor.assignDerivedSourceBindings(fs)
	op, err := visitor.visitFrom(simple, fs)
	if err != nil {
		t.Fatal(err)
	}
	cls, err := classifySelectElements(simple, nil)
	if err != nil {
		t.Fatal(err)
	}
	sq := selectQueryFromClassification(cls, fs)
	check := func(t *testing.T, root logical.LogicalOperator, wantJoined bool) {
		t.Helper()
		var carriers []*logical.LogicalCTE
		var walk func(logical.LogicalOperator)
		walk = func(node logical.LogicalOperator) {
			if c, ok := node.(*logical.LogicalCTE); ok {
				carriers = append(carriers, c)
				return // The exported source, never hidden definitions inside Body.
			}
			for _, child := range node.Children() {
				walk(child)
			}
		}
		if root == nil {
			t.Fatal("rebuild returned no plan")
		}
		walk(root)
		want := 1
		if wantJoined {
			want = 2
		}
		if len(carriers) != want {
			t.Fatalf("exported carriers=%d, want %d", len(carriers), want)
		}
		for i, c := range carriers {
			binding, body := sq.bindingID, sq.catalogAwareInnerPlan
			if i == 1 {
				binding, body = sq.joins[0].bindingID, sq.joins[0].catalogAwareInnerPlan
			}
			scan, ok := c.Main.(*logical.LogicalScan)
			if binding == "" || c.Name() != binding || c.Binding != binding || !ok || scan.Table != binding || c.Body() != body {
				t.Fatalf("carrier lost identity or rebuilt Body: binding=%q carrier=%#v main=%#v", binding, c, c.Main)
			}
		}
	}
	check(t, op, true)
	check(t, buildLogicalPlanForSelect(sq), true)
	catalog, err := buildLogicalPlanForSelectWithCTECatalog(sq, md, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	check(t, catalog, true)
	single := *sq
	single.joins = nil
	check(t, buildOuterPlanOnDerived(&single, sq.catalogAwareInnerPlan), false)
	selectResolver, err := buildSelectScopeChecked(sq, md, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, resolver := range []*expr.Resolver{selectResolver, buildProjectionResolverWithCTEScopes(sq, md, "", nil)} {
		if resolver == nil || resolver.Scope().Parent() != outer.Scope() {
			t.Fatal("scope rebuild lost the actual lexical parent")
		}
		bound, err := resolver.ResolveIdentifierPath([]semantic.Identifier{semantic.FromNormalized("D"), semantic.FromNormalized("PRICE")})
		if err != nil {
			t.Fatal(err)
		}
		correlations := values.GetCorrelatedToOfValue(bound)
		if _, ok := correlations[values.NamedCorrelationIdentifier(sq.bindingID)]; !ok || len(correlations) != 1 {
			t.Fatalf("derived reference lost its private owner: %v", correlations)
		}
		if bound.Type().Code() != values.TypeCodeInt {
			t.Fatalf("derived PRICE type=%v, want INTEGER", bound.Type())
		}
	}
	sources := buildOuterScopeSources(sq, md, "", nil)
	if len(sources) != 2 || sources[0].Alias.Name() != "D" || sources[0].CorrelationName != sq.bindingID || sources[1].Alias.Name() != "J" || sources[1].CorrelationName != sq.joins[0].bindingID {
		t.Fatalf("nested planner source metadata lost lexical/runtime identities: %+v", sources)
	}
}

// Plain references and expanded stars must inherit the same declared attribute
// name, including references admitted by the case-insensitive lookup extension.
func TestProjectionInheritsResolvedUnqualifiedName(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(`CREATE TABLE QCASE (id BIGINT, "KeepCase" BIGINT, plain BIGINT, PRIMARY KEY (id))`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		projection, want string
	}{
		{`"KeepCase"`, "KeepCase"},
		{`KeepCase`, "KeepCase"},
		{`"KEEPCASE"`, "KeepCase"},
		{`"keepcase"`, "KeepCase"},
		{`"plain"`, "PLAIN"},
		{`"keepcase" AS "chosen"`, "chosen"},
		{`QCASE."KeepCase"`, "KeepCase"},
	} {
		t.Run(tc.projection, func(t *testing.T) {
			t.Parallel()
			root, err := parseQueryFromSelect(t, "SELECT "+tc.projection+" FROM QCASE")
			if err != nil {
				t.Fatal(err)
			}
			op, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(root)
			if err != nil {
				t.Fatal(err)
			}
			proj := findProjection(op)
			if proj == nil || len(proj.Aliases) != 1 || proj.Aliases[0] != tc.want {
				t.Fatalf("projection = %+v, want inherited name %q", proj, tc.want)
			}
		})
	}
}

func TestProjectionKeepsScalarQOVSQLName(t *testing.T) {
	t.Parallel()
	for _, binding := range []string{"V", "Q$DUP1"} {
		t.Run(binding, func(t *testing.T) {
			t.Parallel()
			value, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(binding), values.NotNullLong)
			if err != nil {
				t.Fatal(err)
			}
			proj := logical.NewProject(logical.NewScan("QARR", "", ""), []string{"v"}, nil)
			proj.ProjectedValues = []values.Value{value}
			sq := &selectQuery{}
			sq.projCols = []projCol{{name: "v", bare: "v"}}
			publishInheritedProjectionNames(proj, sq)
			if len(proj.Aliases) != 1 || proj.Aliases[0] != "v" {
				t.Fatalf("SQL names = %v, want [v], not quantifier binding %q", proj.Aliases, binding)
			}
		})
	}
}

func TestBuildScalarRetainsCorrelationCarriedByCTEBody(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE seed (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	parent := semantic.NewScope(nil)
	if err := parent.AddSource(semantic.ScopeSource{
		Alias: semantic.FromNormalized("O"), CorrelationName: "O",
		Table: &semantic.StaticTable{
			TableName: semantic.FromSegments([]string{"O"}, false),
			TableColumns: []semantic.Column{{
				Id: semantic.FromNormalized("ID"), Type: "BIGINT", Nullable: true,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, bodySQL, scalarSQL string
		correlated               bool
	}{
		{"outer_through_cte", "SELECT o.id AS v FROM seed", "SELECT MAX(v) FROM c", true},
		{"scalar_alias_matches_captured_outer", "SELECT o.id AS v FROM seed", "SELECT MAX(v) FROM c o", true},
		{"local_alias_shadows_outer", "SELECT o.id AS v FROM seed o", "SELECT MAX(v) FROM c", false},
		{"local_column", "SELECT id AS v FROM seed", "SELECT MAX(v) FROM c", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodyQuery, err := parseQueryFromSelect(t, tc.bodySQL)
			if err != nil {
				t.Fatal(err)
			}
			bodyVisitor := NewPlanVisitor(md)
			bodyVisitor.enclosingScope = parent
			body, err := bodyVisitor.VisitQuery(bodyQuery)
			if err != nil {
				t.Fatalf("bind CTE body against O: %v", err)
			}
			source, exact := exactVirtualScopeSource("C", body, md, nil, nil)
			if !exact {
				t.Fatal("bound CTE body has no exact V column")
			}
			registry := testCTERegistry(map[string]logical.LogicalOperator{"C": body})
			source.CTE = registry.Lookup("C")
			planner := &existsSubqueryPlanner{
				md: md, outerScope: parent,
				cteScopes:    map[string]semantic.ScopeSource{"C": source},
				cteProducers: registry,
			}
			q, err := parseQueryFromSelect(t, tc.scalarSQL)
			if err != nil {
				t.Fatal(err)
			}
			clause := planner.newClause()
			alias, resultType, err := clause.BuildScalar(q)
			if err == nil {
				err = clause.admitValues([]values.Value{values.NewScalarSubqueryValue(alias, resultType)})
			}
			if err != nil {
				t.Fatalf("BuildScalar: %v", err)
			}
			if !resultType.Equals(values.NullableLong) {
				t.Fatalf("scalar result type = %v, want nullable BIGINT", resultType)
			}
			var innerPlan logical.LogicalOperator
			if len(planner.correlatedScalarSubqueries) == 1 {
				innerPlan = planner.correlatedScalarSubqueries[0].InnerPlan
			} else if len(planner.scalarSubqueries) == 1 {
				innerPlan = planner.scalarSubqueries[0].Plan
			}
			ref, _, err := query.TranslateToCascadesWithError(innerPlan, md)
			if err != nil || ref == nil {
				t.Fatalf("translate registered scalar: ref=%v, err=%v", ref, err)
			}
			free := ref.GetCorrelatedTo()
			t.Logf("registered scalar free correlations: %v", free)
			wantCorrelated, wantIndependent := 0, 1
			if tc.correlated {
				wantCorrelated, wantIndependent = 1, 0
			}
			_, hasOuter := free[values.NamedCorrelationIdentifier("O")]
			if len(free) != wantCorrelated || hasOuter != tc.correlated {
				t.Fatalf("bound CTE free correlations = %v, want O present=%v and count=%d", free, tc.correlated, wantCorrelated)
			}
			if len(planner.correlatedScalarSubqueries) != wantCorrelated || len(planner.scalarSubqueries) != wantIndependent {
				t.Fatalf("CTE scalar registered correlated=%d independent=%d, want %d/%d",
					len(planner.correlatedScalarSubqueries), len(planner.scalarSubqueries), wantCorrelated, wantIndependent)
			}
		})
	}
}

// A query-body access counter travels with the actual parsed query, including
// through newly constructed visitors. It observes repeated body traversal without
// a global hook or a production-side counter, and is local to one test tree.
type countedDerivedQuery struct {
	antlrgen.IQueryContext
	bodyReads int
}

func (q *countedDerivedQuery) QueryExpressionBody() antlrgen.IQueryExpressionBodyContext {
	q.bodyReads++
	return q.IQueryContext.QueryExpressionBody()
}

func TestDerivedBindingQueryBodyAccessIsLinear(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	for depth := 1; depth <= 4; depth++ {
		t.Run(strings.Repeat("nested_", depth), func(t *testing.T) {
			t.Parallel()
			sql := "SELECT id FROM t"
			for range depth {
				sql = "SELECT id FROM (" + sql + ") d"
			}
			q, err := parseQueryFromSelect(t, sql)
			if err != nil {
				t.Fatal(err)
			}
			var counters []*countedDerivedQuery
			var wrap func(antlr.Tree) antlr.Tree
			wrap = func(node antlr.Tree) antlr.Tree {
				children := node.GetChildren()
				for i, child := range children {
					children[i] = wrap(child)
				}
				if query, ok := node.(antlrgen.IQueryContext); ok {
					counted := &countedDerivedQuery{IQueryContext: query}
					counters = append(counters, counted)
					return counted
				}
				return node
			}
			wrapped := wrap(q).(antlrgen.IQueryContext)
			if len(counters) != depth+1 {
				t.Fatalf("counted %d query bodies at depth %d, want %d", len(counters), depth, depth+1)
			}
			if _, err := NewPlanVisitor(template.Underlying()).VisitQuery(wrapped); err != nil {
				t.Fatal(err)
			}
			total := 0
			for _, counted := range counters {
				if counted.bodyReads == 0 {
					t.Fatal("counter did not witness its query body")
				}
				total += counted.bodyReads
			}
			t.Logf("depth=%d query bodies=%d body accesses=%d", depth, len(counters), total)
			if total > 2*(depth+1) {
				t.Fatalf("depth %d rebuilt query bodies: %d accesses, want at most %d", depth, total, 2*(depth+1))
			}
		})
	}
}

func TestPreparedDerivedBodyRetainsDuplicateNamesAndTypes(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL(
		"CREATE TABLE a (id BIGINT, k BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE b (id BIGINT, k DOUBLE, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	sql := "SELECT x.k, y.k FROM a x JOIN b y ON x.id = y.id"
	for depth := 0; depth <= 3; depth++ {
		q, err := parseQueryFromSelect(t, sql)
		if err != nil {
			t.Fatal(err)
		}
		body, err := NewPlanVisitor(md).buildCTEBodyQuery(q)
		if err != nil {
			t.Fatalf("depth %d body: %v", depth, err)
		}
		source, exact := exactVirtualScopeSource("D", body, md, nil, nil)
		if !exact {
			t.Fatalf("depth %d prepared body lost exact schema", depth)
		}
		columns := source.Table.Columns()
		if len(columns) != 2 || columns[0].Id.Name() != "K" || columns[1].Id.Name() != "K" ||
			columns[0].Type != "BIGINT" || columns[1].Type != "DOUBLE" {
			t.Fatalf("depth %d prepared columns = %+v, want K BIGINT / K DOUBLE", depth, columns)
		}
		flowed := expr.SourceRowType(source)
		if flowed == nil || len(flowed.Fields) != 2 || flowed.Fields[0].Name == "" || flowed.Fields[1].Name == "" ||
			flowed.Fields[0].Name == flowed.Fields[1].Name ||
			flowed.Fields[0].Ordinal != 0 || flowed.Fields[1].Ordinal != 1 ||
			!flowed.Fields[0].FieldType.Equals(values.NullableLong) || !flowed.Fields[1].FieldType.Equals(values.NullableDouble) {
			t.Fatalf("depth %d flowed row = %v, want distinct physical slots [0] BIGINT / [1] DOUBLE", depth, flowed)
		}
		scope := semantic.NewScope(nil)
		if err := scope.AddSource(source); err != nil {
			t.Fatal(err)
		}
		_, _, _, err = scope.ResolvePathNested([]semantic.Identifier{semantic.FromNormalized("D"), semantic.FromNormalized("K")})
		var ambiguous *semantic.AmbiguousColumnError
		if !errors.As(err, &ambiguous) {
			t.Fatalf("depth %d lookup D.K = %v, want ambiguous duplicate SQL label", depth, err)
		}
		sql = "SELECT d.* FROM (" + sql + ") d"
	}
}

func TestPreparedPromotedDerivedBodyIsNotRevisited(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	for _, sql := range []string{
		"SELECT (SELECT MAX(v) FROM c) AS x FROM t UNION ALL SELECT 9.5 AS other FROM t",
		"WITH local_t AS (SELECT id FROM t) SELECT (SELECT MAX(v) FROM c) AS x FROM local_t UNION ALL SELECT 9.5 AS other FROM local_t",
	} {
		bodyQuery, err := parseQueryFromSelect(t, "SELECT id AS v FROM t")
		if err != nil {
			t.Fatal(err)
		}
		visitor := NewPlanVisitor(md)
		cteBody, err := visitor.VisitQuery(bodyQuery)
		if err != nil {
			t.Fatal(err)
		}
		cteSource, exact := exactVirtualScopeSource("C", cteBody, md, nil, nil)
		if !exact {
			t.Fatal("CTE C has no exact schema")
		}
		visitor.cteProducers = testCTERegistry(map[string]logical.LogicalOperator{"C": cteBody})
		cteSource.CTE = visitor.cteProducers.Lookup("C")
		visitor.cteScopes = map[string]semantic.ScopeSource{"C": cteSource}
		q, err := parseQueryFromSelect(t, sql)
		if err != nil {
			t.Fatal(err)
		}
		counted := &countedDerivedQuery{IQueryContext: q}
		body, err := visitor.buildCTEBodyQuery(counted)
		if err != nil {
			t.Fatalf("prepare complete body: %v", err)
		}
		reads := counted.bodyReads
		if reads == 0 {
			t.Fatal("counter did not witness preparation")
		}
		source, err := boundDerivedSource(md, "D", "PRIVATE_D", body, visitor.cteScopes)
		if counted.bodyReads != reads {
			t.Errorf("bound source revisited prepared query: reads %d -> %d", reads, counted.bodyReads)
		}
		if err != nil {
			t.Errorf("promoted retained body %q: %v", sql, err)
			continue
		}
		columns := source.Table.Columns()
		if source.CorrelationName != "PRIVATE_D" || len(columns) != 1 || columns[0].Id.Name() != "X" ||
			columns[0].Type != "DOUBLE" || !columns[0].Nullable {
			t.Errorf("promoted source = %+v columns=%+v, want PRIVATE_D(X DOUBLE NULL)", source, columns)
		}
	}
}

func TestLegacyDerivedBindingPreparesPromotedBodyOnce(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	q, err := parseQueryFromSelect(t, "SELECT id AS x FROM t UNION ALL SELECT 9.5 AS other FROM t")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedDerivedQuery{IQueryContext: q}
	source, err := buildDerivedTableSourceWithCTEsChecked(template.Underlying(), "D", counted, defaultEmbeddedSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if counted.bodyReads != 1 {
		t.Errorf("legacy adapter accessed body %d times, want one owner-side preparation", counted.bodyReads)
	}
	columns := source.Table.Columns()
	if len(columns) != 1 || columns[0].Id.Name() != "X" || columns[0].Type != "DOUBLE" || !columns[0].Nullable {
		t.Fatalf("promoted columns = %+v, want X DOUBLE NULL", columns)
	}
}

func TestCorrelatedExistsDerivedBodyRetainsCTEEnvironment(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	bodyQuery, err := parseQueryFromSelect(t, "SELECT id AS v FROM t")
	if err != nil {
		t.Fatal(err)
	}
	body, err := NewPlanVisitor(md).VisitQuery(bodyQuery)
	if err != nil {
		t.Fatal(err)
	}
	source, exact := exactVirtualScopeSource("C", body, md, nil, nil)
	if !exact {
		t.Fatal("CTE has no schema")
	}
	outer, exact := exactVirtualScopeSource("O", logical.NewScan("T", "O"), md, nil, nil)
	if !exact {
		t.Fatal("outer table has no schema")
	}
	parent := semantic.NewScope(nil)
	if err := parent.AddSource(outer); err != nil {
		t.Fatal(err)
	}
	registry := testCTERegistry(map[string]logical.LogicalOperator{"C": body})
	source.CTE = registry.Lookup("C")
	planner := &existsSubqueryPlanner{
		md: md, outerScope: parent, outerScopes: []semantic.ScopeSource{outer},
		cteScopes:    map[string]semantic.ScopeSource{"C": source},
		cteProducers: registry,
	}
	q, err := parseQueryFromSelect(t, "SELECT o.id FROM (SELECT (SELECT MAX(v) FROM c) AS x FROM t) d WHERE d.x = o.id")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := planner.bindQuery(q)
	if err != nil {
		t.Fatalf("bind correlated derived body: %v", err)
	}
	lowered, err := lowerBoundExists(bound)
	op := lowered.plan
	if err != nil || op == nil {
		t.Fatalf("correlated derived body lost C's defining environment: op=%v err=%v", op, err)
	}
	ref, scalars, err := query.TranslateToCascadesWithError(op, md)
	if err != nil || ref == nil {
		t.Fatalf("translate retained correlated body: ref=%v err=%v", ref, err)
	}
	if len(scalars) != 1 {
		t.Fatalf("pre-evaluated scalars = %d, want the retained MAX(V) plan", len(scalars))
	}
	free := ref.GetCorrelatedTo()
	if _, present := free[scalars[0].Alias]; !present || len(free) != 1 {
		t.Fatalf("body correlations = %v, want exactly retained scalar %v", free, scalars[0].Alias)
	}
	// EXISTS carries the outer/inner comparison on its attachment, not inside
	// the returned FROM body. Both identities must survive that boundary.
	carrier, ok := op.(*logical.LogicalCTE)
	if !ok {
		t.Fatalf("derived carrier = %T, want retained CTE body", op)
	}
	joinFree := predicates.GetCorrelatedToOfPredicate(lowered.join)
	_, hasOuter := joinFree[values.NamedCorrelationIdentifier("O")]
	_, hasInner := joinFree[values.NamedCorrelationIdentifier(carrier.Binding)]
	if len(joinFree) != 2 || !hasOuter || !hasInner {
		t.Fatalf("EXISTS comparison correlations = %v, want O and %s", joinFree, carrier.Binding)
	}
}

func TestBoundDerivedSourcePreservesFailureClass(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	projection := func(typs ...values.Type) logical.LogicalOperator {
		p := &logical.LogicalProject{}
		for _, typ := range typs {
			p.Projections = append(p.Projections, "X")
			p.ProjectedValues = append(p.ProjectedValues, &values.ConstantValue{Typ: typ})
		}
		return p
	}
	for _, tc := range []struct {
		name string
		body logical.LogicalOperator
		code api.ErrorCode
	}{
		{"missing_preparation", nil, api.ErrCodeUnsupportedQuery},
		{"union_width", logical.NewUnion([]logical.LogicalOperator{
			projection(values.NotNullLong), projection(values.NotNullDouble, values.NotNullLong),
		}, false), api.ErrCodeUnionIncorrectColumnCount},
		{"union_type", logical.NewUnion([]logical.LogicalOperator{
			projection(values.NotNullLong), projection(values.NotNullString),
		}, false), api.ErrCodeUnionIncompatibleColumns},
		{"unrepresentable_nullable_array_element", projection(&values.ArrayType{
			ElementType: values.NullableLong,
		}), api.ErrCodeUnsupportedQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source, err := boundDerivedSource(template.Underlying(), "D", "PRIVATE_D", tc.body, nil)
			var apiErr *api.Error
			if source.Table != nil || !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Fatalf("derived source=%+v err=%v, want empty source and typed %s", source, err, tc.code)
			}
		})
	}
}

func TestExistsFinalAttachmentTypeMatchesTranslatedRow(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL(
		"CREATE TABLE t (id BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE flags (k BIGINT, PRIMARY KEY (k)) " +
			"CREATE TABLE seed (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	q, err := parseQueryFromSelect(t, `SELECT o.id FROM t o, flags f WHERE EXISTS (
		SELECT 1 FROM seed s, t m WHERE f.k > 0 AND EXISTS (
			SELECT 1 FROM flags nf WHERE nf.k > 0)) ORDER BY o.id`)
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	op, err := NewPlanVisitor(md).VisitQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	attachments := 0
	owned := make(map[values.CorrelationIdentifier]*expressions.Reference)
	var visit func(logical.LogicalOperator)
	visit = func(op logical.LogicalOperator) {
		if filter, ok := op.(*logical.LogicalFilter); ok {
			for _, attachment := range filter.ExistsSubqueries {
				attachments++
				if attachment.Input == nil {
					t.Fatal("attachment has no owned producer")
				}
				owned[attachment.Alias] = attachment.Input.Reference()
				ref, _, err := query.TranslateToCascadesWithError(attachment.Plan, md)
				if err != nil || ref == nil {
					t.Fatalf("translate attachment: ref=%v, err=%v", ref, err)
				}
				result := ref.Get().GetResultValue()
				if result == nil || result.Type() == nil || attachment.FlowedType == nil {
					t.Fatal("attachment has no exact result type")
				}
				t.Logf("attachment %s: declared=%s translated=%s", attachment.Alias.Name(),
					values.DescribeType(attachment.FlowedType), values.DescribeType(result.Type()))
				if !attachment.FlowedType.Equals(result.Type()) {
					t.Errorf("attachment %s: declared row disagrees with translated result", attachment.Alias.Name())
				}
				visit(attachment.Plan)
			}
		}
		for _, child := range op.Children() {
			visit(child)
		}
	}
	visit(op)
	root, _, err := query.TranslateToCascadesWithError(op, md)
	if err != nil || root == nil {
		t.Fatalf("translate enclosing query: %v", err)
	}
	seen := make(map[values.CorrelationIdentifier]bool)
	var visitReferences func(*expressions.Reference)
	visitReferences = func(ref *expressions.Reference) {
		for _, quantifier := range ref.Get().GetQuantifiers() {
			if expected, ok := owned[quantifier.GetAlias()]; ok {
				if quantifier.GetRangesOver() != expected {
					t.Errorf("attachment %s rebuilt its owned producer", quantifier.GetAlias().Name())
				}
				seen[quantifier.GetAlias()] = true
			}
			visitReferences(quantifier.GetRangesOver())
		}
	}
	visitReferences(root)
	if len(seen) != len(owned) {
		t.Fatalf("consumed %d owned references, want %d", len(seen), len(owned))
	}
	if attachments != 2 {
		t.Fatalf("observed %d attachments, want middle and nested existential", attachments)
	}
}

func TestPrimarySourceRetainsAssignedBinding(t *testing.T) {
	t.Parallel()
	template, err := buildSchemaTemplateFromDDL("CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	for _, table := range []string{"t", "c", "inline_values"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			statement := "SELECT x.id FROM " + table + " x"
			if table == "inline_values" {
				statement = "SELECT x.id FROM VALUES (1) AS x(id)"
			}
			q, err := parseQueryFromSelect(t, statement)
			if err != nil {
				t.Fatal(err)
			}
			simple := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext).QueryTerm().(*antlrgen.SimpleTableContext)
			fs, err := parseFromSource(simple)
			if err != nil {
				t.Fatal(err)
			}
			fs.bindingID = "PRIVATE_X"
			cls, err := classifySelectElements(simple, nil)
			if err != nil {
				t.Fatal(err)
			}
			sq := selectQueryFromClassification(cls, fs)
			visitor := NewPlanVisitor(md)
			if table == "c" {
				source, ok := exactVirtualScopeSource("C", logical.NewScan("T", ""), md, nil, nil)
				if !ok {
					t.Fatal("CTE has no exact source")
				}
				visitor.cteScopes = map[string]semantic.ScopeSource{"C": source}
			}
			from, err := visitor.visitFrom(simple, fs)
			if err != nil {
				t.Fatal(err)
			}
			for name, root := range map[string]logical.LogicalOperator{
				"visitor": from,
				"adapter": buildLogicalPlanForSelect(sq),
			} {
				for len(root.Children()) > 0 {
					root = root.Children()[0]
				}
				var binding, alias string
				switch source := root.(type) {
				case *logical.LogicalScan:
					binding, alias = source.Binding, source.Alias
				case *logical.LogicalInlineValues:
					binding, alias = source.Binding, source.Alias
				default:
					t.Fatalf("%s unexpected primary source: %T", name, root)
				}
				if binding != "PRIVATE_X" || alias != "X" {
					t.Errorf("%s primary source lost assigned binding or SQL alias: %#v", name, root)
				}
			}
			resolver, err := buildSelectScopeChecked(sq, md, "", visitor.cteScopes)
			if err != nil {
				t.Fatal(err)
			}
			projection := buildProjectionResolverWithCTEScopes(sq, md, "", visitor.cteScopes)
			if projection == nil {
				t.Fatal("primary source has no projection resolver")
			}
			for name, sources := range map[string][]semantic.ScopeSource{
				"select":     resolver.Scope().Sources(),
				"projection": projection.Scope().Sources(),
				"subquery":   buildOuterScopeSources(sq, md, "", visitor.cteScopes),
			} {
				if len(sources) != 1 || sources[0].CorrelationName != "PRIVATE_X" || sources[0].Alias.Name() != "X" {
					t.Errorf("%s source disagrees with assigned binding: %+v", name, sources)
					continue
				}
				column, err := expr.SourceColumnValue(sources[0], 0)
				if err != nil {
					t.Fatal(err)
				}
				free := values.GetCorrelatedToOfValue(column)
				if _, bound := free[values.NamedCorrelationIdentifier("PRIVATE_X")]; !bound || len(free) != 1 {
					t.Errorf("%s resolved column correlations=%v, want only PRIVATE_X", name, free)
				}
			}
		})
	}
}

// The catalog SELECT constructor is a separate entry to the same star
// expansion. Driver queries alone do not prove that this entry keeps slots.
func TestCatalogUsingStarPublishesAttributeOrdinals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, selectList string }{
		{"duplicate", "order_id AS id, price AS x, quantity AS x"},
		{"quoted_duplicate", `order_id AS id, price AS "x.y", quantity AS "x.y"`},
		{"unnamed", "order_id AS id, 7, 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := buildTestMetaData(t)
			sq := parseSelect(t, "SELECT * FROM (SELECT "+tc.selectList+" FROM Order) d JOIN (SELECT customer_id AS id FROM Customer) u USING (id)")
			sq.bindingID = "USING_LEFT_SLOT_OWNER"
			op, err := buildLogicalPlanForSelectWithCatalog(sq, md, defaultEmbeddedSchema)
			if err != nil {
				t.Fatal(err)
			}
			project := findProjection(op)
			if project == nil || len(project.Projections) != 3 || len(project.ProjectedValues) != 3 {
				t.Fatalf("USING star must keep the left three attributes and hide only the right ID: %v", op)
			}
			for ordinal, value := range project.ProjectedValues {
				field, ok := values.AsFieldValue(value)
				if !ok {
					t.Fatalf("attribute %d is not a bound FieldValue: %T", ordinal, value)
				}
				path := field.Path().Ordinals()
				if len(path) != 1 || path[0] != ordinal {
					t.Fatalf("attribute %d resolved to ordinals %v, want [%d]", ordinal, path, ordinal)
				}
				free := values.GetCorrelatedToOfValue(value)
				if _, ok := free[values.NamedCorrelationIdentifier(sq.bindingID)]; !ok || len(free) != 1 {
					t.Fatalf("attribute %d lost its retained derived-source owner: %v, want %s", ordinal, free, sq.bindingID)
				}
			}
		})
	}
}

// The compatibility readers return no resolver on source failure, never a
// partially populated scope or a same-named catalog substitute. Checked source
// construction remains the owner of the diagnostic; this test pins the adapters'
// existing nil-on-failure contract rather than broadening SQL admission.
func TestReaderScopesDoNotPublishIncompleteSources(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		query     *selectQuery
		cteScopes map[string]semantic.ScopeSource
		noCatalog bool
	}{
		{name: "missing_primary", query: &selectQuery{tableName: "MISSING"}},
		{name: "missing_join", query: &selectQuery{tableName: "Order", joins: []joinClause{{tableName: "MISSING"}}}},
		{name: "cte_tombstone", query: &selectQuery{tableName: "Order"}, cteScopes: map[string]semantic.ScopeSource{"ORDER": {}}},
		{name: "no_catalog", query: &selectQuery{tableName: "Order"}, noCatalog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := buildTestMetaData(t)
			if tc.noCatalog {
				md = nil
			}
			checked, err := buildSelectScopeChecked(tc.query, md, defaultEmbeddedSchema, tc.cteScopes)
			if err == nil || checked != nil {
				t.Fatalf("checked source construction = (%v, %v), want no scope and an error", checked, err)
			}
			if got := buildProjectionResolverWithCTEScopes(tc.query, md, defaultEmbeddedSchema, tc.cteScopes); got != nil {
				t.Fatal("projection reader published a partial or substitute source")
			}
			if got := buildOuterScopeSources(tc.query, md, defaultEmbeddedSchema, tc.cteScopes); got != nil {
				t.Fatalf("outer-source reader published a partial or substitute source: %+v", got)
			}
		})
	}
}
