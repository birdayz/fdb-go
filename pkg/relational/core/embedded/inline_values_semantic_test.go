package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

func parseInlineValuesSelectQuery(t *testing.T, sql string) *selectQuery {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	statements := root.Statements().AllStatement()
	if len(statements) != 1 || statements[0].SelectStatement() == nil {
		t.Fatalf("parse %q did not produce one SELECT", sql)
	}
	body, ok := statements[0].SelectStatement().Query().QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		t.Fatalf("query body = %T, want QueryTermDefault", statements[0].SelectStatement().Query().QueryExpressionBody())
	}
	simple, ok := body.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		t.Fatalf("query term = %T, want SimpleTable", body.QueryTerm())
	}
	sq, err := extractFromSimpleTable(simple)
	if err != nil {
		t.Fatalf("extractFromSimpleTable(%q): %v", sql, err)
	}
	return sq
}

func TestBuildInlineValuesLogicalNormalizesOneExactRowShape(t *testing.T) {
	t.Parallel()
	sq := parseInlineValuesSelectQuery(t,
		`SELECT V.ID FROM VALUES (1, [101]), (NULL, [201, 202]) AS V (ID, ARR)`)

	source, err := buildInlineValuesLogical(sq.inlineValues, sq.tableAlias, "", nil)
	if err != nil {
		t.Fatalf("buildInlineValuesLogical: %v", err)
	}
	row, ok := source.ResultType().(*values.RecordType)
	if !ok || len(row.Fields) != 2 {
		t.Fatalf("result type = %#v, want exact two-field record", source.ResultType())
	}
	if row.Fields[0].Name != "ID" || row.Fields[0].FieldType.Code() != values.TypeCodeInt ||
		!row.Fields[0].FieldType.IsNullable() {
		t.Fatalf("ID type = %#v, want nullable INT", row.Fields[0])
	}
	arr, ok := row.Fields[1].FieldType.(*values.ArrayType)
	if !ok || arr.ElementType.Code() != values.TypeCodeInt || arr.ElementType.IsNullable() {
		t.Fatalf("ARR type = %#v, want ARRAY<INT NOT NULL>", row.Fields[1].FieldType)
	}

	collection, ok := source.CollectionValue().(*values.ArrayConstructorValue)
	if !ok || len(collection.Elements) != 2 {
		t.Fatalf("collection = %#v, want two exact record elements", source.CollectionValue())
	}
	collectionType, ok := collection.Type().(*values.ArrayType)
	if !ok || !collectionType.ElementType.Equals(row) {
		t.Fatalf("collection element type = %#v, want result row %v", collection.Type(), row)
	}
	for i, element := range collection.Elements {
		if !element.Type().Equals(row) {
			t.Fatalf("row %d type = %v, want common row %v", i, element.Type(), row)
		}
	}
}

func TestBuildInlineValuesLogicalRejectsRowWidthAndTypeDisagreement(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{
		`SELECT V.ID FROM VALUES (1), (2, 3) AS V (ID)`,
		`SELECT V.ID FROM VALUES (1), ('x') AS V (ID)`,
	} {
		sq := parseInlineValuesSelectQuery(t, sql)
		if source, err := buildInlineValuesLogical(sq.inlineValues, sq.tableAlias, "", nil); err == nil {
			t.Fatalf("buildInlineValuesLogical(%q) = %#v, nil; want exact-shape rejection", sql, source)
		}
	}
}

