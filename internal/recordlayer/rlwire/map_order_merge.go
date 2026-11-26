package rlwire

import (
	"errors"
	"fmt"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

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

// occurrencesFrom is the bodies of fd's occurrences in raw from offset on.
func occurrencesFrom(fields protoreflect.FieldDescriptors, raw []byte, fd protoreflect.FieldDescriptor, offset int) ([][]byte, error) {
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

// RewriteMaps is raw, the deterministic marshal of a message of type md (each
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
// (ElementPriors).
func RewriteMaps(md protoreflect.MessageDescriptor, raw, prior []byte, reach MapReach, unmarshalPartial PartialUnmarshalFunc) ([]byte, error) {
	if unmarshalPartial == nil {
		return nil, errNilPartialUnmarshal
	}
	if prior == nil {
		return raw, nil
	}
	fields := md.Fields()
	was, err := priorOccurrences(fields, prior)
	if err != nil {
		return nil, err
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
			entries, err := occurrencesFrom(fields, raw, fd, offset-n)
			if err != nil {
				return nil, err
			}
			merged, err := mergeMapEntries(fd, entries, was[fd.Number()], reach, unmarshalPartial)
			if err != nil {
				return nil, err
			}
			out = append(out, merged...)
		case !reach.Reaches(fd.Message()):
			out = append(out, occurrence...)
		default:
			var p []byte
			if fd.IsList() {
				if _, ok := elements[fd.Number()]; !ok {
					current, err := occurrencesFrom(fields, raw, fd, offset-n)
					if err != nil {
						return nil, err
					}
					if elements[fd.Number()], err = ElementPriors(fd.Message(), current, was[fd.Number()], unmarshalPartial); err != nil {
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
			if p == nil {
				// No stored occurrence: the message is new, and its maps are
				// written as a new record's (mergeMapEntries with no prior).
				p = []byte{}
			}
			rewritten, err := RewriteMaps(fd.Message(), body, p, reach, unmarshalPartial)
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

// ElementPriors pairs each element of a repeated message field, current (the
// bodies the deterministic marshal wrote), with the stored element whose maps
// it is written after: ElementPriorsWithin at MaxLCSCells.
func ElementPriors(md protoreflect.MessageDescriptor, current, prior [][]byte, unmarshalPartial PartialUnmarshalFunc) ([][]byte, error) {
	return ElementPriorsWithin(md, current, prior, MaxLCSCells, unmarshalPartial)
}

// ElementPriorsWithin matches the elements by content along a longest common
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
func ElementPriorsWithin(md protoreflect.MessageDescriptor, current, prior [][]byte, maxCells int, unmarshalPartial PartialUnmarshalFunc) ([][]byte, error) {
	if unmarshalPartial == nil {
		return nil, errNilPartialUnmarshal
	}
	canonical := func(body []byte) (string, error) {
		m := dynamicpb.NewMessage(md)
		// Compared as the record's elements read, stored and current alike.
		if err := unmarshalPartial(body, m); err != nil {
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
		lcsMatchElements(cs, ps, match, claimed)
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

func lcsMatchElements(cs, ps []string, match []int, claimed []bool) {
	n, m := len(cs), len(ps)
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

// MaxLCSCells bounds the longest-common-subsequence table ElementPriors builds
// (the product of the two lists' lengths).
const MaxLCSCells = 1 << 20

type priorMapEntry struct {
	key   any
	body  []byte
	entry *dynamicpb.Message
}

// mergeMapEntries writes map field fd's entries: current, the entry bodies the
// deterministic marshal wrote (one per key, in key order), over prior, the
// entry bodies the replaced message stored. A key whose value is what prior
// holds for it (its last entry, as a map reads it) keeps every entry prior
// stored for it, each at its position, re-encoded with its own value
// (canonicalEntry): Java's load-then-save of that list. A key whose value
// changed is written once, at its first position, its value's own maps in the
// order of prior's last value for it; a new key follows prior's keys, in key
// order. A key prior held and the map no longer does is gone.
func mergeMapEntries(fd protoreflect.FieldDescriptor, current, prior [][]byte, reach MapReach, unmarshalPartial PartialUnmarshalFunc) ([]byte, error) {
	keyOf := func(entry *dynamicpb.Message) any { return entry.Get(fd.MapKey()).MapKey().Interface() }
	now := map[any][]byte{}
	nowEntry := map[any]*dynamicpb.Message{}
	var order []any
	for _, body := range current {
		entry, err := parseMapEntry(fd, body, unmarshalPartial)
		if err != nil {
			return nil, err
		}
		if len(entry.GetUnknown()) > 0 {
			// The entry holds what its type does not declare: a map value's
			// undeclared closed-enum number (asJavaForSave's write view).
			// Java's DynamicMessage reads the entry keeping it as the
			// entry's unknown field and writes it back after the value.
			if body, err = canonicalEntry(fd, entry, body, reach, unmarshalPartial); err != nil {
				return nil, err
			}
		}
		now[keyOf(entry)] = body
		nowEntry[keyOf(entry)] = entry
		order = append(order, keyOf(entry))
	}
	var was []priorMapEntry
	last := map[any]int{}
	for _, body := range prior {
		entry, err := parseMapEntry(fd, body, unmarshalPartial)
		if err != nil {
			return nil, &priorBytesError{err}
		}
		last[keyOf(entry)] = len(was)
		was = append(was, priorMapEntry{key: keyOf(entry), body: body, entry: entry})
	}
	unchanged, err := computeUnchangedMapKeys(nowEntry, was, last)
	if err != nil {
		return nil, err
	}
	return emitMergedMapEntries(fd, now, nowEntry, order, was, last, unchanged, reach, unmarshalPartial)
}

func computeUnchangedMapKeys(nowEntry map[any]*dynamicpb.Message, was []priorMapEntry, last map[any]int) (map[any]bool, error) {
	canonical := proto.MarshalOptions{Deterministic: true, AllowPartial: true}
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
		entry, ok := nowEntry[k]
		if !ok {
			continue
		}
		a, err := keyAndValue(entry)
		if err != nil {
			return nil, err
		}
		b, err := keyAndValue(was[i].entry)
		if err != nil {
			return nil, err
		}
		// An entry that holds unknown fields of its own (an undeclared number
		// Go holds) is unchanged only where the stored entry holds the same:
		// otherwise the stored entry's form would drop the number.
		unchanged[k] = string(a) == string(b) &&
			(len(entry.GetUnknown()) == 0 || string(entry.GetUnknown()) == string(was[i].entry.GetUnknown()))
	}
	return unchanged, nil
}

func emitMergedMapEntries(fd protoreflect.FieldDescriptor, now map[any][]byte, nowEntry map[any]*dynamicpb.Message, order []any, was []priorMapEntry, last map[any]int, unchanged map[any]bool, reach MapReach, unmarshalPartial PartialUnmarshalFunc) ([]byte, error) {
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
			entry, err := canonicalEntry(fd, p.entry, p.body, reach, unmarshalPartial)
			if err != nil {
				return nil, err
			}
			emit(entry)
		case !done[p.key]:
			done[p.key] = true
			if len(nowEntry[p.key].GetUnknown()) > 0 {
				emit(body) // canonical above
				continue
			}
			entry, err := RewriteMaps(fd.Message(), body, was[last[p.key]].body, reach, unmarshalPartial)
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
func canonicalEntry(fd protoreflect.FieldDescriptor, entry *dynamicpb.Message, body []byte, reach MapReach, unmarshalPartial PartialUnmarshalFunc) ([]byte, error) {
	keyFD, valueFD := fd.MapKey(), fd.MapValue()
	out, err := AppendEntryField(nil, keyFD, entry.Get(keyFD))
	if err != nil {
		return nil, err
	}
	vmd := valueFD.Message()
	if vmd == nil {
		out, err = AppendEntryField(out, valueFD, entry.Get(valueFD))
		if err != nil {
			return nil, err
		}
		return append(out, entry.GetUnknown()...), nil
	}
	value, err := proto.MarshalOptions{Deterministic: true, AllowPartial: true}.Marshal(entry.Get(valueFD).Message().Interface())
	if err != nil {
		return nil, err
	}
	if reach.Reaches(vmd) {
		was, err := priorOccurrences(fd.Message().Fields(), body)
		if err != nil {
			return nil, err
		}
		var prior []byte
		for _, b := range was[valueFD.Number()] {
			prior = append(prior, b...)
		}
		if value, err = RewriteMaps(vmd, value, prior, reach, unmarshalPartial); err != nil {
			return nil, err
		}
	}
	out = protowire.AppendBytes(protowire.AppendTag(out, valueFD.Number(), protowire.BytesType), value)
	return append(out, entry.GetUnknown()...), nil
}

// AppendEntryField appends a map entry's scalar key or value, v of field fd,
// as its wire encoding, whatever its value.
func AppendEntryField(b []byte, fd protoreflect.FieldDescriptor, v protoreflect.Value) ([]byte, error) {
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

// MarshalMapRecord is serializeUnionOver's inner bytes for a record whose type
// reaches a map: the deterministic marshal (map entries in key order), each
// map then written as Java's load-then-save of prior writes it (RewriteMaps),
// prior being the inner bytes of the record it replaces (nil for a new record,
// whose maps stay in key order), reach the record type's (NewMapReach). The
// required-field check is the one the record's marshal path makes without a
// map: none for a vtproto message, protobuf-go's for any other.
func MarshalMapRecord(record proto.Message, prior []byte, reach MapReach, unmarshalPartial PartialUnmarshalFunc) ([]byte, error) {
	if unmarshalPartial == nil {
		return nil, errNilPartialUnmarshal
	}
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
		reach = NewMapReach(md)
	}
	out, err := RewriteMaps(md, inner, prior, reach, unmarshalPartial)
	if pe := (*priorBytesError)(nil); errors.As(err, &pe) {
		// The replaced record's bytes do not parse as this type: nothing to
		// keep, and the decoder refuses them wherever they are read.
		return inner, nil
	}
	return out, err
}
