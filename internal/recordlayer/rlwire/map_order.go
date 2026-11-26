package rlwire

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A map field's entries are evaluated in the order the record's stored bytes
// hold them, as Java evaluates them. Java reads a stored record as a
// DynamicMessage, which keeps a map field as the list of entry messages in the
// order it parsed them, and a record it saves from a generated message is
// serialized in the map's own iteration order, which is the order it evaluates;
// either way a record's entries are visited in its bytes' order. The order is
// visible in stored bytes wherever two entries write one key and the last write
// wins: a covering VALUE index whose key two entries share (the value is the
// last entry's), and a TEXT index whose group two entries share a token in.
//
// That is the default serializer's behaviour (DynamicMessageRecordSerializer,
// FDBRecordStore's default). A store opened with a serializer that parses into
// a generated class reads a map into a LinkedHashMap, which collapses a key
// written twice to one entry in its first position; Go reads as the default
// serializer does.
//
// Go keeps a map as a Go map, which has no order, so:
//   - a record Go DECODES from stored bytes keeps those bytes (RecordWire), and
//     its map entries are read back from them in wire order, duplicates of a key
//     included, as a DynamicMessage holds them;
//   - a record Go SAVES is written with its map entries in a chosen order and is
//     evaluated from the bytes written: serializeUnionOver marshals a type that
//     reaches a map with the deterministic marshal (never vtproto's MarshalVT,
//     whose map order is Go's random iteration order), then writes each map as
//     Java's load-then-save of the record it replaces writes it (RewriteMaps):
//     that record's entries in their order, a key written twice included, each
//     re-encoded with its key and value, for every key whose value is
//     unchanged; a changed key once, at its first position; new keys after, in
//     key order (MapKeyLess). A new record's maps are in key order. So a record
//     Go loads and saves unchanged has its map entries written as Java writes
//     them back, and its index entries stay what they were; its field order is
//     the deterministic marshal's, a oneof member after the other fields, where
//     Java writes field-number order (DIVERGENCES.md, map entry order).
//
// The wire order is used only while the message still holds what its bytes
// hold: a caller may mutate a loaded message, and one whose map no longer
// matches its bytes is evaluated in key order, as a message Go is about to save.

// PartialUnmarshalFunc unmarshals a serialized record or map entry partially
// under Java's dynamic-message decode rules.
type PartialUnmarshalFunc func(b []byte, m proto.Message) error

var errNilPartialUnmarshal = errors.New("rlwire: partial unmarshal function is required")

// RecordWire is the stored bytes of a record message decoded from them, kept
// only for a record type that reaches a map field.
type RecordWire struct {
	bytes            []byte
	reach            MapReach
	unmarshalPartial PartialUnmarshalFunc

	once    sync.Once
	entries map[wireMapField][]protoreflect.Message
	err     error
}

// wireMapField names one map field of one message within a decoded record: the
// message by identity (a generated message's pointer, or a dynamic message's),
// which is the message the key expression evaluates.
type wireMapField struct {
	msg   proto.Message
	field protoreflect.FieldNumber
}

// NewRecordWire returns the wire of a record decoded from raw, or nil when
// reachesMap is false.
func NewRecordWire(reachesMap bool, reach MapReach, raw []byte, unmarshalPartial PartialUnmarshalFunc) *RecordWire {
	if !reachesMap {
		return nil
	}
	return &RecordWire{
		bytes:            raw,
		reach:            reach,
		unmarshalPartial: unmarshalPartial,
	}
}

// Bytes returns the stored record bytes held by w, or nil when w is nil.
func (w *RecordWire) Bytes() []byte {
	if w == nil {
		return nil
	}
	return w.bytes
}

