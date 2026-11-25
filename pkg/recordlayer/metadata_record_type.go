// Portions derived from FoundationDB Record Layer (RecordMetaDataBuilder.java,
// RecordMetaData.java, RecordType.java, KeyExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"math"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// RecordTypeBuilder provides methods to configure a specific record type
type RecordTypeBuilder struct {
	recordType *RecordType
	builder    *RecordMetaDataBuilder
}

// SetPrimaryKey sets the primary key expression, refusing declared version columns as Java does.
func (rtb *RecordTypeBuilder) SetPrimaryKey(keyExpr KeyExpression) *RecordTypeBuilder {
	rtb.builder.handedKeys = append(rtb.builder.handedKeys, keyExpr)
	if countVersionColumns(keyExpr) != 0 {
		rtb.builder.recordBuildError(&MetaDataError{Message: "Version in primary key not supported"})
		return rtb
	}
	rtb.recordType.PrimaryKey = keyExpr
	return rtb
}

// SetRecordTypeKey overrides the auto-derived record type key for this record type.
// By default, the record type index (proto field number order) is used.
// Matches Java's RecordTypeBuilder.setRecordTypeKey(Key.Evaluated), which
// delegates to RecordTypeIndexesBuilder.setRecordTypeKey: reject anything that
// is not a primitive, then store TupleTypeUtil.toTupleEquivalentValue(key).
//
// Canonicalizing HERE, not at every read, is the whole point. The key is read
// by duplicate-key validation, by proto serialization, by DeleteRecordsWhere's
// prefix comparison and by the tuple packer, and each of those asks a
// different question of it — equality, encodability, wire bytes. A value
// normalized only on the evaluation path leaves the others looking at a
// different representation of the same key, which is how int64(7) and uint(7)
// came to be two "distinct" keys that encode to identical bytes.
//
// A rejected key is reported from Build() rather than panicking: Java throws
// MetaDataException from the setter, but a Go builder setter returns the
// builder for chaining and library code must not panic, so the error is
// deferred to the one call that already returns one.
func (rtb *RecordTypeBuilder) SetRecordTypeKey(key any) *RecordTypeBuilder {
	canonical, err := canonicalRecordTypeKey(key)
	if err != nil {
		// Wrapped with %w, not flattened into a message: the caller has to be
		// able to match the cause with errors.As, which is how Go says
		// `catch (MetaDataException e)`.
		rtb.builder.recordBuildError(fmt.Errorf("record type %q: %w", rtb.recordType.Name, err))
		return rtb
	}
	rtb.recordType.explicitRecordTypeKey = canonical
	return rtb
}

// RecordTypeKeyTypeError reports a record type key whose Go type cannot be a
// record type key. Java's equivalent is the MetaDataException "Only primitive
// types are allowed as record type key" thrown by
// RecordTypeIndexesBuilder.setRecordTypeKey.
type RecordTypeKeyTypeError struct {
	// Key is the offending value.
	Key any
	// Reason distinguishes the two ways a key is refused. Empty means the Go
	// TYPE is not one a record type key may have, which is Java's wording
	// verbatim. A non-empty reason means the type is fine but this VALUE
	// cannot be represented, where Java's message would be actively
	// misleading — a uint64 above max int64 is a primitive, and saying
	// otherwise sends the reader looking for the wrong mistake.
	Reason string
}

func (e *RecordTypeKeyTypeError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("record type key %v (%T) cannot be used: %s", e.Key, e.Key, e.Reason)
	}
	return fmt.Sprintf("only primitive types are allowed as record type key, got %T (%v)", e.Key, e.Key)
}

func (*RecordTypeKeyTypeError) JavaRecordCoreException() {}

