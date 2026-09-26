package embedded

import (
	"errors"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/protoname"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

func TestLateralCollectionBinding(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, tc := range []struct {
		name, sql, owner string
		ordinals         []int
	}{
		{"base", `SELECT x FROM T1, T1.ARR1 x`, "T1", []int{1}},
		{"computed", `SELECT x FROM (SELECT CAST([1.0E20, -1.0E20] AS BIGINT ARRAY) AS a FROM T1) q, q.a x`, "Q", []int{0}},
		{"quoted", `SELECT x FROM (SELECT id, CAST([1.0E20] AS BIGINT ARRAY) AS "a.b" FROM T1) AS "q.q", "q.q"."a.b" x`, "Q.Q", []int{1}},
		{"cte_labels", `WITH c("a.b") AS (SELECT CAST([1.0E20] AS BIGINT ARRAY) a FROM T1) SELECT x FROM c q, q."a.b" x`, "Q", []int{0}},
		{"exists_inner", `SELECT ID FROM U WHERE EXISTS (SELECT 1 FROM T1, T1.ARR1 x WHERE x = U.V)`, "Q$BOUND1", []int{1}},
		{"exists_fast_inner", `SELECT ID FROM U WHERE EXISTS (SELECT U.ID FROM T1, T1.ARR1 x)`, "Q$BOUND1", []int{1}},
		{"earlier_owner", `SELECT x FROM T1 a, U b, a.ARR1 x`, "A", []int{1}},
		{"later_source", `SELECT x FROM T1 a, a.ARR1 x, U b`, "A", []int{1}},
		{"rebuild", `SELECT T1.*, x FROM T1, T1.ARR1 x`, "T1", []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, catalog := range []bool{false, true} {
				root, err := parseQueryFromSelect(t, tc.sql)
				if err != nil {
					t.Fatal(err)
				}
				var op logical.LogicalOperator
				if catalog {
					op, err = buildLogicalPlanForQueryWithCatalog(root, md)
				} else {
					op, err = NewPlanVisitor(md).VisitQuery(root)
				}
				if err != nil {
					t.Fatalf("catalog=%v: %v", catalog, err)
				}
				var found []*logical.LogicalUnnest
				var walk func(logical.LogicalOperator)
				walk = func(node logical.LogicalOperator) {
					if u, ok := node.(*logical.LogicalUnnest); ok {
						found = append(found, u)
					}
					if filter, ok := node.(*logical.LogicalFilter); ok {
						for _, subquery := range filter.ExistsSubqueries {
							walk(subquery.Plan)
						}
					}
					if cte, ok := node.(*logical.LogicalCTE); ok {
						walk(cte.Main)
						return
					}
					for _, child := range node.Children() {
						walk(child)
					}
				}
				walk(op)
				if len(found) != 1 {
					t.Fatalf("catalog=%v: got %d lateral nodes", catalog, len(found))
				}
				if tc.name == "quoted" && !reflect.DeepEqual(found[0].Segments, []string{"q.q", "a.b"}) {
					t.Errorf("quoted identifier segments changed: %v", found[0].Segments)
				}
				// Scope.AddSource canonicalizes runtime correlation keys, separately
				// from the quote-preserving identifiers used to resolve the source.
				collection, ok := values.AsFieldValue(found[0].CorrelatedCollection)
				if !ok {
					t.Fatalf("catalog=%v: lateral collection was not semantically bound", catalog)
				}
				if !reflect.DeepEqual(collection.Path().Ordinals(), tc.ordinals) {
					t.Errorf("catalog=%v: path=%v, want %v", catalog, collection.Path().Ordinals(), tc.ordinals)
				}
				qov, ok := values.AsQuantifiedObjectValue(collection.ChildValue())
				if !ok || qov.Correlation().Name() != tc.owner {
					t.Fatalf("catalog=%v: collection root=%v, want owner %q", catalog, collection.ChildValue(), tc.owner)
				}
				array, ok := collection.Type().(*values.ArrayType)
				if !ok || array.ElementType.Code() != values.TypeCodeLong {
					t.Fatalf("catalog=%v: collection type=%v, want LONG ARRAY", catalog, collection.Type())
				}
			}
		})
	}
}

