package recordlayer

import (
	"fmt"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
)

// IndexCarryClass is how a rebuilt index definition relates to the stored index
// of the same name (RFC-257 WS-J section 4): what a new template version carries
// for it, and what a template restore admits between two stored versions.
type IndexCarryClass int

const (
	// IndexEquivalent: every compared field is equal, so the stored Index
	// message is carried as it was stored.
	IndexEquivalent IndexCarryClass = iota
	// IndexWidened: every compared field is equal except the root, and the
	// roots differ only in literal carriers that store the same tuple bytes
	// (literalCarriersEquivalent, the one-way arm the evolution validator runs
	// under SetAllowLiteralCarrierWidening). The entries are the same bytes.
	IndexWidened
	// IndexChanged: any other difference; the index is rebuilt.
	IndexChanged
)

func (c IndexCarryClass) String() string {
	switch c {
	case IndexEquivalent:
		return "EQUIVALENT"
	case IndexWidened:
		return "WIDENED"
	case IndexChanged:
		return "CHANGED"
	}
	return fmt.Sprintf("IndexCarryClass(%d)", int(c))
}

// ClassifyIndexCarry compares a stored Index message with a rebuilt one of the
// same name and returns its class and, for a class other than EQUIVALENT, the
// name of the Index field that decides it: root_expression for WIDENED, and for
// CHANGED the first field (in field-number order) whose difference is not a
// literal-carrier widening.
//
// Each side is read as Java's Index(proto) reads it (Index.java:194-235, Go
// indexFromProto): the deprecated index_type becomes the type and options it
// names, and the stored option list beside it is not read; an absent type is
// VALUE; the deprecated value_expression is folded into the root; a RANK, COUNT,
// MAX_EVER, MIN_EVER or SUM root that is not a grouping is compared as the
// grouping Java wraps it in; an absent root is refused. A side that does not
// load returns its error. The comparison then walks the Index descriptor, so a
// field a later proto sync adds is compared (as proto equality of its stored
// value) without a code change:
//   - record_type as a set;
//   - name, and a field this function does not name, by proto equality;
//   - index_type and value_expression through the fields they fold into;
//   - root_expression as the root Java reads (the fold and the grouping wrap
//     above), and on a difference, under the one-way literal-carrier arm;
//   - type as Java reads it, options as a map (order-insensitive: an index
//     stored before the option-order fix differs only in order, which no reader
//     sees);
//   - predicate by proto equality, both absent included (a changed WHERE changes
//     which records the index holds, and no validator compares predicates);
//   - added_version, last_modified_version and subspace_key not at all: the carry
//     assigns them.
//
// Unknown fields and extensions are not compared: neither engine reads them.
func ClassifyIndexCarry(stored, rebuilt *gen.Index) (IndexCarryClass, string, error) {
	storedIdx, err := indexFromProto(stored)
	if err != nil {
		return IndexChanged, "", fmt.Errorf("stored index %s: %w", stored.GetName(), err)
	}
	rebuiltIdx, err := indexFromProto(rebuilt)
	if err != nil {
		return IndexChanged, "", fmt.Errorf("rebuilt index %s: %w", rebuilt.GetName(), err)
	}
	storedRoot := storedIdx.RootExpression.ToKeyExpression()
	rebuiltRoot := rebuiltIdx.RootExpression.ToKeyExpression()

	class := IndexEquivalent
	firstDiff := ""
	sm, rm := stored.ProtoReflect(), rebuilt.ProtoReflect()
	fields := sm.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		var equal bool
		switch fd.Name() {
		case "added_version", "last_modified_version", "subspace_key":
			continue
		case "index_type", "value_expression":
			// Read through type, options and root.
			continue
		case "record_type":
			equal = sameStringSet(stored.GetRecordType(), rebuilt.GetRecordType())
		case "type":
			equal = storedIdx.Type == rebuiltIdx.Type
		case "options":
			equal = maps.Equal(storedIdx.Options, rebuiltIdx.Options)
		case "root_expression":
			if proto.Equal(storedRoot, rebuiltRoot) {
				continue
			}
			if literalCarriersEquivalent(storedRoot.ProtoReflect(), rebuiltRoot.ProtoReflect(), false) {
				if class == IndexEquivalent {
					class, firstDiff = IndexWidened, string(fd.Name())
				}
				continue
			}
			equal = false
		default:
			// name, predicate, and any field added after this was written.
			equal = sameFieldValue(sm, rm, fd)
		}
		if !equal {
			if class != IndexChanged {
				class, firstDiff = IndexChanged, string(fd.Name())
			}
		}
	}
	return class, firstDiff, nil
}

// sameStringSet reports whether a and b hold the same strings, ignoring order
// and repetition.
func sameStringSet(a, b []string) bool {
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(slices.Compact(as), slices.Compact(bs))
}

// sameFieldValue reports whether field fd is equally present in a and b and, when
// present, proto-equal.
func sameFieldValue(a, b protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
	if a.Has(fd) != b.Has(fd) {
		return false
	}
	if !a.Has(fd) {
		return true
	}
	return a.Get(fd).Equal(b.Get(fd))
}
