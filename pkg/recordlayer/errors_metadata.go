// Portions derived from FoundationDB Record Layer (MetaDataException.java,
// RecordMetaDataBuilder.java, RecordMetaData.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"strings"

	"fdb.dev/internal/recordlayer/protoname"
	"fdb.dev/internal/recordlayer/textcmp"
)

// Phase 2: Missing error types for implemented features

// MetaDataError is returned for metadata validation failures.
// Matches Java's com.apple.foundationdb.record.metadata.MetaDataException.
type MetaDataError struct {
	Message string
	// Cause is Java's exception cause, when the MetaDataException was built with
	// one (MetaDataException(message, cause)); Error is the message alone, as
	// Java's getMessage is.
	Cause error
}

func (e *MetaDataError) Error() string {
	return e.Message
}

func (*MetaDataError) JavaRecordCoreException() {}

// Unwrap reports this as the RecordCoreError Java's MetaDataException is (it
// extends RecordCoreException), so a caller matching RecordCoreError catches it
// as `catch (RecordCoreException)` does, and returns the cause, so errors.As
// reaches it as Java's getCause does.
func (e *MetaDataError) Unwrap() []error {
	parent := &RecordCoreError{Message: e.Message}
	if e.Cause == nil {
		return []error{parent}
	}
	return []error{parent, e.Cause}
}

// metaDataException marks the Go types whose Java class is MetaDataException
// or a subclass of it (IsMetaDataException).
type metaDataException interface{ javaMetaDataException() }

func (*MetaDataError) javaMetaDataException()                     {}
func (*MetaDataProtoDeserializationError) javaMetaDataException() {}
func (*UnknownIndexTypeError) javaMetaDataException()             {}
func (*IndexVersionTooNewError) javaMetaDataException()           {}
func (*IndexNotFoundError) javaMetaDataException()                {}
func (*MetaDataVersionMustIncreaseError) javaMetaDataException()  {}
func (*RecordTypeKeyTypeError) javaMetaDataException()            {}
func (*GoOnlyCollationError) javaMetaDataException()              {}
func (*InvalidNameError) javaMetaDataException()                  {}

// IsMetaDataException reports whether err's outermost Java exception is a
// MetaDataException, as Java's `re instanceof MetaDataException` tests the
// exception thrown and not its causes (ExceptionUtil.recordCoreToRelationalException).
// The outermost Java exception is the first error of this package met walking
// err's wrappers (fmt's %w, and the first of several), each of which is a Java
// exception class; a MetaDataError that is only the cause of another is not.
func IsMetaDataException(err error) bool {
	switch OutermostJavaError(err).(type) {
	case metaDataException:
		return true
	case *protoname.InvalidNameError:
		// Java's ProtoUtils.InvalidNameException extends MetaDataException
		// (ProtoUtils.java:153); leaf callers of protoname may still emit
		// *protoname.InvalidNameError directly.
		return true
	case *textcmp.MetaDataError:
		return true
	}
	return false
}

// unknownRecordTypeError is Java's RecordMetaData.unknownTypeException
// (RecordMetaData.java:825-827), which getRecordType, getIndexableRecordType and
// getRecordTypeForDescriptor throw for a name the meta-data does not hold.
func unknownRecordTypeError(name string) *MetaDataError {
	return &MetaDataError{Message: "Unknown record type " + name}
}

// MetaDataProtoDeserializationError is Java's
// RecordMetaDataBuilder.MetaDataProtoDeserializationException, a
// MetaDataException "Error converting from protobuf" whose cause is the
// failure: a key expression of the meta-data proto that cannot be read (an
// index's root, a record type's primary key, the record-count key; a
// KeyExpressionDeserializationError), or the subspace-key counter settings
// disagreeing (RecordMetaDataBuilder.java:187-292).
type MetaDataProtoDeserializationError struct {
	Cause error
}

func (e *MetaDataProtoDeserializationError) Error() string { return "Error converting from protobuf" }

// Unwrap reports it as the MetaDataError Java's exception is, and its cause.
func (e *MetaDataProtoDeserializationError) Unwrap() []error {
	return []error{&MetaDataError{Message: e.Error(), Cause: e.Cause}, e.Cause}
}

func (*MetaDataProtoDeserializationError) JavaRecordCoreException() {}

// MetaDataEvolutionError describes a schema evolution violation.
type MetaDataEvolutionError struct {
	Message string
}

func (e *MetaDataEvolutionError) Error() string {
	return e.Message
}

// MetaDataVersionMustIncreaseError is returned by SaveRecordMetaData when
// the new metadata's version is not strictly greater than the stored one.
// This check is unconditional — it does not consult the evolution
// validator's allowNoVersionChange knob, exactly like Java, where
// saveAndSetCurrent throws before the validator ever runs.
// Matches Java's MetaDataException("meta-data version must increase")
// with log keys OLD / NEW.
type MetaDataVersionMustIncreaseError struct {
	OldVersion int32
	NewVersion int32
}

