package recordlayer

import (
	"bytes"
	"strings"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// buildDeterminismTestMessageDesc builds a descriptor for a record type with
// several scalar fields spread across non-adjacent field numbers. The spread
// (and the number of fields) makes it likely that dynamicpb's default,
// map-order field iteration would emit fields in a different order — and thus
// produce different bytes — on repeated marshals if determinism were off.
func buildDeterminismTestMessageDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("serialize_determinism_test.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("serdet"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Rec"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:   proto.String("f_a"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				},
				{
					Name:   proto.String("f_b"),
					Number: proto.Int32(3),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				},
				{
					Name:   proto.String("f_c"),
					Number: proto.Int32(5),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
				},
				{
					Name:   proto.String("f_d"),
					Number: proto.Int32(7),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
				},
				{
					Name:   proto.String("f_e"),
					Number: proto.Int32(9),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build file descriptor: %v", err)
	}
	return fd.Messages().Get(0)
}

// newDeterminismTestMessage returns a fully populated *dynamicpb.Message for the
// descriptor. Building a fresh message each time is deliberate: dynamicpb keeps
// fields in a Go map, so a fresh message can iterate them in a different order,
// which is exactly the nondeterminism the fix guards against.
func newDeterminismTestMessage(md protoreflect.MessageDescriptor) proto.Message {
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("f_a"), protoreflect.ValueOfInt64(0x0123456789abcdef))
	m.Set(md.Fields().ByName("f_b"), protoreflect.ValueOfString("beta-value"))
	m.Set(md.Fields().ByName("f_c"), protoreflect.ValueOfInt32(1337))
	m.Set(md.Fields().ByName("f_d"), protoreflect.ValueOfBool(true))
	m.Set(md.Fields().ByName("f_e"), protoreflect.ValueOfString("epsilon-value"))
	return m
}

// unwrapUnionInner extracts the inner record bytes from a serializeUnion output
// (a single length-delimited field tagged with unionFieldNumber).
func unwrapUnionInner(t *testing.T, union []byte, unionFieldNumber protowire.Number) []byte {
	t.Helper()
	num, typ, n := protowire.ConsumeTag(union)
	if n < 0 {
		t.Fatalf("consume union tag: %v", protowire.ParseError(n))
	}
	if num != unionFieldNumber {
		t.Fatalf("union field number = %d, want %d", num, unionFieldNumber)
	}
	if typ != protowire.BytesType {
		t.Fatalf("union wire type = %v, want BytesType", typ)
	}
	inner, m := protowire.ConsumeBytes(union[n:])
	if m < 0 {
		t.Fatalf("consume union bytes: %v", protowire.ParseError(m))
	}
	if n+m != len(union) {
		t.Fatalf("union has trailing bytes: consumed %d of %d", n+m, len(union))
	}
	return inner
}

// legacyFieldOrderPremiseViolation reports why md falls outside the set of
// descriptors for which order.LegacyFieldOrder — the ordering
// proto.MarshalOptions{Deterministic: true} marshals with — collapses to plain
// ascending field number. It returns "" when the premise holds.
//
// LegacyFieldOrder sorts extension fields before non-extension fields, then
// non-oneof fields before oneof fields, then by field number. Ascending field
// number is therefore the emitted order only for a descriptor that declares
// neither extension ranges nor a non-synthetic oneof. A synthetic oneof (the
// wrapper proto3 `optional` generates) does not count — LegacyFieldOrder skips
// synthetic oneofs explicitly — so it is not treated as a violation here.
func legacyFieldOrderPremiseViolation(md protoreflect.MessageDescriptor) string {
	if n := md.ExtensionRanges().Len(); n > 0 {
		return "it declares extension range(s); a set extension field is emitted before all non-extension fields"
	}
	for i := 0; i < md.Oneofs().Len(); i++ {
		od := md.Oneofs().Get(i)
		if !od.IsSynthetic() {
			return "it declares oneof " + string(od.Name()) + "; oneof members are emitted after every non-oneof field regardless of field number"
		}
	}
	return ""
}

