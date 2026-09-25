package recordlayer

import (
	"errors"
	"fmt"
	"math"
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
//     evaluated from the bytes written: serializeUnionOver marshals a type that
//     reaches a map with the deterministic marshal (never vtproto's MarshalVT,
//     whose map order is Go's random iteration order), then writes each map as
//     Java's load-then-save of the record it replaces writes it (rewriteMaps):
//     that record's entries in their order, a key written twice included, each
//     re-encoded with its key and value, for every key whose value is
//     unchanged; a changed key once, at its first position; new keys after, in
//     key order (mapKeyLess). A new record's maps are in key order. So a record
//     Go loads and saves unchanged has its map entries written as Java writes
//     them back, and its index entries stay what they were; its field order is
//     the deterministic marshal's, a oneof member after the other fields, where
//     Java writes field-number order (DIVERGENCES.md, map entry order).
//
// The wire order is used only while the message still holds what its bytes
// hold: a caller may mutate a loaded message, and one whose map no longer
// matches its bytes is evaluated in key order, as a message Go is about to save.

// recordWire is the stored bytes of a record message decoded from them, kept
// only for a record type that reaches a map field.
type recordWire struct {
	bytes []byte
	reach mapReach

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
	return &recordWire{bytes: bytes, reach: rt.mapReach}
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
		reach := w.reach
		if _, ok := reach[root.ProtoReflect().Descriptor()]; !ok {
			reach = newMapReach(root.ProtoReflect().Descriptor())
		}
		w.err = collectWireMapEntries(root.ProtoReflect(), w.bytes, w.entries, reach)
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

// mapReach answers whether a message type can hold a map field at any depth.
// newMapReach fills it at Build for every message type a meta-data's record
// types reach, and it is read-only from then on, so the walks of every record
// of the meta-data share it: a type it lacks (a descriptor from elsewhere) is
// answered without being stored. It is never a global cache: a meta-data load
// builds fresh descriptors, and a process-wide map keyed by them would keep
// every loaded descriptor graph alive; this one lives as long as the meta-data.
type mapReach map[protoreflect.MessageDescriptor]bool

// newMapReach is the reach of every message type roots reach, message and group
// fields followed: a type reaches a map if it has a map field or a message
// field of a type that does, found as a fixpoint so recursive types terminate.
func newMapReach(roots ...protoreflect.MessageDescriptor) mapReach {
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
	r := make(mapReach, len(types))
	for _, md := range types {
		r[md] = false
	}
	for changed := true; changed; {
		changed = false
		for _, md := range types {
			if r[md] {
				continue
			}
			fields := md.Fields()
			for i := 0; i < fields.Len(); i++ {
				if fd := fields.Get(i); fd.IsMap() || fd.Message() != nil && r[fd.Message()] {
					r[md], changed = true, true
					break
				}
			}
		}
	}
	return r
}

func (r mapReach) reaches(md protoreflect.MessageDescriptor) bool {
	if v, ok := r[md]; ok {
		return v
	}
	return newMapReach(md)[md]
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

// priorBytesError is a failure to parse the bytes of the record a save
// replaces: they do not parse as the record's type, so they have no map order
// to keep, and the decoder refuses them wherever they are read.
type priorBytesError struct{ err error }

func (e *priorBytesError) Error() string { return "prior record bytes: " + e.err.Error() }
func (e *priorBytesError) Unwrap() error { return e.err }

// priorOccurrences is, for each message, group or map field of one message's
// bytes, the bodies of the occurrences a decoder keeps (a map field's are its
// entries), in order.
func priorOccurrences(fields protoreflect.FieldDescriptors, raw []byte) (map[protowire.Number][][]byte, error) {
	kept, err := oneofSurvivors(fields, raw)
	if err != nil {
		return nil, &priorBytesError{err}
	}
	out := map[protowire.Number][][]byte{}
	for offset := 0; offset < len(raw); {
		fd, body, n, err := fieldBody(fields, raw[offset:])
		if err != nil {
			return nil, &priorBytesError{err}
		}
		at := offset
		offset += n
		if fd != nil && body != nil && kept(fd, at) {
			out[fd.Number()] = append(out[fd.Number()], body)
		}
	}
	return out, nil
}

// rewriteMaps is raw, the deterministic marshal of a message of type md (each
// map in key order), with every map it reaches written as Java's load-then-save
// of prior, the bytes of the message it replaces, writes it, nil prior leaving
// raw as it is. Java's default serializer reads a record as a DynamicMessage,
// whose map field is the list of entries in stored order, a key written twice
// included, and writes that list back, each entry re-encoded with its key and
// its value (measured: a stored entry without a value is written back with
// its default). A Go map holds one value per key, so each map is written from
// prior's entries in their order (mergeMapEntries); a singular message field is
// matched with the merge of its kept occurrences in prior (bytes concatenated
// parse as the merge), and a repeated one's elements with prior's by content
// (elementPriors).
func rewriteMaps(md protoreflect.MessageDescriptor, raw, prior []byte, reach mapReach) ([]byte, error) {
	if prior == nil {
		return raw, nil
	}
	fields := md.Fields()
	was, err := priorOccurrences(fields, prior)
	if err != nil {
		return nil, err
	}
	// occurrencesFrom is the bodies of fd's occurrences in raw from offset on.
	occurrencesFrom := func(fd protoreflect.FieldDescriptor, offset int) ([][]byte, error) {
		var bodies [][]byte
		for offset < len(raw) {
			next, body, k, err := fieldBody(fields, raw[offset:])
			if err != nil {
				return nil, err
			}
			if next == fd && body != nil {
				bodies = append(bodies, body)
			}
			offset += k
		}
		return bodies, nil
	}
	out := make([]byte, 0, len(raw))
	written := map[protowire.Number]bool{}
	elements := map[protowire.Number][][]byte{}
	element := map[protowire.Number]int{}
	for offset := 0; offset < len(raw); {
		fd, body, n, err := fieldBody(fields, raw[offset:])
		if err != nil {
			return nil, err
		}
		occurrence := raw[offset : offset+n]
		offset += n
		switch {
		case fd == nil || body == nil:
			out = append(out, occurrence...)
		case fd.IsMap():
			// All of the map's entries are written where its first one was
			// (the deterministic marshal writes them together).
			if written[fd.Number()] {
				continue
			}
			written[fd.Number()] = true
			entries, err := occurrencesFrom(fd, offset-n)
			if err != nil {
				return nil, err
			}
			merged, err := mergeMapEntries(fd, entries, was[fd.Number()], reach)
			if err != nil {
				return nil, err
			}
			out = append(out, merged...)
		case !reach.reaches(fd.Message()):
			out = append(out, occurrence...)
		default:
			var p []byte
			if fd.IsList() {
				if _, ok := elements[fd.Number()]; !ok {
					current, err := occurrencesFrom(fd, offset-n)
					if err != nil {
						return nil, err
					}
					if elements[fd.Number()], err = elementPriors(fd.Message(), current, was[fd.Number()]); err != nil {
						return nil, err
					}
				}
				p = elements[fd.Number()][element[fd.Number()]]
				element[fd.Number()]++
			} else {
				for _, b := range was[fd.Number()] {
					p = append(p, b...)
				}
			}
			rewritten, err := rewriteMaps(fd.Message(), body, p, reach)
			if err != nil {
				return nil, err
			}
			if fd.Kind() == protoreflect.GroupKind {
				out = protowire.AppendTag(out, fd.Number(), protowire.StartGroupType)
				out = append(out, rewritten...)
				out = protowire.AppendTag(out, fd.Number(), protowire.EndGroupType)
			} else {
				out = protowire.AppendBytes(protowire.AppendTag(out, fd.Number(), protowire.BytesType), rewritten)
			}
		}
	}
	return out, nil
}

// elementPriors pairs each element of a repeated message field, current (the
// bodies the deterministic marshal wrote), with the stored element whose maps
// it is written after: elementPriorsWithin at maxLCSCells.
func elementPriors(md protoreflect.MessageDescriptor, current, prior [][]byte) ([][]byte, error) {
	return elementPriorsWithin(md, current, prior, maxLCSCells)
}

// elementPriorsWithin matches the elements by content along a longest common
// subsequence of the two lists when the table has at most maxCells cells (on a
// tie the earlier current element is passed over, so an element keeps the
// stored one at its own position when it can). That pairs an unchanged run of
// elements with its own stored elements, in order, when elements are inserted,
// removed or changed around it, so Java's element keeps its own order, unless
// an inserted element equals a stored one: stored [B] saved as [B', B] with B'
// equal to B pairs the stored B with B', the element at its position, and the
// B that was stored is written as a new element (its maps in key order). The elements the subsequence leaves out (moved past each other,
// a swap) then take the first unmatched stored element with their content, in
// order; past maxCells cells that is the whole matching, and an unchanged
// element can then take an earlier stored element equal to it in content
// rather than its own (stored [A1, B, A2], A1 and A2 equal but for their maps'
// order, saved as [B, A]: A takes A1's order where Java's keeps A2's). A current
// element still unmatched takes the stored element at its position when that
// one is unmatched too (an element changed in place), and otherwise none (its
// maps in key order). A Go message carries no identity across a load and a
// save, so equal elements reordered among themselves are matched in order, and
// a changed element that also moved cannot be told from a new one
// (DIVERGENCES.md).
func elementPriorsWithin(md protoreflect.MessageDescriptor, current, prior [][]byte, maxCells int) ([][]byte, error) {
	canonical := func(body []byte) (string, error) {
		m := dynamicpb.NewMessage(md)
		if err := (proto.UnmarshalOptions{AllowPartial: true}).Unmarshal(body, m); err != nil {
			return "", err
		}
		b, err := proto.MarshalOptions{Deterministic: true, AllowPartial: true}.Marshal(m)
		return string(b), err
	}
	ps := make([]string, len(prior))
	for j, body := range prior {
		c, err := canonical(body)
		if err != nil {
			return nil, &priorBytesError{err}
		}
		ps[j] = c
	}
	cs := make([]string, len(current))
	for i, body := range current {
		c, err := canonical(body)
		if err != nil {
			return nil, err
		}
		cs[i] = c
	}
	n, m := len(cs), len(ps)
	match := make([]int, n) // the stored element each current one takes, or -1
	for i := range match {
		match[i] = -1
	}
	claimed := make([]bool, m)
	if n*m <= maxCells {
		lcs := make([][]int32, n+1)
		for i := range lcs {
			lcs[i] = make([]int32, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				switch {
				case cs[i] == ps[j]:
					lcs[i][j] = lcs[i+1][j+1] + 1
				case lcs[i+1][j] >= lcs[i][j+1]:
					lcs[i][j] = lcs[i+1][j]
				default:
					lcs[i][j] = lcs[i][j+1]
				}
			}
		}
		for i, j := 0, 0; i < n && j < m; {
			switch {
			case cs[i] == ps[j] && lcs[i][j] == lcs[i+1][j+1]+1:
				match[i], claimed[j] = j, true
				i, j = i+1, j+1
			case lcs[i+1][j] >= lcs[i][j+1]:
				i++
			default:
				j++
			}
		}
	}
	// Elements the subsequence left out (moved past each other, as in a swap)
	// are matched by content, in order.
	for i := range cs {
		if match[i] >= 0 {
			continue
		}
		for j := range ps {
			if !claimed[j] && ps[j] == cs[i] {
				match[i], claimed[j] = j, true
				break
			}
		}
	}
	out := make([][]byte, n)
	for i := range current {
		switch {
		case match[i] >= 0:
			out[i] = prior[match[i]]
		case i < m && !claimed[i]:
			claimed[i], out[i] = true, prior[i]
		}
	}
	return out, nil
}

// maxLCSCells bounds the longest-common-subsequence table elementPriors builds
// (the product of the two lists' lengths).
const maxLCSCells = 1 << 20

// mergeMapEntries writes map field fd's entries: current, the entry bodies the
// deterministic marshal wrote (one per key, in key order), over prior, the
// entry bodies the replaced message stored. A key whose value is what prior
// holds for it (its last entry, as a map reads it) keeps every entry prior
// stored for it, each at its position, re-encoded with its own value
// (canonicalEntry): Java's load-then-save of that list. A key whose value
// changed is written once, at its first position, its value's own maps in the
// order of prior's last value for it; a new key follows prior's keys, in key
// order. A key prior held and the map no longer does is gone.
func mergeMapEntries(fd protoreflect.FieldDescriptor, current, prior [][]byte, reach mapReach) ([]byte, error) {
	keyOf := func(entry *dynamicpb.Message) any { return entry.Get(fd.MapKey()).MapKey().Interface() }
	canonical := proto.MarshalOptions{Deterministic: true, AllowPartial: true}
	now := map[any][]byte{}
	var order []any
	for _, body := range current {
		entry, err := parseMapEntry(fd, body)
		if err != nil {
			return nil, err
		}
		now[keyOf(entry)] = body
		order = append(order, keyOf(entry))
	}
	type priorEntry struct {
		key   any
		body  []byte
		entry *dynamicpb.Message
	}
	var was []priorEntry
	last := map[any]int{}
	for _, body := range prior {
		entry, err := parseMapEntry(fd, body)
		if err != nil {
			return nil, &priorBytesError{err}
		}
		last[keyOf(entry)] = len(was)
		was = append(was, priorEntry{key: keyOf(entry), body: body, entry: entry})
	}
	// Unchanged is compared as canonical bytes, so a NaN value equals itself,
	// over the key and the value alone: the fields an entry carries beyond
	// them are not part of the map (protobuf-go drops them when it decodes the
	// map), and Java's load-then-save keeps them with the entry.
	keyAndValue := func(entry *dynamicpb.Message) ([]byte, error) {
		if len(entry.GetUnknown()) > 0 {
			entry = proto.Clone(entry).(*dynamicpb.Message)
			entry.SetUnknown(nil)
		}
		return canonical.Marshal(entry)
	}
	unchanged := map[any]bool{}
	for k, i := range last {
		body, ok := now[k]
		if !ok {
			continue
		}
		entry, err := parseMapEntry(fd, body)
		if err != nil {
			return nil, err
		}
		a, err := keyAndValue(entry)
		if err != nil {
			return nil, err
		}
		b, err := keyAndValue(was[i].entry)
		if err != nil {
			return nil, err
		}
		unchanged[k] = string(a) == string(b)
	}
	var out []byte
	emit := func(entry []byte) {
		out = protowire.AppendBytes(protowire.AppendTag(out, fd.Number(), protowire.BytesType), entry)
	}
	done := map[any]bool{}
	for _, p := range was {
		body, ok := now[p.key]
		switch {
		case !ok:
		case unchanged[p.key]:
			entry, err := canonicalEntry(fd, p.entry, p.body, reach)
			if err != nil {
				return nil, err
			}
			emit(entry)
		case !done[p.key]:
			done[p.key] = true
			entry, err := rewriteMaps(fd.Message(), body, was[last[p.key]].body, reach)
			if err != nil {
				return nil, err
			}
			emit(entry)
		}
	}
	for _, k := range order {
		if _, had := last[k]; !had {
			emit(now[k])
		}
	}
	return out, nil
}

// canonicalEntry is a stored map entry as Java writes it back: its key and its
// value, each written whatever its value (holdKeyAndValue set a missing one's
// default; a proto3 entry's zero key or value too, which a marshal of the entry
// as a message would drop for want of presence, and which protobuf-go's map
// marshal and Java's MapEntry both write), the value's own maps in the order
// body stores them, then the fields the entry carries that its type does not
// declare, as body stores them (Java's entry is a DynamicMessage, which keeps
// its unknown fields and writes them after its known ones).
func canonicalEntry(fd protoreflect.FieldDescriptor, entry *dynamicpb.Message, body []byte, reach mapReach) ([]byte, error) {
	keyFD, valueFD := fd.MapKey(), fd.MapValue()
	out, err := appendEntryField(nil, keyFD, entry.Get(keyFD))
	if err != nil {
		return nil, err
	}
	vmd := valueFD.Message()
	if vmd == nil {
		out, err = appendEntryField(out, valueFD, entry.Get(valueFD))
		if err != nil {
			return nil, err
		}
		return append(out, entry.GetUnknown()...), nil
	}
	value, err := proto.MarshalOptions{Deterministic: true, AllowPartial: true}.Marshal(entry.Get(valueFD).Message().Interface())
	if err != nil {
		return nil, err
	}
	if reach.reaches(vmd) {
		was, err := priorOccurrences(fd.Message().Fields(), body)
		if err != nil {
			return nil, err
		}
		var prior []byte
		for _, b := range was[valueFD.Number()] {
			prior = append(prior, b...)
		}
		if value, err = rewriteMaps(vmd, value, prior, reach); err != nil {
			return nil, err
		}
	}
	out = protowire.AppendBytes(protowire.AppendTag(out, valueFD.Number(), protowire.BytesType), value)
	return append(out, entry.GetUnknown()...), nil
}

// appendEntryField appends a map entry's scalar key or value, v of field fd,
// as its wire encoding, whatever its value.
func appendEntryField(b []byte, fd protoreflect.FieldDescriptor, v protoreflect.Value) ([]byte, error) {
	n := fd.Number()
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), protowire.EncodeBool(v.Bool())), nil
	case protoreflect.EnumKind:
		return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), uint64(int64(v.Enum()))), nil
	case protoreflect.Int32Kind, protoreflect.Int64Kind:
		return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), uint64(v.Int())), nil
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v.Uint()), nil
	case protoreflect.Sint32Kind, protoreflect.Sint64Kind:
		return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), protowire.EncodeZigZag(v.Int())), nil
	case protoreflect.Fixed32Kind:
		return protowire.AppendFixed32(protowire.AppendTag(b, n, protowire.Fixed32Type), uint32(v.Uint())), nil
	case protoreflect.Sfixed32Kind:
		return protowire.AppendFixed32(protowire.AppendTag(b, n, protowire.Fixed32Type), uint32(int32(v.Int()))), nil
	case protoreflect.FloatKind:
		return protowire.AppendFixed32(protowire.AppendTag(b, n, protowire.Fixed32Type), math.Float32bits(float32(v.Float()))), nil
	case protoreflect.Fixed64Kind:
		return protowire.AppendFixed64(protowire.AppendTag(b, n, protowire.Fixed64Type), v.Uint()), nil
	case protoreflect.Sfixed64Kind:
		return protowire.AppendFixed64(protowire.AppendTag(b, n, protowire.Fixed64Type), uint64(v.Int())), nil
	case protoreflect.DoubleKind:
		return protowire.AppendFixed64(protowire.AppendTag(b, n, protowire.Fixed64Type), math.Float64bits(v.Float())), nil
	case protoreflect.StringKind:
		return protowire.AppendString(protowire.AppendTag(b, n, protowire.BytesType), v.String()), nil
	case protoreflect.BytesKind:
		return protowire.AppendBytes(protowire.AppendTag(b, n, protowire.BytesType), v.Bytes()), nil
	}
	return nil, fmt.Errorf("map entry field %s has kind %v", fd.FullName(), fd.Kind())
}