// canonicalRecordTypeKey validates and canonicalizes a record type key, the
// port of Java's setRecordTypeKey guard followed by
// TupleTypeUtil.toTupleEquivalentValue.
//
// The accepted set is Java's — null, Number, Boolean, String, byte[] —
// intersected with what the tuple encoder can encode AND what the metadata
// proto can carry. Everything else is refused HERE, where the caller learns
// about it from Build(), rather than at pack time: the encoder's default arm
// panics ("unencodable element"), so a value the builder accepts but the
// encoder cannot write turns a metadata mistake into a panic on the save path.
// A named integer type (`type k int`) is refused for that exact reason — it is
// not one of the encoder's cases.
//
// The proto clause is the second half of that rule and is what excludes two
// values the ENCODER would take. RecordKeyExpressionProto.Value has exactly
// one integer field per width (double, float, int64, bool, string, bytes,
// int32) and no unsigned or big-integer field, so:
//
//   - a big.Int is refused even though the encoder writes it, because no
//     metadata carrying one could ever be exported or read back; and
//   - a uint/uint64 above math.MaxInt64 is refused for the same reason. It
//     packs, but ToProto cannot write it ("unsupported value type uint64"),
//     which is the accepted-here / broken-there split this whole function exists to remove. Java cannot
//     express it either: its only unsigned-capable Number is BigInteger, and
//     LiteralKeyExpression.toProtoValue funnels every non-Integer Number
//     through longValue(), silently TRUNCATING it to a wrong key.
//
// Refusing at the door is what keeps validation, serialization, prefix
// comparison and packing looking at one representation.
//
// Canonicalization is limited to what leaves the tuple encoding IDENTICAL:
// every integer that fits in an int64 becomes an int64, because the tuple
// encoding of an integer does not depend on the signedness or width of the Go
// type it arrived in. uint/uint64 above math.MaxInt64 stay unsigned — the
// encoder writes them natively and no int64 can represent them. float32 and
// float64 are NOT folded together: they have different tuple type codes.
func canonicalRecordTypeKey(key any) (any, error) {
	switch k := key.(type) {
	case nil:
		return nil, nil
	case int:
		return int64(k), nil
	case int8:
		return int64(k), nil
	case int16:
		return int64(k), nil
	case int32:
		return int64(k), nil
	case int64:
		return k, nil
	case uint8:
		return int64(k), nil
	case uint16:
		return int64(k), nil
	case uint32:
		return int64(k), nil
	case uint:
		if uint64(k) > math.MaxInt64 {
			return nil, &RecordTypeKeyTypeError{Key: key, Reason: "above max int64, and the metadata proto has no unsigned field to carry it"}
		}
		return int64(k), nil
	case uint64:
		if k > math.MaxInt64 {
			return nil, &RecordTypeKeyTypeError{Key: key, Reason: "above max int64, and the metadata proto has no unsigned field to carry it"}
		}
		return int64(k), nil
	case string:
		return k, nil
	case []byte:
		// A nil slice is Java's null byte[] reference, which its setter's
		// `recordTypeKey == null` arm accepts as "no explicit key" — so it
		// must land as an untyped nil here. Returning a typed-nil []byte
		// instead made HasExplicitRecordTypeKey report true for a key that
		// proto serialization then dropped, so the type silently fell back to
		// its union field number on reload.
		if k == nil {
			return nil, nil
		}
		// Copied, as Java's ByteString.copyFrom copies: the key must not
		// change under the metadata if the caller reuses its slice.
		//
		// make+copy, NOT append([]byte(nil), k...): append returns a NIL
		// slice for an empty input, and nil is how this field spells "absent".
		// An empty bytes key is a real key — Java's ByteString.EMPTY is
		// non-null and its reader tests hasBytesValue(), field PRESENCE, not
		// emptiness — and the proto layer here preserves that presence too (a
		// non-nil empty slice marshals to the tag with length 0). Nilling it
		// in the copy is what made the key vanish across ToProto/FromProto,
		// leaving every record written under the empty-bytes prefix
		// unreachable.
		out := make([]byte, len(k))
		copy(out, k)
		return out, nil
	case bool:
		return k, nil
	case float32:
		return k, nil
	case float64:
		return k, nil
	default:
		return nil, &RecordTypeKeyTypeError{Key: key}
	}
}