// assertAscendingFields walks a serialized message and fails if the top-level
// field numbers are not in non-decreasing (ascending) order.
//
// Ascending order is NOT a general property of Deterministic:true. That option
// marshals in order.LegacyFieldOrder (extensions first, then non-oneof before
// oneof, then field number), so a message carrying an extension or a
// non-synthetic oneof is emitted out of field-number order by design. Nor is it
// a claim about Java's encoding: upstream disclaims cross-language canonicality,
// and any permutation of top-level fields decodes to the same message anyway.
// What Deterministic:true actually guarantees, and what the callers of this
// helper depend on, is byte-stability.
//
// The ascending claim holds for the descriptors exercised here only because they
// are extension-free and oneof-free. That premise is asserted rather than
// assumed, because the walk alone cannot detect its own invalidation: put the
// test descriptor's HIGHEST-numbered field in a oneof and LegacyFieldOrder still
// emits ascending numbers, so the walk passes green while the claim it is named
// for has become false. The premise check is what turns that into a failure.
func assertAscendingFields(t *testing.T, md protoreflect.MessageDescriptor, msg []byte) {
	t.Helper()
	if why := legacyFieldOrderPremiseViolation(md); why != "" {
		t.Fatalf("assertAscendingFields called on %s, whose Deterministic:true encoding is not ordered by field number: %s", md.FullName(), why)
	}
	prev := protowire.Number(0)
	rest := msg
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			t.Fatalf("consume tag: %v", protowire.ParseError(n))
		}
		if num < prev {
			t.Fatalf("field number %d follows %d — not ascending in %x", num, prev, msg)
		}
		prev = num
		rest = rest[n:]
		skip := protowire.ConsumeFieldValue(num, typ, rest)
		if skip < 0 {
			t.Fatalf("consume field value: %v", protowire.ParseError(skip))
		}
		rest = rest[skip:]
	}
}

// TestSerializeUnion_DynamicMessageDeterministic pins the fix in serializeUnion:
// the reflection slow path (hit only by *dynamicpb.Message — the SQL/relational
// row shape, which has no MarshalVT/SizeVT fast path) must serialize a given
// record to byte-identical output on every call and in ascending field-number
// order. Before the fix this path used plain proto.Marshal, which iterates a
// dynamic message's fields in map order and so wrote the same row to different
// bytes across writes — caught by the DST SQL workload's determinism oracle.
func TestSerializeUnion_DynamicMessageDeterministic(t *testing.T) {
	t.Parallel()
	md := buildDeterminismTestMessageDesc(t)

	// Guard the premise: a *dynamicpb.Message must NOT satisfy the VT fast/slow
	// paths, so serializeUnion truly exercises the deterministic reflection path
	// under test. Generated protos implement these interfaces and never get here.
	var probe proto.Message = dynamicpb.NewMessage(md)
	if _, ok := probe.(interface {
		SizeVT() int
		MarshalToSizedBufferVT([]byte) (int, error)
	}); ok {
		t.Fatalf("dynamicpb.Message unexpectedly implements the SizeVT fast path")
	}
	if _, ok := probe.(interface{ MarshalVT() ([]byte, error) }); ok {
		t.Fatalf("dynamicpb.Message unexpectedly implements MarshalVT")
	}

	rt := &RecordType{Name: "Rec", unionFieldNumber: 1}

	// First serialization is the reference.
	first, err := serializeUnion(newDeterminismTestMessage(md), rt)
	if err != nil {
		t.Fatalf("serializeUnion: %v", err)
	}
	if len(first) == 0 {
		t.Fatalf("serializeUnion produced empty output")
	}

	// The inner record bytes must be in ascending field-number order (which for
	// this extension-free, oneof-free descriptor is what LegacyFieldOrder emits).
	inner := unwrapUnionInner(t, first, rt.unionFieldNumber)
	assertAscendingFields(t, md, inner)

	// Byte stability across many fresh messages (fresh => fresh map iteration
	// order, the source of the original nondeterminism).
	const iterations = 256
	for i := 0; i < iterations; i++ {
		got, err := serializeUnion(newDeterminismTestMessage(md), rt)
		if err != nil {
			t.Fatalf("serializeUnion iteration %d: %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("serializeUnion not byte-stable at iteration %d:\n first=%x\n   got=%x", i, first, got)
		}
	}
}

// buildOneofTestMessageDesc builds a descriptor whose LOWER-numbered field lives
// in a oneof and whose higher-numbered field does not, so LegacyFieldOrder must
// emit field 2 before field 1.
func buildOneofTestMessageDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("serialize_determinism_oneof_test.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("serdetoneof"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("RecWithOneof"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:       proto.String("in_oneof"),
					Number:     proto.Int32(1),
					Label:      descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:       descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
					OneofIndex: proto.Int32(0),
				},
				{
					Name:   proto.String("plain"),
					Number: proto.Int32(2),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				},
			},
			OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("choice")}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build oneof file descriptor: %v", err)
	}
	return fd.Messages().Get(0)
}

// buildExtendableTestMessageDesc builds a descriptor that declares an extension
// range. LegacyFieldOrder emits any set extension ahead of every declared field,
// so an extendable message's deterministic encoding is not ordered by field
// number either — a second, independent way the ascending claim can go false.
func buildExtendableTestMessageDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("serialize_determinism_ext_test.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("serdetext"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("RecExtendable"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("plain"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			}},
			ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{{
				Start: proto.Int32(100),
				End:   proto.Int32(200),
			}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build extendable file descriptor: %v", err)
	}
	return fd.Messages().Get(0)
}

