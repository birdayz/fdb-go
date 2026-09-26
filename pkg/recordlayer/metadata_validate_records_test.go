package recordlayer

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
)

// validateRecordsFile builds a records file whose union "RecordTypeUnion" holds
// one field per entry of unionFields; mutate edits the file before it is built.
func validateRecordsFile(t *testing.T, mutate func(*descriptorpb.FileDescriptorProto)) *RecordMetaDataBuilder {
	t.Helper()
	field := func(name string, number int32, kind descriptorpb.FieldDescriptorProto_Type, target string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(number), Type: kind.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
		if target != "" {
			f.TypeName = proto.String(target)
		}
		return f
	}
	unionOpts := &descriptorpb.MessageOptions{}
	proto.SetExtension(unionOpts, gen.E_Record, &gen.RecordTypeOptions{Usage: gen.RecordTypeOptions_UNION.Enum()})
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("validate_records.proto"), Package: proto.String("vr"), Syntax: proto.String("proto2"),
		Dependency: []string{"record_metadata_options.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("T"), Field: []*descriptorpb.FieldDescriptorProto{
				field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
			}},
			{Name: proto.String("Envelope"), Options: unionOpts, Field: []*descriptorpb.FieldDescriptorProto{
				field("_T", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T"),
			}},
		},
	}
	if mutate != nil {
		mutate(fdp)
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("building the test descriptor: %v", err)
	}
	// SetRecords finds the union as Java's setRecords does (fetchUnionDescriptor),
	// so a shape that renames the union is still found.
	b := NewRecordMetaDataBuilder().SetRecords(fd)
	if rt := b.recordTypes["T"]; rt != nil {
		rt.PrimaryKey = Field("id")
	}
	return b
}

