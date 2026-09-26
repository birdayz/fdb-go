package recordlayer

import (
	"errors"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A closed enum field holding a number its enum does not declare is, in
// protobuf-java's parser, not set: the parser keeps the number as an unknown
// varint field (MessageReflection.mergeFieldFrom, for a generated message and a
// DynamicMessage alike) and goes on, so the field holds the last DECLARED
// occurrence, a oneof sibling is not cleared by an undeclared member, and the
// required-field check that closes the parse (buildParsed) sees the field
// unset. protobuf-go keeps the number in the field. The same stored bytes
// therefore read as a set field in Go and an absent one in Java: a required
// fan type Java refuses as missing Go took for SCALAR, a stored record's
// unrecognised enum value Java indexes as null Go indexed as the number.
//
// Every decode of bytes a Java engine shares reads them as Java parses them
// (javaDecodeRule.unmarshal): a scan of the wire bytes, over the fields that can
// hold a closed enum, looks for an undeclared number; bytes with none are
// decoded by protobuf-go as they are, which is Java's reading of them, and bytes
// with one are decoded field occurrence by field occurrence, each undeclared
// number set aside for the unknown fields, then the required fields checked
// (javaDecodeRule.merge). A message Go holds in memory with an undeclared
// number in a closed enum field (built through a generated setter or dynamicpb;
// Java cannot build one) is given Java's reading of the bytes Go would write for
// it before Go evaluates or writes it (closedEnumsAsJava).

// javaDecodeRule is how protobuf-java parses the bytes one decode reads.
type javaDecodeRule struct {
	// limit is protobuf-go's RecursionLimit for the decode. protobuf-go counts
	// the root message against it (internal/impl/decode.go, unmarshalPointer);
	// protobuf-java counts only nested ones, refusing a nested message or
	// group once 100 are open (CodedInputStream.checkRecursionLimit, before the
	// depth is raised), so bytes Java parses as the root admit 101 levels in
	// all: 101. A record is the root of Go's decode but one level below the
	// union message Java parses (DynamicMessageRecordSerializer): 100.
	limit int
	// generated is a generated Java class's map rule: an entry whose closed
	// enum value is undeclared goes whole to the unknown fields (protoc's Java
	// map code, mergeUnknownLengthDelimitedField). Otherwise a DynamicMessage's:
	// the entry is a message like any other, so it is kept, its value an unknown
	// field of the entry, and it reads the enum's default.
	generated bool
	// resolver resolves extensions. Java parses meta-data with its extension
	// registry, and protobuf-go's global registry holds the same generated
	// extensions; a record is parsed with the empty registry
	// (DynamicMessage.parseFrom), so its extensions are unknown fields.
	resolver *protoregistry.Types
	// allowPartial skips the closing required-field check, for a decode whose
	// caller asked for a partial parse.
	allowPartial bool
}

var (
	javaRootRule = javaDecodeRule{limit: 101, generated: true, resolver: protoregistry.GlobalTypes}
	// javaVTRule is the root rule for the types a store decodes with vtproto:
	// Java parses them with no extension registry, and vtproto keeps an
	// extension as an unknown field.
	javaVTRule      = javaDecodeRule{limit: 101, generated: true, resolver: new(protoregistry.Types)}
	javaRecordRule  = javaDecodeRule{limit: 100, resolver: new(protoregistry.Types)}
	javaPartialRule = javaDecodeRule{limit: 100, resolver: new(protoregistry.Types), allowPartial: true}
)

func (rule javaDecodeRule) options() proto.UnmarshalOptions {
	return proto.UnmarshalOptions{RecursionLimit: rule.limit, Resolver: rule.resolver, AllowPartial: rule.allowPartial}
}

// unmarshal decodes b into m as protobuf-java parses it. x is the closed-enum
// reach m's type is read under.
func (rule javaDecodeRule) unmarshal(b []byte, m proto.Message, x *closedEnumReach) error {
	if !rule.hasUndeclared(b, m.ProtoReflect().Descriptor(), x) {
		return rule.options().Unmarshal(b, m)
	}
	return rule.slow(b, m, x)
}

// hasUndeclared reports whether b, a message of type md, holds a closed enum's
// undeclared number where x's plans read.
func (rule javaDecodeRule) hasUndeclared(b []byte, md protoreflect.MessageDescriptor, x *closedEnumReach) bool {
	return x != nil && x.undeclaredIn(b, md, rule, rule.limit)
}

// slow is the decode of bytes that hold an undeclared number.
func (rule javaDecodeRule) slow(b []byte, m proto.Message, x *closedEnumReach) error {
	proto.Reset(m)
	if err := rule.merge(b, m.ProtoReflect(), x, rule.limit); err != nil {
		return err
	}
	if rule.allowPartial {
		return nil
	}
	return proto.CheckInitialized(m)
}

// errJavaRecursion is protobuf-java's refusal of bytes nested past its limit,
// which protobuf-go's own decode reports as its recursion error.
var errJavaRecursion = errors.New("Protocol message had too many levels of nesting.  May be malicious.  Use CodedInputStream.setRecursionLimit() to increase the depth limit.")

// UnmarshalAsJava decodes b into m, a message of a generated type, as
// protobuf-java parses it as the root of a parse (javaRootRule): meta-data, a
// catalog row's template, a pending write. Store headers, index-build stamps,
// heartbeats and continuations, all of generated types with a vtproto decoder,
// are read by UnmarshalVTAsJava.
func UnmarshalAsJava(b []byte, m proto.Message) error {
	return javaRootRule.unmarshal(b, m, generatedReach(m.ProtoReflect().Descriptor()))
}

// UnmarshalRecordAsJava decodes b into m, a record or a value of a record's
// field (a dynamic message, as a DynamicMessage), as protobuf-java parses a
// stored record: with the empty extension registry, a map entry kept, and
// protobuf-java's limit one level below a root. A partial decode skips the
// required-field check.
func UnmarshalRecordAsJava(b []byte, m proto.Message, partial bool) error {
	rule := javaRecordRule
	if partial {
		rule = javaPartialRule
	}
	md := m.ProtoReflect().Descriptor()
	if _, dynamic := m.(*dynamicpb.Message); dynamic {
		return rule.unmarshal(b, m, dynamicReach(md))
	}
	// A generated type's reach is cached; the empty resolver keeps its
	// extensions unknown, so the reach's extension plans resolve none.
	return rule.unmarshal(b, m, generatedReach(md))
}

// UnmarshalVTAsJava is UnmarshalAsJava for the generated protos a store reads
// as the root of a parse with no extension registry (headers, stamps,
// heartbeats, continuations), through the type's vtproto decoder when that
// reads the bytes as Java does: vtproto keeps an extension as an unknown field,
// as Java's parse without a registry does, but has no recursion limit, so a
// type whose messages nest without bound, or deeper than protobuf-java's
// limit, is decoded by protobuf-go under the limit instead (vtBounded).
func UnmarshalVTAsJava(m interface {
	proto.Message
	UnmarshalVT([]byte) error
}, b []byte,
) error {
	md := m.ProtoReflect().Descriptor()
	x := generatedReach(md)
	if javaVTRule.hasUndeclared(b, md, x) {
		return javaVTRule.slow(b, m, x)
	}
	if !vtBounded(md, javaVTRule.limit) {
		return javaVTRule.options().Unmarshal(b, m)
	}
	return m.UnmarshalVT(b)
}

// vtBoundedTypes caches vtBounded per generated type and limit.
var vtBoundedTypes sync.Map // vtBoundedKey -> bool

type vtBoundedKey struct {
	md    protoreflect.MessageDescriptor
	limit int
}

// vtBounded reports whether no message of type md can open more than limit
// message levels: md's message fields form no cycle and nest at most that
// deep. Unknown groups are not
// bounded by a schema: vtproto, like protobuf-go (which consumes them with its
// own limit of 10,000), reads them deeper than protobuf-java's 100 (DIVERGENCES.md).
func vtBounded(md protoreflect.MessageDescriptor, limit int) bool {
	key := vtBoundedKey{md, limit}
	if v, ok := vtBoundedTypes.Load(key); ok {
		return v.(bool)
	}
	const cyclic = -1
	depth := map[protoreflect.MessageDescriptor]int{}
	var levels func(protoreflect.MessageDescriptor) int
	levels = func(md protoreflect.MessageDescriptor) int {
		if d, ok := depth[md]; ok {
			return d // cyclic while md is on the stack
		}
		depth[md] = cyclic
		deepest := 0
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			if sub := fields.Get(i).Message(); sub != nil {
				d := levels(sub)
				if d == cyclic {
					return cyclic
				}
				if d > deepest {
					deepest = d
				}
			}
		}
		depth[md] = deepest + 1
		return deepest + 1
	}
	d := levels(md)
	bounded := d != cyclic && d <= limit
	vtBoundedTypes.Store(key, bounded)
	return bounded
}