// TestLegacyFieldOrderPremise pins the premise assertAscendingFields rests on,
// and the upstream fact that makes the premise necessary.
//
// Deterministic:true marshals in order.LegacyFieldOrder, which puts every
// non-oneof field ahead of every oneof member regardless of field number. So
// "the deterministic encoding is in ascending field-number order" is TRUE only
// for extension-free, oneof-free descriptors — which the descriptors these tests
// build happen to be. Nothing pinned that, so adding a oneof to a test
// descriptor would have quietly made assertAscendingFields assert something
// false about its input.
//
// The third assertion is the load-bearing one: it MEASURES the emitted order for
// a oneof-bearing descriptor rather than restating the doc comment. If upstream
// ever switches the deterministic path to NumberFieldOrder (there is a standing
// TODO in proto/encode.go proposing exactly that), this goes red — which is
// precisely the change that would make the premise guard unnecessary.
func TestLegacyFieldOrderPremise(t *testing.T) {
	t.Parallel()

	// (1) The descriptor assertAscendingFields is actually used on satisfies the
	// premise, so its ascending claim is true for that input.
	plain := buildDeterminismTestMessageDesc(t)
	if why := legacyFieldOrderPremiseViolation(plain); why != "" {
		t.Fatalf("%s no longer satisfies the ascending-field-order premise: %s\n"+
			"assertAscendingFields asserts non-decreasing field numbers, which Deterministic:true "+
			"only produces for extension-free, oneof-free descriptors — fix the helper or the descriptor.",
			plain.FullName(), why)
	}

	// (2) A descriptor with a non-synthetic oneof does not.
	withOneof := buildOneofTestMessageDesc(t)
	why := legacyFieldOrderPremiseViolation(withOneof)
	if why == "" {
		t.Fatalf("%s declares oneof %q but the premise check reported no violation — "+
			"the check cannot detect the case it exists for",
			withOneof.FullName(), withOneof.Oneofs().Get(0).Name())
	}
	if !strings.Contains(why, "oneof") {
		t.Fatalf("premise violation for a oneof descriptor reads %q, which does not name the oneof", why)
	}

	// (2b) Neither does one that declares an extension range. This is the second,
	// independent arm of LegacyFieldOrder's ordering; a guard that caught only the
	// oneof case would still let an extendable descriptor through.
	extendable := buildExtendableTestMessageDesc(t)
	whyExt := legacyFieldOrderPremiseViolation(extendable)
	if whyExt == "" {
		t.Fatalf("%s declares extension range(s) but the premise check reported no violation — "+
			"a set extension field is emitted before every declared field, so ascending order does not hold",
			extendable.FullName())
	}
	if !strings.Contains(whyExt, "extension") {
		t.Fatalf("premise violation for an extendable descriptor reads %q, which does not name the extension range", whyExt)
	}

	// (3) And that is not pedantry — measure what Deterministic:true actually emits.
	m := dynamicpb.NewMessage(withOneof)
	m.Set(withOneof.Fields().ByName("in_oneof"), protoreflect.ValueOfInt64(7))
	m.Set(withOneof.Fields().ByName("plain"), protoreflect.ValueOfInt64(9))
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatalf("deterministic marshal of oneof message: %v", err)
	}
	got := topLevelFieldNumbers(t, encoded)
	want := []protowire.Number{2, 1}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Deterministic:true emitted top-level field order %v for %s, want %v.\n"+
			"LegacyFieldOrder is supposed to place the non-oneof field 2 before oneof member 1. "+
			"If upstream switched to NumberFieldOrder this is now ascending, and the "+
			"legacyFieldOrderPremiseViolation guard can be dropped; if it is some third order, "+
			"assertAscendingFields' documented claim needs rechecking. bytes=%x",
			got, withOneof.FullName(), want, encoded)
	}
}

// topLevelFieldNumbers returns the top-level field numbers in the order they
// appear in a serialized message.
func topLevelFieldNumbers(t *testing.T, msg []byte) []protowire.Number {
	t.Helper()
	var nums []protowire.Number
	rest := msg
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			t.Fatalf("consume tag: %v", protowire.ParseError(n))
		}
		nums = append(nums, num)
		rest = rest[n:]
		skip := protowire.ConsumeFieldValue(num, typ, rest)
		if skip < 0 {
			t.Fatalf("consume field value: %v", protowire.ParseError(skip))
		}
		rest = rest[skip:]
	}
	return nums
}