// TestUnnestRecordPublicationSeparatesWholeObjectFromStar pins Java's record
// element attributes: without AT the whole alias is ephemeral and the members
// are exposed directly; with AT the whole element and ordinal are visible.
func TestUnnestRecordPublicationSeparatesWholeObjectFromStar(t *testing.T) {
	t.Parallel()
	element := semantic.Column{Type: "RECORD", StructTypeName: "ITEM", StructFields: []semantic.Column{
		{Id: semantic.FromNormalized("K"), Type: "BIGINT"},
		{Id: semantic.FromNormalized("A"), Type: "BIGINT", IsArray: true},
	}}
	for _, at := range []string{"", "P"} {
		t.Run("AT="+at, func(t *testing.T) {
			t.Parallel()
			src, ok := unnestVirtualScopeSourceWithElement(joinClause{alias: "X", aliasExplicit: true, bindingID: "Q$DUP1", atAlias: at}, &element)
			if !ok {
				t.Fatal("exact record element was not published")
			}
			columns := semantic.NonEphemeral(src.Table.Columns())
			want := []string{"K", "A"}
			if at != "" {
				want = []string{"X", "P"}
			}
			if len(columns) != len(want) {
				t.Fatalf("star columns = %v, want %v", columns, want)
			}
			for i, column := range columns {
				if column.Id.Name() != want[i] {
					t.Fatalf("star column[%d] = %s, want %s", i, column.Id.Name(), want[i])
				}
			}
			scope := semantic.NewScope(nil)
			if err := scope.AddSource(src); err != nil {
				t.Fatal(err)
			}
			resolver := expr.New(semantic.NewAnalyzer(semantic.NewInMemoryCatalog(), false), scope)
			whole, err := resolver.ResolveIdentifierPath([]semantic.Identifier{semantic.FromNormalized("X")})
			if err != nil {
				t.Fatal(err)
			}
			record, ok := whole.Type().(*values.RecordType)
			if !ok || record.RecordName != "ITEM" || len(record.Fields) != 2 {
				t.Fatalf("whole element type = %v, want ITEM(K,A)", whole.Type())
			}
			leaf, err := resolver.ResolveIdentifierPath([]semantic.Identifier{semantic.FromNormalized("X"), semantic.FromNormalized("K")})
			if err != nil || !leaf.Type().Equals(values.NotNullLong) {
				t.Fatalf("qualified member = %v / %v", leaf, err)
			}
			field, ok := values.AsFieldValue(leaf)
			wantPath := []int{0}
			if at != "" {
				wantPath = []int{0, 0}
			}
			if !ok || !reflect.DeepEqual(field.Path().Ordinals(), wantPath) {
				t.Fatalf("member access = %v, want %v", leaf, wantPath)
			}
		})
	}
}

