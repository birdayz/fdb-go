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
// That is the default serializer's behaviour (DynamicMessageRecordSerializer,
// FDBRecordStore's default). A store opened with a serializer that parses into
// a generated class reads a map into a LinkedHashMap, which collapses a key
// written twice to one entry in its first position; Go reads as the default
// serializer does.
//
// Go keeps a map as a Go map, which has no order, so:
//   - a record Go DECODES from stored bytes keeps those bytes (recordWire), and
//     its map entries are read back from them in wire order, duplicates of a key
//     included, as a DynamicMessage holds them;
//   - a record Go SAVES is written with its map entries in a chosen order and is
//     evaluated from the bytes written: serializeUnion marshals a type that
//     reaches a map with the deterministic marshal (never vtproto's MarshalVT,
//     whose map order is Go's random iteration order), then orders each map as
//     the record it replaces stored it, the keys the replaced record held first
//     in its order and the rest after, in key order (mapKeyLess), as Java's
//     parsed map keeps the stored order and appends a new key. A new record's
//     maps are in key order. So a record Go loads and saves unchanged keeps its
//     bytes' map order, and its index entries stay what they were.
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

// newRecordWire returns the wire of a record of type rt decoded from bytes, or
// nil when rt reaches no map field.
func newRecordWire(rt *RecordType, bytes []byte) *recordWire {
	if rt == nil || !rt.reachesMap {
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
		w.err = collectWireMapEntries(root.ProtoReflect(), w.bytes, w.entries, mapReach{})
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
func collectWireMapEntries(m protoreflect.Message, raw []byte, out map[wireMapField][]protoreflect.Message, reach mapReach) error {
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
			entry, err := parseMapEntry(fd, body)
			if err != nil {
				return err
			}
			if reach.reaches(fd.Message()) {
				if err := collectWireMapEntries(entry, body, out, reach); err != nil {
					return err
				}
			}
			key := wireMapField{msg: m.Interface(), field: fd.Number()}
			out[key] = append(out[key], entry)
		case !reach.reaches(fd.Message()):
			continue
		case fd.IsList():
			i := occurrences[fd.Number()]
			occurrences[fd.Number()]++
			list := m.Get(fd).List()
			if i >= list.Len() {
				return fmt.Errorf("field %s occurs more often in the bytes than in the message", fd.FullName())
			}
			if err := collectWireMapEntries(list.Get(i).Message(), body, out, reach); err != nil {
				return err
			}
		default:
			if !m.Has(fd) {
				return fmt.Errorf("field %s is in the bytes and not in the message", fd.FullName())
			}
			if err := collectWireMapEntries(m.Get(fd).Message(), body, out, reach); err != nil {
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
// read from bytes has the shape of one sortedMapEntries builds from the map:
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

// mapReach answers whether a message type can hold a map field at any depth,
// memoized for one walk or one meta-data build. It is never a global cache: a
// meta-data load builds fresh descriptors, and a process-wide map keyed by them
// would keep every loaded descriptor graph alive.
type mapReach map[protoreflect.MessageDescriptor]bool

func (r mapReach) reaches(md protoreflect.MessageDescriptor) bool {
	if v, ok := r[md]; ok {
		return v
	}
	v := reachesMap(md, map[protoreflect.FullName]bool{})
	r[md] = v
	return v
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

// parseMapEntry reads one map entry's bytes as the entry message, its key and
// value set as the Go map the decoder filled holds them (holdKeyAndValue).
// Partial: a required field unset in a map value was the decoder's to refuse.
func parseMapEntry(fd protoreflect.FieldDescriptor, body []byte) (*dynamicpb.Message, error) {
	entry := dynamicpb.NewMessage(fd.Message())
	if err := (proto.UnmarshalOptions{Merge: true, AllowPartial: true}).Unmarshal(body, entry); err != nil {
		return nil, err
	}
	holdKeyAndValue(entry)
	return entry, nil
}

// fieldBody splits one field's value off raw: the field, its wire type, the
// bytes of a message or group (nil for any other value), and the rest of raw.
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
	if fd == nil {
		return nil, nil, n + size, nil
	}
	switch {
	case typ == protowire.BytesType && fd.Kind() == protoreflect.MessageKind:
		b, k := protowire.ConsumeBytes(value)
		if k < 0 {
			return nil, nil, 0, protowire.ParseError(k)
		}
		return fd, b, n + size, nil
	case typ == protowire.StartGroupType && fd.Kind() == protoreflect.GroupKind:
		b, k := protowire.ConsumeGroup(num, value)
		if k < 0 {
			return nil, nil, 0, protowire.ParseError(k)
		}
		return fd, b, n + size, nil
	}
	return fd, nil, n + size, nil
}

// mapEntryValueBody is the bytes of a map entry's message value (field 2), nil
// when it has none.
func mapEntryValueBody(fd protoreflect.FieldDescriptor, entry []byte) ([]byte, error) {
	var value []byte
	fields := fd.Message().Fields()
	for len(entry) > 0 {
		vfd, body, n, err := fieldBody(fields, entry)
		if err != nil {
			return nil, err
		}
		if vfd != nil && vfd.Number() == 2 && body != nil {
			value = body
		}
		entry = entry[n:]
	}
	return value, nil
}

// mapOrders is, per map instance of a record (a path of field numbers, repeated
// indexes and map keys), its keys in the order the record's bytes hold them, a
// key written twice in its first position, as the LinkedHashMap of Java's
// generated-message parse keeps it (a Go map holds the key once, so a Go save
// collapses it; its value is the last one, as both engines' maps hold it).
type mapOrders map[string][]any

// collectMapOrders reads the map orders of raw, a message of type md.
func collectMapOrders(md protoreflect.MessageDescriptor, raw []byte, path string, out mapOrders, reach mapReach) error {
	occurrences := map[protoreflect.FieldNumber]int{}
	fields := md.Fields()
	kept, err := oneofSurvivors(fields, raw)
	if err != nil {
		return err
	}
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
			entry, err := parseMapEntry(fd, body)
			if err != nil {
				return err
			}
			key := entry.Get(fd.MapKey()).MapKey().Interface()
			at := fmt.Sprintf("%s/%d", path, fd.Number())
			seen := false
			for _, k := range out[at] {
				if k == key {
					seen = true
					break
				}
			}
			if !seen {
				out[at] = append(out[at], key)
			}
			if vmd := fd.MapValue().Message(); vmd != nil && reach.reaches(vmd) {
				value, err := mapEntryValueBody(fd, body)
				if err != nil {
					return err
				}
				if err := collectMapOrders(vmd, value, fmt.Sprintf("%s{%v}", at, key), out, reach); err != nil {
					return err
				}
			}
		case !reach.reaches(fd.Message()):
		case fd.IsList():
			i := occurrences[fd.Number()]
			occurrences[fd.Number()]++
			if err := collectMapOrders(fd.Message(), body, fmt.Sprintf("%s/%d[%d]", path, fd.Number(), i), out, reach); err != nil {
				return err
			}
		default:
			if err := collectMapOrders(fd.Message(), body, fmt.Sprintf("%s/%d", path, fd.Number()), out, reach); err != nil {
				return err
			}
		}
	}
	return nil
}

// orderMapEntries rewrites raw, a message of type md, in place: each run of
// one map field's entries is put in prior's order for its instance, the keys
// prior lists first and the rest after in their current order. Only the order
// of whole entries moves, so no length changes.
func orderMapEntries(md protoreflect.MessageDescriptor, raw []byte, path string, prior mapOrders, reach mapReach) error {
	type entryRange struct {
		start, end int
		key        any
	}
	occurrences := map[protoreflect.FieldNumber]int{}
	fields := md.Fields()
	var run []entryRange
	var runField protoreflect.FieldDescriptor
	flush := func() {
		if len(run) > 1 {
			rank := map[any]int{}
			for i, k := range prior[fmt.Sprintf("%s/%d", path, runField.Number())] {
				rank[k] = i
			}
			ordered := append([]entryRange(nil), run...)
			sort.SliceStable(ordered, func(i, j int) bool {
				ri, iok := rank[ordered[i].key]
				rj, jok := rank[ordered[j].key]
				switch {
				case iok && jok:
					return ri < rj
				default:
					return iok && !jok
				}
			})
			buf := make([]byte, 0, run[len(run)-1].end-run[0].start)
			for _, e := range ordered {
				buf = append(buf, raw[e.start:e.end]...)
			}
			copy(raw[run[0].start:], buf)
		}
		run, runField = nil, nil
	}
	for offset := 0; offset < len(raw); {
		fd, body, n, err := fieldBody(fields, raw[offset:])
		if err != nil {
			return err
		}
		start := offset
		offset += n
		if runField != nil && fd != runField {
			flush()
		}
		if fd == nil || body == nil {
			continue
		}
		switch {
		case fd.IsMap():
			entry, err := parseMapEntry(fd, body)
			if err != nil {
				return err
			}
			key := entry.Get(fd.MapKey()).MapKey().Interface()
			if vmd := fd.MapValue().Message(); vmd != nil && reach.reaches(vmd) {
				value, err := mapEntryValueBody(fd, body)
				if err != nil {
					return err
				}
				if err := orderMapEntries(vmd, value, fmt.Sprintf("%s/%d{%v}", path, fd.Number(), key), prior, reach); err != nil {
					return err
				}
			}
			runField = fd
			run = append(run, entryRange{start: start, end: offset, key: key})
		case !reach.reaches(fd.Message()):
		case fd.IsList():
			i := occurrences[fd.Number()]
			occurrences[fd.Number()]++
			if err := orderMapEntries(fd.Message(), body, fmt.Sprintf("%s/%d[%d]", path, fd.Number(), i), prior, reach); err != nil {
				return err
			}
		default:
			if err := orderMapEntries(fd.Message(), body, fmt.Sprintf("%s/%d", path, fd.Number()), prior, reach); err != nil {
				return err
			}
		}
	}
	flush()
	return nil
}

// marshalMapRecord is serializeUnion's inner bytes for a record whose type
// reaches a map: the deterministic marshal (map entries in key order), each
// map then ordered as prior, the inner bytes of the record it replaces (nil for
// a new record), stored it. The required-field check is the one the record's
// marshal path makes without a map: none for a vtproto message, protobuf-go's
// for any other.
func marshalMapRecord(record proto.Message, prior []byte) ([]byte, error) {
	_, vt := record.(interface{ MarshalVT() ([]byte, error) })
	inner, err := proto.MarshalOptions{Deterministic: true, AllowPartial: vt}.Marshal(record)
	if err != nil || prior == nil {
		return inner, err
	}
	md := record.ProtoReflect().Descriptor()
	reach := mapReach{}
	orders := mapOrders{}
	if err := collectMapOrders(md, prior, "", orders, reach); err != nil {
		// The replaced record's bytes do not parse as this type: nothing to
		// keep, and the decoder refuses them wherever they are read.
		return inner, nil //nolint:nilerr // key order stands
	}
	if err := orderMapEntries(md, inner, "", orders, reach); err != nil {
		return nil, err
	}
	return inner, nil
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