func TestInlineValuesScopeCarriesExactPrimaryAndLateralArrayShape(t *testing.T) {
	t.Parallel()
	sq := parseInlineValuesSelectQuery(t,
		`SELECT "values"."id", "val", "at" `+
			`FROM VALUES (1, [101]), (2, [201, 202, 203]) AS "values" ("id", "arr"), `+
			`"values"."arr" AS "val" AT "at"`)
	resolver := buildSelectScope(sq, buildTestMetaData(t), defaultEmbeddedSchema, nil)
	if resolver == nil {
		t.Fatal("inline VALUES FROM scope declined")
	}

	assertField := func(qualifier, name, correlation string, ordinal int, code values.TypeCode) {
		t.Helper()
		resolved, err := resolver.ResolveIdentifier(
			semantic.FromNormalized(qualifier), semantic.FromNormalized(name))
		if err != nil {
			t.Fatalf("resolve %s.%s: %v", qualifier, name, err)
		}
		field, ok := values.AsFieldValue(resolved)
		if !ok {
			t.Fatalf("resolved %s.%s = %T, want exact FieldValue", qualifier, name, resolved)
		}
		owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
		if !ok || owner.Correlation().Name() != correlation {
			t.Fatalf("resolved %s.%s owner = %#v, want %q", qualifier, name, field.ChildValue(), correlation)
		}
		if got := field.Path().Ordinals(); len(got) != 1 || got[0] != ordinal {
			t.Fatalf("resolved %s.%s path = %v, want [%d]", qualifier, name, got, ordinal)
		}
		if field.ResultType().Code() != code || field.ResultType().IsNullable() {
			t.Fatalf("resolved %s.%s type = %v, want NOT NULL %v", qualifier, name, field.ResultType(), code)
		}
	}

	assertField("values", "id", "VALUES", 0, values.TypeCodeInt)
	assertField("val", "val", "VAL", 0, values.TypeCodeInt)
	assertField("val", "at", "VAL", 1, values.TypeCodeInt)

	op := buildLogicalPlanForSelect(sq)
	if op == nil {
		t.Fatal("semantic builder declined inline VALUES + lateral array source")
	}
	var inline *logical.LogicalInlineValues
	var visit func(logical.LogicalOperator)
	visit = func(candidate logical.LogicalOperator) {
		if candidate == nil || inline != nil {
			return
		}
		if found, ok := candidate.(*logical.LogicalInlineValues); ok {
			inline = found
			return
		}
		for _, child := range candidate.Children() {
			visit(child)
		}
	}
	visit(op)
	if inline == nil || inline.Alias != "values" {
		t.Fatalf("logical tree has no authored inline VALUES leaf: %s", op.Explain(""))
	}
}

func TestBuildInlineValuesLogicalRetagsNestedDefinitionAndCommonType(t *testing.T) {
	t.Parallel()
	sq := parseInlineValuesSelectQuery(t,
		`SELECT A.B FROM VALUES (1, 2.0, (3, 4, 'foo')), `+
			`(10, 90.2, (5, 6.0, 'bar')) AS A(B, C, W(X, Y, Z))`)

	source, err := buildInlineValuesLogical(sq.inlineValues, sq.tableAlias, "", nil)
	if err != nil {
		t.Fatalf("build nested inline VALUES: %v", err)
	}
	row, ok := source.ResultType().(*values.RecordType)
	if !ok || len(row.Fields) != 3 {
		t.Fatalf("result type = %#v, want B/C/W record", source.ResultType())
	}
	nested, ok := row.Fields[2].FieldType.(*values.RecordType)
	if !ok || nested.RecordName != "" || nested.Nullable || len(nested.Fields) != 3 {
		t.Fatalf("W type = %#v, want exact non-null anonymous RECORD<X,Y,Z>", row.Fields[2].FieldType)
	}
	for i, want := range []string{"X", "Y", "Z"} {
		if nested.Fields[i].Name != want || nested.Fields[i].Ordinal != i {
			t.Fatalf("W field %d = %#v, want %s at ordinal %d", i, nested.Fields[i], want, i)
		}
	}
	if nested.Fields[1].FieldType.Code() != values.TypeCodeDouble {
		t.Fatalf("W.Y common type = %v, want DOUBLE after INT/DOUBLE promotion", nested.Fields[1].FieldType)
	}
	if _, err := values.SnapshotExactType(row); err != nil {
		t.Fatalf("retagged result type is not exact: %v", err)
	}

	collection := source.CollectionValue().(*values.ArrayConstructorValue)
	for rowIndex, element := range collection.Elements {
		top, ok := element.(*values.RecordConstructorValue)
		if !ok || len(top.Fields) != 3 {
			t.Fatalf("row %d = %T, want three-field RecordConstructorValue", rowIndex, element)
		}
		w, ok := top.Fields[2].Value.(*values.RecordConstructorValue)
		if !ok || w.TypeName() != "" || !w.Type().Equals(nested) {
			t.Fatalf("row %d W constructor = %#v type=%v, want anonymous exact %v", rowIndex, top.Fields[2].Value, top.Fields[2].Value.Type(), nested)
		}
		for i, want := range []string{"X", "Y", "Z"} {
			if w.Fields[i].Name != want {
				t.Fatalf("row %d W field %d name = %q, want %q", rowIndex, i, w.Fields[i].Name, want)
			}
		}
	}
}