func TestSelectAliasRequiresAS(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, tc := range []struct {
		name, sql string
		missing   bool
	}{
		{"direct_implicit_label", `SELECT q.x FROM (SELECT ID x FROM T1) q`, true},
		{"direct_inherited_label", `SELECT q.ID FROM (SELECT ID x FROM T1) q`, false},
		{"direct_explicit_label", `SELECT q.x FROM (SELECT ID AS x FROM T1) q`, false},
		{"computed_implicit_label", `SELECT q.x FROM (SELECT CAST(ID AS BIGINT) x FROM T1) q`, true},
		{"computed_explicit_label", `SELECT q.x FROM (SELECT CAST(ID AS BIGINT) AS x FROM T1) q`, false},
		{"cte_implicit_label", `WITH q AS (SELECT CAST(ID AS BIGINT) x FROM T1) SELECT q.x FROM q`, true},
		{"cte_explicit_label", `WITH q AS (SELECT CAST(ID AS BIGINT) AS x FROM T1) SELECT q.x FROM q`, false},
		{"computed_metadata_label", `SELECT q."_0" FROM (SELECT CAST(ID AS BIGINT) x FROM T1) q`, true},
		{"computed_canonical_label", `SELECT q."CAST(ID AS BIGINT)" FROM (SELECT CAST(ID AS BIGINT) x FROM T1) q`, true},
		{"aggregate_implicit_label", `SELECT q.x FROM (SELECT SUM(ID) x FROM T1) q`, true},
		{"aggregate_explicit_label", `SELECT q.x FROM (SELECT SUM(ID) AS x FROM T1) q`, false},
		{"count_implicit_label", `SELECT q.x FROM (SELECT COUNT(*) x FROM T1) q`, true},
		{"count_explicit_label", `SELECT q.x FROM (SELECT COUNT(*) AS x FROM T1) q`, false},
		{"grouped_count_explicit_label", `SELECT q.x FROM (SELECT COUNT(*) AS x FROM T1 GROUP BY ID) q`, false},
		{"grouped_count_implicit_label", `SELECT q.x FROM (SELECT COUNT(*) x FROM T1 GROUP BY ID) q`, true},
		{"grouped_count_cte_explicit_label", `WITH q AS (SELECT COUNT(*) AS x FROM T1 GROUP BY ID) SELECT q.x FROM q`, false},
		{"computed_group_no_aggregate", `SELECT q."ID+1" FROM (SELECT ID+1 FROM T1 GROUP BY ID+1) q`, true},
		{"computed_group_with_aggregate", `SELECT q."ID+1" FROM (SELECT ID+1, COUNT(*) FROM T1 GROUP BY ID+1) q`, true},
		{"computed_group_implicit_alias", `SELECT q."ID+1" FROM (SELECT ID+1 x, COUNT(*) FROM T1 GROUP BY ID+1) q`, true},
		{"computed_group_explicit_alias", `SELECT q.x FROM (SELECT ID+1 AS x, COUNT(*) FROM T1 GROUP BY ID+1) q`, false},
		{"computed_group_having", `SELECT q."ID+1" FROM (SELECT ID+1 FROM T1 GROUP BY ID+1 HAVING COUNT(*) > 0) q`, true},
		{"mixed_count_implicit_label", `SELECT q.x FROM (SELECT COUNT(*) x, SUM(ID) AS y FROM T1) q`, true},
		{"mixed_count_explicit_label", `SELECT q.x FROM (SELECT COUNT(*) AS x, SUM(ID) AS y FROM T1) q`, false},
		{"cte_suffix_not_sql_name", `WITH d AS (SELECT ID AS k, ID AS k, ARR1 AS a FROM T1), e AS (SELECT DISTINCT * FROM d) SELECT e.k_2 FROM e`, true},
		{"cte_unique_name_survives_duplicates", `WITH d AS (SELECT ID AS k, ID AS k, ARR1 AS a FROM T1), e AS (SELECT DISTINCT * FROM d) SELECT e.a FROM e`, false},
		{"from_table_implicit", `SELECT q.ID FROM T1 q`, false},
		{"from_table_explicit", `SELECT q.ID FROM T1 AS q`, false},
		{"from_derived_explicit", `SELECT q.x FROM (SELECT ID AS x FROM T1) AS q`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, catalog := range []bool{false, true} {
				root, err := parseQueryFromSelect(t, tc.sql)
				if err != nil {
					t.Fatal(err)
				}
				if catalog {
					_, err = buildLogicalPlanForQueryWithCatalog(root, md)
				} else {
					_, err = NewPlanVisitor(md).VisitQuery(root)
				}
				if !tc.missing {
					if err != nil {
						t.Fatalf("catalog=%v: %v", catalog, err)
					}
					continue
				}
				var se *api.Error
				if !errors.As(err, &se) || se.Code != api.ErrCodeUndefinedColumn {
					t.Fatalf("catalog=%v: implicit SELECT alias error = %v, want 42703", catalog, err)
				}
			}
		})
	}
}