// unmarshalRecord decodes a record of this type from its stored bytes, as
// DynamicMessageRecordSerializer parses it. A generated Go type is decoded by
// its vtproto decoder when the bytes hold no closed enum's undeclared number
// and the type cannot nest past the record limit (vtproto has none); otherwise
// by the rule.
func (rt *RecordType) unmarshalRecord(b []byte) (proto.Message, error) {
	msg := rt.newMessage()
	rule := javaRecordRule
	if rt.closedEnumRoot != nil && rt.closedEnumReach.undeclaredInPlan(b, rt.closedEnumRoot, rt.Descriptor, rule, rule.limit) {
		if err := rule.slow(b, msg, rt.closedEnumReach); err != nil {
			return nil, err
		}
		return msg, nil
	}
	if rt.decodeVT {
		if err := msg.(interface{ UnmarshalVT([]byte) error }).UnmarshalVT(b); err != nil {
			return nil, err
		}
		return msg, nil
	}
	if err := rule.options().Unmarshal(b, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// closedEnumReach answers whether a message type can hold a closed enum field
// at any depth (or, for a generated type, an extension, which a message
// type's descriptor cannot enumerate), and for each such type the fields a scan
// or walk reads: its closed-enum fields and the message fields whose types
// reach one. It is filled for every type its roots reach when it is made and
// read-only after, so concurrent decodes share it. A record type's is made at
// Build, with its map reach, and counts no extension (Java parses a record
// with the empty registry); the generated protos the store decodes share one
// per root type (generatedClosedEnumReach).
type closedEnumReach struct {
	reach      map[protoreflect.MessageDescriptor]bool
	plans      map[protoreflect.MessageDescriptor]*closedEnumPlan
	extensions bool
}

// closedEnumPlan is the fields a scan or walk of one message type reads, and
// by number (a dense table up to the highest one when that is small, which it
// is for every relational record, else a map), and whether it resolves
// extensions.
type closedEnumPlan struct {
	fields     []protoreflect.FieldDescriptor
	dense      []planEntry
	byNumber   map[protowire.Number]*planEntry
	extensions bool
}

// planEntry is one field a plan reads, and for a message field (or a map's
// message value) of a type in the same reach, that type's plan.
type planEntry struct {
	fd  protoreflect.FieldDescriptor
	sub *closedEnumPlan
	// enum is the closed enum's declared values when fd is a (non-map)
	// closed-enum field, so the scan asks no descriptor in its loop.
	enum protoreflect.EnumValueDescriptors
}

// maxDensePlanNumber bounds the dense table's length.
const maxDensePlanNumber = 1024

// entry is the field a plan reads at number num, nil when it reads none.
func (plan *closedEnumPlan) entry(num protowire.Number) *planEntry {
	if plan.byNumber == nil {
		if int(num) < len(plan.dense) && plan.dense[num].fd != nil {
			return &plan.dense[num]
		}
		return nil
	}
	return plan.byNumber[num]
}

// field is the field a plan reads at number num, nil when it reads none.
func (plan *closedEnumPlan) field(num protowire.Number) protoreflect.FieldDescriptor {
	if e := plan.entry(num); e != nil {
		return e.fd
	}
	return nil
}

// planMessage is the message type a field's plan entry descends into: the
// field's own message, or a map's message value.
func planMessage(fd protoreflect.FieldDescriptor) protoreflect.MessageDescriptor {
	if fd.IsMap() {
		return fd.MapValue().Message()
	}
	return fd.Message()
}

func newClosedEnumReach(extensions bool, roots ...protoreflect.MessageDescriptor) *closedEnumReach {
	reach := newTypeReach(func(md protoreflect.MessageDescriptor) bool {
		if extensions && md.ExtensionRanges().Len() > 0 {
			return true
		}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			if fd.IsMap() {
				fd = fd.MapValue()
			}
			if closedEnumField(fd) {
				return true
			}
		}
		return false
	}, roots...)
	r := &closedEnumReach{
		reach: reach, plans: make(map[protoreflect.MessageDescriptor]*closedEnumPlan), extensions: extensions,
	}
	for md, reaches := range reach {
		if reaches {
			r.plans[md] = &closedEnumPlan{extensions: extensions && md.ExtensionRanges().Len() > 0}
		}
	}
	for md, plan := range r.plans {
		fields := md.Fields()
		highest := protowire.Number(0)
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			v := fd
			if fd.IsMap() {
				v = fd.MapValue()
			}
			if closedEnumField(v) || v.Message() != nil && reach[v.Message()] {
				plan.fields = append(plan.fields, fd)
				highest = max(highest, fd.Number())
			}
		}
		entryOf := func(fd protoreflect.FieldDescriptor) planEntry {
			e := planEntry{fd: fd}
			if sub := planMessage(fd); sub != nil {
				e.sub = r.plans[sub]
			}
			if !fd.IsMap() && closedEnumField(fd) {
				e.enum = fd.Enum().Values()
			}
			return e
		}
		if highest < maxDensePlanNumber {
			plan.dense = make([]planEntry, highest+1)
			for _, fd := range plan.fields {
				plan.dense[fd.Number()] = entryOf(fd)
			}
		} else {
			plan.byNumber = make(map[protowire.Number]*planEntry, len(plan.fields))
			for _, fd := range plan.fields {
				e := entryOf(fd)
				plan.byNumber[fd.Number()] = &e
			}
		}
	}
	return r
}