// TestValidateRecordsMatchesJava drives every arm of the port of Java's
// RecordMetaDataBuilder.validateRecords (validateDataTypes + validateUnion) with the
// exact message Java raises; each shape is also measured against the JVM by the
// conformance spec "Java refuses the same records descriptors".
func TestValidateRecordsMatchesJava(t *testing.T) {
	t.Parallel()
	msg := func(fdp *descriptorpb.FileDescriptorProto, name string) *descriptorpb.DescriptorProto {
		for _, m := range fdp.MessageType {
			if m.GetName() == name {
				return m
			}
		}
		t.Fatalf("no message %s", name)
		return nil
	}
	addField := func(m *descriptorpb.DescriptorProto, name string, number int32, kind descriptorpb.FieldDescriptorProto_Type, target string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(number), Type: kind.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
		if target != "" {
			f.TypeName = proto.String(target)
		}
		m.Field = append(m.Field, f)
		return f
	}
	usage := func(u gen.RecordTypeOptions_Usage) *descriptorpb.MessageOptions {
		o := &descriptorpb.MessageOptions{}
		proto.SetExtension(o, gen.E_Record, &gen.RecordTypeOptions{Usage: u.Enum()})
		return o
	}
	cases := []struct {
		name   string
		mutate func(*descriptorpb.FileDescriptorProto)
		want   string // "" = builds
	}{
		{"valid", nil, ""},
		{"signed kinds", func(f *descriptorpb.FileDescriptorProto) {
			m := msg(f, "T")
			for i, k := range []descriptorpb.FieldDescriptorProto_Type{
				descriptorpb.FieldDescriptorProto_TYPE_INT32, descriptorpb.FieldDescriptorProto_TYPE_SINT32,
				descriptorpb.FieldDescriptorProto_TYPE_SINT64, descriptorpb.FieldDescriptorProto_TYPE_SFIXED32,
				descriptorpb.FieldDescriptorProto_TYPE_SFIXED64, descriptorpb.FieldDescriptorProto_TYPE_BOOL,
				descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_BYTES,
				descriptorpb.FieldDescriptorProto_TYPE_FLOAT, descriptorpb.FieldDescriptorProto_TYPE_DOUBLE,
			} {
				addField(m, "s"+k.String(), int32(10+i), k, "")
			}
		}, ""},
		{"uint32", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "T"), "u", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT32, "")
		}, "Field u in message vr.T has illegal unsigned type UINT32"},
		{"uint64", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "T"), "u", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT64, "")
		}, "Field u in message vr.T has illegal unsigned type UINT64"},
		{"fixed32", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "T"), "u", 2, descriptorpb.FieldDescriptorProto_TYPE_FIXED32, "")
		}, "Field u in message vr.T has illegal unsigned type FIXED32"},
		{"fixed64", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "T"), "u", 2, descriptorpb.FieldDescriptorProto_TYPE_FIXED64, "")
		}, "Field u in message vr.T has illegal unsigned type FIXED64"},
		{"unsigned in a nested message", func(f *descriptorpb.FileDescriptorProto) {
			inner := &descriptorpb.DescriptorProto{Name: proto.String("Inner")}
			addField(inner, "n", 1, descriptorpb.FieldDescriptorProto_TYPE_FIXED32, "")
			f.MessageType = append(f.MessageType, inner)
			addField(msg(f, "T"), "inner", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.Inner")
		}, "Field n in message vr.Inner has illegal unsigned type FIXED32"},
		{"repeated union field", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "Envelope"), "_T2", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T").
				Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		}, "Union field _T2 should not be repeated"},
		// fetchUnionDescriptor decides this before validateUnion runs: a message
		// named RecordTypeUnion beside a usage=UNION one is a second union.
		{"a second union candidate named RecordTypeUnion", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{Name: proto.String("RecordTypeUnion"), Options: usage(gen.RecordTypeOptions_RECORD)})
			addField(msg(f, "RecordTypeUnion"), "id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")
			addField(msg(f, "Envelope"), "_R", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.RecordTypeUnion")
		}, "Only one union descriptor is allowed"},
		// The one shape that reaches validateUnion's relational-union arm: the union
		// IS RecordTypeUnion and has a field of its own type.
		{"the relational union as its own union field", func(f *descriptorpb.FileDescriptorProto) {
			msg(f, "Envelope").Name = proto.String("RecordTypeUnion")
			addField(msg(f, "RecordTypeUnion"), "_R", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.RecordTypeUnion")
		}, "Union message type RecordTypeUnion cannot be a union field."},
		// Precedence: Java throws the first fault, data types before the union, and
		// the union's fields in field order.
		{"a non-message union field before a repeated one", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "Envelope"), "x", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")
			addField(msg(f, "Envelope"), "_T2", 3, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T").
				Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		}, "Union field x is not a message"},
		{"a repeated union field before a non-message one", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "Envelope"), "_T2", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.T").
				Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
			addField(msg(f, "Envelope"), "x", 3, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")
		}, "Union field _T2 should not be repeated"},
		{"an unsigned field before a union fault", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "T"), "u", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT32, "")
			addField(msg(f, "Envelope"), "x", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")
		}, "Field u in message vr.T has illegal unsigned type UINT32"},
		{"nested usage as a union field", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{Name: proto.String("N"), Options: usage(gen.RecordTypeOptions_NESTED)})
			addField(msg(f, "N"), "id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")
			addField(msg(f, "Envelope"), "_N", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".vr.N")
		}, "Union field _N has type N which is not a record"},
		{"record usage missing from the union", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{Name: proto.String("R"), Options: usage(gen.RecordTypeOptions_RECORD)})
			addField(msg(f, "R"), "id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")
		}, "Record message type R must be a union field."},
		{"non-message union field", func(f *descriptorpb.FileDescriptorProto) {
			addField(msg(f, "Envelope"), "x", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")
		}, "Union field x is not a message"},
		// What a Go build before this port stored and no longer loads: a union
		// found only by Go's old default name, UnionDescriptor, with no usage option.
		{"a union named UnionDescriptor without a usage option", func(f *descriptorpb.FileDescriptorProto) {
			msg(f, "Envelope").Name = proto.String("UnionDescriptor")
			msg(f, "UnionDescriptor").Options = nil
		}, "Union descriptor is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := validateRecordsFile(t, tc.mutate).Build()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want a build, got %v", err)
				}
				return
			}
			// Build returns the FIRST fault itself, as Java throws it: not a join
			// of every fault, whose first element would hide a reordering.
			md, ok := err.(*MetaDataError)
			if !ok {
				t.Fatalf("want the MetaDataError %q itself, got %T: %v", tc.want, err, err)
			}
			if md.Message != tc.want {
				t.Fatalf("message = %q, want Java's %q", md.Message, tc.want)
			}
		})
	}
}