func TestSelectOutputNamesValidateAfterBinding(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, tc := range []struct {
		sql  string
		code api.ErrorCode
	}{
		{`SELECT ID AS "bad()" FROM T1`, api.ErrCodeInvalidName},
		{`SELECT CAST(ID AS BIGINT) AS "bad()" FROM T1`, api.ErrCodeInvalidName},
		{`SELECT SUM(ID) AS "SUM(ID)" FROM T1`, api.ErrCodeInvalidName},
		{`SELECT COUNT(*) AS "COUNT(*)" FROM T1`, api.ErrCodeInvalidName},
		{`SELECT ID, COUNT(*) AS "bad()" FROM T1 GROUP BY ID`, api.ErrCodeInvalidName},
		{`SELECT missing AS "bad()" FROM T1`, api.ErrCodeUndefinedColumn},
		{`SELECT ID "bad()" FROM T1`, ""},
		{`SELECT CAST(ID AS BIGINT) "bad()" FROM T1`, ""},
		{`SELECT COUNT(*) "bad()" FROM T1`, ""},
		{`SELECT q."_0" FROM (SELECT COUNT(*) AS "_0" FROM T1) q`, ""},
		{`SELECT q."a.b" FROM (SELECT COUNT(*) AS "a.b" FROM T1) q`, ""},
		{`SELECT q."a$b" FROM (SELECT CAST(ID AS BIGINT) AS "a$b" FROM T1) q`, ""},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			for _, catalog := range []bool{false, true} {
				root, err := parseQueryFromSelect(t, tc.sql)
				if err != nil {
					t.Fatal(err)
				}
				if catalog {
					_, err = buildLogicalPlanForQueryWithCatalog(root, md)
				} else {
					_, err = NewPlanVisitor(md).VisitQuery(root)
				}
				if tc.code == "" {
					if err != nil {
						t.Fatalf("catalog=%t: %v", catalog, err)
					}
					continue
				}
				var se *api.Error
				if !errors.As(err, &se) || se.Code != tc.code {
					t.Fatalf("catalog=%t: %v, want %s", catalog, err, tc.code)
				}
				if tc.code == api.ErrCodeInvalidName {
					var cause *protoname.InvalidNameError
					if !errors.As(err, &cause) {
						t.Fatalf("catalog=%t: invalid-name cause lost: %v", catalog, err)
					}
				}
			}
		})
	}
}

func TestSelectReferencePublishesInheritedOutputName(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, tc := range []struct {
		sql, name string
	}{
		{`SELECT ID x FROM T1`, "ID"},
		{`SELECT q.ID x FROM T1 q`, "ID"},
		{`SELECT ID AS x FROM T1`, "X"},
		{`SELECT q."a.b" FROM (SELECT COUNT(*) AS "a.b" FROM T1) q`, "a.b"},
		{`SELECT q."a.b" FROM (SELECT CAST(ID AS BIGINT) AS "a.b" FROM T1) q`, "a.b"},
		{`SELECT CAST(ID AS BIGINT) x FROM T1`, ""},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			for _, catalog := range []bool{false, true} {
				root, err := parseQueryFromSelect(t, tc.sql)
				if err != nil {
					t.Fatal(err)
				}
				var op logical.LogicalOperator
				if catalog {
					op, err = buildLogicalPlanForQueryWithCatalog(root, md)
				} else {
					op, err = NewPlanVisitor(md).VisitQuery(root)
				}
				if err != nil {
					t.Fatal(err)
				}
				proj := findProjection(op)
				if proj == nil || len(proj.Projections) != 1 {
					t.Fatalf("catalog=%t: missing one-slot projection: %v", catalog, op)
				}
				name := ""
				if len(proj.Aliases) != 0 {
					name = proj.Aliases[0]
				}
				if name != tc.name {
					t.Fatalf("catalog=%t: output name = %q, want %q", catalog, name, tc.name)
				}
			}
		})
	}
}

