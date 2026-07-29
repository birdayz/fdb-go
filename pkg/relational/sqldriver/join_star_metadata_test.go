package sqldriver_test

// SELECT * metadata pins for the ordinal-top derivation arm
// (deriveColumnsFromJoin):
//
//  1. The leak discriminator must be STRUCTURAL (a leg subplan carrying the
//     positional-merge RC), never name-based: a user column literally named
//     `_0` is a legal identifier, and a name-keyed check would reroute a
//     working gated join's metadata off the qualified merge path.
//  2. A STRUCT-typed array element must report STRUCT — valueTypeName had no
//     TypeCodeRecord case, so the element fell to the BIGINT fallback.

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

// TestStarMetadataUserColumnNamedOrdinal pins that a plain gated 2-way join
// whose schema carries a column literally named "_0" keeps the MERGE path's
// alias-qualified column metadata — the ordinal-top arm must not fire (no
// positional-merge leg exists). Against a name-keyed discriminator this test
// is RED: the arm would reroute to bare RC-field names and the qualified
// datum keys (and dup-name discrimination) would vanish.
func TestStarMetadataUserColumnNamedOrdinal(t *testing.T) {
	t.Parallel()
	b := metadata.NewSchemaTemplateBuilder().SetName("w5starmeta")
	b.AddTable("PZERO", []metadata.ColumnSpec{
		metadata.NewColumnSpec("PID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("_0", api.NewLongType(true), 2),
	}, []string{"PID"})
	b.AddTable("QZERO", []metadata.ColumnSpec{
		metadata.NewColumnSpec("QID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("QV", api.NewLongType(true), 2),
	}, []string{"QID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()
	plan, perr := embedded.PlanRecordQueryWithMetadata(
		`SELECT * FROM PZERO, QZERO`, md, nil)
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	defs := embedded.ResultColumnDefsForPlan(plan, md)
	if len(defs) != 4 {
		t.Fatalf("got %d columns, want 4: %+v", len(defs), defs)
	}
	// The merge path qualifies column NAMES under the leg aliases (the datum
	// keys downstream reads resolve); the RV arm emits bare RC field names
	// and can never produce a qualified Name. One qualified Name proves the
	// merge path ran despite the `_0` column.
	qualified := false
	for _, d := range defs {
		if strings.Contains(d.Name, ".") {
			qualified = true
			break
		}
	}
	if !qualified {
		t.Fatalf("no alias-qualified column Name in %+v — the ordinal-top arm misfired on the user column named _0 (the name-keyed leak discriminator)", defs)
	}
}

// buildW5StructStarMetadata builds a DISJOINT two-table schema where the
// array column's element is a STRUCT (repeated message field) — the
// multi-source gathered star over it exposes the element column's TYPE
// metadata.
func buildW5StructStarMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("w5_struct_star_test.proto"),
		Package: proto.String("fdb.test.w5structstar"),
		Syntax:  proto.String("proto2"),
	}
	sitem := &descriptorpb.DescriptorProto{
		Name: proto.String("WItem"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("SKU"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
		},
	}
	ws := &descriptorpb.DescriptorProto{
		Name: proto.String("WS"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("WID"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			},
			{
				Name: proto.String("SITEMS"), Number: proto.Int32(2),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".fdb.test.w5structstar.WItem"),
			},
		},
	}
	wx := &descriptorpb.DescriptorProto{
		Name: proto.String("WX"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("XID"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			},
		},
	}
	union := &descriptorpb.DescriptorProto{
		Name: proto.String("UnionDescriptor"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("_WS"), Number: proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".fdb.test.w5structstar.WS"),
			},
			{
				Name: proto.String("_WX"), Number: proto.Int32(2),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".fdb.test.w5structstar.WX"),
			},
		},
	}
	fdp.MessageType = []*descriptorpb.DescriptorProto{sitem, ws, wx, union}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	mdBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mdBuilder.SetSplitLongRecords(false)
	mdBuilder.SetStoreRecordVersions(false)
	mdBuilder.SetVersion(1)
	mdBuilder.SetRecordCountKey(recordlayer.RecordTypeKey())
	mdBuilder.GetRecordType("WS").SetPrimaryKey(recordlayer.Field("WID"))
	mdBuilder.GetRecordType("WX").SetPrimaryKey(recordlayer.Field("XID"))
	md, err := mdBuilder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