// TestFetchUnionDescriptorMatchesJava drives every arm of the port of Java's
// fetchUnionDescriptor through SetRecords, the path that discovers the union.
func TestFetchUnionDescriptorMatchesJava(t *testing.T) {
	t.Parallel()
	usage := func(u gen.RecordTypeOptions_Usage) *descriptorpb.MessageOptions {
		o := &descriptorpb.MessageOptions{}
		proto.SetExtension(o, gen.E_Record, &gen.RecordTypeOptions{Usage: u.Enum()})
		return o
	}
	msg := func(name string, opts *descriptorpb.MessageOptions, target string) *descriptorpb.DescriptorProto {
		m := &descriptorpb.DescriptorProto{Name: proto.String(name), Options: opts}
		if target == "" {
			m.Field = []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("id"), Number: proto.Int32(1),
				Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			}}
		} else {
			m.Field = []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("_T"), Number: proto.Int32(1),
				Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(target),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			}}
		}
		return m
	}
	cases := []struct {
		name     string
		messages []*descriptorpb.DescriptorProto
		union    string // the union SetRecords must pick; "" = the error in want
		want     string
	}{
		{"usage=UNION", []*descriptorpb.DescriptorProto{msg("T", nil, ""), msg("Env", usage(gen.RecordTypeOptions_UNION), ".fu.T")}, "Env", ""},
		{"named RecordTypeUnion", []*descriptorpb.DescriptorProto{msg("T", nil, ""), msg("RecordTypeUnion", nil, ".fu.T")}, "RecordTypeUnion", ""},
		{"a message named UnionDescriptor without usage is no union", []*descriptorpb.DescriptorProto{msg("T", nil, ""), msg("UnionDescriptor", nil, ".fu.T")}, "", "Union descriptor is required"},
		{"two usage=UNION", []*descriptorpb.DescriptorProto{msg("T", nil, ""), msg("A", usage(gen.RecordTypeOptions_UNION), ".fu.T"), msg("B", usage(gen.RecordTypeOptions_UNION), ".fu.T")}, "", "Only one union descriptor is allowed"},
		{"usage=UNION and a RecordTypeUnion", []*descriptorpb.DescriptorProto{msg("T", nil, ""), msg("A", usage(gen.RecordTypeOptions_UNION), ".fu.T"), msg("RecordTypeUnion", nil, ".fu.T")}, "", "Only one union descriptor is allowed"},
		{"NESTED RecordTypeUnion", []*descriptorpb.DescriptorProto{msg("T", nil, ""), msg("A", usage(gen.RecordTypeOptions_UNION), ".fu.T"), msg("RecordTypeUnion", usage(gen.RecordTypeOptions_NESTED), "")}, "", "Message type RecordTypeUnion cannot have NESTED usage"},
		{"none", []*descriptorpb.DescriptorProto{msg("T", nil, "")}, "", "Union descriptor is required"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fdp := &descriptorpb.FileDescriptorProto{
				Name: proto.String(fmt.Sprintf("fetch_union_%d.proto", i)), Package: proto.String(fmt.Sprintf("fu%d", i)),
				Syntax: proto.String("proto2"), Dependency: []string{"record_metadata_options.proto"}, MessageType: tc.messages,
			}
			for _, m := range fdp.MessageType {
				for _, f := range m.Field {
					if f.TypeName != nil {
						f.TypeName = proto.String(fmt.Sprintf(".fu%d.T", i))
					}
				}
			}
			fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
			if err != nil {
				t.Fatalf("descriptor: %v", err)
			}
			b := NewRecordMetaDataBuilder().SetRecords(fd)
			if tc.union != "" {
				if b.unionDescriptor == nil || string(b.unionDescriptor.Name()) != tc.union {
					t.Fatalf("union = %v, want %s (errors %v)", b.unionDescriptor, tc.union, b.buildErrors)
				}
				return
			}
			_, err = b.Build()
			var md *MetaDataError
			if !errors.As(err, &md) || md.Message != tc.want {
				t.Fatalf("got %v, want Java's %q", err, tc.want)
			}
		})
	}
}

// SetRecordsWithUnionName finds the union as Java does and refuses a name that is
// not the union it found: metadata built around another message would be
// refused by RecordMetaDataFromProto when this binary loads it back.
func TestSetRecordsWithUnionNameRefusesAnotherUnion(t *testing.T) {
	t.Parallel()
	fd := validateRecordsFile(t, nil).fileDescriptor
	if fd == nil {
		t.Fatal("fixture built no records file")
	}
	_, err := NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, "T").Build()
	var me *MetaDataError
	if !errors.As(err, &me) || me.Message != "union message T is not the union descriptor of the records file (found Envelope)" {
		t.Fatalf("SetRecordsWithUnionName(T) = %v, want the found-union refusal", err)
	}
	b := NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, "Envelope")
	b.GetRecordType("T").SetPrimaryKey(Field("id"))
	if _, err := b.Build(); err != nil {
		t.Fatalf("SetRecordsWithUnionName(Envelope), the found union: %v", err)
	}
	// An empty name names no union; SetRecords is the call that takes the found one.
	_, err = NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, "").Build()
	if !errors.As(err, &me) || me.Message != "union message name is empty" {
		t.Fatalf("SetRecordsWithUnionName(\"\") = %v, want the empty-name refusal", err)
	}
}