func TestBuildInlineValuesLogicalRetagsQuotedArrayOfRecordDefinition(t *testing.T) {
	t.Parallel()
	sq := parseInlineValuesSelectQuery(t,
		`SELECT "a"."w" FROM VALUES ([('a', 'b', [1, 2, 3])]), `+
			`([('d', 'e', [10, 20, 30])]) AS "a" ("w"("x", "y", "z"))`)

	source, err := buildInlineValuesLogical(sq.inlineValues, sq.tableAlias, "", nil)
	if err != nil {
		t.Fatalf("build quoted array-of-record inline VALUES: %v", err)
	}
	row := source.ResultType().(*values.RecordType)
	if len(row.Fields) != 1 || row.Fields[0].Name != "w" {
		t.Fatalf("top-level quoted name = %#v, want lowercase w", row.Fields)
	}
	array, ok := row.Fields[0].FieldType.(*values.ArrayType)
	if !ok || array.Nullable {
		t.Fatalf("w type = %#v, want non-null array", row.Fields[0].FieldType)
	}
	element, ok := array.ElementType.(*values.RecordType)
	if !ok || element.RecordName != "" || len(element.Fields) != 3 {
		t.Fatalf("w element = %#v, want anonymous RECORD<x,y,z>", array.ElementType)
	}
	for i, want := range []string{"x", "y", "z"} {
		if element.Fields[i].Name != want {
			t.Fatalf("w element field %d = %q, want quoted lowercase %q", i, element.Fields[i].Name, want)
		}
	}
	zArray, ok := element.Fields[2].FieldType.(*values.ArrayType)
	if !ok || zArray.ElementType.Code() != values.TypeCodeInt {
		t.Fatalf("w.z = %#v, want ARRAY<INT>", element.Fields[2].FieldType)
	}

	collection := source.CollectionValue().(*values.ArrayConstructorValue)
	top := collection.Elements[0].(*values.RecordConstructorValue)
	wArray := top.Fields[0].Value.(*values.ArrayConstructorValue)
	w := wArray.Elements[0].(*values.RecordConstructorValue)
	if w.TypeName() != "" || !w.Type().Equals(element) {
		t.Fatalf("array element constructor type = %v (name %q), want anonymous %v", w.Type(), w.TypeName(), element)
	}
}

func TestBuildInlineValuesLogicalRejectsInvalidNestedDefinitions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		sql  string
	}{
		{"record arity", `SELECT A.W FROM VALUES ((1, 2)) AS A(W(X))`},
		{"scalar nesting", `SELECT A.B FROM VALUES (1) AS A(B(X))`},
		{"scalar array nesting", `SELECT A.B FROM VALUES ([1, 2]) AS A(B(X))`},
		{"row record width drift", `SELECT A.W FROM VALUES ((1, 2)), ((3, 4, 5)) AS A(W(X, Y))`},
		{"row record type drift", `SELECT A.W FROM VALUES ((1, 2)), ((3, 'x')) AS A(W(X, Y))`},
	} {
		t.Run(test.name, func(t *testing.T) {
			sq := parseInlineValuesSelectQuery(t, test.sql)
			if source, err := buildInlineValuesLogical(sq.inlineValues, sq.tableAlias, "", nil); err == nil {
				t.Fatalf("buildInlineValuesLogical(%q) = %#v, nil; want exact nested-shape rejection", test.sql, source)
			}
		})
	}
}

func TestRetagInlineValuesRecordTypeIsCopyOnWriteAndRejectsForeignIdentity(t *testing.T) {
	t.Parallel()
	fields := []values.Field{
		{Name: "_0", FieldType: values.NotNullInt, Ordinal: 0},
		{Name: "_1", FieldType: values.NotNullString, Ordinal: 1},
	}
	source := values.NewRecordType("", false, fields)
	definitions := []inlineValuesColumnDefinition{{Name: "X"}, {Name: "Y"}}
	retagged, err := retagInlineValuesRecordType(source, definitions)
	if err != nil {
		t.Fatalf("retag exact anonymous record: %v", err)
	}
	if retagged == source || retagged.RecordName != "" ||
		retagged.Fields[0].Name != "X" || retagged.Fields[1].Name != "Y" {
		t.Fatalf("retagged = %#v, want a copied, still anonymous RECORD<X,Y>", retagged)
	}
	if source.RecordName != "" || source.Fields[0].Name != "_0" || source.Fields[1].Name != "_1" {
		t.Fatalf("retag mutated source type: %#v", source)
	}
	if _, err := values.SnapshotExactType(retagged); err != nil {
		t.Fatalf("retagged type is not exact: %v", err)
	}

	foreign := values.NewRecordType("FOREIGN", false, fields)
	if _, err := retagInlineValuesRecordType(foreign, definitions); err == nil ||
		!strings.Contains(err.Error(), "nominal record type") {
		t.Fatalf("foreign nominal record retag error = %v, want loud nominal-identity rejection", err)
	}
	malformed := &values.RecordType{Fields: []values.Field{{Name: "_0", FieldType: values.NotNullInt, Ordinal: 1}}}
	if _, err := retagInlineValuesRecordType(malformed, definitions[:1]); err == nil ||
		!strings.Contains(err.Error(), "malformed ordinal") {
		t.Fatalf("malformed ordinal retag error = %v, want loud ordinal rejection", err)
	}
}