// recordTypeKeyDedupKey renders a record type key as the bytes it will occupy
// in a key, which is the only thing that decides whether two record types
// collide. Comparing the values themselves cannot answer that: int64(7) and
// uint(7) are different values that encode identically (a real collision),
// while "abc" and []byte("abc") are equal-looking and encode differently
// (tuple type codes 0x02 and 0x01, not a collision), and []byte is not even
// comparable in Go.
// recordTypeKeyIdentity renders a record type key as the bytes it occupies in
// a key. It is the ONE identity every comparison of record type keys goes
// through — duplicate detection at Build, lookup by key, and the evolution
// validator's old-vs-new matching — because those questions are the same
// question and answering them with different functions is how they came to
// disagree.
//
// Two keys are the same key exactly when they encode to the same bytes. That
// is what Java compares, by a different route: it stores a byte[] key as a
// ByteString and asks .equals(), so a ByteString never equals a String and
// int64(7) never coexists with a colliding spelling. Folding bytes into a
// string to make them comparable — which the general-purpose subspace-key
// normalizer does — merges "k" and []byte("k"), two keys with DIFFERENT tuple
// type codes that live in different key spaces. Build admits both, so any
// comparison that folds them lets a lookup return either type depending on map
// iteration order.
//
// Reports false for a value that cannot be a record type key, so callers can
// answer "no match" instead of packing something the encoder would panic on.
// Every stored key is already canonical (both doors into the field
// canonicalize), so false here means the CALLER supplied a non-key value.
func recordTypeKeyIdentity(key any) (string, bool) {
	canonical, err := canonicalRecordTypeKey(key)
	if err != nil {
		return "", false
	}
	return string(tuple.Tuple{canonical}.Pack()), true
}

// GetRecordType returns the record type for the given name.
//
// Record types are keyed by their STORAGE name — the ProtoUtils-escaped
// descriptor name, which is what the wire carries. A caller holding a USER
// identifier (SQL text: a table declared `"foo$table"` is stored as
// FOO__1TABLE) resolves through the escape on a miss.
//
// Java does not need this because its relational layer keeps its OWN table
// map keyed by the user name (RecordLayerSchemaTemplate.getTable) and never
// addresses RecordMetaData with a user identifier; Go's relational layer
// uses RecordMetaData directly as its table catalog, so the translation
// lives here — ONE boundary that every SQL path already funnels through —
// rather than being sprinkled over the 25 call sites.
//
// The fallback fires only when the direct key misses, so it never SHADOWS a
// stored type and ToProtoBufCompliantName is deterministic. It is not,
// however, unambiguous: the escaping is not injective across the two
// namespaces, so a schema declaring both MY__1TABLE (the storage form of SQL
// MY$TABLE) and MY__01TABLE (the storage form of SQL MY__1TABLE) resolves a
// lookup for the SQL name MY__1TABLE to the FIRST of those — the wrong entry.
// No ordering fixes it; either order prices one of the pair wrong. See
// AmbiguousDeclaredNames, which is how a caller computing over all record
// types detects that case, and TestGetRecordTypeMisResolvesAnAmbiguousPair,
// which pins the behaviour so a reordering "fix" fails loudly.
func (m *RecordMetaData) GetRecordType(name string) *RecordType {
	if rt, ok := m.recordTypes[name]; ok {
		return rt
	}
	if storage, err := ToProtoBufCompliantName(name); err == nil && storage != name {
		if rt, ok := m.recordTypes[storage]; ok {
			return rt
		}
	}
	// Metadata loaded from Java (a stored template, a metadata file) may name
	// a type with an escape the encoder would not produce for its user name
	// (`___T6__2__UNESCAPED` is the user name `___T6.__UNESCAPED`, whose own
	// encoding differs). Java keys a template's tables by the decoded name
	// (RecordLayerSchemaTemplate.fromRecordMetadata), so a last lookup
	// compares decoded names; it runs only when both keys above miss.
	var found *RecordType
	for stored, rt := range m.recordTypes {
		if stored != name && ToUserIdentifier(stored) == name {
			if found != nil {
				return nil
			}
			found = rt
		}
	}
	return found
}

