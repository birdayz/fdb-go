package recordlayer

import (
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// A closed (proto2) enum field holding a number its enum does not declare is,
// in protobuf-java's parser, not set: the parser keeps the number as an unknown
// varint field (MessageReflection.mergeFieldFrom, for a generated message and a
// DynamicMessage alike). protobuf-go keeps the number in the field. The same
// stored bytes therefore read as a set field in Go and an absent one in Java:
// a required fan type Java refuses as missing Go took for SCALAR, a stored
// record's unrecognised enum value Java indexes as null Go indexed as the
// number. closedEnumsAsJava gives a decoded message Java's reading, and every
// decode of bytes a Java engine shares applies it.

// closedEnumReach answers whether a message type can hold a closed enum field
// at any depth (or an extension, which a message type's descriptor cannot
// enumerate), and for each such type the fields a walk reads: its closed-enum
// fields and the message fields whose types reach one. It is filled for every
// type its roots reach when it is made and read-only after, so concurrent
// decodes share it. A record type's is made at Build, with its map reach; the
// generated protos the store decodes share one per root type
// (generatedClosedEnumReach).
type closedEnumReach struct {
	reach map[protoreflect.MessageDescriptor]bool
	plans map[protoreflect.MessageDescriptor]closedEnumPlan
}

// closedEnumPlan is the fields a walk of one message type reads.
type closedEnumPlan struct {
	fields     []protoreflect.FieldDescriptor
	extensions bool
}

func newClosedEnumReach(roots ...protoreflect.MessageDescriptor) *closedEnumReach {
	reach := newTypeReach(func(md protoreflect.MessageDescriptor) bool {
		if md.ExtensionRanges().Len() > 0 {
			return true
		}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			if fd.IsMap() {
				fd = fd.MapValue()
			}
			if isClosedEnum(fd) {
				return true
			}
		}
		return false
	}, roots...)
	r := &closedEnumReach{reach: reach, plans: make(map[protoreflect.MessageDescriptor]closedEnumPlan)}
	for md, reaches := range reach {
		if !reaches {
			continue
		}
		plan := closedEnumPlan{extensions: md.ExtensionRanges().Len() > 0}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			v := fd
			if fd.IsMap() {
				v = fd.MapValue()
			}
			if isClosedEnum(v) || v.Message() != nil && reach[v.Message()] {
				plan.fields = append(plan.fields, fd)
			}
		}
		r.plans[md] = plan
	}
	return r
}

func (r *closedEnumReach) reaches(md protoreflect.MessageDescriptor) bool {
	if v, ok := r.reach[md]; ok {
		return v
	}
	return newClosedEnumReach(md).reach[md]
}

// generatedClosedEnumReach caches the reach of generated message types, whose
// descriptors live for the process; a dynamic descriptor is never stored here
// (a meta-data load builds fresh ones, and a process-wide map keyed by them would
// keep every loaded descriptor graph alive).
var generatedClosedEnumReach sync.Map // protoreflect.MessageDescriptor -> *closedEnumReach

// UnmarshalAsJava decodes b into m, a message of a generated type, as
// protobuf-java decodes it: proto.Unmarshal, then a closed enum's undeclared
// number moved to the unknown fields. Every decode of bytes a Java engine
// shares (store headers, index-build stamps, pending writes, continuations,
// stored meta-data) goes through it or unmarshalVTAsJava.
func UnmarshalAsJava(b []byte, m proto.Message) error {
	if err := javaUnmarshalOptions.Unmarshal(b, m); err != nil {
		return err
	}
	javaClosedEnums(m)
	return nil
}

// javaUnmarshalOptions parses with protobuf-java's recursion limit
// (CodedInputStream's default, 100 nested messages), so bytes nested deeper
// than Java can parse are refused at parse, as Java refuses them ("Protocol
// message had too many levels of nesting"), where protobuf-go's default limit
// is 10,000. The same limit gives Java's boundary: a key expression nested 49
// deep in a meta-data proto parses in both engines, and 50 in neither (JVM spec
// "RFC-257 a key expression nested past protobuf's recursion limit").
var javaUnmarshalOptions = proto.UnmarshalOptions{RecursionLimit: 100}

// unmarshalVTAsJava is UnmarshalAsJava through a vtproto type's own decoder.
func unmarshalVTAsJava(m interface {
	proto.Message
	UnmarshalVT([]byte) error
}, b []byte,
) error {
	if err := m.UnmarshalVT(b); err != nil {
		return err
	}
	javaClosedEnums(m)
	return nil
}

// javaClosedEnums is closedEnumsAsJava for a message of a generated type.
func javaClosedEnums(m proto.Message) {
	r := m.ProtoReflect()
	md := r.Descriptor()
	cached, ok := generatedClosedEnumReach.Load(md)
	if !ok {
		cached, _ = generatedClosedEnumReach.LoadOrStore(md, newClosedEnumReach(md))
	}
	closedEnumsAsJava(r, cached.(*closedEnumReach))
}

// closedEnumsAsJava gives a record of this type, decoded from stored bytes,
// Java's reading of them (DynamicMessageRecordSerializer parses a DynamicMessage).
func (rt *RecordType) closedEnumsAsJava(msg proto.Message) {
	if rt.reachesClosedEnum {
		closedEnumsAsJava(msg.ProtoReflect(), rt.closedEnumReach)
	}
}

