package sqldriver_test

// RFC-173 W5 — SELECT * metadata pins for the two review findings on the
// ordinal-top derivation arm (deriveColumnsFromJoin):
//
//  1. The leak discriminator must be STRUCTURAL (a leg subplan carrying the
//     S3 positional-merge RC), never name-based: a user column literally
//     named `_0` is a legal identifier, and the name-keyed check rerouted a
//     today-working gated join's metadata off the qualified merge path.
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

// TestRFC173W5_StarMetadata_UserColumnNamedOrdinal pins finding 1: a plain
// gated 2-way join whose schema carries a column literally named "_0" keeps
// the MERGE path's alias-qualified column metadata — the ordinal-top arm must
// not fire (no positional-merge leg exists). Against the retired name-keyed
// discriminator this test is RED: the arm rerouted to bare RC-field names and
// the qualified datum keys (and dup-name discrimination) vanished.
func TestRFC173W5_StarMetadata_UserColumnNamedOrdinal(t *testing.T) {
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
// metadata (finding 2).
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

// TestRFC173W5_StarMetadata_StructElementType pins finding 2: the gathered
// multi-source star's STRUCT-typed element column reports STRUCT, never the
// BIGINT fallback (valueTypeName had no TypeCodeRecord case).
func TestRFC173W5_StarMetadata_StructElementType(t *testing.T) {
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
