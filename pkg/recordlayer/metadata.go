// Portions derived from FoundationDB Record Layer (RecordMetaDataBuilder.java,
// RecordMetaData.java, RecordType.java, KeyExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"sort"

	"fdb.dev/internal/recordlayer/protoname"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// RecordMetaData describes the schema for records stored in a record store.
// This is a simplified version for our MVP - just enough to define record types
// and their primary keys.
type RecordMetaData struct {
	// Map of record type names to their definitions
	recordTypes map[string]*RecordType

	// Whether two declared record types collide across the SQL and storage
	// namespaces, DERIVED AT CONSTRUCTION by Build alongside
	// fieldNumberToRecordType, not memoised on first ask.
	//
	// A lazy memo would answer whatever the declared set happened to be the
	// first time anyone asked, so its staleness under mutation would depend on
	// call ORDER -- mutate-then-ask and ask-then-mutate-then-ask giving
	// different answers for the same object. Deriving it here makes the value
	// unambiguously "the declared set Build saw", which is the same contract
	// every other derived field on this struct already has.
	//
	// It is computed rather than recomputed per call because the CLI asks once
	// per rendered name: a record scan is O(records x types) and a type listing
	// O(types^2), re-deriving one answer for every row it prints. O(types) once.
	ambiguousNames []string
	ambiguousFound bool

	// The protobuf file descriptor
	fileDescriptor protoreflect.FileDescriptor

	// recordsSourceProto, when non-nil, is the ORIGINAL FileDescriptorProto
	// fileDescriptor was built from; ToProto() emits it verbatim (Java:
	// FileDescriptor.toProto() returns the source proto unchanged). Nil for
	// metadata built from compiled Go protos, where the
	// protodesc.ToFileDescriptorProto reconstruction is the only source.
	recordsSourceProto *descriptorpb.FileDescriptorProto

	// Imported source protos retain presence and unknown fields lost by reflection.
	dependencySourceProtos map[string]*descriptorpb.FileDescriptorProto

	// Schema version
	version int

	// RecordCountKey is the key expression used for maintaining record counts.
	// If nil, record counting is disabled (matching Java's behavior).
	// Java equivalent: RecordMetaData.getRecordCountKey()
	recordCountKey KeyExpression

	// storeRecordVersions enables automatic version assignment under DEFAULT.
	// Per-save behaviors and supplied versions override it.
	storeRecordVersions bool

	// splitLongRecords controls whether records >100KB are split across
	// multiple FDB key-value pairs. When true, records exceeding
	// SplitRecordSize (100KB) are split into chunks. When false,
	// attempting to save a record >100KB returns an error.
	// Java equivalent: RecordMetaData.isSplitLongRecords()
	splitLongRecords bool

	// indexes holds all indexes by name (for lookup and HasIndexes check).
	// Java equivalent: RecordMetaData.getAllIndexes()
	indexes map[string]*Index

	// universalIndexes apply to all record types.
	// Java equivalent: RecordMetaData.getUniversalIndexes()
	universalIndexes []*Index

	// formerIndexes tracks deleted indexes for schema evolution safety.
	// Java equivalent: RecordMetaData.getFormerIndexes()
	formerIndexes []*FormerIndex

	// unionDescriptor is the records file's union message, found as Java's
	// fetchUnionDescriptor finds it. Every built RecordMetaData has one: Java has
	// no union-less mode, and neither does Build.
	// Matches Java's RecordMetaData.getUnionDescriptor().
	unionDescriptor protoreflect.MessageDescriptor

	// fieldNumberToRecordType maps union field numbers to record types for
	// direct wire format decoding (avoids UnionDescriptor allocation).
	fieldNumberToRecordType map[protowire.Number]*RecordType

	// subspaceKeyCounter / usesSubspaceKeyCounter are the counter-based
	// subspace-key ASSIGNMENT SCHEME state (proto fields 10 and 11). They are
	// not decoration: they decide the on-disk prefix every future index gets.
	//
	// Per-index subspace keys round-trip on their own, so existing data is safe
	// either way. What is lost by dropping these is the SCHEME: a metadata that
	// went through a Go round-trip without them comes back with the counter
	// disabled, and the next index added — by Java or by Go — is keyed by NAME
	// instead of by counter. Nothing errors; the assignment discipline just
	// silently changes underneath the store.
	// Java: RecordMetaData.subspaceKeyCounter / usesSubspaceKeyCounter.
	subspaceKeyCounter     int64
	usesSubspaceKeyCounter bool

	// preserved holds the proto fields this port does not model, kept verbatim
	// so a Go round-trip re-emits them. See preservedMetaDataFields.
	preserved preservedMetaDataFields

	// usesLocalRecordsDescriptor is set by WithLocalFileDescriptor; ToProto then
	// refuses, as Java's toProto does (RecordMetaData.java:643-645).
	usesLocalRecordsDescriptor bool
}