// MapEntries returns the entries of map field fd of message m, a message
// within root, in the order root's bytes hold them, and whether that order
// applies: false when root was not decoded from bytes, when the bytes do not
// parse as they did when root was decoded, or when m's map no longer holds
// what the bytes hold (the message was changed after it was loaded).
func (w *RecordWire) MapEntries(root proto.Message, m protoreflect.Message, fd protoreflect.FieldDescriptor) ([]protoreflect.Message, bool) {
	if w == nil || root == nil {
		return nil, false
	}
	w.once.Do(func() {
		if w.unmarshalPartial == nil {
			w.err = errNilPartialUnmarshal
			return
		}
		w.entries = map[wireMapField][]protoreflect.Message{}
		reach := w.reach
		if _, ok := reach[root.ProtoReflect().Descriptor()]; !ok {
			reach = NewMapReach(root.ProtoReflect().Descriptor())
		}
		w.err = collectWireMapEntries(root.ProtoReflect(), w.bytes, w.entries, reach, w.unmarshalPartial)
	})
	if w.err != nil {
		return nil, false
	}
	entries, ok := w.entries[wireMapField{msg: m.Interface(), field: fd.Number()}]
	if !ok {
		// The bytes hold no entry of this field: in order only if the map is
		// empty too.
		return nil, m.Get(fd).Map().Len() == 0
	}
	if !wireEntriesMatchMap(entries, m.Get(fd).Map(), fd) {
		return nil, false
	}
	return entries, true
}

// collectWireMapEntries walks raw, the bytes m was decoded from, alongside m,
// and records the entries of every map field it reaches, in wire order. A
// singular message field that occurs more than once is merged by the decoder,
// so its occurrences are walked into the one message in turn, which appends
// their map entries in order, as a merge of DynamicMessages appends them. The
// i-th occurrence of a repeated message field is the list's i-th element.
func collectWireMapEntries(m protoreflect.Message, raw []byte, out map[wireMapField][]protoreflect.Message, reach MapReach, unmarshalPartial PartialUnmarshalFunc) error {
	if unmarshalPartial == nil {
		return errNilPartialUnmarshal
	}
	fields := m.Descriptor().Fields()
	kept, err := oneofSurvivors(fields, raw)
	if err != nil {
		return err
	}
	occurrences := map[protoreflect.FieldNumber]int{}
	for offset := 0; offset < len(raw); {
		fd, body, n, err := fieldBody(fields, raw[offset:])
		if err != nil {
			return err
		}
		at := offset
		offset += n
		if fd == nil || body == nil || !kept(fd, at) {
			continue
		}
		switch {
		case fd.IsMap():
			entry, err := parseMapEntry(fd, body, unmarshalPartial)
			if err != nil {
				return err
			}
			if reach.Reaches(fd.Message()) {
				if err := collectWireMapEntries(entry, body, out, reach, unmarshalPartial); err != nil {
					return err
				}
			}
			key := wireMapField{msg: m.Interface(), field: fd.Number()}
			out[key] = append(out[key], entry)
		case !reach.Reaches(fd.Message()):
			continue
		case fd.IsList():
			i := occurrences[fd.Number()]
			occurrences[fd.Number()]++
			list := m.Get(fd).List()
			if i >= list.Len() {
				return fmt.Errorf("field %s occurs more often in the bytes than in the message", fd.FullName())
			}
			if err := collectWireMapEntries(list.Get(i).Message(), body, out, reach, unmarshalPartial); err != nil {
				return err
			}
		default:
			if !m.Has(fd) {
				return fmt.Errorf("field %s is in the bytes and not in the message", fd.FullName())
			}
			if err := collectWireMapEntries(m.Get(fd).Message(), body, out, reach, unmarshalPartial); err != nil {
				return err
			}
		}
	}
	return nil
}

// oneofSurvivors reports which field occurrences of raw a decoder keeps for a
// oneof: those of the member written last, from its first occurrence after
// the last occurrence of any other member, since writing a member clears the
// oneof's other member, and a later occurrence of one member merges into it.
// An occurrence of a field in no oneof is always kept.
func oneofSurvivors(fields protoreflect.FieldDescriptors, raw []byte) (func(protoreflect.FieldDescriptor, int) bool, error) {
	member := map[protoreflect.OneofDescriptor]protoreflect.FieldDescriptor{}
	since := map[protoreflect.OneofDescriptor]int{}
	for offset := 0; offset < len(raw); {
		fd, _, n, err := fieldBody(fields, raw[offset:])
		if err != nil {
			return nil, err
		}
		if fd != nil {
			if od := fd.ContainingOneof(); od != nil && member[od] != fd {
				member[od], since[od] = fd, offset
			}
		}
		offset += n
	}
	return func(fd protoreflect.FieldDescriptor, offset int) bool {
		od := fd.ContainingOneof()
		return od == nil || (member[od] == fd && offset >= since[od])
	}, nil
}

