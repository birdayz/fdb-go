package sqldriver_test

// The null-born nullability upgrade across join legs, pinned on the ONE shape
// that can reach it: record-layer metadata carrying a proto2 REQUIRED field.
//
// WHY THESE ARE METADATA ASSERTIONS AND NOT ROW ASSERTIONS. No SQL query can
// reach this code path, so there is no query whose ROWS could differ. The
// upgrade is guarded by a projected column deriving api.ColumnNoNulls, and on
// the projection path that has exactly one source: a proto field whose
// cardinality is REQUIRED (cascades_generator.go, deriveProjectionColumnDef).
// The SQL DDL emitter never emits one — metadata/builder.go's addField assigns
// LABEL_OPTIONAL to every scalar and struct and LABEL_REPEATED to a flat array,
// and has no LABEL_REQUIRED branch at all, mirroring Java's
// Type.Record.defineProtoType. That emitter property is not left as prose here:
// TestDDLEmitterNeverEmitsRequired (pkg/relational/core/metadata) pins it and
// names this test as one of the three things that go live if it ever changes.
// MEASURED over the full factorycorpus at the
// commit that added this test: 216,848 projection slots derived, ZERO reaching
// the NoNulls guard, while 58,232 of those slots came from plans whose
// null-supplying (DefaultOnEmpty) set was NON-EMPTY. That second number is what
// makes the first meaningful: the outer-join population is richly present and
// the guard still never fires.
//
// So a reader must NOT read this file as "a shape that happens to be untested".
// It is a shape that is UNREACHABLE through SQL and reachable through the
// record-layer API, which accepts any proto2 descriptor — including REQUIRED
// fields, which Java's record layer fully supports. Go and Java apps share a
// cluster, so that metadata is a real input even though our DDL cannot write
// it. The harness (embedded.ResultColumnDefsForPlan) runs the SAME production
// derivation the driver hands the client, without needing FDB or a seeded row.
//
// The defect being pinned: the projected reference `B.VAL` used to be resolved
// by COMPOSING the string "B.VAL" and handing it to descriptorForColumn, which
// matches by BARE name over every join-leaf descriptor and tie-breaks the
// qualifier against the DESCRIPTOR's name — the table, never a correlation. So
// for `LEFTT A LEFT JOIN RIGHTT B` the tie-break could not fire, first-match
// answered LEFTT, and RIGHTT's null-born upgrade never ran: B.VAL reported
// NoNulls on a column whose unmatched rows serve SQL NULL. The reference is now
// resolved structurally, by leg plan plus leg-relative ordinal, which is how
// Java identifies it (ResolvedAccessor compares getOrdinal() alone —
// FieldValue.java:684,:689).

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// buildCrossLegRequiredValMetadata builds two record types that AGREE on their
// shared column VAL — same type, same REQUIRED cardinality — so the descriptor
// agreement gate keeps first-match and cannot separate them. Only the join
// shape distinguishes the legs, which is the whole point: the answer differs
// per result slot while both slots hand the name lookup the same candidates.
//
// Built as a raw proto2 descriptor rather than through a schema template
// because the template path cannot express REQUIRED (see the file-top comment).
func buildCrossLegRequiredValMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	req := descriptorpb.FieldDescriptorProto_LABEL_REQUIRED
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE

	field := func(name string, num int32, label descriptorpb.FieldDescriptorProto_Label) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num),
			Label: label.Enum(), Type: i64.Enum(),
		}
	}
	unionField := func(name string, num int32, typeName string) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num),
			Label: opt.Enum(), Type: msg.Enum(), TypeName: proto.String(typeName),
		}
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("cross_leg_required_val.proto"),
		Package: proto.String("xlegreq"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("LEFTT"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("LID", 1, req),
					// The column under test on the PRESERVED leg.
					field("VAL", 2, req),
				},
			},
			{
				Name: proto.String("RIGHTT"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("RID", 1, req),
					// The column under test on the NULL-SUPPLYING leg. Same
					// name, same type, same cardinality as LEFTT.VAL.
					field("VAL", 2, req),
				},
			},
			{
				Name: proto.String("RecordTypeUnion"),
				Field: []*descriptorpb.FieldDescriptorProto{
					unionField("_LEFTT", 1, ".xlegreq.LEFTT"),
					unionField("_RIGHTT", 2, ".xlegreq.RIGHTT"),
				},
			},
		},
	}

	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build file descriptor: %v", err)
	}
	b := recordlayer.NewRecordMetaDataBuilder().
		SetRecordsWithUnionName(fd, "RecordTypeUnion")
	b.GetRecordType("LEFTT").SetRecordTypeKey(int64(0)).SetPrimaryKey(recordlayer.Field("LID"))
	b.GetRecordType("RIGHTT").SetRecordTypeKey(int64(1)).SetPrimaryKey(recordlayer.Field("RID"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

// TestCrossLegNullBorn_RequiredColumnOnNullSupplyingLeg is the pin. Both legs
// declare VAL as REQUIRED, so both slots start at NoNulls and the descriptor
// agreement gate cannot tell the legs apart. The correct answers differ per
// slot, and only a structural (leg plan + leg-relative ordinal) resolution can
// produce them:
//
//   - A.VAL sits on the PRESERVED leg of a LEFT JOIN. It is never null-extended,
//     so its REQUIRED cardinality stands: NoNulls.
//   - B.VAL sits on the NULL-SUPPLYING leg. An unmatched left row pads it with
//     SQL NULL, so the metadata must report Nullable regardless of the proto's
//     REQUIRED — which is what Java reports off the flowed result type.
//
// Both a VALUE and the RELATIONSHIP are asserted: a test that only checked the
// two slots DIFFER would pass against any mutation that moved both together.
func TestCrossLegNullBorn_RequiredColumnOnNullSupplyingLeg(t *testing.T) {
	t.Parallel()
	md := buildCrossLegRequiredValMetadata(t)
	plan, perr := embedded.PlanRecordQueryWithMetadata(
		`SELECT A.VAL, B.VAL FROM LEFTT A LEFT JOIN RIGHTT B ON B.RID = A.LID`, md, nil)
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	defs := embedded.ResultColumnDefsForPlan(plan, md)
	if len(defs) != 2 {
		t.Fatalf("got %d columns, want 2: %+v", len(defs), defs)
	}

	// The preserved leg keeps its declared NOT NULL.
	if defs[0].Nullable != api.ColumnNoNulls {
		t.Errorf("A.VAL: nullable=%d, want ColumnNoNulls(%d) — LEFTT.VAL is proto REQUIRED "+
			"and the PRESERVED leg of a LEFT JOIN is never null-extended; defs=%+v",
			defs[0].Nullable, api.ColumnNoNulls, defs)
	}
	// The null-supplying leg is null-extended by the outer join.
	if defs[1].Nullable != api.ColumnNullable {
		t.Errorf("B.VAL: nullable=%d, want ColumnNullable(%d) — RIGHTT.VAL is on the "+
			"NULL-SUPPLYING leg of a LEFT JOIN, so unmatched left rows serve SQL NULL. "+
			"Reporting NoNulls is the composed-name defect: resolving the reference by "+
			"the string \"B.VAL\" matches VAL in BOTH leaf descriptors and tie-breaks the "+
			"qualifier against the DESCRIPTOR's name (a table, never a correlation), so "+
			"first-match answered LEFTT and RIGHTT's null-born upgrade never ran; defs=%+v",
			defs[1].Nullable, api.ColumnNullable, defs)
	}
	// The relationship, asserted separately from the values above: the two
	// slots must not agree, because agreeing is exactly what first-match does.
	if defs[0].Nullable == defs[1].Nullable {
		t.Errorf("both slots report nullable=%d — the two legs were not distinguished at "+
			"all; the per-slot answer must differ (NoNulls, Nullable); defs=%+v",
			defs[0].Nullable, defs)
	}
}