// UserDefinedFunctions returns the schema's SQL functions (MetaData field 14).
func (m *RecordMetaData) UserDefinedFunctions() []*metadatapb.PUserDefinedFunction {
	return m.preserved.userDefinedFunctions
}

// StoredQueries returns the stored queries (MetaData field 16).
func (m *RecordMetaData) StoredQueries() []*metadatapb.PStoredQuery {
	return m.preserved.storedQueries
}

// Views returns the schema's SQL views (MetaData field 15), in stored order.
func (m *RecordMetaData) Views() []*metadatapb.PView {
	return m.preserved.views
}

// preservedMetaDataFields carries the MetaData proto fields the Go port does
// not model (12: joined_record_types, 13: unnested_record_types,
// 14: user_defined_functions, 15: views), so that ToProto re-emits exactly what
// FromProto was given.
//
// This is CLAUDE.md's promise made real rather than a new feature. The port
// scopes out synthetic record types, user-defined functions and views on the
// stated grounds that protobuf "round-trips them via unknown-field
// preservation" — but these are KNOWN fields of a message the port parses, so
// unknown-field preservation never applied to them and they were being dropped
// on the floor. A Go tool that loaded a Java application's metadata and saved it
// back deleted the application's joined types.
//
// Carrying them opaquely is deliberately NOT the same as supporting them. The
// contents are never interpreted, and anything whose behaviour would depend on
// interpreting them must refuse rather than proceed — see
// RecordMetaData.DeclaresSyntheticRecordTypes and its callers.
type preservedMetaDataFields struct {
	joinedRecordTypes    []*metadatapb.JoinedRecordType
	unnestedRecordTypes  []*metadatapb.UnnestedRecordType
	userDefinedFunctions []*metadatapb.PUserDefinedFunction
	views                []*metadatapb.PView
	storedQueries        []*metadatapb.PStoredQuery

	// unknown holds everything the generated Go type has no field for —
	// principally the MetaData extension range (1000-2000), which is where
	// applications and downstream layers hang their own metadata.
	//
	// These really are unknown fields, but that does NOT mean protobuf carries
	// them across this round trip on its own. Unknown-field preservation keeps
	// them attached to the message they were parsed into; ToProto constructs a
	// FRESH metadatapb.MetaData and copies modelled fields onto it, so the original's
	// unknown bytes have no route to the result and were being dropped. That is
	// the same defect as fields 12-15, arrived at from the opposite direction:
	// there the fields were assumed unknown and were not, here they are genuinely
	// unknown and the mechanism still does not reach them.
	//
	// The bytes-level path (FDBMetaDataStore) never had this problem, because it
	// stores the serialized proto verbatim and never builds a RecordMetaData.
	unknown []byte
}

// FormerIndex tracks a deleted index for schema evolution safety.
// Prevents accidental reuse of an index's subspace key after deletion.
// Matches Java's com.apple.foundationdb.record.metadata.FormerIndex.
type FormerIndex struct {
	SubspaceKey    any
	AddedVersion   int
	RemovedVersion int
	FormerName     string
}

// RecordType represents a type of record that can be stored
type RecordType struct {
	// Name of the record type (usually the protobuf message name)
	Name string

	// Protobuf message descriptor
	Descriptor protoreflect.MessageDescriptor

	// Primary key definition
	PrimaryKey KeyExpression

	// Since version (for schema evolution)
	SinceVersion int

	// Record type index in union descriptor (for key construction)
	RecordTypeIndex int

	// Union field descriptor for reflection-based access
	UnionFieldDescriptor protoreflect.FieldDescriptor

	// unionFieldNumber is the proto field number in the UnionDescriptor for this type.
	// Pre-computed at Build() time for direct wire format encoding/decoding.
	unionFieldNumber protowire.Number

	// reachesMap is whether the record message can hold a map field at any
	// depth, computed at Build (record_wire_map_order.go), and mapReach the
	// answer for every message type the meta-data's record types reach, shared
	// read-only by the walks of its records.
	reachesMap bool
	mapReach   mapReach

	// reachesString is whether the record can hold a string at any depth,
	// which the save checks is valid UTF-8 (utf8_strings.go).
	reachesString bool
	stringReach   stringReach

	// closedEnumReach is, like mapReach, the meta-data's answer for every type
	// its record types reach: whether a closed enum field can occur, which a
	// decoded record must then read as Java reads it (proto_closed_enums.go).
	reachesClosedEnum bool
	closedEnumReach   *closedEnumReach
	// closedEnumRoot is this type's own plan in closedEnumReach, nil when it
	// reaches no closed enum; decodeVT whether its vtproto decoder may read a
	// record (a generated type that cannot nest past the record limit).
	closedEnumRoot *closedEnumPlan
	decodeVT       bool

	// newMessage creates an empty record of this type, set at Build: the
	// compiled Go type whose descriptor is Descriptor, else dynamicpb over it.
	newMessage func() proto.Message

	// indexes defined for this record type (single-type)
	indexes []*Index

	// multiTypeIndexes span multiple record types.
	// Java equivalent: RecordType.getMultiTypeIndexes()
	multiTypeIndexes []*Index

	// explicitRecordTypeKey overrides the auto-derived record type key.
	// If nil, RecordTypeIndex is used. Matches Java's RecordType.getRecordTypeKey().
	explicitRecordTypeKey any
}