func (r *closedEnumReach) reaches(md protoreflect.MessageDescriptor) bool {
	_, _, ok := r.planFor(md)
	return ok
}

// planFor is md's plan and the reach it belongs to: r's own for a type of r's
// closure, else the reach of md alone (an extension's message type, or a
// descriptor from elsewhere), cached for a generated type.
func (r *closedEnumReach) planFor(md protoreflect.MessageDescriptor) (*closedEnumPlan, *closedEnumReach, bool) {
	if p, ok := r.plans[md]; ok {
		return p, r, true
	}
	if _, ok := r.reach[md]; ok {
		return nil, r, false
	}
	var other *closedEnumReach
	if r.extensions {
		other = generatedReach(md)
	} else {
		other = newClosedEnumReach(false, md)
	}
	p, ok := other.plans[md]
	return p, other, ok
}

// generatedClosedEnumReach caches the reach of generated message types, whose
// descriptors live for the process; a dynamic descriptor is never stored here
// (a meta-data load builds fresh ones, and a process-wide map keyed by them would
// keep every loaded descriptor graph alive).
var generatedClosedEnumReach sync.Map // protoreflect.MessageDescriptor -> *closedEnumReach

func generatedReach(md protoreflect.MessageDescriptor) *closedEnumReach {
	if cached, ok := generatedClosedEnumReach.Load(md); ok {
		return cached.(*closedEnumReach)
	}
	cached, _ := generatedClosedEnumReach.LoadOrStore(md, newClosedEnumReach(true, md))
	return cached.(*closedEnumReach)
}