// holdKeyAndValue sets an entry's key and value to what the decoder puts into
// a Go map for it, the default of a field the entry's bytes lack, so an entry
// read from bytes has the shape of one SortedMapEntries builds from the map:
// both fields set. Java's DynamicMessage fills a missing map value's default
// too (measured: an entry without its value indexes as 0).
func holdKeyAndValue(entry *dynamicpb.Message) {
	fields := entry.Descriptor().Fields()
	for _, fd := range []protoreflect.FieldDescriptor{fields.ByNumber(1), fields.ByNumber(2)} {
		if entry.Has(fd) {
			continue
		}
		if fd.Message() != nil {
			entry.Set(fd, protoreflect.ValueOfMessage(dynamicpb.NewMessage(fd.Message())))
		} else {
			entry.Set(fd, fd.Default())
		}
	}
}

// wireEntriesMatchMap reports whether entries, read with a later entry of a key
// replacing an earlier one, hold exactly what the map holds.
func wireEntriesMatchMap(entries []protoreflect.Message, mp protoreflect.Map, fd protoreflect.FieldDescriptor) bool {
	keyField, valueField := fd.Message().Fields().ByNumber(1), fd.Message().Fields().ByNumber(2)
	last := make(map[any]protoreflect.Value, len(entries))
	for _, e := range entries {
		last[e.Get(keyField).MapKey().Interface()] = e.Get(valueField)
	}
	if len(last) != mp.Len() {
		return false
	}
	match := true
	mp.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		w, ok := last[k.Interface()]
		if !ok || !w.Equal(v) {
			match = false
		}
		return match
	})
	return match
}

// MapReach answers whether a message type can hold a map field at any depth.
// NewMapReach fills it at Build for every message type a meta-data's record
// types reach, and it is read-only from then on, so the walks of every record
// of the meta-data share it: a type it lacks (a descriptor from elsewhere) is
// answered without being stored. It is never a global cache: a meta-data load
// builds fresh descriptors, and a process-wide map keyed by them would keep
// every loaded descriptor graph alive; this one lives as long as the meta-data.
type MapReach map[protoreflect.MessageDescriptor]bool

// NewMapReach is the reach of every message type roots reach, message and group
// fields followed: a type reaches a map if it has a map field or a message
// field of a type that does, found as a fixpoint so recursive types terminate.
func NewMapReach(roots ...protoreflect.MessageDescriptor) MapReach {
	return NewTypeReach(func(md protoreflect.MessageDescriptor) bool {
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			if fields.Get(i).IsMap() {
				return true
			}
		}
		return false
	}, roots...)
}

// NewTypeReach is, for every message type roots reach (message and group fields
// followed), whether it or a type it reaches satisfies holds, found as a
// fixpoint so recursive types terminate.
func NewTypeReach(holds func(protoreflect.MessageDescriptor) bool, roots ...protoreflect.MessageDescriptor) map[protoreflect.MessageDescriptor]bool {
	var types []protoreflect.MessageDescriptor
	seen := map[protoreflect.MessageDescriptor]bool{}
	var visit func(md protoreflect.MessageDescriptor)
	visit = func(md protoreflect.MessageDescriptor) {
		if md == nil || seen[md] {
			return
		}
		seen[md] = true
		types = append(types, md)
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			visit(fields.Get(i).Message())
		}
	}
	for _, md := range roots {
		visit(md)
	}
	// Every type of the closure gets its answer, false included.
	r := make(map[protoreflect.MessageDescriptor]bool, len(types))
	for _, md := range types {
		r[md] = holds(md)
	}
	for changed := true; changed; {
		changed = false
		for _, md := range types {
			if r[md] {
				continue
			}
			fields := md.Fields()
			for i := 0; i < fields.Len(); i++ {
				if fd := fields.Get(i); fd.Message() != nil && r[fd.Message()] {
					r[md], changed = true, true
					break
				}
			}
		}
	}
	return r
}

// Reaches reports whether md can hold a map field at any depth.
func (r MapReach) Reaches(md protoreflect.MessageDescriptor) bool {
	if v, ok := r[md]; ok {
		return v
	}
	return NewMapReach(md)[md]
}

