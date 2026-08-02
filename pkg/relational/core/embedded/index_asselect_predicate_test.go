package embedded

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// RFC-202 S5 goldens: the sparse-index (WHERE) predicate arm of the AS-SELECT
// generator, asserted byte-identical (at the RecordMetaDataProto.Predicate
// level) to what Java's MaterializedViewIndexGenerator stores for the same
// declaration. Pipeline under test: getTopLevelPredicate
// (MaterializedViewIndexGenerator.java:640-682) → DNF normalization without
// simplification (:675) → IndexPredicate.isSupported (:676) → the
// dnf-yields-no-ranges fallback to the non-normalized conjunction (:677-679)
// → IndexPredicate.fromQueryPredicate(...).toProto() (:169-172).

const asSelectPredicateDDL = `
	CREATE TABLE tp(pk bigint, price bigint, stock bigint, qty integer, weight double, name string, flag boolean, primary key(pk))
`

func buildPredicateIndex(t *testing.T, indexDDL string) (*recordlayer.Index, error) {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(asSelectPredicateDDL + "\n" + indexDDL)
	if err != nil {
		return nil, err
	}
	for _, idx := range tmpl.Underlying().GetAllIndexes() {
		if idx.Name == "PIDX" {
			return idx, nil
		}
	}
	t.Fatalf("index PIDX not found after %q", indexDDL)
	return nil, nil
}

// Proto shorthands for the expected bytes — each shape written as the Java
// PoJo serialization it was ported from (IndexPredicate.java, IndexComparison.java).
func predVP(path []string, cmp *gen.Comparison) *gen.Predicate {
	return &gen.Predicate{ValuePredicate: &gen.ValuePredicate{Value: path, Comparison: cmp}}
}

func predAnd(children ...*gen.Predicate) *gen.Predicate {
	return &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: children}}
}

func predOr(children ...*gen.Predicate) *gen.Predicate {
	return &gen.Predicate{OrPredicate: &gen.OrPredicate{Children: children}}
}

func predConst(v gen.ConstantPredicate_ConstantValue) *gen.Predicate {
	return &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{Value: v.Enum()}}
}

func cmpSimple(t gen.ComparisonType, operand *gen.Value) *gen.Comparison {
	return &gen.Comparison{SimpleComparison: &gen.SimpleComparison{Type: t.Enum(), Operand: operand}}
}

func cmpNull(isNull bool) *gen.Comparison {
	return &gen.Comparison{NullComparison: &gen.NullComparison{IsNull: &isNull}}
}

func valInt(n int32) *gen.Value      { return &gen.Value{IntValue: &n} }
func valLong(n int64) *gen.Value     { return &gen.Value{LongValue: &n} }
func valStr(s string) *gen.Value     { return &gen.Value{StringValue: &s} }
func valBool(b bool) *gen.Value      { return &gen.Value{BoolValue: &b} }
func valDouble(n float64) *gen.Value { return &gen.Value{DoubleValue: &n} }