// dynamicReach is the reach of a record's type or of a value of one of its
// fields, made for the call: a dynamic descriptor is not cached.
func dynamicReach(md protoreflect.MessageDescriptor) *closedEnumReach {
	return newClosedEnumReach(false, md)
}

// extensionField resolves field number num of a message of type md as an
// extension, as the decode's resolver does; nil when it resolves none.
func (plan *closedEnumPlan) extensionField(md protoreflect.MessageDescriptor, num protowire.Number, resolver *protoregistry.Types) protoreflect.FieldDescriptor {
	if !plan.extensions || resolver == nil || !md.ExtensionRanges().Has(num) {
		return nil
	}
	xt, err := resolver.FindExtensionByNumber(md.FullName(), num)
	if err != nil {
		return nil
	}
	return xt.TypeDescriptor()
}

// undeclaredIn reports whether b, a message of type md, holds a closed enum's
// undeclared number in a field r's plans read, at most remaining levels deep.
// Malformed bytes, and bytes nested past the limit, report false: the decode
// that follows refuses them.
func (r *closedEnumReach) undeclaredIn(b []byte, md protoreflect.MessageDescriptor, rule javaDecodeRule, remaining int) bool {
	plan, r, ok := r.planFor(md)
	if !ok {
		return false
	}
	return r.undeclaredInPlan(b, plan, md, rule, remaining)
}

// undeclaredInPlan is undeclaredIn with md's plan in hand.
func (r *closedEnumReach) undeclaredInPlan(b []byte, plan *closedEnumPlan, md protoreflect.MessageDescriptor, rule javaDecodeRule, remaining int) bool {
	if remaining <= 0 {
		return false
	}
	for len(b) > 0 {
		var num protowire.Number
		var typ protowire.Type
		if b[0] < 0x80 {
			// A one-byte tag, every field numbered below 16.
			num, typ = protowire.Number(b[0]>>3), protowire.Type(b[0]&7)
			b = b[1:]
		} else {
			var n int
			num, typ, n = protowire.ConsumeTag(b)
			if n < 0 {
				return false
			}
			b = b[n:]
		}
		e := plan.entry(num)
		if e == nil && plan.extensions {
			if fd := plan.extensionField(md, num, rule.resolver); fd != nil {
				e = &planEntry{fd: fd}
				if !fd.IsMap() && closedEnumField(fd) {
					e.enum = fd.Enum().Values()
				}
			}
		}
		switch typ {
		case protowire.VarintType:
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			if e != nil && e.enum != nil && e.enum.ByNumber(enumNumber(v)) == nil {
				return true
			}
		case protowire.Fixed32Type:
			if len(b) < 4 {
				return false
			}
			b = b[4:]
		case protowire.Fixed64Type:
			if len(b) < 8 {
				return false
			}
			b = b[8:]
		case protowire.BytesType:
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			if e != nil && r.undeclaredInBytes(e, v, rule, remaining) {
				return true
			}
		case protowire.StartGroupType:
			v, vn := protowire.ConsumeGroup(num, b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			if e != nil && e.fd.Kind() == protoreflect.GroupKind && r.undeclaredInMessage(e, v, rule, remaining-1) {
				return true
			}
		default:
			return false
		}
	}
	return false
}

// undeclaredInMessage reports whether body, one occurrence of a message field,
// holds an undeclared number.
func (r *closedEnumReach) undeclaredInMessage(e *planEntry, body []byte, rule javaDecodeRule, remaining int) bool {
	md := planMessage(e.fd)
	if e.sub != nil {
		return r.undeclaredInPlan(body, e.sub, md, rule, remaining)
	}
	return r.undeclaredIn(body, md, rule, remaining)
}

// undeclaredInBytes reports whether body, one length-delimited occurrence of
// the field e reads, holds an undeclared number: a packed list of a closed
// enum, a message, or a map entry.
func (r *closedEnumReach) undeclaredInBytes(e *planEntry, body []byte, rule javaDecodeRule, remaining int) bool {
	fd := e.fd
	switch {
	case fd.IsMap():
		value := fd.MapValue()
		for len(body) > 0 {
			num, vtyp, n := protowire.ConsumeTag(body)
			if n < 0 {
				return false
			}
			body = body[n:]
			vn := protowire.ConsumeFieldValue(num, vtyp, body)
			if vn < 0 {
				return false
			}
			v := body[:vn]
			body = body[vn:]
			if num != 2 {
				continue
			}
			switch {
			case closedEnumField(value) && vtyp == protowire.VarintType:
				if n, _ := protowire.ConsumeVarint(v); !declaredEnum(value, enumNumber(n)) {
					return true
				}
			case value.Message() != nil && vtyp == protowire.BytesType:
				sub, _ := protowire.ConsumeBytes(v)
				if r.undeclaredInMessage(e, sub, rule, remaining-2) {
					return true
				}
			}
		}
	case closedEnumField(fd):
		if !fd.IsList() {
			return false
		}
		for len(body) > 0 {
			n, vn := protowire.ConsumeVarint(body)
			if vn < 0 {
				return false
			}
			if !declaredEnum(fd, enumNumber(n)) {
				return true
			}
			body = body[vn:]
		}
	case fd.Kind() == protoreflect.MessageKind:
		return r.undeclaredInMessage(e, body, rule, remaining-1)
	}
	return false
}

// merge decodes b into m occurrence by occurrence, as protobuf-java's parser
// reads it: a closed enum's undeclared number is set aside for m's unknown
// fields and never reaches the field, so the field keeps the last declared
// occurrence, a oneof keeps its member, and a message field whose type reaches
// a closed enum is decoded the same way one level down. Every other occurrence
// is merged by protobuf-go, which is Java's reading of it. remaining is the
// number of message levels b may open, m's included.
func (rule javaDecodeRule) merge(b []byte, m protoreflect.Message, r *closedEnumReach, remaining int) error {
	if remaining <= 0 {
		return errJavaRecursion
	}
	opts := proto.UnmarshalOptions{Merge: true, AllowPartial: true, RecursionLimit: remaining, Resolver: rule.resolver}
	md := m.Descriptor()
	plan, r, ok := r.planFor(md)
	if !ok {
		return opts.Unmarshal(b, m.Interface())
	}
	var moved [][]byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		vn := protowire.ConsumeFieldValue(num, typ, b[n:])
		if vn < 0 {
			return protowire.ParseError(vn)
		}
		occurrence := b[:n+vn]
		b = b[n+vn:]
		fd := plan.field(num)
		if fd == nil && plan.extensions {
			fd = plan.extensionField(md, num, rule.resolver)
		}
		if fd == nil {
			if err := opts.Unmarshal(occurrence, m.Interface()); err != nil {
				return err
			}
			continue
		}
		set, err := rule.mergeField(m, fd, num, typ, occurrence, occurrence[n:], r, remaining)
		if err != nil {
			return err
		}
		moved = append(moved, set...)
	}
	if len(moved) > 0 {
		unknown := []byte(m.GetUnknown())
		for _, f := range moved {
			unknown = insertUnknownField(unknown, f)
		}
		m.SetUnknown(unknown)
	}
	return nil
}