// TestSerializeUnion_DeterministicBeatsPlainMarshal is the differential guard:
// it asserts serializeUnion's inner bytes are EXACTLY the Deterministic:true
// encoding, not merely self-consistent bytes.
//
// The repetition is load-bearing, not padding. A dynamicpb message keeps its
// fields in a small Go map that is never grown or deleted from, so ranging it
// yields a ROTATION of insertion order — for these five fields, one of five,
// picked per range. A single comparison therefore agrees with the
// Deterministic:true encoding one time in five even with the fix reverted. Looping drives
// the odds of a false pass to (1/5)^n, which is what makes this a detector
// rather than a coin flip.
func TestSerializeUnion_DeterministicBeatsPlainMarshal(t *testing.T) {
	t.Parallel()
	md := buildDeterminismTestMessageDesc(t)
	rt := &RecordType{Name: "Rec", unionFieldNumber: 4}

	want, err := proto.MarshalOptions{Deterministic: true}.Marshal(newDeterminismTestMessage(md))
	if err != nil {
		t.Fatalf("deterministic marshal: %v", err)
	}

	const iterations = 256
	for i := 0; i < iterations; i++ {
		union, err := serializeUnion(newDeterminismTestMessage(md), rt)
		if err != nil {
			t.Fatalf("serializeUnion iteration %d: %v", i, err)
		}
		inner := unwrapUnionInner(t, union, rt.unionFieldNumber)
		if !bytes.Equal(inner, want) {
			t.Fatalf("serializeUnion inner bytes are not the Deterministic:true encoding at iteration %d:\n inner=%x\n  want=%x", i, inner, want)
		}

		// And the Deterministic:true encoding is itself stable across repeats.
		got, err := proto.MarshalOptions{Deterministic: true}.Marshal(newDeterminismTestMessage(md))
		if err != nil {
			t.Fatalf("deterministic marshal iteration %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("deterministic marshal not stable at iteration %d", i)
		}
	}
}

// TestPersistedProtosHaveNoMapFields pins the negative result that lets the
// remaining proto.Marshal call sites on the persistence and continuation paths
// stay as they are. Those sites marshal GENERATED messages, whose fast path
// emits a fixed, code-generated field order and is therefore byte-stable — with
// one exception: a proto3 map<> field is emitted in Go map order unless
// Deterministic:true is set. No proto in this repo declares a map field today
// (the store header's user-field map is modeled as `repeated UserFieldEntry`,
// which is order-stable), so those sites are already byte-stable.
//
// Byte-stability, not canonicality, is the property at stake: nothing on these
// paths requires the bytes to match what Java would emit, only that Go re-emits
// its own bytes for unchanged content.
//
// If this test fails, that reasoning has expired: the newly-mapped message makes
// every proto.Marshal on a persisted or continuation-bound proto nondeterministic,
// and each such call site needs proto.MarshalOptions{Deterministic: true}.
func TestPersistedProtosHaveNoMapFields(t *testing.T) {
	t.Parallel()

	// Sweep every message this repo defines, by proto package rather than by Go
	// type name: naming a handful of roots would silently stop covering a proto
	// added later, which is exactly when this sentinel needs to fire.
	const ourPackage = "com.apple.foundationdb."

	var withMaps []string
	var checked int
	var walk func(msgs protoreflect.MessageDescriptors)
	walk = func(msgs protoreflect.MessageDescriptors) {
		for i := 0; i < msgs.Len(); i++ {
			md := msgs.Get(i)
			checked++
			for j := 0; j < md.Fields().Len(); j++ {
				if fd := md.Fields().Get(j); fd.IsMap() {
					withMaps = append(withMaps, string(md.FullName())+"."+string(fd.Name()))
				}
			}
			walk(md.Messages()) // nested types
		}
	}
	var files int
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(fd.Package()), ourPackage) {
			files++
			walk(fd.Messages())
		}
		return true
	})

	// The metadata proto must be linked into this test binary for the sweep to
	// mean anything; touching it also guards against the registry coming up empty.
	if want := (*gen.MetaData)(nil).ProtoReflect().Descriptor().FullName(); !strings.HasPrefix(string(want), ourPackage) {
		t.Fatalf("gen.MetaData full name %q no longer starts with %q — the package filter needs updating", want, ourPackage)
	}
	if files == 0 || checked < 50 {
		t.Fatalf("descriptor sweep reached %d files / %d messages — the registry filter stopped matching, so this sentinel is not actually checking anything", files, checked)
	}
	if len(withMaps) > 0 {
		t.Fatalf("proto map field(s) reached from a persisted/continuation root: %v\n"+
			"Generated-message proto.Marshal emits map entries in Go map order, so every "+
			"proto.Marshal on these paths must now pass proto.MarshalOptions{Deterministic: true} "+
			"(see serializeUnion and appendContProtoMessage).", withMaps)
	}
}