func TestGroupingAliasPreservesSelectNameProvenance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sql     string
		aliased bool
		name    string
	}{
		{`SELECT g FROM T1 GROUP BY ID AS g`, false, "G"},
		{`SELECT g x FROM T1 GROUP BY ID AS g`, false, "G"},
		{`SELECT g AS x FROM T1 GROUP BY ID AS g`, true, "X"},
		{`SELECT g, COUNT(*) FROM T1 GROUP BY ID AS g`, false, "G"},
		{`SELECT COUNT(*), g FROM T1 GROUP BY ID AS g`, false, "G"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			sq := parseSelect(t, tc.sql)
			var found bool
			for _, ac := range sq.aggCols {
				if ac.groupCol == "" {
					continue
				}
				found = true
				if ac.outputAliased != tc.aliased || ac.outputInheritedName != "G" || aggregateOutputSQLName(ac) != tc.name {
					t.Fatalf("grouping alias altered SELECT provenance: AS=%v inherited=%q SQL=%q, want %v/G/%s", ac.outputAliased, ac.outputInheritedName, aggregateOutputSQLName(ac), tc.aliased, tc.name)
				}
			}
			if !found {
				t.Fatal("no grouping output was exercised")
			}
		})
	}
}

func TestCTEProjectionSourceKeepsExactRow(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, tc := range []struct {
		name, sql      string
		labels, fields []string
	}{
		{"direct_duplicates", `SELECT ID AS K, ID AS K, ARR1 AS A FROM T1`, []string{"K", "K", "A"}, []string{"K", "K_2", "A"}},
		{"join_star_chain", `WITH d AS (SELECT T1.ID AS K, U.ID AS K, U.V AS N FROM T1, U) SELECT DISTINCT * FROM d`, []string{"K", "K", "N"}, []string{"K", "K_2", "N"}},
		{"reordered_chain", `WITH d AS (SELECT T1.ID AS K, U.ID AS K, U.V AS N FROM T1, U) SELECT N, N, N AS M FROM d`, []string{"N", "N", "M"}, []string{"N", "N_2", "M"}},
		{"anonymous_star_chain", `WITH d AS (SELECT CAST(ID AS BIGINT) FROM T1) SELECT * FROM d`, []string{""}, []string{"_0"}},
		{"anonymous_shifted_star", `WITH d AS (SELECT CAST(ID AS BIGINT) FROM T1) SELECT U.ID, d.* FROM U, d`, []string{"ID", ""}, []string{"ID", "_1"}},
		{"column_list", `WITH d(z) AS (SELECT CAST(ID AS BIGINT) FROM T1) SELECT z FROM d`, []string{"Z"}, []string{"Z"}},
		// Publication types only the seed; Q is not registered until this returns.
		{"recursive_seed_only", `SELECT ID AS K, ID AS K, ARR1 AS A FROM T1 UNION ALL SELECT * FROM Q`, []string{"K", "K", "A"}, []string{"K", "K_2", "A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := parseQueryFromSelect(t, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			source, ok, err := buildCTEColumnSource(md, "Q", body, nil)
			if err != nil || !ok {
				t.Fatalf("source: ok=%t err=%v", ok, err)
			}
			var labels, fields []string
			for _, column := range source.Table.Columns() {
				labels = append(labels, column.Id.Name())
			}
			row := expr.SourceRowType(source)
			if row == nil {
				t.Fatal("source has no exact row")
			}
			for _, field := range row.Fields {
				fields = append(fields, field.Name)
			}
			if !reflect.DeepEqual(labels, tc.labels) || !reflect.DeepEqual(fields, tc.fields) {
				t.Fatalf("SQL labels=%v, flowed fields=%v; want labels=%v, fields=%v", labels, fields, tc.labels, tc.fields)
			}
		})
	}
}