// Metadata a Go build before the validateRecords port STORED, loaded back through
// RecordMetaDataFromProto (the metadata store's and the relational catalog's
// loader): each shape the port now refuses is refused on load with the target's
// message, so a store holding it stops opening (the upgrade path is in
// rfcs/257-java-upgrade-audit/ws-j-design.md section 4c).
func TestPreviouslyStoredMetaDataIsRefusedOnLoad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*descriptorpb.FileDescriptorProto)
		want   string
	}{
		{"union named UnionDescriptor without a usage option", func(f *descriptorpb.FileDescriptorProto) {
			for _, m := range f.MessageType {
				if m.GetName() == "Envelope" {
					m.Name, m.Options = proto.String("UnionDescriptor"), nil
				}
			}
		}, "Union descriptor is required"},
		{"no union at all", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = f.MessageType[:1]
		}, "Union descriptor is required"},
		{"an unsigned field", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[0].Field = append(f.MessageType[0].Field, &descriptorpb.FieldDescriptorProto{
				Name: proto.String("u"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum(),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			})
		}, "Field u in message vr.T has illegal unsigned type UINT32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fdp := protodesc.ToFileDescriptorProto(validateRecordsFile(t, nil).fileDescriptor)
			tc.mutate(fdp)
			stored := &gen.MetaData{
				Records:     fdp,
				RecordTypes: []*gen.RecordType{{Name: proto.String("T"), PrimaryKey: Field("id").ToKeyExpression()}},
				Version:     proto.Int32(1),
			}
			_, err := RecordMetaDataFromProto(stored)
			md, ok := err.(*MetaDataError)
			if !ok || md.Message != tc.want {
				t.Fatalf("RecordMetaDataFromProto = %v (%T), want the MetaDataError %q", err, err, tc.want)
			}
		})
	}
}

