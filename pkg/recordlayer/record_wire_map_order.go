package recordlayer

import (
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
// Go keeps a map as a Go map, which has no order, so:
//   - a record Go SAVES is evaluated in key order (mapKeyLess) and is written in
//     key order: serializeUnion marshals a type that reaches a map field with the
//     deterministic marshal, which sorts map entries the same way, never with
//     vtproto's MarshalVT, whose map order is Go's random iteration order;
//   - a record Go DECODES from stored bytes keeps those bytes (recordWire), and
//     its map entries are read back from them in wire order, duplicates of a key
//     included, as a DynamicMessage holds them.
//
// The wire order is used only while the message still holds what its bytes
// hold: a caller may mutate a loaded message, and one whose map no longer
// matches its bytes is evaluated in key order, as a message Go is about to save.

// recordWire is the stored bytes of a record message decoded from them, kept
// only for a record type that reaches a map field.
type recordWire struct {
	bytes []byte

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

// newRecordWire returns the wire of a record message of type md decoded from
// bytes, or nil when md reaches no map field.
func newRecordWire(md protoreflect.MessageDescriptor, bytes []byte) *recordWire {
	if !messageReachesMap(md) {
		return nil
	}
	return &recordWire{bytes: bytes}
}

// mapEntries returns the entries of map field fd of message m, a message
// within root, in the order root's bytes hold them, and whether that order
// applies: false when root was not decoded from bytes, when the bytes do not
// parse as they did when root was decoded, or when m's map no longer holds
// what the bytes hold (the message was changed after it was loaded).
func (w *recordWire) mapEntries(root proto.Message, m protoreflect.Message, fd protoreflect.FieldDescriptor) ([]protoreflect.Message, bool) {
	if w == nil || root == nil {
		return nil, false
	}
	w.once.Do(func() {
		w.entries = map[wireMapField][]protoreflect.Message{}
		w.err = collectWireMapEntries(root.ProtoReflect(), w.bytes, w.entries)
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
func collectWireMapEntries(m protoreflect.Message, raw []byte, out map[wireMapField][]protoreflect.Message) error {
	fields := m.Descriptor().Fields()
	occurrences := map[protoreflect.FieldNumber]int{}
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return protowire.ParseError(n)
		}
		raw = raw[n:]
		size := protowire.ConsumeFieldValue(num, typ, raw)
		if size < 0 {
			return protowire.ParseError(size)
		}
		value := raw[:size]
		raw = raw[size:]

		fd := fields.ByNumber(num)
		if fd == nil || (fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind) {
			continue
		}
		var body []byte
		switch {
		case typ == protowire.BytesType && fd.Kind() == protoreflect.MessageKind:
			b, k := protowire.ConsumeBytes(value)
			if k < 0 {
				return protowire.ParseError(k)
			}
			body = b
		case typ == protowire.StartGroupType && fd.Kind() == protoreflect.GroupKind:
			b, k := protowire.ConsumeGroup(num, value)
			if k < 0 {
				return protowire.ParseError(k)
			}
			body = b
		default:
			// A wire type the field does not have: the decoder kept it as
			// an unknown field.
			continue
		}

		switch {
		case fd.IsMap():
			entry := dynamicpb.NewMessage(fd.Message())
			if err := (proto.UnmarshalOptions{Merge: true}).Unmarshal(body, entry); err != nil {
				return err
			}
			holdKeyAndValue(entry)
			if messageReachesMap(fd.Message()) {
				if err := collectWireMapEntries(entry, body, out); err != nil {
					return err
				}
			}
			key := wireMapField{msg: m.Interface(), field: num}
			out[key] = append(out[key], entry)
		case !messageReachesMap(fd.Message()):
			continue
		case fd.IsList():
			i := occurrences[num]
			occurrences[num]++
			list := m.Get(fd).List()
			if i >= list.Len() {
				return fmt.Errorf("field %s occurs more often in the bytes than in the message", fd.FullName())
			}
			if err := collectWireMapEntries(list.Get(i).Message(), body, out); err != nil {
				return err
			}
		default:
			if !m.Has(fd) {
				return fmt.Errorf("field %s is in the bytes and not in the message", fd.FullName())
			}
			if err := collectWireMapEntries(m.Get(fd).Message(), body, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// holdKeyAndValue sets an entry's key and value to what the decoder puts into
// a Go map for it, the default of a field the entry's bytes lack, so an entry
// read from bytes has the shape of one sortedMapEntries builds from the map:
// both fields set.
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

// mapReach memoizes messageReachesMap per descriptor.
var mapReach sync.Map // protoreflect.MessageDescriptor -> bool

// messageReachesMap reports whether a message of type md can hold a map field,
// at any depth.
func messageReachesMap(md protoreflect.MessageDescriptor) bool {
	if v, ok := mapReach.Load(md); ok {
		return v.(bool)
	}
	reaches := reachesMap(md, map[protoreflect.FullName]bool{})
	mapReach.Store(md, reaches)
	return reaches
}

func reachesMap(md protoreflect.MessageDescriptor, visiting map[protoreflect.FullName]bool) bool {
	if visiting[md.FullName()] {
		return false
	}
	visiting[md.FullName()] = true
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.IsMap() {
			return true
		}
		if (fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind) && reachesMap(fd.Message(), visiting) {
			return true
		}
	}
	return false
}

// sortedMapEntries is a map's entries in key order (mapKeyLess), the order Go
// evaluates a message it has not decoded from bytes, and the order
// serializeUnion writes them in. Each entry holds the key and the value.
func sortedMapEntries(mp protoreflect.Map, fd protoreflect.FieldDescriptor) []protoreflect.Message {
	keys := make([]protoreflect.MapKey, 0, mp.Len())
	mp.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		keys = append(keys, k)
		return true
	})
	sort.Slice(keys, func(i, j int) bool { return mapKeyLess(keys[i], keys[j]) })
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
