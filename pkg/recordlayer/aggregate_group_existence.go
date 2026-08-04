package recordlayer

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Group existence for aggregate indexes (RFC-209).
//
// An aggregate index's key set is not the set of groups. It over-approximates
// (an atomic ADD decrements an accumulator to zero and leaves the key behind,
// so a group emptied by DELETE or by an UPDATE that moved its last row away
// keeps an entry) and, for SUM and COUNT(col), it under-approximates (no entry
// is written for a NULL value, so a group whose every value is NULL has no key
// at all).
//
// The repair is a group-existence source: a COUNT(*) index over the SAME
// grouping key, whose non-zero keys are exactly the live groups. This file
// holds the two metadata-level facts the read side derives that source from —
// which grouping key an index groups by, and what a companion is named.
//
// Nothing here writes anything to FDB.

// GroupCountCompanionSuffix is reserved on user-supplied index names. The DDL
// derives a companion COUNT(*) index's name by appending it to the owning
// index's name, so the name is deterministic: two stores built from the same
// DDL must contain the same stored bytes, which a randomized or counter-based
// name would break.
//
// Reserving it narrows the DDL surface relative to Java, which accepts any
// name. That is deliberate and is the lesser of two evils: without reservation,
// create-if-absent could collide with a pre-existing user index that carries
// the derived name but a different structure, and the user would see a
// duplicate-name error at schema-creation time rather than at the point they
// chose the name.
//
// The reservation applies at CREATE ONLY, never at load. A schema that already
// contains an index whose name ends with the suffix — written by Java, or by an
// older Go binary — must open and read normally. Companion matching is
// structural and the name carries no semantic weight, so such a name is inert
// at read time and there is nothing to reject; rejecting at load would let this
// design render an existing Java-written schema unreadable.
const GroupCountCompanionSuffix = "__GROUP_COUNT"

// GroupCountCompanionName derives the companion COUNT(*) index's name from the
// name of the aggregate index it serves.
func GroupCountCompanionName(ownerIndexName string) string {
	return ownerIndexName + GroupCountCompanionSuffix
}

// GroupingKeyExpressionOf returns the GROUPING half of a grouping key
// expression: the leading columns the index groups by, with the trailing
// aggregated columns removed. For a COUNT(*) index (no aggregated columns) that
// is the whole key.
//
// Returns false when the split cannot be made exactly — no grouping columns at
// all (an ungrouped aggregate), or a child expression that straddles the
// grouping/grouped boundary. Declining is the only safe answer: a companion
// matched on a mis-split key would drive the merge from the wrong group set.
func GroupingKeyExpressionOf(g *GroupingKeyExpression) (KeyExpression, bool) {
	if g == nil {
		return nil, false
	}
	groupingCount := g.GetGroupingCount()
	if groupingCount <= 0 {
		return nil, false
	}
	if g.GetGroupedCount() == 0 {
		return g.wholeKey, true
	}
	composite, ok := g.wholeKey.(*CompositeKeyExpression)
	if !ok {
		// A non-composite whole key producing more columns than it groups by
		// cannot be split structurally.
		return nil, false
	}
	var grouping []KeyExpression
	columns := 0
	for _, child := range composite.SubKeyExpressions() {
		if columns >= groupingCount {
			break
		}
		size := child.ColumnSize()
		if columns+size > groupingCount {
			return nil, false // child straddles the boundary
		}
		grouping = append(grouping, child)
		columns += size
	}
	if columns != groupingCount || len(grouping) == 0 {
		return nil, false
	}
	if len(grouping) == 1 {
		return grouping[0], true
	}
	return Concat(grouping...), true
}

// GroupingSignature returns a normalized, deterministic encoding of an index's
// grouping key expression. Two aggregate indexes group identically iff their
// signatures are byte-equal, which is what makes companion discovery structural
// (RFC-209 §5.2) rather than a stored reference.
//
// The encoding is the grouping key expression's proto with proto2 defaults
// cleared, marshalled deterministically — the same normalization the wire
// conformance test applies before comparing Go's index metadata against Java's.
// Clearing defaults matters because Java materializes fields Go leaves unset,
// so a Java-written index and a Go-written one can describe the same grouping
// with different bytes.
//
// Returns nil when no signature can be derived; callers must treat nil as "no
// companion match", never as "matches everything".
func GroupingSignature(g *GroupingKeyExpression) []byte {
	groupingKey, ok := GroupingKeyExpressionOf(g)
	if !ok || groupingKey == nil {
		return nil
	}
	keyProto := groupingKey.ToKeyExpression()
	if keyProto == nil {
		return nil
	}
	normalized := proto.Clone(keyProto)
	clearProto2Defaults(normalized.ProtoReflect())
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(normalized)
	if err != nil {
		return nil
	}
	// A zero-length encoding is a real possibility for an all-defaults message,
	// and it must not be confused with "no signature". Prefix a marker byte so
	// every derived signature is non-empty.
	return append([]byte{'g'}, encoded...)
}

// IsGroupCountCompanionFor reports whether idx is a COUNT(*) index that can act
// as the group-existence companion of an index with the given grouping
// signature over the given record types.
//
// Structural only: the name is never consulted, so a companion created by an
// older binary under a different name, or declared by the user, matches just as
// well as one this binary would emit.
func IsGroupCountCompanionFor(idx *Index, groupingSignature []byte, recordTypeNames []string, md *RecordMetaData) bool {
	if idx == nil || len(groupingSignature) == 0 || md == nil {
		return false
	}
	if idx.Type != IndexTypeCount {
		return false
	}
	gke, ok := idx.RootExpression.(*GroupingKeyExpression)
	if !ok {
		return false
	}
	candidateSignature := GroupingSignature(gke)
	if len(candidateSignature) == 0 || string(candidateSignature) != string(groupingSignature) {
		return false
	}
	return sameRecordTypeSet(md.RecordTypesForIndex(idx), recordTypeNames)
}

// sameRecordTypeSet reports whether the record types an index covers are
// exactly the named set. A companion over a different (or wider) set of record
// types counts different rows, so equality — not overlap — is required.
func sameRecordTypeSet(types []*RecordType, names []string) bool {
	if len(types) != len(names) {
		return false
	}
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	for _, t := range types {
		if t == nil {
			return false
		}
		if _, ok := want[t.Name]; !ok {
			return false
		}
	}
	return true
}

// clearProto2Defaults recursively clears optional proto2 fields that hold their
// declared default. Java's serializer materializes such fields; Go leaves them
// unset. Comparing without this would report two identical groupings as
// different purely by authorship.
func clearProto2Defaults(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				clearProto2Defaults(list.Get(i).Message())
			}
		case fd.IsMap() && fd.MapValue().Kind() == protoreflect.MessageKind:
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				clearProto2Defaults(mv.Message())
				return true
			})
		case fd.IsList(), fd.IsMap():
			// non-message collections carry no defaults to clear
		case fd.Kind() == protoreflect.MessageKind:
			clearProto2Defaults(v.Message())
		case fd.HasPresence() && fd.Cardinality() != protoreflect.Required:
			// Required fields must stay set even at their default.
			def := fd.Default()
			switch fd.Kind() {
			case protoreflect.EnumKind:
				if v.Enum() == def.Enum() {
					m.Clear(fd)
				}
			case protoreflect.BytesKind:
				if string(v.Bytes()) == string(def.Bytes()) {
					m.Clear(fd)
				}
			default:
				if v.Interface() == def.Interface() {
					m.Clear(fd)
				}
			}
		}
		return true
	})
}