// GetRecordTypeForDescriptor is Java's RecordMetaData.getRecordTypeForDescriptor
// (RecordMetaData.java:219-223): the record type of descriptor's name, which
// must hold that very descriptor; a same-named message of another descriptor
// (a compiled type over loaded meta-data, without WithLocalFileDescriptor) is not it.
func (m *RecordMetaData) GetRecordTypeForDescriptor(descriptor protoreflect.MessageDescriptor) (*RecordType, error) {
	name := string(descriptor.Name())
	rt := m.GetRecordType(name)
	if rt == nil {
		return nil, unknownRecordTypeError(name)
	}
	if rt.Descriptor != descriptor {
		return nil, &MetaDataError{Message: "descriptor did not match record type"}
	}
	return rt, nil
}

// GetIndexes returns the indexes defined for this record type (single-type only).
// Does not include multi-type or universal indexes.
// Matches Java's RecordType.getIndexes().
func (rt *RecordType) GetIndexes() []*Index {
	return rt.indexes
}

// GetMultiTypeIndexes returns the multi-type indexes for this record type.
// Matches Java's RecordType.getMultiTypeIndexes().
func (rt *RecordType) GetMultiTypeIndexes() []*Index {
	return rt.multiTypeIndexes
}

// GetAllIndexes returns all indexes for this record type (single-type + multi-type).
// Does not include universal indexes.
// Matches Java's RecordType.getAllIndexes().
func (rt *RecordType) GetAllIndexes() []*Index {
	if len(rt.multiTypeIndexes) == 0 {
		return rt.indexes
	}
	all := make([]*Index, 0, len(rt.indexes)+len(rt.multiTypeIndexes))
	all = append(all, rt.indexes...)
	all = append(all, rt.multiTypeIndexes...)
	return all
}

// HasExplicitRecordTypeKey returns true if the record type key was explicitly set.
// Matches Java's RecordType.hasExplicitRecordTypeKey().
func (rt *RecordType) HasExplicitRecordTypeKey() bool {
	return rt.explicitRecordTypeKey != nil
}

// GetRecordTypeKey returns the explicit record type key if set, or falls back
// to the record type index. Matches Java's RecordType.getRecordTypeKey().
//
// The stored value is ALREADY canonical — SetRecordTypeKey and the proto
// reader canonicalize on the way in, as Java's setter does — so this returns
// it verbatim. Normalizing on the way out instead would leave every other
// consumer of the field (duplicate validation, proto export, prefix
// comparison) looking at the raw value while only the evaluation path saw the
// canonical one, which is the split that let equal-encoding keys pass
// validation as distinct.
//
// The derived arm converts the record type index because RecordTypeIndex is a
// declared int field, not a caller-supplied value: the conversion is exact and
// total, and it gives int and int64 spellings of a key one representation.
func (rt *RecordType) GetRecordTypeKey() any {
	if rt.explicitRecordTypeKey != nil {
		return rt.explicitRecordTypeKey
	}
	return int64(rt.RecordTypeIndex)
}

// PrimaryKeyHasRecordTypePrefix returns true if this record type's primary key
// starts with a RecordTypeKeyExpression — i.e. its records live in a contiguous
// record-type-keyed sub-range of the records space.
// Matches Java's RecordType.primaryKeyHasRecordTypePrefix().
func (rt *RecordType) PrimaryKeyHasRecordTypePrefix() bool {
	return primaryKeyStartsWithRecordType(rt.PrimaryKey)
}