// mergeField merges one occurrence of a field a plan reads into m, returning
// the unknown fields it sets aside.
func (rule javaDecodeRule) mergeField(m protoreflect.Message, fd protoreflect.FieldDescriptor, num protowire.Number, typ protowire.Type,
	occurrence, value []byte, r *closedEnumReach, remaining int,
) ([][]byte, error) {
	opts := proto.UnmarshalOptions{Merge: true, AllowPartial: true, RecursionLimit: remaining, Resolver: rule.resolver}
	whole := func() ([][]byte, error) { return nil, opts.Unmarshal(occurrence, m.Interface()) }
	switch {
	case fd.IsMap():
		if typ != protowire.BytesType {
			return whole()
		}
		return rule.mergeMapEntry(m, fd, occurrence, value, r, remaining)
	case closedEnumField(fd):
		switch {
		case typ == protowire.VarintType:
			n, _ := protowire.ConsumeVarint(value)
			if declaredEnum(fd, enumNumber(n)) {
				return whole()
			}
			return [][]byte{appendUnknownEnum(nil, num, enumNumber(n))}, nil
		case typ == protowire.BytesType && fd.IsList():
			packed, _ := protowire.ConsumeBytes(value)
			var kept []byte
			var moved [][]byte
			for len(packed) > 0 {
				n, vn := protowire.ConsumeVarint(packed)
				if vn < 0 {
					return whole()
				}
				if declaredEnum(fd, enumNumber(n)) {
					kept = protowire.AppendVarint(kept, n)
				} else {
					moved = append(moved, appendUnknownEnum(nil, num, enumNumber(n)))
				}
				packed = packed[vn:]
			}
			if len(kept) > 0 {
				if err := opts.Unmarshal(protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), kept), m.Interface()); err != nil {
					return nil, err
				}
			}
			return moved, nil
		}
		return whole()
	case fd.Kind() == protoreflect.MessageKind && typ == protowire.BytesType,
		fd.Kind() == protoreflect.GroupKind && typ == protowire.StartGroupType:
		var body []byte
		if typ == protowire.BytesType {
			body, _ = protowire.ConsumeBytes(value)
		} else {
			body, _ = protowire.ConsumeGroup(num, value)
		}
		if fd.IsList() {
			list := m.Mutable(fd).List()
			e := list.NewElement()
			if err := rule.merge(body, e.Message(), r, remaining-1); err != nil {
				return nil, err
			}
			list.Append(e)
			return nil, nil
		}
		sub := m.NewField(fd)
		if err := rule.merge(body, sub.Message(), r, remaining-1); err != nil {
			return nil, err
		}
		if m.Has(fd) {
			proto.Merge(m.Mutable(fd).Message().Interface(), sub.Message().Interface())
		} else {
			m.Set(fd, sub)
		}
		return nil, nil
	}
	return whole()
}