// parseMapEntry reads one map entry's bytes as the entry message, its key and
// value set as the Go map the decoder filled holds them (holdKeyAndValue).
// Partial: a required field unset in a map value was the decoder's to refuse.
func parseMapEntry(fd protoreflect.FieldDescriptor, body []byte, unmarshalPartial PartialUnmarshalFunc) (*dynamicpb.Message, error) {
	if unmarshalPartial == nil {
		return nil, errNilPartialUnmarshal
	}
	entry := dynamicpb.NewMessage(fd.Message())
	// Java's DynamicMessage entry keeps a value it cannot read as an unknown
	// field and reads the default (proto_closed_enums.go).
	if err := unmarshalPartial(body, entry); err != nil {
		return nil, err
	}
	holdKeyAndValue(entry)
	return entry, nil
}

// fieldBody splits one field's value off raw: the field (nil when unknown, or
// when the occurrence's wire type is not the field's, which a decoder keeps as
// an unknown field), the bytes of a message or group (nil for any other
// value), and the length consumed.
func fieldBody(fields protoreflect.FieldDescriptors, raw []byte) (protoreflect.FieldDescriptor, []byte, int, error) {
	num, typ, n := protowire.ConsumeTag(raw)
	if n < 0 {
		return nil, nil, 0, protowire.ParseError(n)
	}
	size := protowire.ConsumeFieldValue(num, typ, raw[n:])
	if size < 0 {
		return nil, nil, 0, protowire.ParseError(size)
	}
	value := raw[n : n+size]
	fd := fields.ByNumber(num)
	if fd == nil || !wireTypeFits(fd, typ) {
		return nil, nil, n + size, nil
	}
	switch fd.Kind() {
	case protoreflect.MessageKind:
		b, k := protowire.ConsumeBytes(value)
		if k < 0 {
			return nil, nil, 0, protowire.ParseError(k)
		}
		return fd, b, n + size, nil
	case protoreflect.GroupKind:
		b, k := protowire.ConsumeGroup(num, value)
		if k < 0 {
			return nil, nil, 0, protowire.ParseError(k)
		}
		return fd, b, n + size, nil
	}
	return fd, nil, n + size, nil
}

// wireTypeFits reports whether an occurrence of wire type typ is a value of fd,
// as the decoder reads it (a packed repeated scalar is length-delimited).
func wireTypeFits(fd protoreflect.FieldDescriptor, typ protowire.Type) bool {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.StringKind, protoreflect.BytesKind:
		return typ == protowire.BytesType
	case protoreflect.GroupKind:
		return typ == protowire.StartGroupType
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return typ == protowire.Fixed32Type || (fd.IsList() && typ == protowire.BytesType)
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return typ == protowire.Fixed64Type || (fd.IsList() && typ == protowire.BytesType)
	default:
		return typ == protowire.VarintType || (fd.IsList() && typ == protowire.BytesType)
	}
}

// MapKeyLess orders map keys of one kind: bool, integers, or strings.
func MapKeyLess(a, b protoreflect.MapKey) bool {
	switch av := a.Interface().(type) {
	case bool:
		return !av && b.Bool()
	case string:
		return av < b.String()
	case int32, int64:
		return a.Int() < b.Int()
	default:
		return a.Uint() < b.Uint()
	}
}

// SortedMapEntries is a map's entries in key order (MapKeyLess), the order Go
// evaluates a message it has not decoded from bytes, and the order
// serializeUnionOver writes a new record's in. Each entry holds the key and the value.
func SortedMapEntries(mp protoreflect.Map, fd protoreflect.FieldDescriptor) []protoreflect.Message {
	keys := make([]protoreflect.MapKey, 0, mp.Len())
	mp.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		keys = append(keys, k)
		return true
	})
	sort.Slice(keys, func(i, j int) bool { return MapKeyLess(keys[i], keys[j]) })
	entryDesc := fd.Message()
	keyField, valueField := entryDesc.Fields().ByNumber(1), entryDesc.Fields().ByNumber(2)
	entries := make([]protoreflect.Message, 0, len(keys))
	for _, k := range keys {
		entry := dynamicpb.NewMessage(entryDesc)
		entry.Set(keyField, k.Value())
		entry.Set(valueField, mp.Get(k))
		entries = append(entries, entry)
	}
	return entries
}