// KeyExpression represents an expression that extracts key components from a record.
// Matches Java's KeyExpression interface which returns List<Key.Evaluated>.
type KeyExpression interface {
	// Evaluate extracts key tuples from a record.
	// Returns a list of key tuples (each tuple is a []any).
	// Single-valued expressions return one tuple; fan-out expressions
	// (e.g. repeated fields) return multiple tuples.
	//
	// record is the top-level stored record context (provides version, PK, etc.).
	// msg is the current message being evaluated (changes during nesting into sub-messages).
	// Either or both may be nil.
	//
	// Matches Java's KeyExpression.evaluateMessage(FDBRecord, Message) -> List<Key.Evaluated>.
	Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error)

	// FieldNames returns the field names this expression accesses
	FieldNames() []string

	// ColumnSize returns the number of tuple elements this expression produces.
	// Matches Java's KeyExpression.getColumnSize().
	ColumnSize() int

	// ToKeyExpression serializes this expression to its protobuf representation.
	// Matches Java's KeyExpression.toKeyExpression().
	ToKeyExpression() *metadatapb.KeyExpression
}

// FileDescriptor returns the schema's protobuf file descriptor: the stored records
// file even under WithLocalFileDescriptor, as Java's getRecordsDescriptor (RecordMetaDataBuilder.java:275).
func (m *RecordMetaData) FileDescriptor() protoreflect.FileDescriptor { return m.fileDescriptor }

// RecordTypes returns all record types.
//
// It returns the live map, not a copy. Build derives ambiguousNames from the
// map's key set, so changing keys after Build leaves AmbiguousDeclaredNames
// describing the set Build saw. Constructing a RecordMetaData literal bypasses
// that derivation entirely; copying here would not protect direct field writes.
func (m *RecordMetaData) RecordTypes() map[string]*RecordType {
	return m.recordTypes
}

// Version returns the metadata version
func (m *RecordMetaData) Version() int {
	return m.version
}

// GetRecordCountKey returns the key expression used for record counting.
// Returns nil if counting is disabled.
func (m *RecordMetaData) GetRecordCountKey() KeyExpression {
	return m.recordCountKey
}

// IsStoreRecordVersions returns whether record versioning is enabled.
func (m *RecordMetaData) IsStoreRecordVersions() bool {
	return m.storeRecordVersions
}

// IsSplitLongRecords returns whether records >100KB are split across multiple KV pairs.
func (m *RecordMetaData) IsSplitLongRecords() bool {
	return m.splitLongRecords
}

// DeclaresSyntheticRecordTypes reports whether the metadata DECLARES joined or
// unnested record types — types this port carries verbatim (see
// preservedMetaDataFields) but does not model.
//
// It exists so that "Go does not model synthetic types" cannot be silently
// mistaken for "this metadata has none". Those are different claims, and only
// the second one licenses treating the record-type set as complete. A caller
// that computes over "all record types" — a scan range, a coverage decision, a
// count — is computing over a set that omits the synthetic ones, and must
// refuse rather than answer from the partial set.
func (m *RecordMetaData) DeclaresSyntheticRecordTypes() bool {
	return len(m.preserved.joinedRecordTypes) > 0 || len(m.preserved.unnestedRecordTypes) > 0
}

// SyntheticRecordTypeNames returns the declared joined/unnested type names, for
// diagnostics. The port does not model these types; this reads their names off
// the carried protos so an error can say which ones it refused for.
func (m *RecordMetaData) SyntheticRecordTypeNames() []string {
	names := make([]string, 0,
		len(m.preserved.joinedRecordTypes)+len(m.preserved.unnestedRecordTypes))
	for _, jt := range m.preserved.joinedRecordTypes {
		names = append(names, jt.GetName())
	}
	for _, ut := range m.preserved.unnestedRecordTypes {
		names = append(names, ut.GetName())
	}
	sort.Strings(names)
	return names
}

// GetSubspaceKeyCounter returns the counter-based subspace-key counter value.
// Matches Java's RecordMetaData.getSubspaceKeyCounter().
func (m *RecordMetaData) GetSubspaceKeyCounter() int64 {
	return m.subspaceKeyCounter
}

