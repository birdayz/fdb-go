package embedded

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// The index generator's arms over the translated graph: IndexSpec's walk
// (predicate ownership, the skipped existential leg, the single type filter),
// QuantifierValues' scoped resolution (a derived table's projection, an unnest
// leg's element, two unnests of one array) and the front-end refusal of a
// subquery ORDER BY. Each case pins the graph the translator hands the
// generator — so a translator change that stops producing the shape an arm
// handles fails here instead of leaving the arm silently unexercised — and the
// generator's answer on it. Every shape is also a row of the WS-J index
// oracle (conformance/ws_j_index_fidelity_conformance_test.go), which compares
// these answers with Java 4.14.2.0's; the Java class of each is noted.

const resolverArmsDDL = `
	CREATE TABLE t(id bigint, a bigint, b bigint, primary key(id))
	CREATE TABLE u(id bigint, primary key(id))
	CREATE TYPE AS STRUCT it(k string)
	CREATE TABLE t6(id bigint, a bigint, c it array, primary key(id))
	CREATE TABLE t4(id bigint, col2 bigint, col4 bigint array, primary key(id))
`

func TestIndexGenerator_ResolverArms(t *testing.T) {
	t.Parallel()
	f := recordlayer.Field
	concat := recordlayer.Concat
	bGreaterThan1 := predVP([]string{"B"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valLong(1)))
	col4Values := recordlayer.Nest("COL4", recordlayer.FanOut("values"))

	cases := []struct {
		name  string
		index string
		graph string
		// An accepted definition's key and stored predicate (nil: none).
		key  recordlayer.KeyExpression
		pred *gen.Predicate
		// A refused one's code and message.
		code api.ErrorCode
		msg  string
	}{
		{
			// The filter owns the predicate (IndexSpec.java:387-402).
			name:  "where",
			index: `create index ix as select a from t where b > 1 order by a`,
			graph: "Select(Sort(Filter(FullUnorderedScan)))",
			key:   f("A"), pred: bGreaterThan1,
		},
		{
			// The derived table's filter owns the predicate, the derived
			// table's projection resolves d.a to the scanned A, and the stored
			// predicate is the plain WHERE's. Java: equal.
			name:  "derived_with_predicate",
			index: `create index ix as select d.a from (select a from t where b > 1) as d order by d.a`,
			graph: "Select(Sort(Select(FullUnorderedScan)))",
			key:   f("A"), pred: bGreaterThan1,
		},
		{
			// A filter above an owner. Java: the same refusal.
			name:  "derived_and_outer_predicate",
			index: `create index ix as select d.a from (select a, b from t where b > 1) as d where d.a > 2 order by d.a`,
			graph: "Select(Sort(Filter(Select(FullUnorderedScan))))",
			code:  api.ErrCodeUnsupportedOperation, msg: "Unsupported index definition, found predicate in inner-select",
		},
		{
			// A filter over the group by. Java: the same refusal.
			name:  "having",
			index: `create index ix as select a, count(*) from t group by a having count(*) > 1`,
			graph: "Select(Filter(GroupBy(FullUnorderedScan)))",
			code:  api.ErrCodeUnsupportedOperation, msg: "Unsupported index definition, found predicate in select-having",
		},
		{
			// A derived table's ORDER BY is refused before any plan check, as
			// Java's visitor refuses it below the top level. Java: the same.
			name:  "derived_sorted",
			index: `create index ix as select d.a from (select a from t order by a) as d order by d.a`,
			graph: "Select(Sort(Select(Sort(FullUnorderedScan))))",
			code:  api.ErrCodeUnsupportedOperation, msg: "order by is not supported in subquery",
		},
		{
			// The existential leg is not walked (Java's DDL never adds the
			// EXISTS operator), so the one scan is the one type filter and the
			// refusal is the EXISTS predicate's. Java: the same code, message
			// "Unsupported predicate '<alias> NOT_NULL'".
			name:  "exists_uncorrelated",
			index: `create index ix as select a from t where exists (select 1 from u) order by a`,
			graph: "Select(Sort(Select(FullUnorderedScan, E:Select(FullUnorderedScan))))",
			code:  api.ErrCodeUnsupportedOperation, msg: "Unsupported predicate '",
		},
		{
			// Two scans under a join are two type filters. Java: the same.
			name:  "join",
			index: `create index ix as select t.a from t, u where t.id = u.id order by t.a`,
			graph: "Select(Sort(Select(FullUnorderedScan, FullUnorderedScan)))",
			code:  api.ErrCodeUnsupportedOperation, msg: "Unsupported query, expected to find exactly one type filter operator",
		},
		{
			// The ORDER BY key must be a projected column (DdlVisitor.java:274).
			// Java: the same.
			name:  "order_key_not_projected",
			index: `create index ix as select a from t order by b`,
			graph: "Select(Sort(FullUnorderedScan))",
			code:  api.ErrCodeInvalidColumnReference,
			msg:   "Cannot create index and order by an expression that is not present in the projection list",
		},
		{
			// An array column reached without an unnest carries no marker.
			// Java: the same refusal.
			name:  "array_without_unnest",
			index: `create index ix as select col4 from t4 order by col4`,
			graph: "Select(Sort(FullUnorderedScan))",
			code:  api.ErrCodeUnsupportedOperation,
			msg:   "Unsupported index definition, cannot create index on array field 'COL4' without unnesting",
		},
		{
			// The unnest leg's element is the array's, reached through the
			// nullable array's wrapper. Java: equal.
			name:  "unnest_comma",
			index: `create index ix as select t.col2, "e" from t4 as t, t.col4 as "e" order by t.col2, "e"`,
			graph: "Select(Sort(Select(FullUnorderedScan, Explode)))",
			key:   concat(f("COL2"), col4Values),
		},
		{
			// Two unnests of one array are two trie children (two markers),
			// not a disconnected reference. Java: equal.
			name:  "unnest_same_array_twice",
			index: `create index ix as select "e1", "e2" from t4 as t, t.col4 as "e1", t.col4 as "e2" order by "e1", "e2"`,
			graph: "Select(Sort(Select(Select(FullUnorderedScan, Explode), Explode)))",
			key:   concat(col4Values, col4Values),
		},
		{
			// A derived table over an unnest: the leg window above the select
			// resolves ek.k into the element. Java: equal.
			name:  "unnest_derived",
			index: `create index ix as select a, ek.k from t6, (select k from t6.c) as ek order by a, ek.k`,
			graph: "Select(Sort(Select(FullUnorderedScan, Select(Explode))))",
			key:   concat(f("A"), recordlayer.Nest("C", recordlayer.NestFanOut("values", f("K")))),
		},
		{
			// The unnest's derived table is correlated to r; the outer
			// predicate on r is the join select's. Java: equal.
			name:  "unnest_outer_predicate",
			index: `create index ix as select sq."e" from t4 as r, (select "e" from r.col4 as "e") as sq where r.col2 > 1 order by sq."e"`,
			graph: "Select(Sort(Select(FullUnorderedScan, Select(Explode))))",
			key:   col4Values,
			pred:  predVP([]string{"COL2"}, cmpSimple(gen.ComparisonType_GREATER_THAN, valLong(1))),
		},
		{
			// A non-field value ends the trie's run; the field after it starts
			// a new one. Java: equal.
			name:  "arith_then_its_operand",
			index: `create index ix as select a + b, a from t order by a + b, a`,
			graph: "Select(Sort(FullUnorderedScan))",
			key:   concat(recordlayer.FunctionExpr("add", concat(f("A"), f("B"))), f("A")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := indexGraphSignature(t, resolverArmsDDL, tc.index); got != tc.graph {
				t.Fatalf("the translator hands the generator\n  %s\nwant\n  %s", got, tc.graph)
			}
			tmpl, err := buildSchemaTemplateFromDDL(resolverArmsDDL + "\n" + tc.index)
			if tc.key == nil {
				var ae *api.Error
				if !errors.As(err, &ae) || ae.Code != tc.code || !strings.Contains(ae.Message, tc.msg) {
					t.Fatalf("got %v, want %s %q", err, tc.code, tc.msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("DDL failed: %v", err)
			}
			var idx *recordlayer.Index
			for _, i := range tmpl.Underlying().GetAllIndexes() {
				if i.Name == "IX" {
					idx = i
				}
			}
			if idx == nil {
				t.Fatal("index IX not built")
			}
			if want, got := tc.key.ToKeyExpression(), idx.RootExpression.ToKeyExpression(); !proto.Equal(want, got) {
				t.Errorf("key\n got: %v\nwant: %v", got, want)
			}
			if got := idx.GetPredicateProto(); !proto.Equal(got, tc.pred) {
				t.Errorf("predicate\n got: %v\nwant: %v", got, tc.pred)
			}
		})
	}
}