// mergeMapEntry merges one entry of a map whose value is a closed enum or
// reaches one. The key is protobuf-go's reading of the entry; the value is
// Java's: a DynamicMessage entry's value field keeps its last declared
// occurrence and otherwise reads the default, and a message value is decoded
// as merge decodes; a generated class's entry whose last value is undeclared
// goes whole to the unknown fields.
func (rule javaDecodeRule) mergeMapEntry(m protoreflect.Message, fd protoreflect.FieldDescriptor,
	occurrence, value []byte, r *closedEnumReach, remaining int,
) ([][]byte, error) {
	opts := proto.UnmarshalOptions{AllowPartial: true, RecursionLimit: remaining, Resolver: rule.resolver}
	holder := m.New()
	if err := opts.Unmarshal(occurrence, holder.Interface()); err != nil {
		return nil, err
	}
	var key protoreflect.MapKey
	holder.Get(fd).Map().Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		key = k
		return false
	})
	vd := fd.MapValue()
	want := protowire.VarintType
	if vd.Message() != nil {
		want = protowire.BytesType
	}
	// The value occurrences, in order; one of another wire type is an unknown
	// field of the entry, in both engines.
	entry, _ := protowire.ConsumeBytes(value)
	var values [][]byte
	for len(entry) > 0 {
		num, typ, n := protowire.ConsumeTag(entry)
		if n < 0 {
			break
		}
		vn := protowire.ConsumeFieldValue(num, typ, entry[n:])
		if vn < 0 {
			break
		}
		if num == 2 && typ == want {
			values = append(values, entry[n:n+vn])
		}
		entry = entry[n+vn:]
	}
	mp := m.Mutable(fd).Map()
	if closedEnumField(vd) {
		if rule.generated {
			if len(values) > 0 {
				if last, _ := protowire.ConsumeVarint(values[len(values)-1]); !declaredEnum(vd, enumNumber(last)) {
					return [][]byte{append([]byte(nil), occurrence...)}, nil
				}
			}
			mp.Set(key, holder.Get(fd).Map().Get(key))
			return nil, nil
		}
		v := vd.Default()
		for _, raw := range values {
			if n, _ := protowire.ConsumeVarint(raw); declaredEnum(vd, enumNumber(n)) {
				v = protoreflect.ValueOfEnum(enumNumber(n))
			}
		}
		mp.Set(key, v)
		return nil, nil
	}
	v := mp.NewValue()
	for _, raw := range values {
		body, _ := protowire.ConsumeBytes(raw)
		if err := rule.merge(body, v.Message(), r, remaining-2); err != nil {
			return nil, err
		}
	}
	mp.Set(key, v)
	return nil, nil
}

// enumNumber is protobuf-java's readEnum of a varint: its low 32 bits, as an
// int32 (protobuf-go's decode truncates the same way).
func enumNumber(v uint64) protoreflect.EnumNumber {
	return protoreflect.EnumNumber(int32(v))
}

// closedEnumField is protobuf-java's FieldDescriptor.legacyEnumFieldTreatedAsClosed
// (Descriptors.java): an enum field is closed when its enum is, and, in a file
// with dependencies, also when the java feature legacy_closed_enum resolves to
// true for the field, which it does by default in a proto2 file: a proto2
// file's field of an open enum (one a proto3 file declares) is closed in Java.
func closedEnumField(fd protoreflect.FieldDescriptor) bool {
	if fd.Kind() != protoreflect.EnumKind || fd.Enum() == nil {
		return false
	}
	if fd.Enum().IsClosed() {
		return true
	}
	file := fd.ParentFile()
	if file == nil || file.Imports().Len() == 0 {
		return false
	}
	return javaLegacyClosedEnum(fd)
}

// javaFeaturesNumber is the java extension of FeatureSet (java_features.proto,
// "extend FeatureSet { optional JavaFeatures java = 1001; }"), whose field 1 is
// legacy_closed_enum.
const javaFeaturesNumber = 1001

// javaLegacyClosedEnum resolves the java feature legacy_closed_enum for fd: the
// nearest explicit setting on the field, its enclosing messages or its file,
// else the edition's default, true for proto2 and false for proto3 and every
// edition since 2023.
func javaLegacyClosedEnum(fd protoreflect.FieldDescriptor) bool {
	if v, ok := explicitLegacyClosedEnum(fd.Options()); ok {
		return v
	}
	for p := fd.Parent(); p != nil; p = p.Parent() {
		if v, ok := explicitLegacyClosedEnum(p.Options()); ok {
			return v
		}
		if _, isFile := p.(protoreflect.FileDescriptor); isFile {
			break
		}
	}
	return fd.ParentFile().Syntax() == protoreflect.Proto2
}