// TestStarMetadataStructElementType pins that the gathered multi-source
// star's STRUCT-typed element column reports STRUCT, never the BIGINT
// fallback (valueTypeName had no TypeCodeRecord case).
func TestStarMetadataStructElementType(t *testing.T) {
	t.Parallel()
	md := buildW5StructStarMetadata(t)
	plan, perr := embedded.PlanRecordQueryWithMetadata(
		`SELECT * FROM WS, WX, WS."SITEMS" AS "EL"`, md, nil)
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	labels := embedded.ResultColumnLabelsForPlan(plan, md)
	types := embedded.ResultColumnTypesForPlan(plan, md)
	if fmt.Sprintf("%v", labels) != "[WID SITEMS XID EL]" {
		t.Fatalf("labels = %v, want [WID SITEMS XID EL]", labels)
	}
	if types[len(types)-1] != "STRUCT" {
		t.Fatalf("element column type = %q (all types %v), want STRUCT — the TypeCodeRecord case is missing and the BIGINT fallback silently mistyped the struct element", types[len(types)-1], types)
	}
}

// buildTwinLayoutStarMetadata builds two tables with the IDENTICAL declared
// column list — `ID`, `SITEMS` — differing only in the array column's ELEMENT
// kind: `WS.SITEMS` is repeated int64, `WX.SITEMS` is repeated message.
//
// Everything a leaf name or a row-shape signature could say about the array
// column is therefore the same on both legs. Only the leg the reference
// actually reads distinguishes them.
func buildTwinLayoutStarMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("twin_layout_star_test.proto"),
		Package: proto.String("fdb.test.twinlayoutstar"),
		Syntax:  proto.String("proto2"),
	}
	titem := &descriptorpb.DescriptorProto{
		Name: proto.String("TItem"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("SKU"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
		},
	}
	table := func(name string, structElement bool) *descriptorpb.DescriptorProto {
		items := &descriptorpb.FieldDescriptorProto{
			Name: proto.String("SITEMS"), Number: proto.Int32(2),
			Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
			Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
		}
		if structElement {
			items.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
			items.TypeName = proto.String(".fdb.test.twinlayoutstar.TItem")
		}
		return &descriptorpb.DescriptorProto{
			Name: proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name: proto.String("ID"), Number: proto.Int32(1),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				},
				items,
			},
		}
	}
	union := &descriptorpb.DescriptorProto{
		Name: proto.String("UnionDescriptor"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("_WS"), Number: proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".fdb.test.twinlayoutstar.WS"),
			},
			{
				Name: proto.String("_WX"), Number: proto.Int32(2),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".fdb.test.twinlayoutstar.WX"),
			},
		},
	}
	fdp.MessageType = []*descriptorpb.DescriptorProto{
		titem, table("WS", false), table("WX", true), union,
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	mdBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mdBuilder.SetSplitLongRecords(false)
	mdBuilder.SetStoreRecordVersions(false)
	mdBuilder.SetVersion(1)
	mdBuilder.SetRecordCountKey(recordlayer.RecordTypeKey())
	mdBuilder.GetRecordType("WS").SetPrimaryKey(recordlayer.Field("ID"))
	mdBuilder.GetRecordType("WX").SetPrimaryKey(recordlayer.Field("ID"))
	md, err := mdBuilder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

// TestStarMetadataTwinLayoutTypesTheUnnestedLeg is the IDENTITY dimension of
// the element-type derivation: the same leaf name, in two legs whose row
// layouts are indistinguishable, with two different element kinds behind it.
//
// The element column's type used to be resolved by matching the collection
// reference's DISPLAY name against every leg descriptor of the join, first
// match wins — so the answer was decided by which leg the planner happened to
// put on the outer side, not by which leg the unnest reads. Half the cases
// below reported the OTHER leg's element kind: a struct element typed BIGINT,
// the exact mistyping TestStarMetadataStructElementType pins for a disjoint
// schema, re-armed by a duplicate name.
//
// The cases are chosen so that no single-element fix passes:
//
//   - Both leg ORDERS appear for the same unnest target, so a fix that
//     narrows the search to one leg but picks the wrong one fails half of them.
//   - Both TARGETS appear, so answering STRUCT unconditionally fails.
//   - The two layouts are structurally IDENTICAL, so choosing the descriptor
//     by row shape alone is not enough — the reference's leg has to be the
//     thing consulted.
func TestStarMetadataTwinLayoutTypesTheUnnestedLeg(t *testing.T) {
	t.Parallel()
	md := buildTwinLayoutStarMetadata(t)
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "struct-element leg planned outer",
			sql:  `SELECT * FROM WS AS A, WX AS B, B."SITEMS" AS "EL"`,
			want: "STRUCT",
		},
		{
			name: "struct-element leg planned inner",
			sql:  `SELECT * FROM WX AS B, WS AS A, B."SITEMS" AS "EL"`,
			want: "STRUCT",
		},
		{
			name: "struct-element leg unaliased",
			sql:  `SELECT * FROM WS, WX, WX."SITEMS" AS "EL"`,
			want: "STRUCT",
		},
		{
			name: "scalar-element leg planned outer",
			sql:  `SELECT * FROM WX AS B, WS AS A, A."SITEMS" AS "EL"`,
			want: "BIGINT",
		},
		{
			name: "scalar-element leg planned inner",
			sql:  `SELECT * FROM WS AS A, WX AS B, A."SITEMS" AS "EL"`,
			want: "BIGINT",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, perr := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
			if perr != nil {
				t.Fatalf("plan: %v", perr)
			}
			labels := embedded.ResultColumnLabelsForPlan(plan, md)
			types := embedded.ResultColumnTypesForPlan(plan, md)
			if fmt.Sprintf("%v", labels) != "[ID SITEMS ID SITEMS EL]" {
				t.Fatalf("labels = %v, want [ID SITEMS ID SITEMS EL] — the gathered-star arm did not fire, so the element type below proves nothing", labels)
			}
			if got := types[len(types)-1]; got != tc.want {
				t.Fatalf("element column type = %q (all types %v), want %q — the unnested array column was resolved by a leaf name BOTH legs declare, instead of by its ordinal in the leg the Explode actually reads",
					got, types, tc.want)
			}
		})
	}
}