func (e *MetaDataVersionMustIncreaseError) Error() string {
	return fmt.Sprintf("meta-data version must increase (old: %d, new: %d)", e.OldVersion, e.NewVersion)
}

func (*MetaDataVersionMustIncreaseError) JavaRecordCoreException() {}

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

// RecordTypeKeyUnresolvedError reports an evaluation of a record type key
// against a record whose type was never resolved. Java cannot reach this: its
// FDBRecord always carries a RecordType, and the save path evaluates the
// primary key against an FDBStoredRecordBuilder that has been given the type
// before any key is computed (FDBRecordStore.saveTypedRecord). Go's callers
// must do the same. It is an ERROR rather than a fallback to the message's
// type NAME because that fallback wrote a string where the integer type key
// belongs — bytes Java reads as a different key entirely — and did it
// silently.
type RecordTypeKeyUnresolvedError struct {
	// MessageType is the proto message name that was being evaluated, empty
	// when there was no message either.
	MessageType string
}

func (e *RecordTypeKeyUnresolvedError) Error() string {
	return fmt.Sprintf("record type key evaluated without a resolved record type (message type %q): "+
		"the caller must supply the FDBStoredRecord's RecordType", e.MessageType)
}

// SyntheticRecordTypesNotModeledError reports metadata declaring JOINED or
// UNNESTED record types, which this port carries opaquely rather than modelling.
//
// Collection refuses such a schema before scanning. RecordTypes() omits those
// declarations, so a run would read every record in the store to produce a set
// that is a PARTIAL view of the schema by construction — and would then write a
// header vouching for it. Completeness is undecidable against a partial set, so
// there is no reading of that outcome a planner could safely use.
type SyntheticRecordTypesNotModeledError struct {
	// TypeNames are the synthetic declarations found, so a caller can say which.
	//
	// STORAGE names -- and unlike every other name field outside
	// StatisticsStatus, they stay that way all the way out: Error() prints them
	// verbatim too, for the reason given there. A programmatic consumer matching
	// against metadata needs them in the namespace metadata uses, and here so
	// does the human.
	TypeNames []string
}

// Error renders the refusal for a human, naming the declarations EXACTLY as the
// metadata stores them.
//
// The reason is the PRIOR on what produced the string -- not the domain, and
// not proof of provenance. Both kinds of name end up as protobuf message names
// (Java's SyntheticRecordTypeBuilder.buildDescriptor sets the synthetic one via
// DescriptorProto.setName, so it is validated exactly like a record type's), so
// the domain does not separate them; and DecodeOnceIfReversible is a pure
// function of the string, so its round-trip test would run identically on
// either. What differs is what could have written the name:
//
//   - A record type declared through SQL DDL passes through
//     protoname.ToProtoBufCompliantName. Among stored names that round-trip,
//     escaped ones are therefore the case worth serving, and decoding is the
//     best available reading. It is NOT proof -- SetRecords copies descriptor
//     names verbatim -- which is exactly why that guard exists instead of an
//     unconditional decode.
//
//   - Nothing escapes into a synthetic name. Java's
//     RecordMetaDataBuilder.addJoinedRecordType / addUnnestedRecordType store
//     the string their caller passed, and this port never CREATES one; it only
//     round-trips what Java wrote. So a synthetic MY__1JOINED that round-trips
//     is not evidence of escaping, and decoding it would invent MY$JOINED -- a
//     declaration the operator cannot find.
//
// That matters most precisely here: this refusal exists because the port does
// not model these types, so the only thing an operator can do with the name is
// match it against their Java-side metadata, which holds the verbatim string.
//
// The list arrives sorted by SyntheticRecordTypeNames and stays in that order;
// with no decoding step there are not two namespaces to disagree about.
func (e *SyntheticRecordTypesNotModeledError) Error() string {
	return fmt.Sprintf("metadata declares synthetic record types this port does not model (%s); "+
		"statistics collection refused rather than scanning the store for a set that "+
		"could never be complete", strings.Join(e.TypeNames, ", "))
}

// GoOnlyCollationError rejects provider-specific collation keys without explicit
// Go-only opt-in. It unwraps to MetaDataError; Java has no equivalent refusal.
type GoOnlyCollationError struct {
	Function       string
	IndexName      string // the refused index, or "" for a primary key
	RecordTypeName string // the record type whose primary key is refused
	RecordCountKey bool   // the legacy record-count expression is refused
}

func (e *GoOnlyCollationError) Error() string {
	subject := fmt.Sprintf("index %q", e.IndexName)
	if e.RecordCountKey {
		subject = "record count key"
	} else if e.IndexName == "" {
		subject = fmt.Sprintf("primary key of record type %q", e.RecordTypeName)
	}
	return fmt.Sprintf("%s uses %s, whose Go collation keys are not the bytes Java writes; "+
		"only a store no Java process opens may use it, opened with StoreBuilder.SetGoOnlyCollation(true)",
		subject, e.Function)
}

func (*GoOnlyCollationError) JavaRecordCoreException() {}

func (e *GoOnlyCollationError) Unwrap() error {
	return &MetaDataError{Message: e.Error()}
}