// TestRecordsFileWithALegacyNamedUnionDescriptorLoadsAsTheTargetLoadsIt pins
// shape (d) of rfcs/257-java-upgrade-audit/ws-j-design.md section 4c: a records
// file with a message named UnionDescriptor that is not the union
// fetchUnionDescriptor finds. Before the upgrade Go took that message for the
// union; the target does not, and neither does Go now, on any path. Data a
// pre-release Go build framed by it is not supported (RFC-257, "Verification and
// review gates" item 9), so every shape loads and builds. (A RecordTypeUnion
// beside a usage=UNION message never gets this far: the target's own search
// refuses it, "Only one union descriptor is allowed", which the conformance spec
// "Java refuses the same records descriptors" pins.)
func TestRecordsFileWithALegacyNamedUnionDescriptorLoadsAsTheTargetLoadsIt(t *testing.T) {
	t.Parallel()
	message := func(name string, withUsage bool) *descriptorpb.DescriptorProto {
		m := &descriptorpb.DescriptorProto{Name: proto.String(name), Field: []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("_T"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), TypeName: proto.String(".vr.T"),
		}}}
		if withUsage {
			m.Options = &descriptorpb.MessageOptions{}
			proto.SetExtension(m.Options, gen.E_Record, &gen.RecordTypeOptions{Usage: gen.RecordTypeOptions_UNION.Enum()})
		}
		return m
	}
	scalarMessage := func(name string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("id"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}}}
	}
	withField := func(m *descriptorpb.DescriptorProto, name, typeName string) *descriptorpb.DescriptorProto {
		m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(int32(len(m.Field) + 1)), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), TypeName: proto.String(typeName),
		})
		return m
	}
	// repeated makes a message's last field repeated.
	repeated := func(m *descriptorpb.DescriptorProto) *descriptorpb.DescriptorProto {
		m.Field[len(m.Field)-1].Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		return m
	}
	for _, tc := range []struct {
		name          string
		messages      []*descriptorpb.DescriptorProto // beside the base file's T and usage=UNION Envelope
		dropEnvelope  bool
		legacy, found string   // empty legacy: the file loads on every path
		storedLoads   bool     // the stored path loads though the code path refuses
		recordTypes   []string // record types beside T, each keyed on id
		indexOn       string   // a record type given a value index on id
		heldBy        string   // the refusal's HeldBy: the holder reachable from the union found
	}{
		{name: "UnionDescriptor beside a usage=UNION message", messages: []*descriptorpb.DescriptorProto{message("UnionDescriptor", false)}, legacy: "UnionDescriptor", found: "Envelope"},
		{name: "UnionDescriptor beside RecordTypeUnion, no usage", messages: []*descriptorpb.DescriptorProto{message("UnionDescriptor", false), message("RecordTypeUnion", false)}, dropEnvelope: true, legacy: "UnionDescriptor", found: "RecordTypeUnion"},
		{name: "UnionDescriptor is the usage=UNION message", messages: []*descriptorpb.DescriptorProto{message("UnionDescriptor", true)}, dropEnvelope: true},
		{name: "RecordTypeUnion alone, no usage", messages: []*descriptorpb.DescriptorProto{message("RecordTypeUnion", false)}, dropEnvelope: true},
		{name: "neither legacy name"},
		// An index on a record type the loop made does not stop the pre-upgrade
		// loader.
		{name: "UnionDescriptor beside a usage=UNION message, T indexed", messages: []*descriptorpb.DescriptorProto{message("UnionDescriptor", false)}, legacy: "UnionDescriptor", found: "Envelope", indexOn: "T"},
		// A UnionDescriptor none of whose fields made a record type before the
		// upgrade framed nothing: it loads wherever it sits. That is a SQL table or
		// STRUCT of that name with scalar columns.
		{name: "a scalar UnionDescriptor as a union field's type", messages: []*descriptorpb.DescriptorProto{
			scalarMessage("UnionDescriptor"),
			withField(message("Envelope2", true), "_UnionDescriptor", ".vr.UnionDescriptor"),
		}, dropEnvelope: true, recordTypes: []string{"UnionDescriptor"}},
		{name: "a scalar UnionDescriptor as the type of a record type's field", messages: []*descriptorpb.DescriptorProto{
			scalarMessage("UnionDescriptor"),
			withField(scalarMessage("T2"), "s", ".vr.UnionDescriptor"),
			withField(message("Envelope2", true), "_T2", ".vr.T2"),
		}, dropEnvelope: true, recordTypes: []string{"T2"}},
		{name: "a scalar UnionDescriptor as the type of a message the union does not reach", messages: []*descriptorpb.DescriptorProto{
			scalarMessage("UnionDescriptor"),
			withField(scalarMessage("Holder"), "s", ".vr.UnionDescriptor"),
		}},
		// The pre-upgrade loop tried the name first: `_X` named a record type only
		// when X is a top-level message, and was skipped otherwise whatever its
		// type, so a message-typed `_Nope` framed nothing ...
		{name: "a UnionDescriptor whose only message field is an unresolved `_X`", messages: []*descriptorpb.DescriptorProto{
			withField(scalarMessage("UnionDescriptor"), "_Nope", ".vr.T"),
		}},
		// ... and a scalar `_T` framed record type T.
		{name: "a UnionDescriptor with a scalar `_T`", messages: []*descriptorpb.DescriptorProto{
			{Name: proto.String("UnionDescriptor"), Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("_T"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			}}},
		}, legacy: "UnionDescriptor", found: "Envelope"},
		// One whose fields made record types may have framed records, wherever it
		// sits.
		{name: "a legacy UnionDescriptor held by an envelope message", messages: []*descriptorpb.DescriptorProto{
			message("UnionDescriptor", false),
			withField(&descriptorpb.DescriptorProto{Name: proto.String("Batch")}, "records", ".vr.UnionDescriptor"),
		}, legacy: "UnionDescriptor", found: "Envelope"},
		{name: "a legacy UnionDescriptor held by a record type", messages: []*descriptorpb.DescriptorProto{
			message("UnionDescriptor", false),
			withField(scalarMessage("Batch"), "records", ".vr.UnionDescriptor"),
			withField(message("Envelope2", true), "_Batch", ".vr.Batch"),
		}, dropEnvelope: true, legacy: "UnionDescriptor", found: "Envelope2", recordTypes: []string{"Batch"}, heldBy: "Batch"},
		// Held as an array's element: a repeated field, as the wrapper message
		// of a nullable ARRAY column holds its elements.
		{name: "a legacy UnionDescriptor held as an array's element", messages: []*descriptorpb.DescriptorProto{
			message("UnionDescriptor", false),
			repeated(withField(&descriptorpb.DescriptorProto{Name: proto.String("Array")}, "values", ".vr.UnionDescriptor")),
			withField(scalarMessage("Batch"), "items", ".vr.Array"),
			withField(message("Envelope2", true), "_Batch", ".vr.Batch"),
		}, dropEnvelope: true, legacy: "UnionDescriptor", found: "Envelope2", recordTypes: []string{"Batch"}, heldBy: "Array"},
		// Held two levels down, as a STRUCT nested in a STRUCT holds it.
		{name: "a legacy UnionDescriptor held through a nested message", messages: []*descriptorpb.DescriptorProto{
			message("UnionDescriptor", false),
			withField(&descriptorpb.DescriptorProto{Name: proto.String("Inner")}, "records", ".vr.UnionDescriptor"),
			withField(scalarMessage("Batch"), "inner", ".vr.Inner"),
			withField(message("Envelope2", true), "_Batch", ".vr.Batch"),
		}, dropEnvelope: true, legacy: "UnionDescriptor", found: "Envelope2", recordTypes: []string{"Batch"}, heldBy: "Inner"},
		// A table named UnionDescriptor with a column typed by another table, which
		// the target's DDL creates ("WS-J a column typed by a table").
		{name: "a table named UnionDescriptor with a table-typed column", messages: []*descriptorpb.DescriptorProto{
			withField(scalarMessage("UnionDescriptor"), "a", ".vr.T"),
			withField(message("Envelope2", true), "_UnionDescriptor", ".vr.UnionDescriptor"),
		}, dropEnvelope: true, legacy: "UnionDescriptor", found: "Envelope2", recordTypes: []string{"UnionDescriptor"}, heldBy: "Envelope2"},
		// The replay: the pre-upgrade loader refused a record type with no stored
		// primary key, and an index on a record type its loop did not make, so the
		// stored form of each of these framed nothing and loads; built in code, the
		// pre-upgrade program is unknown and it is refused.
		{name: "a legacy UnionDescriptor that holds itself", messages: []*descriptorpb.DescriptorProto{
			withField(message("UnionDescriptor", false), "next", ".vr.UnionDescriptor"),
		}, legacy: "UnionDescriptor", found: "Envelope", storedLoads: true},
		{name: "a table named UnionDescriptor with a STRUCT-typed column", messages: []*descriptorpb.DescriptorProto{
			scalarMessage("S"),
			withField(scalarMessage("UnionDescriptor"), "s", ".vr.S"),
			withField(message("Envelope2", true), "_UnionDescriptor", ".vr.UnionDescriptor"),
		}, dropEnvelope: true, legacy: "UnionDescriptor", found: "Envelope2", storedLoads: true, recordTypes: []string{"UnionDescriptor"}, heldBy: "Envelope2"},
		{name: "a table named UnionDescriptor with a table-typed column and an index", messages: []*descriptorpb.DescriptorProto{
			withField(scalarMessage("UnionDescriptor"), "a", ".vr.T"),
			withField(message("Envelope2", true), "_UnionDescriptor", ".vr.UnionDescriptor"),
		}, dropEnvelope: true, legacy: "UnionDescriptor", found: "Envelope2", storedLoads: true, recordTypes: []string{"UnionDescriptor"}, indexOn: "UnionDescriptor", heldBy: "Envelope2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fdp := protodesc.ToFileDescriptorProto(validateRecordsFile(t, nil).fileDescriptor)
			if tc.dropEnvelope {
				fdp.MessageType = fdp.MessageType[:1]
			}
			fdp.MessageType = append(fdp.MessageType, tc.messages...)
			stored := &gen.MetaData{
				Records:     fdp,
				RecordTypes: []*gen.RecordType{{Name: proto.String("T"), PrimaryKey: Field("id").ToKeyExpression()}},
				Version:     proto.Int32(1),
			}
			for _, name := range tc.recordTypes {
				stored.RecordTypes = append(stored.RecordTypes, &gen.RecordType{Name: proto.String(name), PrimaryKey: Field("id").ToKeyExpression()})
			}
			if tc.indexOn != "" {
				stored.Indexes = []*gen.Index{{Name: proto.String("I"), RecordType: []string{tc.indexOn}, RootExpression: Field("id").ToKeyExpression()}}
			}
			fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
			if err != nil {
				t.Fatalf("building the test descriptor: %v", err)
			}
			inCode := func(b *RecordMetaDataBuilder) (*RecordMetaData, error) {
				b.GetRecordType("T").SetPrimaryKey(Field("id"))
				for _, name := range tc.recordTypes {
					b.GetRecordType(name).SetPrimaryKey(Field("id"))
				}
				if tc.indexOn != "" {
					b.AddIndex(tc.indexOn, NewIndex("I", Field("id")))
				}
				return b.Build()
			}
			// Every path reads the file as the target does: the union is the
			// one fetchUnionDescriptor finds. Records a pre-release Go build
			// framed by a message named UnionDescriptor are not supported
			// (RFC-257, "Verification and review gates" item 9), so nothing is
			// refused for them.
			check := func(path string, err error, _ bool) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s = %v, want it to load", path, err)
				}
			}
			_, err = RecordMetaDataFromProto(stored)
			check("RecordMetaDataFromProto", err, tc.storedLoads)
			// Metadata built in code finds the union the same way, and took the
			// message named UnionDescriptor before the upgrade too.
			_, err = inCode(NewRecordMetaDataBuilder().SetRecords(fd))
			check("SetRecords", err, false)
			if tc.legacy != "" {
				// A caller that names the found union builds, and its stored form
				// loads back.
				md, err := inCode(NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, tc.found))
				if err != nil {
					t.Fatalf("SetRecordsWithUnionName(%s) = %v, want it to build", tc.found, err)
				}
				mdProto, err := md.ToProto()
				if err != nil {
					t.Fatalf("ToProto: %v", err)
				}
				_, err = RecordMetaDataFromProto(mdProto)
				check("RecordMetaDataFromProto(ToProto)", err, tc.storedLoads)
			}
		})
	}
}