// IsSynthetic reports whether this is a synthetic record type (one assembled from
// other records, e.g. a joined type). The Go port does not model synthetic record
// types — they are out of scope (see CLAUDE.md) — so this is always false. Kept as
// a method for 1:1 fidelity with Java's RecordType.isSynthetic() so callers (e.g.
// the typed-records range preset) read identically to the Java algorithm.
//
// The constant false is SOUND ONLY BECAUSE no synthetic type can reach a
// *RecordType. Record types are built from the union descriptor's fields, and a
// joined or unnested type is declared in the metadata proto rather than in the
// union — so it never becomes one of these. The dangerous reading is that
// synthetic types are absent from the METADATA, which is not what this says and
// is not true once fields 12/13 are carried. Callers that need the metadata-level
// question must ask RecordMetaData.DeclaresSyntheticRecordTypes.
func (rt *RecordType) IsSynthetic() bool {
	return false
}

// GetRecordTypeFromRecordTypeKey returns the record type with the given type key.
// Returns nil if no record type matches.
// Matches Java's RecordMetaData.getRecordTypeFromRecordTypeKey(), which scans
// the record types comparing getRecordTypeKey().equals(recordTypeKey).
//
// Comparison goes through recordTypeKeyIdentity — the same identity the
// duplicate check uses — so a lookup can never answer a question Build did not
// already settle. Comparing through the general subspace-key normalizer folded
// a []byte key into the string of the same bytes, and Build admits that pair
// as two distinct types, so the lookup returned whichever of them the map
// happened to yield first.
func (m *RecordMetaData) GetRecordTypeFromRecordTypeKey(key any) *RecordType {
	wanted, ok := recordTypeKeyIdentity(key)
	if !ok {
		return nil
	}
	for _, rt := range m.recordTypes {
		if id, idOK := recordTypeKeyIdentity(rt.GetRecordTypeKey()); idOK && id == wanted {
			return rt
		}
	}
	return nil
}

// CommonPrimaryKey returns the primary key expression if all record types share
// the same one, or nil if they differ. Uses structural comparison via
// keyExpressionEquals. Matches Java's RecordMetaData.commonPrimaryKey().
func (m *RecordMetaData) CommonPrimaryKey() KeyExpression {
	var common KeyExpression
	first := true
	for _, rt := range m.recordTypes {
		if first {
			common = rt.PrimaryKey
			first = false
		} else if !keyExpressionEquals(common, rt.PrimaryKey) {
			return nil
		}
	}
	return common
}

// CommonPrimaryKeyLength returns the number of columns in the primary key if
// all record types have the same PK length, or -1 if they differ.
// Matches Java's RecordMetaData.commonPrimaryKeyLength().
func (m *RecordMetaData) CommonPrimaryKeyLength() int {
	common := -1
	first := true
	for _, rt := range m.recordTypes {
		size := rt.PrimaryKey.ColumnSize()
		if first {
			common = size
			first = false
		} else if common != size {
			return -1
		}
	}
	return common
}

// PrimaryKeyHasRecordTypePrefix returns true if all record types have a
// primary key that starts with a RecordTypeKeyExpression.
// Matches Java's RecordMetaData.primaryKeyHasRecordTypePrefix().
func (m *RecordMetaData) PrimaryKeyHasRecordTypePrefix() bool {
	for _, rt := range m.recordTypes {
		if !primaryKeyStartsWithRecordType(rt.PrimaryKey) {
			return false
		}
	}
	return true
}

// primaryKeyStartsWithRecordType checks if a key expression starts with RecordTypeKeyExpression.
func primaryKeyStartsWithRecordType(expr KeyExpression) bool {
	if expr == nil {
		return false
	}
	if _, ok := expr.(*RecordTypeKeyExpression); ok {
		return true
	}
	if comp, ok := expr.(*CompositeKeyExpression); ok && len(comp.expressions) > 0 {
		_, ok := comp.expressions[0].(*RecordTypeKeyExpression)
		return ok
	}
	return false
}