// TestStarMetadataPlainThreeWayKeepsQualifiedNames pins that a PLAIN 3-way
// join's partition can also leave a positional-merge subplan — with NO
// unnest and NO `_N` leak in the merged columns — so the structural leg
// check alone is not enough: it would reroute the SELECT * metadata to bare
// RC names, dropping the alias-qualified duplicate-name keys
// (`TA.K`/`TB.K`/`TC.K`) by-name reads rely on. The arm must ALSO require
// the gathered-unnest signature (an Explode-bearing FlatMap leg).
func TestStarMetadataPlainThreeWayKeepsQualifiedNames(t *testing.T) {
	t.Parallel()
	b := metadata.NewSchemaTemplateBuilder().SetName("w5star3way")
	for _, tbl := range []string{"TA", "TB", "TC"} {
		b.AddTable(tbl, []metadata.ColumnSpec{
			metadata.NewColumnSpec(tbl+"ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		}, []string{tbl + "ID"})
	}
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()
	plan, perr := embedded.PlanRecordQueryWithMetadata(
		// The CROSS form plans NLJ-shaped (the equijoin form goes through the
		// correlated FlatMap path, whose bare-name fold is a separate,
		// pre-existing issue, not this arm's regression surface).
		`SELECT * FROM TA, TB, TC`, md, nil)
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	defs := embedded.ResultColumnDefsForPlan(plan, md)
	qualifiedK := 0
	for _, d := range defs {
		up := strings.ToUpper(d.Name)
		if strings.HasSuffix(up, ".K") {
			qualifiedK++
		}
	}
	if qualifiedK < 2 {
		t.Fatalf("only %d alias-qualified K columns in %+v — the ordinal-top arm misfired on a plain 3-way join (positional-merge leg without any unnest) and dropped the qualified duplicate-name keys", qualifiedK, defs)
	}
}