// A (d1) file, a message named UnionDescriptor beside the union, gets the
// target's faults: one whose record type has no primary key, or one on no field,
// gets that primary-key fault, and one whose index names no field gets that
// index fault, on both paths that find the union; each fault is asserted by its
// message and class. Without a fault it loads.
func TestLegacyNamedUnionDescriptorGetsTheTargetsFaults(t *testing.T) {
	t.Parallel()
	legacy := &descriptorpb.DescriptorProto{Name: proto.String("UnionDescriptor"), Field: []*descriptorpb.FieldDescriptorProto{{
		Name: proto.String("_T"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), TypeName: proto.String(".vr.T"),
	}}}
	fdp := protodesc.ToFileDescriptorProto(validateRecordsFile(t, nil).fileDescriptor)
	fdp.MessageType = append(fdp.MessageType, legacy)
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("building the test descriptor: %v", err)
	}
	const (
		noPrimaryKey    = "Record type T must have a primary key"
		primaryKeyNope  = "Descriptor T does not have field: nope"
		indexOnNoField  = "Descriptor T does not have field: nope"
		shapeDFaultName = "a refusal of its own"
	)
	// Each arm reports on its own (Errorf), so a mutation that moves the refusal
	// shows every arm it reddens, not only the first.
	// The key faults are Java's KeyExpression.InvalidExpressionException,
	// the missing primary key its MetaDataException.
	want := func(path string, err error, message string) {
		t.Helper()
		keyClass := message != noPrimaryKey
		var md *MetaDataError
		var ke *KeyExpressionError
		got := ""
		switch {
		case errors.As(err, &ke):
			got = ke.Message
		case errors.As(err, &md):
			got = md.Message
		}
		if got != message || errors.As(err, &ke) != keyClass || errors.As(err, &md) == keyClass {
			t.Errorf("%s = %v (%T), want the target's fault %q as its class (key expression: %t), not %s", path, err, err, message, keyClass, shapeDFaultName)
		}
	}
	_, err = NewRecordMetaDataBuilder().SetRecords(fd).Build()
	want("SetRecords without a primary key", err, noPrimaryKey)
	b := NewRecordMetaDataBuilder().SetRecords(fd)
	b.GetRecordType("T").SetPrimaryKey(Field("nope"))
	_, err = b.Build()
	want("SetRecords with a primary key on no field", err, primaryKeyNope)
	b = NewRecordMetaDataBuilder().SetRecords(fd)
	b.GetRecordType("T").SetPrimaryKey(Field("id"))
	b.AddIndex("T", NewIndex("T$nope", Field("nope")))
	_, err = b.Build()
	want("SetRecords with an index on no field", err, indexOnNoField)
	stored := &gen.MetaData{
		Records:     fdp,
		RecordTypes: []*gen.RecordType{{Name: proto.String("T")}},
		Version:     proto.Int32(1),
	}
	_, err = RecordMetaDataFromProto(stored)
	want("RecordMetaDataFromProto without a primary key", err, noPrimaryKey)
	stored.RecordTypes[0].PrimaryKey = Field("nope").ToKeyExpression()
	_, err = RecordMetaDataFromProto(stored)
	want("RecordMetaDataFromProto with a primary key on no field", err, primaryKeyNope)
	stored.RecordTypes[0].PrimaryKey = Field("id").ToKeyExpression()
	stored.Indexes = []*gen.Index{{Name: proto.String("T$nope"), RecordType: []string{"T"}, RootExpression: Field("nope").ToKeyExpression()}}
	_, err = RecordMetaDataFromProto(stored)
	want("RecordMetaDataFromProto with an index on no field", err, indexOnNoField)
	// Without the fault the same file loads, on both paths, as the target
	// loads it.
	stored.Indexes = nil
	if _, err := RecordMetaDataFromProto(stored); err != nil {
		t.Errorf("RecordMetaDataFromProto = %v, want it to load", err)
	}
	b = NewRecordMetaDataBuilder().SetRecords(fd)
	b.GetRecordType("T").SetPrimaryKey(Field("id"))
	if _, err := b.Build(); err != nil {
		t.Errorf("SetRecords = %v, want it to build", err)
	}
}