// explicitLegacyClosedEnum reads legacy_closed_enum from a descriptor's
// options' features, where the java extension (not registered in Go) is an
// unknown field.
func explicitLegacyClosedEnum(opts proto.Message) (bool, bool) {
	var features *descriptorpb.FeatureSet
	switch o := opts.(type) {
	case *descriptorpb.FieldOptions:
		features = o.GetFeatures()
	case *descriptorpb.MessageOptions:
		features = o.GetFeatures()
	case *descriptorpb.FileOptions:
		features = o.GetFeatures()
	}
	if features == nil {
		return false, false
	}
	value, found := false, false
	for raw := []byte(features.ProtoReflect().GetUnknown()); len(raw) > 0; {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			break
		}
		vn := protowire.ConsumeFieldValue(num, typ, raw[n:])
		if vn < 0 {
			break
		}
		if num == javaFeaturesNumber && typ == protowire.BytesType {
			java, _ := protowire.ConsumeBytes(raw[n:])
			for len(java) > 0 {
				jn, jt, k := protowire.ConsumeTag(java)
				if k < 0 {
					break
				}
				jv := protowire.ConsumeFieldValue(jn, jt, java[k:])
				if jv < 0 {
					break
				}
				if jn == 1 && jt == protowire.VarintType {
					v, _ := protowire.ConsumeVarint(java[k:])
					value, found = v != 0, true
				}
				java = java[k+jv:]
			}
		}
		raw = raw[n+vn:]
	}
	return value, found
}

func declaredEnum(fd protoreflect.FieldDescriptor, n protoreflect.EnumNumber) bool {
	return fd.Enum().Values().ByNumber(n) != nil
}

// asJava is msg, a record of this type Go is about to evaluate and write, as
// Java reads the bytes Go writes for it: msg itself when it holds no closed
// enum's undeclared number, else a clone with Java's reading (the caller's
// message is not changed).
func (rt *RecordType) asJava(msg proto.Message) proto.Message {
	read, _ := rt.asJavaForSave(msg)
	return read
}

// asJavaForSave is a save's two views of msg, the caller's message, which is
// not changed. read is Java's reading of the bytes the save writes (asJava):
// what the save keys, counts and indexes, and returns. write is the message the
// save serializes: read, but with a DynamicMessage map value that holds an
// undeclared number still holding it. Java reads such an entry keeping the
// number in the entry's own unknown fields, which it writes back after the
// entry's value (measured: key, default, then the number), and a dynamicpb map
// has no per-entry unknown fields to hold it; so the number stays in write's map
// and the map rewrite writes the entry in Java's form (mergeMapEntries,
// canonicalEntry). Both are msg itself when it holds no undeclared number.
func (rt *RecordType) asJavaForSave(msg proto.Message) (read, write proto.Message) {
	if !rt.reachesClosedEnum || !holdsUndeclared(msg.ProtoReflect(), rt.closedEnumReach) {
		return msg, msg
	}
	write = proto.Clone(msg)
	if !closedEnumsAsJavaKeeping(write.ProtoReflect(), rt.closedEnumReach, false, true) {
		return write, write
	}
	read = proto.Clone(write)
	closedEnumsAsJava(read.ProtoReflect(), rt.closedEnumReach, false)
	return read, write
}

// holdsUndeclared reports whether m holds a closed enum's undeclared number
// where r's plans read.
func holdsUndeclared(m protoreflect.Message, r *closedEnumReach) bool {
	found := false
	walkClosedEnums(m, r, func(protoreflect.Message, protoreflect.FieldDescriptor) bool {
		found = true
		return false
	})
	return found
}

// walkClosedEnums calls visit for every populated field of m, at any depth,
// that holds a closed enum's undeclared number, until visit returns false.
func walkClosedEnums(m protoreflect.Message, r *closedEnumReach, visit func(protoreflect.Message, protoreflect.FieldDescriptor) bool) bool {
	plan, r, ok := r.planFor(m.Descriptor())
	if !ok {
		return true
	}
	fields := make([]protoreflect.FieldDescriptor, 0, len(plan.fields))
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
	for _, fd := range fields {
		v := m.Get(fd)
		switch {
		case fd.IsMap():
			value := fd.MapValue()
			more := true
			v.Map().Range(func(_ protoreflect.MapKey, e protoreflect.Value) bool {
				switch {
				case closedEnumField(value):
					if !declaredEnum(value, e.Enum()) {
						more = visit(m, fd)
						return false
					}
				case value.Message() != nil:
					more = walkClosedEnums(e.Message(), r, visit)
				}
				return more
			})
			if !more {
				return false
			}
		case fd.IsList():
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				if closedEnumField(fd) {
					if !declaredEnum(fd, list.Get(i).Enum()) {
						if !visit(m, fd) {
							return false
						}
						break
					}
				} else if fd.Message() != nil && !walkClosedEnums(list.Get(i).Message(), r, visit) {
					return false
				}
			}
		case closedEnumField(fd):
			if !declaredEnum(fd, v.Enum()) && !visit(m, fd) {
				return false
			}
		case fd.Message() != nil:
			if !walkClosedEnums(v.Message(), r, visit) {
				return false
			}
		}
	}
	return true
}

