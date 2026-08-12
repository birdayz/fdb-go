package metadata

// The pin for a load-bearing NEGATIVE result: the SQL DDL descriptor emitter
// never produces a proto2 REQUIRED field.
//
// THE CLAIM IS STRUCTURAL, NOT SAMPLED. An earlier wording here said "for ANY
// column shape it can express", resting universality on the template below —
// which is a sample (8 columns, no boolean, no enum, no nullable integer), so
// it could not carry that weight. The real ground is that addField cannot
// express REQUIRED at all: it contains exactly THREE label assignments and no
// REQUIRED branch —
//
//	builder.go:951  LABEL_OPTIONAL   nullable array (wrapper message)
//	builder.go:957  LABEL_REPEATED   non-nullable array (flat repeated)
//	builder.go:963  LABEL_OPTIONAL   everything else — scalars and structs
//
// so no input can steer it to REQUIRED, whatever column kinds exist. The
// template below CORROBORATES that over the shapes it covers; it does not
// establish it. That ordering also makes this pin robust against a column kind
// added tomorrow: the structural claim still holds, and the template not
// covering the new kind is no longer a hole in the argument.
//
// WHY THIS IS PINNED RATHER THAN LEFT AS PROSE. Several comments elsewhere rest
// on it as a fact about reachability, not as a stylistic note:
//
//   - the null-born nullability upgrade in cascades_generator.go
//     (deriveColumnsFromProjection) is gated on a projected column deriving
//     api.ColumnNoNulls, whose ONLY source on that path is a REQUIRED field;
//   - TestFDB_CrossLegAgreementGate_NullBornNotCovered pins the descriptor
//     agreement gate's cross-leg hole as UNREACHABLE through the driver, and
//     that unreachability is this emitter property and nothing else;
//   - TestCrossLegNullBorn_RequiredColumnOnNullSupplyingLeg justifies being a
//     METADATA test rather than a row test on the same property, and says so.
//
// Each of those reads as "covered" while being vacuous, which is the expensive
// direction. If this emitter ever starts emitting REQUIRED, all three become
// live and one of them silently stops testing what it claims — so the change
// must be loud HERE, at the source, rather than inferred later from a puzzling
// green somewhere else.
//
// Java parity: the emitter mirrors Type.Record.defineProtoType, which passes
// LABEL_OPTIONAL always. Note this pins the EMITTER only. Java's record layer
// accepts a pre-existing proto2 REQUIRED field as first-class,
// evolution-protected metadata — MetaDataEvolutionValidator forbids ADDING one
// (:264) and equally forbids RELAXING one that exists (:303), and Type.java:2872
// derives nullability as !fieldDescriptor.isRequired(). So REQUIRED metadata is
// a real input a shared cluster can hand us through the record-layer API; what
// cannot mint one is our DDL. Those are different claims and only the second is
// pinned here.

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/pkg/relational/api"
)