// TestStoredMetaDataLoadsInJavasOrder pins RecordMetaDataFromProto's order, Java's
// loadFromProto (RecordMetaDataBuilder.java:272-278): the subspace-key settings,
// then the records, then the indexes, each step's first fault returned before the
// next step runs. The conformance spec "Java refuses the same records descriptors"
// measures the same three shapes on the JVM.
func TestStoredMetaDataLoadsInJavasOrder(t *testing.T) {
	t.Parallel()
	unionFault := func(f *descriptorpb.FileDescriptorProto) {
		for _, m := range f.MessageType {
			if m.GetName() == "Envelope" {
				m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
					Name: proto.String("x"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				})
			}
		}
	}
	unknownIndex := &gen.Index{
		Name: proto.String("I"), RecordType: []string{"Nope"}, RootExpression: Field("id").ToKeyExpression(),
		Type: proto.String("value"), AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
	}
	repeatedOption := []*gen.Index_Option{{Key: proto.String("a"), Value: proto.String("1")}, {Key: proto.String("a"), Value: proto.String("2")}}
	unknownWithRepeatedOption := proto.Clone(unknownIndex).(*gen.Index)
	unknownWithRepeatedOption.Options = repeatedOption
	nextWithRepeatedOption := &gen.Index{
		Name: proto.String("J"), RecordType: []string{"T"}, RootExpression: Field("id").ToKeyExpression(),
		Type: proto.String("value"), AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
		Options: repeatedOption,
	}
	for _, tc := range []struct {
		name        string
		records     func(*descriptorpb.FileDescriptorProto)
		indexes     []*gen.Index
		badCounter  bool
		recordTypes []string
		want        string
	}{
		{"an index on an unknown record type", nil, []*gen.Index{unknownIndex}, false, nil, "Unknown record type Nope"},
		{"the same index beside a union fault", unionFault, []*gen.Index{unknownIndex}, false, nil, "Union field x is not a message"},
		{
			"a subspace-key-counter fault beside a union fault", unionFault, nil, true, nil,
			"subspaceKeyCounter is set but usesSubspaceKeyCounter is not set in the meta-data proto",
		},
		// Per index, the record types are resolved before the index is read
		// (RecordMetaDataBuilder.java:187-219), so neither a repeated option on the
		// same index nor one on a later index hides the unknown type.
		{"an unknown record type on an index with a repeated option", nil, []*gen.Index{unknownWithRepeatedOption}, false, nil, "Unknown record type Nope"},
		{"an unknown record type before a repeated option on the next index", nil, []*gen.Index{unknownIndex, nextWithRepeatedOption}, false, nil, "Unknown record type Nope"},
		// A RecordType entry naming no record type (:221, :986-990).
		{"a record type entry naming no record type", nil, nil, false, []string{"Nope"}, "Unknown record type Nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fdp := protodesc.ToFileDescriptorProto(validateRecordsFile(t, nil).fileDescriptor)
			if tc.records != nil {
				tc.records(fdp)
			}
			stored := &gen.MetaData{
				Records:     fdp,
				RecordTypes: []*gen.RecordType{{Name: proto.String("T"), PrimaryKey: Field("id").ToKeyExpression()}},
				Version:     proto.Int32(1),
			}
			stored.Indexes = tc.indexes
			for _, name := range tc.recordTypes {
				stored.RecordTypes = append(stored.RecordTypes, &gen.RecordType{Name: proto.String(name), PrimaryKey: Field("id").ToKeyExpression()})
			}
			if tc.badCounter {
				stored.SubspaceKeyCounter = proto.Int64(3)
			}
			_, err := RecordMetaDataFromProto(stored)
			if tc.badCounter {
				// Java's MetaDataProtoDeserializationException, whose cause
				// names the fault (RecordMetaDataBuilder.java:290-295).
				var pde *MetaDataProtoDeserializationError
				if !errors.As(err, &pde) {
					t.Fatalf("RecordMetaDataFromProto = %v (%T), want a MetaDataProtoDeserializationError", err, err)
				}
				err = pde.Cause
			}
			var me *MetaDataError
			if !errors.As(err, &me) || me.Message != tc.want {
				t.Fatalf("RecordMetaDataFromProto = %v (%T), want the MetaDataError %q", err, err, tc.want)
			}
		})
	}
}