// UsesSubspaceKeyCounter reports whether index subspace keys are assigned from a
// counter rather than from index names.
// Matches Java's RecordMetaData.usesSubspaceKeyCounter().
func (m *RecordMetaData) UsesSubspaceKeyCounter() bool {
	return m.usesSubspaceKeyCounter
}

// GetUnionDescriptor returns the records file's union message (never nil on a
// built RecordMetaData: a records file without one is refused).
// Matches Java's RecordMetaData.getUnionDescriptor().
func (m *RecordMetaData) GetUnionDescriptor() protoreflect.MessageDescriptor {
	return m.unionDescriptor
}

// GetUnionFieldForRecordType returns the union field descriptor for a record type.
// Every record type of a built RecordMetaData is a union field (validateRecordUnion
// refuses a RECORD-usage message that is not), so this is nil only for a record
// type the metadata does not hold.
// Matches Java's RecordMetaData.getUnionFieldForRecordType().
func (m *RecordMetaData) GetUnionFieldForRecordType(rt *RecordType) protoreflect.FieldDescriptor {
	return rt.UnionFieldDescriptor
}

// AmbiguousDeclaredNames reports a pair of declared record types whose names
// collide across the SQL and storage namespaces, in USER identifiers, or
// ok=false when none collide.
//
// Descriptor names are ESCAPED (protoname.ToProtoBufCompliantName) and the
// escaping is NOT injective across the two namespaces: MY$TABLE is stored as
// MY__1TABLE, while a table whose SQL name IS MY__1TABLE is stored as
// MY__01TABLE. Any lookup that tries a name as given and then its escaped form
// resolves the first of those to the wrong entry, and no ordering fixes it —
// either order prices one of the two wrong.
//
// It lives here, beside DeclaresSyntheticRecordTypes, because it is the same
// KIND of fact: a property of the declarations that decides whether a caller
// computing over "all record types" can trust the answer. Both the statistics
// reader and both collection entry points need it, and a shared property with
// three consumers does not belong in any one of them.
//
// CASE-SENSITIVE, and every gate that cites this function inherits that. The
// collision test is a map lookup on the escaped name, so a schema declaring
// quoted "MY$TABLE" (stored MY__1TABLE) alongside quoted "my__1table" (stored
// my__01table) is NOT reported: the spellings differ only in case. That gap
// does not reach a destructive path, because GetRecordType is case-sensitive
// too and never resolves one to the other -- but a renderer that lowercases
// before comparing would re-open it. Tracked in TODO.md.
//
// Returns USER identifiers because the operator has to act on the SQL names;
// the collision itself is detected in storage space, where it lives.
func (m *RecordMetaData) AmbiguousDeclaredNames() ([]string, bool) {
	if m == nil {
		return nil, false
	}
	return m.ambiguousNames, m.ambiguousFound
}

// computeAmbiguousDeclaredNames is the derivation. Build calls it once and
// stores the result; read AmbiguousDeclaredNames rather than calling this.
func (m *RecordMetaData) computeAmbiguousDeclaredNames() ([]string, bool) {
	if m == nil {
		return nil, false
	}
	declared := m.RecordTypes()
	var worst []string
	for name := range declared {
		escaped, err := protoname.ToProtoBufCompliantName(name)
		if err != nil || escaped == name {
			continue
		}
		if _, collides := declared[escaped]; !collides {
			continue
		}
		pair := []string{
			protoname.ToUserIdentifier(name),
			protoname.ToUserIdentifier(escaped),
		}
		// Deterministic across map iteration order: an operator comparing two
		// runs must not see the pair change.
		if worst == nil || pair[0] < worst[0] {
			worst = pair
		}
	}
	return worst, worst != nil
}

// UserDefinedFunctionName is a stored SQL function's name.
func UserDefinedFunctionName(f *metadatapb.PUserDefinedFunction) string {
	if m := f.GetUserDefinedMacroFunction(); m != nil {
		return m.GetFunctionName()
	}
	return f.GetSqlFunction().GetName()
}

// WithUserDefinedFunctions is m with fns added, each replacing a function of
// its name: the schema as a transaction that declared temporary functions
// sees it.
func (m *RecordMetaData) WithUserDefinedFunctions(fns []*metadatapb.PUserDefinedFunction) *RecordMetaData {
	overlay := *m
	replaced := map[string]bool{}
	for _, f := range fns {
		replaced[UserDefinedFunctionName(f)] = true
	}
	var all []*metadatapb.PUserDefinedFunction
	for _, f := range m.preserved.userDefinedFunctions {
		if !replaced[UserDefinedFunctionName(f)] {
			all = append(all, f)
		}
	}
	overlay.preserved.userDefinedFunctions = append(all, fns...)
	return &overlay
}