// closedEnumsAsJava gives m, a message Go holds in memory, Java's reading of
// the bytes Go writes for it, in place: a closed enum field holding an
// undeclared number is written as that number and read by Java as an unknown
// field, so the number moves to m's unknown fields, at any depth, and a
// repeated field keeps its declared elements in order. A map value of a closed
// enum reads as the enum's default for a DynamicMessage (the entry is kept);
// for a generated class the entry goes to the unknown fields whole.
func closedEnumsAsJava(m protoreflect.Message, r *closedEnumReach, generated bool) {
	closedEnumsAsJavaKeeping(m, r, generated, false)
}

// closedEnumsAsJavaKeeping is closedEnumsAsJava, leaving a DynamicMessage map
// value that holds an undeclared number in place when keepMapValues
// (asJavaForSave's write view). It reports whether it left one.
func closedEnumsAsJavaKeeping(m protoreflect.Message, r *closedEnumReach, generated, keepMapValues bool) bool {
	type fix struct {
		m  protoreflect.Message
		fd protoreflect.FieldDescriptor
	}
	var fixes []fix
	walkClosedEnums(m, r, func(owner protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
		fixes = append(fixes, fix{owner, fd})
		return true
	})
	kept := false
	for _, f := range fixes {
		if keepMapValues && !generated && f.fd.IsMap() {
			kept = true
			continue
		}
		moveUndeclared(f.m, f.fd, generated)
	}
	return kept
}

// moveUndeclared moves the undeclared numbers of one closed-enum field of m.
func moveUndeclared(m protoreflect.Message, fd protoreflect.FieldDescriptor, generated bool) {
	var moved [][]byte
	switch {
	case fd.IsMap():
		value := fd.MapValue()
		mp := m.Mutable(fd).Map()
		var keys []protoreflect.MapKey
		mp.Range(func(k protoreflect.MapKey, e protoreflect.Value) bool {
			if !declaredEnum(value, e.Enum()) {
				keys = append(keys, k)
			}
			return true
		})
		for _, k := range keys {
			if !generated {
				mp.Set(k, value.Default())
				continue
			}
			// The entry as Go writes it, moved whole.
			holder := m.New()
			holder.Mutable(fd).Map().Set(k, mp.Get(k))
			if entry, err := (proto.MarshalOptions{AllowPartial: true}).Marshal(holder.Interface()); err == nil {
				moved = append(moved, entry)
			}
			mp.Clear(k)
		}
	case fd.IsList():
		list := m.Get(fd).List()
		var kept []protoreflect.EnumNumber
		for i := 0; i < list.Len(); i++ {
			if n := list.Get(i).Enum(); declaredEnum(fd, n) {
				kept = append(kept, n)
			} else {
				moved = append(moved, appendUnknownEnum(nil, fd.Number(), n))
			}
		}
		m.Clear(fd)
		if len(kept) > 0 {
			out := m.Mutable(fd).List()
			for _, n := range kept {
				out.Append(protoreflect.ValueOfEnum(n))
			}
		}
	default:
		moved = append(moved, appendUnknownEnum(nil, fd.Number(), m.Get(fd).Enum()))
		m.Clear(fd)
	}
	if len(moved) > 0 {
		unknown := []byte(m.GetUnknown())
		for _, f := range moved {
			unknown = insertUnknownField(unknown, f)
		}
		m.SetUnknown(unknown)
	}
}

// javaUnknownKind orders a field's unknown values as protobuf-java's
// UnknownFieldSet.Field writes them: varints, fixed32s, fixed64s,
// length-delimited values, groups.
func javaUnknownKind(t protowire.Type) int {
	switch t {
	case protowire.VarintType:
		return 0
	case protowire.Fixed32Type:
		return 1
	case protowire.Fixed64Type:
		return 2
	case protowire.BytesType:
		return 3
	}
	return 4
}

// insertUnknownField inserts the field f into the unknown fields raw where
// protobuf-java's UnknownFieldSet holds it: fields in number order and, within
// one number, by kind (javaUnknownKind), each kind in the order it arrived.
// Unknown fields Java wrote are in that order already, so a value set aside
// among them goes back where it was.
func insertUnknownField(raw, f []byte) []byte {
	num, ftyp, _ := protowire.ConsumeTag(f)
	for at := 0; at < len(raw); {
		n, typ, tl := protowire.ConsumeTag(raw[at:])
		if tl < 0 {
			break
		}
		vl := protowire.ConsumeFieldValue(n, typ, raw[at+tl:])
		if vl < 0 {
			break
		}
		if n > num || n == num && javaUnknownKind(typ) > javaUnknownKind(ftyp) {
			out := make([]byte, 0, len(raw)+len(f))
			return append(append(append(out, raw[:at]...), f...), raw[at:]...)
		}
		at += tl + vl
	}
	return append(raw, f...)
}

// appendUnknownEnum appends n as the unknown varint field protobuf-java keeps it
// as (UnknownFieldSet.mergeVarintField: the int32 sign-extended to 64 bits).
func appendUnknownEnum(b []byte, num protowire.Number, n protoreflect.EnumNumber) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), uint64(int64(n)))
}