func TestCTESimplePublicationPreservesBodyErrors(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	for _, tc := range []struct {
		sql  string
		code api.ErrorCode
	}{
		{`SELECT MISSING FROM T1`, api.ErrCodeUndefinedColumn},
		{`WITH d AS (SELECT ID AS K, ID AS K FROM T1) SELECT K FROM d`, api.ErrCodeAmbiguousColumn},
		{`WITH d AS (SELECT CAST(ID AS BIGINT) FROM T1) SELECT "_0" FROM d`, api.ErrCodeUndefinedColumn},
		{`WITH d AS (SELECT CAST(ID AS BIGINT) FROM T1) SELECT "CAST(ID AS BIGINT)" FROM d`, api.ErrCodeUndefinedColumn},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			body, err := parseQueryFromSelect(t, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			_, ok, err := buildCTEColumnSource(md, "Q", body, nil)
			var sqlErr *api.Error
			if ok || !errors.As(err, &sqlErr) || sqlErr.Code != tc.code {
				t.Fatalf("body error lost: ok=%t err=%v, want %s", ok, err, tc.code)
			}
		})
	}
}

func TestStarSourceProjectionIdentity(t *testing.T) {
	t.Parallel()
	column := func(name string) semantic.Column {
		return semantic.Column{Id: semantic.FromNormalized(name), Type: "BIGINT"}
	}
	for _, tc := range []struct {
		name                                     string
		columns, flowed                          []semantic.Column
		ordinals                                 []int
		hidden                                   map[string]struct{}
		object                                   *semantic.Column
		shadowing, missingTable, want, wantError bool
	}{
		{name: "plain_identity", columns: []semantic.Column{column("K")}},
		{name: "declared_identity", columns: []semantic.Column{column("K")}, flowed: []semantic.Column{column("K")}, ordinals: []int{0}},
		{name: "duplicate_names", columns: []semantic.Column{column("K"), column("K")}, flowed: []semantic.Column{column("K"), column("K_2")}, want: true},
		{name: "anonymous", columns: []semantic.Column{column("")}, flowed: []semantic.Column{column("_0")}, want: true},
		{name: "extra_flowed_slot", columns: []semantic.Column{column("K")}, flowed: []semantic.Column{column("K"), column("N")}, want: true},
		{name: "reordered_ordinals", columns: []semantic.Column{column("K"), column("K")}, flowed: []semantic.Column{column("K"), column("K")}, ordinals: []int{1, 0}, want: true},
		{name: "hidden", columns: []semantic.Column{column("K"), column("N")}, hidden: map[string]struct{}{"N": {}}, want: true},
		{name: "ephemeral", columns: []semantic.Column{column("K"), {Id: semantic.FromNormalized("VERSION"), Type: "BIGINT", Ephemeral: true}}, want: true},
		{name: "whole_object", columns: []semantic.Column{column("K")}, object: new(column("K")), want: true},
		{name: "shadowing", columns: []semantic.Column{column("K")}, shadowing: true, want: true},
		{name: "no_table", missingTable: true, wantError: true},
		{name: "empty_row"},
		{name: "declared_empty_row", flowed: []semantic.Column{}},
		{name: "column_outside_declared_empty_row", columns: []semantic.Column{column("K")}, flowed: []semantic.Column{}, wantError: true},
		{name: "short_mapping", columns: []semantic.Column{column("K")}, ordinals: []int{}, wantError: true},
		{name: "outside_row", columns: []semantic.Column{column("K")}, ordinals: []int{1}, wantError: true},
		{name: "negative_ordinal", columns: []semantic.Column{column("K")}, ordinals: []int{-1}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := semantic.ScopeSource{FlowedColumns: tc.flowed, ColumnOrdinals: tc.ordinals, HiddenColumns: tc.hidden, FlowedObject: tc.object, Shadowing: tc.shadowing}
			if !tc.missingTable {
				source.Table = &semantic.StaticTable{TableColumns: tc.columns}
			}
			got, err := starSourceNeedsProjection(source)
			if got != tc.want || (err != nil) != tc.wantError {
				t.Fatalf("needs projection=%t err=%v, want %t/error=%t", got, err, tc.want, tc.wantError)
			}
		})
	}
}

