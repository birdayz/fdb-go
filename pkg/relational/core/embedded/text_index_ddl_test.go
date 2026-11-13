package embedded

import (
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

func TestTextIndexScalarColumns(t *testing.T) {
	t.Parallel()
	dot, err := recordlayer.ToProtoBufCompliantName("P.BODY")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  *gen.KeyExpression
		want []string
	}{
		{"nil", nil, nil},
		{"empty", &gen.KeyExpression{}, nil},
		{"empty field", &gen.KeyExpression{Field: &gen.Field{}}, nil},
		{"scalar", recordlayer.Field("BODY").ToKeyExpression(), []string{"BODY"}},
		{"implicit null standin", &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String("BODY"), FanType: gen.Field_SCALAR.Enum()}}, []string{"BODY"}},
		{"unique null standin", recordlayer.FieldWithNullStandin("BODY", recordlayer.FanTypeNone, recordlayer.NullStandinNullUnique).ToKeyExpression(), []string{"BODY"}},
		{"not null standin", recordlayer.FieldWithNullStandin("BODY", recordlayer.FanTypeNone, recordlayer.NullStandinNotNull).ToKeyExpression(), nil},
		{"not null text", recordlayer.GroupBy(recordlayer.FieldWithNullStandin("BODY", recordlayer.FanTypeNone, recordlayer.NullStandinNotNull), recordlayer.Field("TENANT")).ToKeyExpression(), nil},
		{"not null partition", recordlayer.GroupBy(recordlayer.Field("BODY"), recordlayer.FieldWithNullStandin("TENANT", recordlayer.FanTypeNone, recordlayer.NullStandinNotNull)).ToKeyExpression(), nil},
		{"dot is one field", recordlayer.Field(dot).ToKeyExpression(), []string{"P.BODY"}},
		{"then", recordlayer.Concat(recordlayer.Field("TENANT"), recordlayer.Field("BODY")).ToKeyExpression(), []string{"TENANT", "BODY"}},
		{"grouped", recordlayer.GroupBy(recordlayer.Field("BODY"), recordlayer.Field("TENANT")).ToKeyExpression(), []string{"TENANT", "BODY"}},
		{"empty then", &gen.KeyExpression{Then: &gen.Then{}}, nil},
		{"nil then child", &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{nil}}}, nil},
		{"nil grouping", &gen.KeyExpression{Grouping: &gen.Grouping{}}, nil},
		{"fanout", &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String("BODY"), FanType: gen.Field_FAN_OUT.Enum()}}, nil},
		{"concatenate", recordlayer.FieldConcatenate("BODY").ToKeyExpression(), nil},
		{"nesting", recordlayer.Nest("P", recordlayer.Field("BODY")).ToKeyExpression(), nil},
		{"function", recordlayer.FunctionExpr("subtract", recordlayer.Field("TENANT")).ToKeyExpression(), nil},
		{"unsupported child", recordlayer.GroupBy(recordlayer.Field("BODY"), recordlayer.Nest("P", recordlayer.Field("TENANT"))).ToKeyExpression(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := textIndexScalarColumns(tc.key)
			if ok != (tc.want != nil) || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v,%v want %v,%v", got, ok, tc.want, tc.want != nil)
			}
		})
	}
}

func TestFullTextNullStandinPlans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		text    recordlayer.NullStandin
		group   recordlayer.NullStandin
		indexed bool
	}{
		{"ordinary", recordlayer.NullStandinNull, recordlayer.NullStandinNull, true},
		{"unique null", recordlayer.NullStandinNullUnique, recordlayer.NullStandinNullUnique, true},
		{"not null group", recordlayer.NullStandinNull, recordlayer.NullStandinNotNull, false},
		{"not null text", recordlayer.NullStandinNotNull, recordlayer.NullStandinNull, false},
		{"not null both", recordlayer.NullStandinNotNull, recordlayer.NullStandinNotNull, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			template, err := BuildSchemaTemplateFromDDL("CREATE TABLE docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant)")
			if err != nil {
				t.Fatal(err)
			}
			md := template.Underlying()
			md.GetIndex("BODY_TEXT").RootExpression = recordlayer.GroupBy(
				recordlayer.FieldWithNullStandin("BODY", recordlayer.FanTypeNone, tc.text),
				recordlayer.FieldWithNullStandin("TENANT", recordlayer.FanTypeNone, tc.group))
			plan, err := PlanRecordQueryWithMetadata("SELECT id FROM docs WHERE tenant=0 AND TEXT_CONTAINS_ALL(body,'red')", md, nil)
			if err != nil {
				t.Fatal(err)
			}
			shape := plan.Explain()
			if got := strings.Contains(shape, "TextIndexScan(BODY_TEXT"); got != tc.indexed {
				t.Errorf("indexed=%v want=%v: %s", got, tc.indexed, shape)
			}
			if !tc.indexed && (!strings.Contains(shape, "Scan(DOCS") || !strings.Contains(shape, "PredicatesFilter")) {
				t.Errorf("NOT_NULL coordinates must retain record filtering: %s", shape)
			}
		})
	}
}

func TestFullTextMetadataCoordinatePlans(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TYPE AS STRUCT payload (body STRING, tenant BIGINT) CREATE TABLE docs(id BIGINT, body STRING, tenant BIGINT, p payload, "P.BODY" STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)`
	dot, err := recordlayer.ToProtoBufCompliantName("P.BODY")
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []recordlayer.KeyExpression{
		recordlayer.GroupBy(recordlayer.Nest("P", recordlayer.Field("BODY"))),
		recordlayer.GroupBy(recordlayer.Field("BODY"), recordlayer.Nest("P", recordlayer.Field("TENANT"))),
		recordlayer.GroupBy(recordlayer.Field("BODY"), recordlayer.FunctionExpr("subtract", recordlayer.Field("TENANT"))),
		recordlayer.GroupBy(recordlayer.Field(dot)),
	} {
		template, err := BuildSchemaTemplateFromDDL(ddl)
		if err != nil {
			t.Fatal(err)
		}
		md := template.Underlying()
		md.GetIndex("BODY_TEXT").RootExpression = root
		for _, tc := range []struct {
			query string
			want  bool
		}{
			{"SELECT id FROM docs WHERE tenant=1 AND TEXT_CONTAINS_ALL(body,'red')", false},
			{"SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(p.body,'red')", false},
			{`SELECT id FROM docs WHERE TEXT_CONTAINS_ALL("P.BODY",'red')`, reflect.DeepEqual(root.ToKeyExpression(), recordlayer.GroupBy(recordlayer.Field(dot)).ToKeyExpression())},
		} {
			plan, err := PlanRecordQueryWithMetadata(tc.query, md, nil)
			if err != nil {
				t.Fatalf("%s: %v", tc.query, err)
			}
			if strings.Contains(plan.Explain(), "TextIndexScan") != tc.want {
				t.Fatalf("%s: %s", tc.query, plan.Explain())
			}
		}
	}
}