// TestDDLEmitterNeverEmitsRequired walks every field of every message in the
// emitted descriptor for a template covering nullable and non-nullable scalars,
// both array flavours, and nested structs, and asserts none is REQUIRED.
//
// MUTATION-CHECKED, and the count only means anything WITH the mutation that
// produced it. Two different edits to this same one line yield two different
// counts, so a bare "reddens at N fields" is unreproducible; both are recorded
// here with the edit that produces them:
//
//	CANONICAL. builder.go:963, LABEL_OPTIONAL -> LABEL_REQUIRED
//	           (unconditional): reddens at 10 fields —
//	           T.ID, T.S_NULL, T.S_NOTNULL, T.B_NOTNULL, T.D_NOTNULL, T.REC,
//	           OUTER.ON, OUTER.OI, INNER.IX, INNER.IY.
//
//	NARROWER.  builder.go:963 gated on !dt.IsNullable(): reddens at 6 —
//	           T.ID, T.S_NOTNULL, T.B_NOTNULL, T.D_NOTNULL, OUTER.ON, INNER.IY.
//	           It misses the nullable scalar (T.S_NULL) and the three
//	           message-typed struct columns (T.REC, OUTER.OI, INNER.IX), which
//	           are non-array and so take the same branch.
//
// The unconditional one is canonical: it is the minimal edit to the line the
// claim is about, it needs no added conditional, and its 10 fields strictly
// contain the other's 6. Reproduce with that one and you get 10.
func TestDDLEmitterNeverEmitsRequired(t *testing.T) {
	t.Parallel()

	inner := api.NewStructType("INNER", []api.StructField{
		api.NewStructField("IX", api.NewLongType(true), 0),
		api.NewStructField("IY", api.NewStringType(false), 1),
	}, true)
	outer := api.NewStructType("OUTER", []api.StructField{
		api.NewStructField("ON", api.NewLongType(false), 0),
		api.NewStructField("OI", inner, 1),
		// Both array flavours inside a struct: the nullable one becomes a
		// wrapper message, the non-nullable one a flat repeated field.
		api.NewStructField("OARRN", api.NewArrayType(api.NewIntegerType(false), true), 2),
		api.NewStructField("OARRF", api.NewArrayType(api.NewIntegerType(false), false), 3),
	}, true)

	b := NewSchemaTemplateBuilder().SetName("emitreq")
	b.AddTable("T", []ColumnSpec{
		// The primary key column: the shape most likely to be "helpfully"
		// promoted to REQUIRED by a future change, since it can never be NULL
		// in practice. It must still be OPTIONAL on the wire.
		NewColumnSpec("ID", api.NewLongType(false), 1),
		NewColumnSpec("S_NULL", api.NewStringType(true), 2),
		NewColumnSpec("S_NOTNULL", api.NewStringType(false), 3),
		NewColumnSpec("B_NOTNULL", api.NewBytesType(false), 4),
		NewColumnSpec("D_NOTNULL", api.NewDoubleType(false), 5),
		NewColumnSpec("ARR_NULLABLE", api.NewArrayType(api.NewLongType(false), true), 6),
		NewColumnSpec("ARR_FLAT", api.NewArrayType(api.NewLongType(false), false), 7),
		NewColumnSpec("REC", outer, 8),
	}, []string{"ID"})

	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build template: %v", err)
	}
	md := tmpl.Underlying()
	fd := md.FileDescriptor()
	if fd == nil {
		t.Fatal("no file descriptor on the built metadata — the walk below would " +
			"pass vacuously, which is the failure this test exists to prevent")
	}

	// Count what we actually inspected: a walk that reaches nothing reports
	// exactly like a walk that found nothing wrong.
	fields := 0
	msgs := 0
	var walk func(protoreflect.MessageDescriptors)
	walk = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			m := ms.Get(i)
			msgs++
			for j := 0; j < m.Fields().Len(); j++ {
				f := m.Fields().Get(j)
				fields++
				if f.Cardinality() == protoreflect.Required {
					t.Errorf("%s.%s is REQUIRED. The SQL DDL emitter is not supposed to be "+
						"able to produce a REQUIRED field at all: addField holds exactly "+
						"three label assignments (builder.go:951 OPTIONAL for a nullable "+
						"array, :957 REPEATED for a flat array, :963 OPTIONAL for every "+
						"scalar and struct) and no REQUIRED branch, so this is a change to "+
						"that function, not an unlucky column shape. Three things depend "+
						"on it:\n"+
						"  - the null-born nullability upgrade in deriveColumnsFromProjection "+
						"is gated on a column deriving api.ColumnNoNulls, which this re-arms;\n"+
						"  - TestFDB_CrossLegAgreementGate_NullBornNotCovered pins the "+
						"cross-leg agreement-gate hole as UNREACHABLE, and this is what "+
						"makes it reachable;\n"+
						"  - TestCrossLegNullBorn_RequiredColumnOnNullSupplyingLeg justifies "+
						"being metadata-only rather than row-asserting on this same fact.\n"+
						"If the emitter change is intended, all three need revisiting — do "+
						"not relax this assertion on its own.", m.FullName(), f.Name())
				}
			}
			walk(m.Messages())
		}
	}
	walk(fd.Messages())

	// Vacuity guards. These are floors, not exact counts: the emitter is free
	// to add wrapper messages. They exist so a walk that silently stopped
	// reaching the descriptor cannot report success.
	if msgs < 3 {
		t.Errorf("walked only %d message(s); expected at least 3 (the table, the "+
			"union, and at least one struct/wrapper). The REQUIRED check above "+
			"holds VACUOUSLY over a population this small", msgs)
	}
	if fields < 12 {
		t.Errorf("inspected only %d field(s); the template above declares 8 columns "+
			"plus nested struct members, so a count this low means the walk did not "+
			"reach the nested messages and the REQUIRED check is not covering them",
			fields)
	}
}

// TestDDLEmitterNeverEmitsRequired_PositiveControl proves the assertion above
// can FAIL — that protoreflect.Required is a value this walk would actually
// catch, rather than one nothing can ever produce. Without this, a typo in the
// cardinality comparison would leave the pin permanently, silently green.
func TestDDLEmitterNeverEmitsRequired_PositiveControl(t *testing.T) {
	t.Parallel()
	required := descriptorpb.FieldDescriptorProto_LABEL_REQUIRED
	if protoreflect.Cardinality(required) != protoreflect.Required {
		t.Fatalf("LABEL_REQUIRED does not map to protoreflect.Required (%v vs %v); the "+
			"emitter pin's comparison cannot fire and is therefore not testing anything",
			protoreflect.Cardinality(required), protoreflect.Required)
	}
}