func TestCTEScopeCarriesStoredEnum(t *testing.T) {
	t.Parallel()
	md := buildTestMetaData(t)
	body, err := parseQueryFromSelect(t, `SELECT * FROM Order`)
	if err != nil {
		t.Fatal(err)
	}
	source, ok, err := buildCTEColumnSource(md, "C", body, nil)
	if err != nil || !ok {
		t.Fatalf("stored enum source: ok=%t err=%v", ok, err)
	}
	row := expr.SourceRowType(source)
	if row == nil {
		t.Fatal("CTE has no exact row")
	}
	flower, ok := row.Fields[1].FieldType.(*values.RecordType)
	if !ok {
		t.Fatalf("flower is %T", row.Fields[1].FieldType)
	}
	color, ok := flower.Fields[1].FieldType.(*values.EnumType)
	if !ok {
		t.Fatalf("color is %T, want exact ENUM", flower.Fields[1].FieldType)
	}
	if color.EnumName != "com.apple.foundationdb.record.Color" || !color.Nullable || !reflect.DeepEqual(color.Values, []values.EnumValue{{Name: "RED", Number: 1}, {Name: "BLUE", Number: 2}, {Name: "YELLOW", Number: 3}, {Name: "PINK", Number: 4}}) {
		t.Fatalf("color type lost descriptor identity: %+v", color)
	}
}

func TestSemanticEnumRejectsMalformedExactDeclarations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  values.Type
	}{
		{"typed_nil", (*values.EnumType)(nil)},
		{"no_name", &values.EnumType{Values: []values.EnumValue{{Name: "R", Number: 1}}}},
		{"no_members", &values.EnumType{EnumName: "C"}},
		{"empty_member", &values.EnumType{EnumName: "C", Values: []values.EnumValue{{Number: 1}}}},
		{"duplicate_name", &values.EnumType{EnumName: "C", Values: []values.EnumValue{{Name: "R", Number: 1}, {Name: "R", Number: 2}}}},
		{"duplicate_number", &values.EnumType{EnumName: "C", Values: []values.EnumValue{{Name: "R", Number: 1}, {Name: "B", Number: 1}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if column, ok := semanticColumnFromExactType("C", tc.typ); ok {
				t.Fatalf("malformed enum admitted: %+v", column)
			}
		})
	}
}

func TestEnumErrorClassification(t *testing.T) {
	t.Parallel()
	cause := &values.InvalidEnumValueError{Value: "PURPLE"}
	for _, mapped := range []error{mapPredicateWalkError(cause), translateExecError(cause)} {
		var sqlErr *api.Error
		var enumErr *values.InvalidEnumValueError
		if !errors.As(mapped, &sqlErr) || sqlErr.Code != api.ErrCodeInternalError || !errors.As(mapped, &enumErr) || enumErr.Value != "PURPLE" {
			t.Fatalf("enum classification or cause lost: %v", mapped)
		}
	}
}

func TestSemanticColumnRejectsMalformedTypeGraph(t *testing.T) {
	t.Parallel()
	cycle := &values.ArrayType{}
	cycle.ElementType = cycle
	for _, typ := range []values.Type{
		(*values.RecordType)(nil), (*values.ArrayType)(nil), cycle,
		&values.RecordType{Fields: []values.Field{{Name: "C", FieldType: (*values.EnumType)(nil)}}},
	} {
		if column, ok := semanticColumnFromExactType("C", typ); ok {
			t.Fatalf("malformed graph admitted: %+v", column)
		}
	}
}
