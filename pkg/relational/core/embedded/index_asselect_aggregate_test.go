package embedded

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// RFC-202 S3 goldens: the aggregate arm of the AS-SELECT index generator,
// asserted at the KeyExpression-proto level against the shape Java's
// MaterializedViewIndexGenerator emits for the same declaration (tag
// 4.12.11.0). Each case cites the IndexTest.java case or corpus witness it
// was ported from.

const asSelectAggGoldenDDL = `
	CREATE TABLE t1(p1 bigint, a1 bigint, a2 bigint, a3 bigint, b bigint, c bigint, primary key(p1))
	CREATE TABLE t3(p1 bigint, s string, g bigint, primary key(p1))
`

func buildAggTemplateWithIndex(t *testing.T, indexDDL string) (*recordlayer.Index, error) {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(asSelectAggGoldenDDL + "\n" + indexDDL)
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

func TestAsSelectAggregateIndex_KeyExpressionGoldens(t *testing.T) {
	t.Parallel()

	f := recordlayer.Field
	concat := recordlayer.Concat
	fn := recordlayer.FunctionExpr
	lit := recordlayer.Literal

	cases := []struct {
		name    string
		ddl     string
		want    recordlayer.KeyExpression
		typ     string
		options map[string]string
	}{
		// IndexTest.java:1097-1106 createCountStarIndex:
		// GroupingKeyExpression(field(COL1), 0) — the whole key IS the
		// grouping, with NO Empty child in the concat. The deleted legacy arm
		// emitted GroupBy(EmptyKey(), cols...), whose proto carried an Empty
		// child Java never stores — this golden pins the byte fix.
		{
			"count_star_grouped", "CREATE INDEX gidx AS SELECT COUNT(*) FROM t1 GROUP BY a1",
			recordlayer.GroupAll(f("A1")), recordlayer.IndexTypeCount, nil,
		},
		// corpus composite-aggregates.yamsql t2_i1: ungrouped COUNT(*) —
		// GroupingKeyExpression(EMPTY, 0).
		{
			"count_star_ungrouped", "CREATE INDEX gidx AS SELECT COUNT(*) FROM t1",
			recordlayer.GroupAll(recordlayer.EmptyKey()), recordlayer.IndexTypeCount, nil,
		},
		// IndexTest.java:1108-1117 createCountCol: COUNT(col) is
		// COUNT_NOT_NULL through the generic arm — field(COL1).groupBy(COL1).
		{
			"count_col", "CREATE INDEX gidx AS SELECT COUNT(a1) FROM t1 GROUP BY a1",
			recordlayer.GroupBy(f("A1"), f("A1")), recordlayer.IndexTypeCountNotNull, nil,
		},
		// corpus composite-aggregates.yamsql t1_i1: COUNT(col) over a
		// multi-column grouping.
		{
			"count_col_multi_group", "CREATE INDEX gidx AS SELECT COUNT(a1) FROM t1 GROUP BY a2, a3",
			recordlayer.GroupBy(f("A1"), f("A2"), f("A3")), recordlayer.IndexTypeCountNotNull, nil,
		},
		// createIndexAsSelectWithGroupByWorks family: SUM over one grouping
		// column.
		{
			"sum_grouped", "CREATE INDEX gidx AS SELECT SUM(a1) FROM t1 GROUP BY a2",
			recordlayer.GroupBy(f("A1"), f("A2")), recordlayer.IndexTypeSum, nil,
		},
		// corpus composite-aggregates.yamsql t2_i5: ungrouped SUM.
		{
			"sum_ungrouped", "CREATE INDEX gidx AS SELECT SUM(a1) FROM t1",
			recordlayer.Ungrouped(f("A1")), recordlayer.IndexTypeSum, nil,
		},
		// corpus composite-aggregates.yamsql t1_i2 (and the RFC-202 §8(a)
		// mutation-14 grouping-reconciliation witness): the grouping column
		// projected beside the aggregate.
		{
			"sum_with_projected_grouping", "CREATE INDEX gidx AS SELECT a1, SUM(b) FROM t1 GROUP BY a1",
			recordlayer.GroupBy(f("B"), f("A1")), recordlayer.IndexTypeSum, nil,
		},
		// IndexTest.java:956-... createAggregateIndexOnMinMax: plain MIN/MAX
		// are the PERMUTED types with permutedSize 0.
		{
			"min_grouped", "CREATE INDEX gidx AS SELECT MIN(a2) FROM t1 GROUP BY a1",
			recordlayer.GroupBy(f("A2"), f("A1")), recordlayer.IndexTypePermutedMin,
			map[string]string{recordlayer.IndexOptionPermutedSize: "0"},
		},
		// createAggregateIndexOnMinMaxWithGroupingOrderingIncludingMax: the
		// aggregate LAST in the ORDER BY leaves permutedSize at
		// len(grouping) - aggIndex = 1 - 1 = 0.
		{
			"max_order_by_grouping_and_agg",
			"CREATE INDEX gidx AS SELECT a1, MAX(a2) FROM t1 GROUP BY a1 ORDER BY a1, MAX(a2)",
			recordlayer.GroupBy(f("A2"), f("A1")), recordlayer.IndexTypePermutedMax,
			map[string]string{recordlayer.IndexOptionPermutedSize: "0"},
		},
		// createAggregateIndexOnMinMaxWithPermutedOrdering: the aggregate
		// BEFORE the last grouping column → permutedSize = 3 - 2 = 1.
		{
			"max_permuted_ordering",
			"CREATE INDEX gidx AS SELECT a1, a2, a3, MAX(b) FROM t1 GROUP BY a1, a2, a3 ORDER BY a1, a2, MAX(b), a3",
			recordlayer.GroupBy(f("B"), f("A1"), f("A2"), f("A3")), recordlayer.IndexTypePermutedMax,
			map[string]string{recordlayer.IndexOptionPermutedSize: "1"},
		},
		// corpus composite-aggregates.yamsql t3_i1 twin (MIN variant t3_i2):
		// permuted ordering with the grouping split around the aggregate.
		{
			"min_permuted_middle",
			"CREATE INDEX gidx AS SELECT a1, MIN(a2), a3 FROM t1 GROUP BY a1, a3 ORDER BY a1, MIN(a2), a3",
			recordlayer.GroupBy(f("A2"), f("A1"), f("A3")), recordlayer.IndexTypePermutedMin,
			map[string]string{recordlayer.IndexOptionPermutedSize: "1"},
		},
		// IndexTest.java:634-641 createAggregateIndexWithComplexGroupingExpressionCase1:
		// arithmetic grouping expressions become function components of the
		// grouping concat — flattened, never a nested Then.
		{
			"complex_grouping_expressions",
			"CREATE INDEX gidx AS SELECT a1 & 2, b + 3, MAX(b) FROM t1 GROUP BY a1 & 2, b + 3",
			recordlayer.GroupBy(f("B"),
				fn("bitand", concat(f("A1"), lit(int64(2)))),
				fn("add", concat(f("B"), lit(int64(3))))),
			recordlayer.IndexTypePermutedMax,
			map[string]string{recordlayer.IndexOptionPermutedSize: "0"},
		},
		// IndexTest.java:1119-1138 createMinEverLong/createMaxEverLong (the
		// names are historic; without the attribute both build the TUPLE
		// variant).
		{
			"min_ever_tuple", "CREATE INDEX gidx AS SELECT MIN_EVER(a1) FROM t1 GROUP BY a2",
			recordlayer.GroupBy(f("A1"), f("A2")), recordlayer.IndexTypeMinEverTuple, nil,
		},
		{
			"max_ever_tuple", "CREATE INDEX gidx AS SELECT MAX_EVER(a1) FROM t1 GROUP BY a2",
			recordlayer.GroupBy(f("A1"), f("A2")), recordlayer.IndexTypeMaxEverTuple, nil,
		},
		// IndexTest.java:1142-1149 createMaxEverTupleIncorrectType: a STRING
		// operand is fine for the tuple variant.
		{
			"max_ever_tuple_string", "CREATE INDEX gidx AS SELECT MAX_EVER(s) FROM t3 GROUP BY g",
			recordlayer.GroupBy(f("S"), f("G")), recordlayer.IndexTypeMaxEverTuple, nil,
		},
		// WITH ATTRIBUTES LEGACY_EXTREMUM_EVER selects the LONG-based
		// maintainer (MaterializedViewIndexGenerator.java:449-465) — the
		// RFC-202 §8(a) mutation-14 index-TYPE witness (IDX_MV_AGE_MIN_EXTREMUM:
		// min_ever_long, not min_ever_tuple).
		{
			"min_ever_legacy_long",
			"CREATE INDEX gidx AS SELECT MIN_EVER(a1) FROM t1 GROUP BY a2 WITH ATTRIBUTES LEGACY_EXTREMUM_EVER",
			recordlayer.GroupBy(f("A1"), f("A2")), recordlayer.IndexTypeMinEverLong, nil,
		},
		{
			"max_ever_legacy_long",
			"CREATE INDEX gidx AS SELECT MAX_EVER(a1) FROM t1 GROUP BY a2 WITH ATTRIBUTES LEGACY_EXTREMUM_EVER",
			recordlayer.GroupBy(f("A1"), f("A2")), recordlayer.IndexTypeMaxEverLong, nil,
		},
		// IndexTest.java:567-576 createBitMapIndexIsSupported: the raw column
		// is the grouped value and the trailing bitmap_bucket_offset grouping
		// element is REMOVED — field(P1).groupBy(concat(A, B)).
		{
			"bitmap_grouped",
			"CREATE INDEX gidx AS SELECT bitmap_construct_agg(bitmap_bit_position(a1)) as bm, a2, b, bitmap_bucket_offset(a1) as os FROM t1 GROUP BY a2, b, bitmap_bucket_offset(a1)",
			recordlayer.GroupBy(f("A1"), f("A2"), f("B")), recordlayer.IndexTypeBitmapValue, nil,
		},
		// IndexTest.java:578-586 createBitMapIndexWithEmptyGroupIsSupported:
		// grouping was ONLY the bucket offset → ungrouped.
		{
			"bitmap_empty_group",
			"CREATE INDEX gidx AS SELECT bitmap_construct_agg(bitmap_bit_position(a1)) as bm, bitmap_bucket_offset(a1) as os FROM t1 GROUP BY bitmap_bucket_offset(a1)",
			recordlayer.Ungrouped(f("A1")), recordlayer.IndexTypeBitmapValue, nil,
		},
		// corpus bitmap-aggregate-index.yamsql agg_index_2: bitmap_bucket_offset
		// as a plain VALUE-index projection — an ArithmeticValue leaf
		// (MaterializedViewIndexGenerator.java:567-575) with the
		// walker-injected 10000 entry-size literal as its second argument.
		{
			"bitmap_bucket_offset_value_index",
			"CREATE INDEX gidx AS SELECT a1, bitmap_bucket_offset(a2) FROM t1 ORDER BY a1, bitmap_bucket_offset(a2)",
			concat(f("A1"), fn("bitmap_bucket_offset", concat(f("A2"), lit(int64(10000))))),
			recordlayer.IndexTypeValue, nil,
		},
		// GROUP BY with no aggregate at all: Java's aggregateValues comes out
		// empty and the VALUE arm runs over the grouping values (generate
		// :187) — a single-column value index.
		{
			"group_by_without_aggregate", "CREATE INDEX gidx AS SELECT a1 FROM t1 GROUP BY a1",
			f("A1"), recordlayer.IndexTypeValue, nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, err := buildAggTemplateWithIndex(t, tc.ddl)
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
			for k, v := range tc.options {
				if got := idx.Options[k]; got != v {
					t.Errorf("option %q = %q, want %q (options=%v)", k, got, v, idx.Options)
				}
			}
		})
	}
}