// marshalMapRecord is serializeUnionOver's inner bytes for a record whose type
// reaches a map: the deterministic marshal (map entries in key order), each
// map then written as Java's load-then-save of prior writes it (rewriteMaps),
// prior being the inner bytes of the record it replaces (nil for a new record,
// whose maps stay in key order), reach the record type's (newMapReach). The
// required-field check is the one the record's marshal path makes without a
// map: none for a vtproto message, protobuf-go's for any other.
func marshalMapRecord(record proto.Message, prior []byte, reach mapReach) ([]byte, error) {
	_, vt := record.(interface{ MarshalVT() ([]byte, error) })
	inner, err := proto.MarshalOptions{Deterministic: true, AllowPartial: vt}.Marshal(record)
	if err != nil || prior == nil {
		return inner, err
	}
	md := record.ProtoReflect().Descriptor()
	if _, ok := reach[md]; !ok {
		// A message built from other descriptors than the meta-data's (a
		// generated type under meta-data loaded from proto bytes): one reach
		// for this walk, rather than a closure per lookup.
		reach = newMapReach(md)
	}
	out, err := rewriteMaps(md, inner, prior, reach)
	if pe := (*priorBytesError)(nil); errors.As(err, &pe) {
		// The replaced record's bytes do not parse as this type: nothing to
		// keep, and the decoder refuses them wherever they are read.
		return inner, nil
	}
	return out, err
}

// sortedMapEntries is a map's entries in key order (mapKeyLess), the order Go
// evaluates a message it has not decoded from bytes, and the order
// serializeUnionOver writes a new record's in. Each entry holds the key and the value.
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