func isClosedEnum(fd protoreflect.FieldDescriptor) bool {
	return fd.Kind() == protoreflect.EnumKind && fd.Enum() != nil && fd.Enum().IsClosed()
}

func declaredEnum(fd protoreflect.FieldDescriptor, n protoreflect.EnumNumber) bool {
	return fd.Enum().Values().ByNumber(n) != nil
}

// closedEnumsAsJava moves every closed-enum value m holds that its enum does not
// declare out of its field and into m's unknown fields, as a varint of the field's
// number, at any depth; a repeated field keeps its declared elements in order.
// A map value of a closed enum reads as the enum's default: Java's
// DynamicMessage holds a map as its entries, and an entry whose value it cannot
// read keeps its key and reads the default value; a Go map cannot carry the
// entry's unknown field. reach, when not nil, reads only the fields its plan
// for m's type names; nil reads every populated field.
func closedEnumsAsJava(m protoreflect.Message, reach *closedEnumReach) {
	var fields []protoreflect.FieldDescriptor
	if reach != nil {
		plan, ok := reach.plans[m.Descriptor()]
		if !ok {
			if !reach.reaches(m.Descriptor()) {
				return
			}
			// A type outside the reach's closure (a descriptor from elsewhere).
			reach = newClosedEnumReach(m.Descriptor())
			plan = reach.plans[m.Descriptor()]
		}
		for _, fd := range plan.fields {
			if m.Has(fd) {
				fields = append(fields, fd)
			}
		}
		if plan.extensions {
			m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
				if fd.IsExtension() {
					fields = append(fields, fd)
				}
				return true
			})
		}
	} else {
		m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
			fields = append(fields, fd)
			return true
		})
	}
	var moved [][]byte
	for _, fd := range fields {
		v := m.Get(fd)
		switch {
		case fd.IsMap():
			value := fd.MapValue()
			mp := v.Map()
			switch {
			case isClosedEnum(value):
				var fix []protoreflect.MapKey
				mp.Range(func(k protoreflect.MapKey, e protoreflect.Value) bool {
					if !declaredEnum(value, e.Enum()) {
						fix = append(fix, k)
					}
					return true
				})
				for _, k := range fix {
					mp.Set(k, value.Default())
				}
			case value.Message() != nil:
				mp.Range(func(_ protoreflect.MapKey, e protoreflect.Value) bool {
					closedEnumsAsJava(e.Message(), reach)
					return true
				})
			}
		case fd.IsList():
			list := v.List()
			switch {
			case isClosedEnum(fd):
				var kept []protoreflect.EnumNumber
				for i := 0; i < list.Len(); i++ {
					if n := list.Get(i).Enum(); declaredEnum(fd, n) {
						kept = append(kept, n)
					} else {
						moved = append(moved, appendUnknownEnum(nil, fd, n))
					}
				}
				if len(kept) != list.Len() {
					m.Clear(fd)
					if len(kept) > 0 {
						out := m.Mutable(fd).List()
						for _, n := range kept {
							out.Append(protoreflect.ValueOfEnum(n))
						}
					}
				}
			case fd.Message() != nil:
				for i := 0; i < list.Len(); i++ {
					closedEnumsAsJava(list.Get(i).Message(), reach)
				}
			}
		case isClosedEnum(fd):
			if n := v.Enum(); !declaredEnum(fd, n) {
				m.Clear(fd)
				moved = append(moved, appendUnknownEnum(nil, fd, n))
			}
		case fd.Message() != nil:
			closedEnumsAsJava(v.Message(), reach)
		}
	}
	if len(moved) > 0 {
		unknown := []byte(m.GetUnknown())
		for _, f := range moved {
			unknown = insertUnknownVarint(unknown, f)
		}
		m.SetUnknown(unknown)
	}
}

// insertUnknownVarint inserts the varint field f into the unknown fields raw
// where protobuf-java's UnknownFieldSet holds it: fields in number order and,
// within one number, varints first, each kind in the order it arrived. Unknown
// fields Java wrote are in that order already, so the enum value it wrote among
// them, which protobuf-go decoded into its field, goes back where it was.
func insertUnknownVarint(raw, f []byte) []byte {
	num, _, _ := protowire.ConsumeTag(f)
	for at := 0; at < len(raw); {
		n, typ, tl := protowire.ConsumeTag(raw[at:])
		if tl < 0 {
			break
		}
		vl := protowire.ConsumeFieldValue(n, typ, raw[at+tl:])
		if vl < 0 {
			break
		}
		if n > num || n == num && typ != protowire.VarintType {
			out := make([]byte, 0, len(raw)+len(f))
			return append(append(append(out, raw[:at]...), f...), raw[at:]...)
		}
		at += tl + vl
	}
	return append(raw, f...)
}

// appendUnknownEnum appends n as the unknown varint field protobuf-java keeps it
// as (UnknownFieldSet.mergeVarintField: the int32 sign-extended to 64 bits).
func appendUnknownEnum(b []byte, fd protoreflect.FieldDescriptor, n protoreflect.EnumNumber) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, fd.Number(), protowire.VarintType), uint64(int64(n)))
}