func TestAsSelectAggregateIndex_Rejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		ddl     string
		code    api.ErrorCode
		message string
	}{
		// IndexTest.java:774-780
		// createAggregateIndexWithGroupByContainingMoreThanOneAggregation.
		{
			"multiple_aggregations",
			"CREATE INDEX gidx AS SELECT SUM(a1), COUNT(a1) FROM t1 GROUP BY a2",
			api.ErrCodeUnsupportedOperation,
			"found group by expression with more than one aggregation",
		},
		// IndexTest.java createAggregateIndexOnMinMaxWithGroupingColumnsMissingInOrdering:
		// a grouping column skipped in the ORDER BY.
		{
			"covering_aggregate_skipped_grouping",
			"CREATE INDEX gidx AS SELECT a1, a2, a3, MAX(b) FROM t1 GROUP BY a1, a2, a3 ORDER BY a1, MAX(b), a3",
			api.ErrCodeUnsupportedOperation,
			"attempt to create a covering aggregate index",
		},
		// createAggregateIndexOnMinMaxWithFinalGroupingColumnsMissingInOrdering:
		// the ORDER BY stops before consuming every grouping column.
		{
			"covering_aggregate_truncated_ordering",
			"CREATE INDEX gidx AS SELECT a1, a2, a3, MAX(b) FROM t1 GROUP BY a1, a2, a3 ORDER BY a1, a2, MAX(b)",
			api.ErrCodeUnsupportedOperation,
			"attempt to create a covering aggregate index",
		},
		// createAggregateIndexOnMinMaxWithGroupingColumnsMissingInResultColumn:
		// ORDER BY references a grouping column the SELECT does not project.
		{
			"order_by_unprojected_grouping",
			"CREATE INDEX gidx AS SELECT a1, a3, MAX(b) FROM t1 GROUP BY a1, a2, a3 ORDER BY a1, a2, MAX(b), a3",
			api.ErrCodeInvalidColumnReference,
			"not present in the projection list",
		},
		// createAggregateIndexWithGroupingColumnsNotMatchingResultOrder.
		{
			"grouping_result_order_mismatch",
			"CREATE INDEX gidx AS SELECT a1, a3, a2, MAX(b) FROM t1 GROUP BY a1, a2, a3",
			api.ErrCodeUnsupportedOperation,
			"Aggregate result value does not align with grouping value",
		},
		// createAggregateIndexWithGroupingColumnMissingInResults.
		{
			"grouping_missing_from_result",
			"CREATE INDEX gidx AS SELECT a1, a2, MAX(b) FROM t1 GROUP BY a1, a2, a3",
			api.ErrCodeUnsupportedOperation,
			"Grouping value absent from aggregate result value",
		},
		// Java :241-243: only the permuted types may be ordered by the
		// aggregate value.
		{
			"sum_ordered_by_aggregate",
			"CREATE INDEX gidx AS SELECT a1, SUM(b) FROM t1 GROUP BY a1 ORDER BY a1, SUM(b)",
			api.ErrCodeUnsupportedOperation,
			"Cannot order sum index by aggregate value",
		},
		// AVG is streamable but not indexable
		// (MaterializedViewIndexGenerator.java:176-178).
		{
			"avg_not_indexable",
			"CREATE INDEX gidx AS SELECT AVG(a1) FROM t1 GROUP BY a2",
			api.ErrCodeUnsupportedOperation,
			"non-indexable aggregation",
		},
		// IndexTest.java:1152-1176 createMaxEverLongIncorrectType: the
		// LONG-based extremum maintainer requires a numeric operand.
		{
			"legacy_extremum_non_numeric",
			"CREATE INDEX gidx AS SELECT MAX_EVER(s) FROM t3 GROUP BY g WITH ATTRIBUTES LEGACY_EXTREMUM_EVER",
			api.ErrCodeInternalError,
			"only numeric types allowed in max_ever_long aggregation operation",
		},
		// Java :414-420: bitmap_construct_agg over a bare column (without
		// bitmap_bit_position) is refused.
		{
			"bitmap_without_bit_position",
			"CREATE INDEX gidx AS SELECT bitmap_construct_agg(a1) as bm, bitmap_bucket_offset(a1) as os FROM t1 GROUP BY bitmap_bucket_offset(a1)",
			api.ErrCodeUnsupportedOperation,
			"expecting a bitmap_bit_position function in bitmap_construct_agg function",
		},
		// Java :480-483: the bucket offset must be the LAST grouping element.
		{
			"bitmap_bucket_offset_not_last",
			"CREATE INDEX gidx AS SELECT bitmap_construct_agg(bitmap_bit_position(a1)) as bm, bitmap_bucket_offset(a1) as os, a2 FROM t1 GROUP BY bitmap_bucket_offset(a1), a2",
			api.ErrCodeUnsupportedOperation,
			"expecting the last element in group by to be a bitmap_bucket_offset function",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildAggTemplateWithIndex(t, tc.ddl)
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