func TestAsSelectSparseIndex_PredicateGoldens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ddl  string
		want *gen.Predicate
	}{
		// Simple comparison over a BIGINT column: the stored comparand
		// follows the COMPARISON's promoted type — the COLUMN's — so the
		// in-int-range literal 20 stores long_value, MEASURED against the
		// live 4.12.11.0 JVM (the D11 stored-bytes comparison,
		// conformance/index_ddl_metadata_conformance_test.go). The
		// ParseHelpers.parseDecimal Integer narrowing (ParseHelpers.java:96-101)
		// describes only the literal as parsed; the comparison promotes it.
		{
			"greater_than_bigint_col",
			"CREATE INDEX pidx AS SELECT name, price FROM tp WHERE price > 20 ORDER BY price",
			predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valLong(20))),
		},
		// The same literal against an INTEGER column stays int_value — the
		// column IS the promoted type (measured, D11 i_sparse_int).
		{
			"greater_than_integer_col",
			"CREATE INDEX pidx AS SELECT qty FROM tp WHERE qty > 20 ORDER BY qty",
			predVP([]string{"QTY"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valInt(20))),
		},
		// A DOUBLE column promotes an integer literal to double_value —
		// the corpus filtered-index twin's exact shape
		// (index-ddl-values-only's idx_mv_filtered_expensive over
		// `price double`; measured, D11 i_sparse_dbl).
		{
			"greater_than_double_col",
			"CREATE INDEX pidx AS SELECT weight FROM tp WHERE weight > 20 ORDER BY weight",
			predVP([]string{"WEIGHT"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valDouble(20))),
		},
		// An out-of-int-range literal against a BIGINT column: literal and
		// column are both LONG — long_value.
		{
			"greater_than_long",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE price > 3000000000",
			predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valLong(3000000000))),
		},
		// String equality — corpus twin: index-ddl-values-only.yamsql
		// IDX_MV_FILTERED_ELECTRONICS.
		{
			"string_equals",
			"CREATE INDEX pidx AS SELECT name FROM tp WHERE name = 'bar'",
			predVP([]string{"NAME"}, cmpSimple(gen.ComparisonType_EQUALS, valStr("bar"))),
		},
		// Boolean equality — corpus twin: sparse-index-tests.yamsql i3's
		// first disjunct.
		{
			"bool_equals",
			"CREATE INDEX pidx AS SELECT flag FROM tp WHERE flag = true",
			predVP([]string{"FLAG"}, cmpSimple(gen.ComparisonType_EQUALS, valBool(true))),
		},
		// NOT_EQUALS is serializable (IndexComparison ComparisonType) even
		// though it cannot bound a scan prefix — the single-comparison DNF
		// fails range expansion and falls back to the identical conjunction.
		{
			"not_equals",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE price <> 5",
			predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_NOT_EQUALS, valLong(5))),
		},
		// Unary null checks → NullComparison (IndexComparison.java:324-327)
		// — corpus twin: recursive-cte.yamsql CHILDIDXNONULLS.
		{
			"is_not_null",
			"CREATE INDEX pidx AS SELECT stock FROM tp WHERE stock IS NOT NULL",
			predVP([]string{"STOCK"}, cmpNull(false)),
		},
		{
			"is_null",
			"CREATE INDEX pidx AS SELECT stock FROM tp WHERE stock IS NULL",
			predVP([]string{"STOCK"}, cmpNull(true)),
		},
		// Conjunction — corpus twin: index-ddl-values-only.yamsql
		// IDX_MV_FILTERED_MULTI. Already in DNF (an AND of variables), so the
		// normalizer leaves it alone and the AND is stored.
		{
			"conjunction",
			"CREATE INDEX pidx AS SELECT name, price, stock FROM tp WHERE price > 15 AND stock > 60 ORDER BY name",
			predAnd(
				predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valLong(15))),
				predVP([]string{"STOCK"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valLong(60))),
			),
		},
		// Disjunction over one key — corpus twin: filter-index.yamsql i1,
		// sparse-index-tests.yamsql i6. Already DNF; ranges expand; the OR is
		// stored.
		{
			"or_same_key",
			"CREATE INDEX pidx AS SELECT name FROM tp WHERE name = 'bar' OR name = 'foo'",
			predOr(
				predVP([]string{"NAME"}, cmpSimple(gen.ComparisonType_EQUALS, valStr("bar"))),
				predVP([]string{"NAME"}, cmpSimple(gen.ComparisonType_EQUALS, valStr("foo"))),
			),
		},
		// Mixed disjunct kinds over one key — corpus twin:
		// sparse-index-tests.yamsql i3/i4 (`tombstone = true or tombstone is
		// not null`).
		{
			"or_eq_and_notnull",
			"CREATE INDEX pidx AS SELECT flag FROM tp WHERE flag = true OR flag IS NOT NULL",
			predOr(
				predVP([]string{"FLAG"}, cmpSimple(gen.ComparisonType_EQUALS, valBool(true))),
				predVP([]string{"FLAG"}, cmpNull(false)),
			),
		},
		// NOT in DNF the normalizer produces: `AND(a=1, OR(a=2, a=3))` is not
		// in normal form, DNF-distributes to `OR(AND(a=1,a=2), AND(a=1,a=3))`
		// whose groups are single-key ranges — the DNF is STORED
		// (MaterializedViewIndexGenerator.java:675, :681). Reverting the
		// normalization stores the conjunction and this golden goes red
		// (RFC-202 §8(a) mutation 10).
		{
			"dnf_materialized",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE price = 1 AND (price = 2 OR price = 3)",
			predOr(
				predAnd(
					predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(1))),
					predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(2))),
				),
				predAnd(
					predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(1))),
					predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(3))),
				),
			),
		},
		// The DNF's groups mix keys (price with stock), so range expansion
		// fails and the NON-normalized conjunction is stored instead
		// (:677-679). Always storing the DNF turns this golden red (RFC-202
		// §8(a) mutation 11).
		{
			"dnf_no_ranges_fallback",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE price = 1 AND (stock = 2 OR stock = 3)",
			predAnd(
				predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(1))),
				predOr(
					predVP([]string{"STOCK"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(2))),
					predVP([]string{"STOCK"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(3))),
				),
			),
		},
		// Single-key DNF whose second group carries a NOT_EQUALS: the range
		// builder REFUSES it (canBeUsedInScanPrefix,
		// RangeConstraints.java:754-784 — NOT_EQUALS cannot bound a scan
		// prefix), so despite every group sharing one key the expansion
		// fails and the conjunction is stored. Removing the admission gate
		// makes the expansion succeed, stores the DNF, and turns this golden
		// red — the gate's own mutation direction, distinct from
		// dnf_no_ranges_fallback's mixed-keys direction.
		{
			"gate_fallback_not_equals",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE price = 1 AND (price = 2 OR price <> 3)",
			predAnd(
				predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(1))),
				predOr(
					predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_EQUALS, valLong(2))),
					predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_NOT_EQUALS, valLong(3))),
				),
			),
		},
		// Boolean constants as the whole WHERE — corpus twin:
		// boolean-ddl.yamsql idx_true / idx_false / idx_null. Java boxes them
		// to the ConstantPredicate singletons (RelOpValue's
		// tryBoxSelfAsConstantPredicate / Expression.Utils.toUnderlyingPredicate)
		// and the PoJo stores TRUE / FALSE / NULL (IndexPredicate.java:455-475).
		{
			"where_true",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE TRUE",
			predConst(gen.ConstantPredicate_TRUE),
		},
		{
			"where_false",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE FALSE",
			predConst(gen.ConstantPredicate_FALSE),
		},
		{
			"where_null",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE NULL",
			predConst(gen.ConstantPredicate_NULL),
		},
		// A sparse AGGREGATE index: the predicate is attached before the
		// value/aggregate split (MaterializedViewIndexGenerator.java:169-174),
		// so a WHERE on a grouped AS-SELECT stores the same predicate proto.
		{
			"aggregate_sparse",
			"CREATE INDEX pidx AS SELECT stock, SUM(price) FROM tp WHERE price > 0 GROUP BY stock",
			predVP([]string{"PRICE"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valLong(0))),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, err := buildPredicateIndex(t, tc.ddl)
			if err != nil {
				t.Fatalf("DDL failed: %v", err)
			}
			got := idx.GetPredicateProto()
			if got == nil {
				t.Fatalf("no predicate stored — the WHERE was dropped (the exact silent full-index build S0 failed closed against)")
			}
			if !proto.Equal(got, tc.want) {
				t.Errorf("predicate proto mismatch\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

// TestAsSelectSparseIndex_FullIndexHasNoPredicate pins the nil arm: a
// WHERE-less definition stores no predicate at all (Java: getTopLevelPredicate
// returns null, setPredicate never runs — MaterializedViewIndexGenerator.java:170).
func TestAsSelectSparseIndex_FullIndexHasNoPredicate(t *testing.T) {
	t.Parallel()
	idx, err := buildPredicateIndex(t, "CREATE INDEX pidx AS SELECT price FROM tp")
	if err != nil {
		t.Fatalf("DDL failed: %v", err)
	}
	if got := idx.GetPredicateProto(); got != nil {
		t.Errorf("full index unexpectedly stored a predicate: %v", got)
	}
}

// TestAsSelectSparseIndex_Rejections pins the fail-closed shapes: predicates
// the IndexPredicate PoJo hierarchy cannot represent are refused, never
// silently dropped (isSupported, IndexPredicate.java:212-232; the
// select-having assert, MaterializedViewIndexGenerator.java:651).
func TestAsSelectSparseIndex_Rejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		ddl     string
		wantMsg string
	}{
		// IN has no IndexComparison PoJo form (Java's ListComparison is not a
		// SimpleComparison — IndexComparison.java:86-96).
		{
			"in_list",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE price IN (1, 2)",
			"Unsupported predicate",
		},
		// LIKE cannot be stored by the SimpleComparison serializer
		// (IndexComparison.java:221-249 has no LIKE arm).
		{
			"like",
			"CREATE INDEX pidx AS SELECT name FROM tp WHERE name LIKE 'x%'",
			"Unsupported predicate",
		},
		// A literal STRICTLY WIDER than its column is Java's promoted-COLUMN
		// shape: the planner wraps the column in promote(col AS LONG), a
		// PromoteValue, and IndexPredicate.isSupported requires a bare
		// FieldValue (IndexPredicate.java:227) — MEASURED against the live
		// JVM (D11's rejects-promoted-column spec): Java fails the CREATE
		// with "Unsupported predicate 'promote(…I AS LONG) LESS_THAN
		// 5000000000'".
		{
			"promoted_column_int_wide_literal",
			"CREATE INDEX pidx AS SELECT qty FROM tp WHERE qty < 5000000000 ORDER BY qty",
			"Unsupported predicate",
		},
		// A field-to-field comparison has no literal comparand — Java's
		// ValueComparison over a FieldValue fails isSupported
		// (IndexComparison.java:89-95: FieldValue is not RangeMatchable).
		{
			"field_rhs",
			"CREATE INDEX pidx AS SELECT price FROM tp WHERE price = stock",
			"Unsupported predicate",
		},
		// HAVING on the grouped form is Java's select-having predicate
		// (MaterializedViewIndexGenerator.java:651).
		{
			"having",
			"CREATE INDEX pidx AS SELECT stock, SUM(price) FROM tp GROUP BY stock HAVING SUM(price) > 10",
			"select-having",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildPredicateIndex(t, tc.ddl)
			if err == nil {
				t.Fatalf("DDL unexpectedly succeeded for %q", tc.ddl)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
