package embedded

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// RFC-202 S2 goldens: the AS-SELECT value-index generator's key expressions,
// asserted byte-identical (at the KeyExpression proto level) to the form
// Java's MaterializedViewIndexGenerator emits for the same declaration. Each
// case cites the Java test or generator rule it was ported from
// (fdb-record-layer @ 4.12.11.0).

const asSelectGoldenDDL = `
	CREATE TABLE t1(p1 bigint, a1 bigint, a2 bigint, a3 bigint, b bigint, c bigint, primary key(p1))
	CREATE TABLE t2(p1 bigint, arr integer array not null, primary key(p1))
`

// buildTemplateWithIndex compiles the golden DDL plus one CREATE INDEX
// clause and returns the built index.
func buildTemplateWithIndex(t *testing.T, indexDDL string) (*recordlayer.Index, error) {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(asSelectGoldenDDL + "\n" + indexDDL)
	if err != nil {
		return nil, err
	}
	for _, idx := range tmpl.Underlying().GetAllIndexes() {
		if idx.Name == "GIDX" {
			return idx, nil
		}
	}
	t.Fatalf("index GIDX not found after %q", indexDDL)
	return nil, nil
}

func TestAsSelectValueIndex_KeyExpressionGoldens(t *testing.T) {
	t.Parallel()

	f := recordlayer.Field
	concat := recordlayer.Concat
	fn := recordlayer.FunctionExpr
	lit := recordlayer.Literal

	cases := []struct {
		name string
		ddl  string
		want recordlayer.KeyExpression
		typ  string
	}{
		// IndexTest.java:663-672 createSimpleValueIndex: single column, no
		// ORDER BY → bare field, splitPoint -1 (generator :196-198).
		{
			"single_no_order", "CREATE INDEX gidx AS SELECT a1 FROM t1",
			f("A1"), recordlayer.IndexTypeValue,
		},
		// IndexTest.java createSimpleValueIndexOnTwoCols: ORDER BY == SELECT.
		{
			"two_cols_in_order", "CREATE INDEX gidx AS SELECT a1, a2 FROM t1 ORDER BY a1, a2",
			concat(f("A1"), f("A2")), recordlayer.IndexTypeValue,
		},
		// IndexTest.java:673-681 createSimpleValueIndexOnTwoColsReverse: the
		// ORDER BY fixes the key order, not the SELECT (reorderValues,
		// generator :387-394).
		{
			"two_cols_reversed", "CREATE INDEX gidx AS SELECT a1, a2 FROM t1 ORDER BY a2, a1",
			concat(f("A2"), f("A1")), recordlayer.IndexTypeValue,
		},
		// IndexTest.java:683-692 createCoveringValueIndex: splitPoint =
		// len(orderBy) strictly inside the projection → keyWithValue
		// (generator :195, :199-200).
		{
			"covering_split", "CREATE INDEX gidx AS SELECT a1, a2, a3 FROM t1 ORDER BY a1, a2",
			recordlayer.KeyWithValue(concat(f("A1"), f("A2"), f("A3")), 2), recordlayer.IndexTypeValue,
		},
		// Single column WITH ORDER BY: splitPoint == len(fieldValues) → bare
		// expression, no keyWithValue (generator :199-203).
		{
			"single_with_order", "CREATE INDEX gidx AS SELECT a1 FROM t1 ORDER BY a1",
			f("A1"), recordlayer.IndexTypeValue,
		},
		// IndexTest.java:522-530 createIndexWithConstantArithmethicInProjection:
		// literal leaves become value() expressions (generator :576-577).
		{
			"constant_arithmetic", "CREATE INDEX gidx AS SELECT 5+1 FROM t1",
			fn("add", concat(lit(int64(5)), lit(int64(1)))), recordlayer.IndexTypeValue,
		},
		// IndexTest.java createIndexWithFieldSumInProjection: arithmetic over
		// fields → function(<op>, concat(args)) (generator :567-575).
		{
			"field_sum", "CREATE INDEX gidx AS SELECT a1 + a2 FROM t1",
			fn("add", concat(f("A1"), f("A2"))), recordlayer.IndexTypeValue,
		},
		// IndexTest.java createIndexWithBitMaskInProjection: bit operators
		// lowercased (bitand), literal operand.
		{
			"bit_mask", "CREATE INDEX gidx AS SELECT a1 & 4 FROM t1",
			fn("bitand", concat(f("A1"), lit(int64(4)))), recordlayer.IndexTypeValue,
		},
		// IndexTest.java:597-613 createIndexWithMultipleFunctionsInProjection
		// (subset): each function is its own component of the concat.
		{
			"multiple_functions",
			"CREATE INDEX gidx AS SELECT b + c, b - c, b * c, b / c, b % c FROM t1 ORDER BY b + c, b - c, b * c, b / c, b % c",
			concat(
				fn("add", concat(f("B"), f("C"))),
				fn("sub", concat(f("B"), f("C"))),
				fn("mul", concat(f("B"), f("C"))),
				fn("div", concat(f("B"), f("C"))),
				fn("mod", concat(f("B"), f("C"))),
			), recordlayer.IndexTypeValue,
		},
		// IndexTest.java:614-631 createIndexWithSomeFunctionsOnlyCovering:
		// ORDER BY reorders the functions and sets the split at 2.
		{
			"functions_covering",
			"CREATE INDEX gidx AS SELECT b + c, b - c, b * c FROM t1 ORDER BY b * c, b - c",
			recordlayer.KeyWithValue(concat(
				fn("mul", concat(f("B"), f("C"))),
				fn("sub", concat(f("B"), f("C"))),
				fn("add", concat(f("B"), f("C"))),
			), 2), recordlayer.IndexTypeValue,
		},
		// IndexTest.java:534-541 createIndexWithCardinalityFunctionOnNonNullableArray:
		// the array is accessed with Concatenate under CARDINALITY
		// (generator :555-566).
		{
			"cardinality_non_nullable", "CREATE INDEX gidx AS SELECT CARDINALITY(arr) FROM t2",
			recordlayer.CardinalityExpr(recordlayer.FieldConcatenate("ARR")), recordlayer.IndexTypeValue,
		},
		// Ordering wrappers (generator :347-363; corpus witnesses
		// IDX_PRICE_DESC / IDX_MV_RATING_NULLS_LAST / IDX_MV_DESC_NULLS_FIRST
		// / IDX_MV_NULLS_FIRST in index-ddl*.yamsql):
		// plain DESC defaults to NULLS LAST.
		{
			"desc_default", "CREATE INDEX gidx AS SELECT a1 FROM t1 ORDER BY a1 DESC",
			fn(recordlayer.OrderFuncDescNullsLast, f("A1")), recordlayer.IndexTypeValue,
		},
		// DESC NULLS FIRST.
		{
			"desc_nulls_first", "CREATE INDEX gidx AS SELECT a1 FROM t1 ORDER BY a1 DESC NULLS FIRST",
			fn(recordlayer.OrderFuncDescNullsFirst, f("A1")), recordlayer.IndexTypeValue,
		},
		// ASC NULLS LAST.
		{
			"asc_nulls_last", "CREATE INDEX gidx AS SELECT a1 FROM t1 ORDER BY a1 ASC NULLS LAST",
			fn(recordlayer.OrderFuncAscNullsLast, f("A1")), recordlayer.IndexTypeValue,
		},
		// ASC NULLS FIRST is the plain ascending encoding — NO wrapper
		// (generator :348-350; RFC-202 gate (a) mutation 4).
		{
			"asc_nulls_first_no_wrapper", "CREATE INDEX gidx AS SELECT a1 FROM t1 ORDER BY a1 ASC NULLS FIRST",
			f("A1"), recordlayer.IndexTypeValue,
		},
		// Mixed per-column direction: the wrapper applies at the LEAF of the
		// ordered column only (generator :598-600; corpus IDX_MV_ASC_DESC).
		{
			"mixed_asc_desc", "CREATE INDEX gidx AS SELECT a1, a2 FROM t1 ORDER BY a1, a2 DESC",
			concat(f("A1"), fn(recordlayer.OrderFuncDescNullsLast, f("A2"))), recordlayer.IndexTypeValue,
		},
		// A field RUN followed by a non-field component: the run compresses
		// into one trie, and the top-level concat FLATTENS it — Java's
		// ThenKeyExpression constructor flattens nested thens
		// (ThenKeyExpression.java:264-270), so the proto is a single flat
		// Then, never Then(Then(A1,A2), fn).
		{
			"field_run_then_function_flat",
			"CREATE INDEX gidx AS SELECT a1, a2, b + c FROM t1 ORDER BY a1, a2, b + c",
			concat(f("A1"), f("A2"), fn("add", concat(f("B"), f("C")))),
			recordlayer.IndexTypeValue,
		},
		// SELECT *: the star expands to every column in declaration order;
		// ORDER BY fixes the prefix (corpus:
		// include-block/shouldPass/simple-include-different-env.yamsql, minus
		// its WHERE which is the S5 predicate arm).
		{
			"select_star", "CREATE INDEX gidx AS SELECT * FROM t1 ORDER BY p1",
			recordlayer.KeyWithValue(concat(f("P1"), f("A1"), f("A2"), f("A3"), f("B"), f("C")), 1),
			recordlayer.IndexTypeValue,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, err := buildTemplateWithIndex(t, tc.ddl)
			if err != nil {
				t.Fatalf("DDL failed: %v", err)
			}
			want := tc.want.ToKeyExpression()
			got := idx.RootExpression.ToKeyExpression()
			if !proto.Equal(want, got) {
				t.Errorf("key expression mismatch for %q\nwant: %v\ngot:  %v", tc.ddl, want, got)
			}
			if idx.Type != tc.typ {
				t.Errorf("index type = %q, want %q", idx.Type, tc.typ)
			}
		})
	}
}

func TestAsSelectValueIndex_Rejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		ddl     string
		code    api.ErrorCode
		message string
	}{
		// IndexTest.java:694-700 createIndexWithoutTopOrder.
		{
			"multi_column_needs_order_by",
			"CREATE INDEX gidx AS SELECT a1, a2 FROM t1",
			api.ErrCodeUnsupportedOperation,
			"value indexes must have an order by clause at the top level",
		},
		// IndexTest.java:710-716 createIndexOrderByUnprojectedColumn —
		// INVALID_COLUMN_REFERENCE (42F10). RFC-202 D3: the value-identity
		// subset test, never the plan's shape.
		{
			"order_by_unprojected",
			"CREATE INDEX gidx AS SELECT a1 FROM t1 ORDER BY a2",
			api.ErrCodeInvalidColumnReference,
			"not present in the projection list",
		},
		// IndexTest.java:702-708 createIndexOrderByUnknownColumns —
		// UNDEFINED_COLUMN via validateTablesAndColumns (RFC-202 D4 mutation 12).
		{
			"order_by_unknown_column",
			"CREATE INDEX gidx AS SELECT a1, a2 FROM t1 ORDER BY a4",
			api.ErrCodeUndefinedColumn, "",
		},
		// SELECT over an unknown column — same post-pass.
		{
			"select_unknown_column",
			"CREATE INDEX gidx AS SELECT nonexistent_col FROM t1",
			api.ErrCodeUndefinedColumn, "",
		},
		// IndexTest.java:511-520 createIndexWithJoiningMoreThanOneTable: any
		// multi-source FROM fails record-type resolution
		// (generator :791-801).
		{
			"join_rejected",
			"CREATE INDEX gidx AS SELECT * FROM t1, t2 ORDER BY t1.p1",
			"", "expected to find exactly one type filter operator",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildTemplateWithIndex(t, tc.ddl)
			if err == nil {
				t.Fatalf("DDL unexpectedly succeeded: %q", tc.ddl)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not an *api.Error: %v", err)
			}
			if tc.code != "" && apiErr.Code != tc.code {
				t.Errorf("error code = %s, want %s (%v)", apiErr.Code, tc.code, err)
			}
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.message)
			}
		})
	}
}

// TestAsSelectValueIndex_Unique pins that UNIQUE on the AS-SELECT form lands
// in the index options — where uniqueness lives in the stored proto
// (IndexOptions.UNIQUE_OPTION; Java DdlVisitor.java:215 → generate(…,
// isUnique, …)). This retires the S0 fail-closed rejection for the value arm.
func TestAsSelectValueIndex_Unique(t *testing.T) {
	t.Parallel()
	idx, err := buildTemplateWithIndex(t,
		"CREATE UNIQUE INDEX gidx AS SELECT a1 FROM t1 ORDER BY a1")
	if err != nil {
		t.Fatalf("DDL failed: %v", err)
	}
	if !idx.IsUnique() {
		t.Error("UNIQUE dropped: index is not unique")
	}
	if idx.Options[recordlayer.IndexOptionUnique] != "true" {
		t.Errorf("unique option missing from options map: %v", idx.Options)
	}
}
