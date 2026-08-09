package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// scanRangeExecutionIdentityBuilder builds the part of a range-set
// continuation fingerprint that describes the physical execution surrounding
// the evaluated comparison ranges. Every field has a length-delimited name,
// an explicit wire kind, and a length-delimited payload. That makes identities
// unambiguous even when adjacent strings can be repartitioned (for example,
// ["ab", "c"] versus ["a", "bc"]). The final value is a full SHA-256 digest;
// unlike plan HashCode or Explain output, neither truncation nor display text is
// used as continuation identity.
type scanRangeExecutionIdentityBuilder struct {
	fields bytes.Buffer
}

const (
	scanIdentityBytes byte = iota + 1
	scanIdentityString
	scanIdentityBool
	scanIdentityInt64
	scanIdentityStringList
	scanIdentityType
	scanIdentityTypeList
	scanIdentityFloat64List
	scanIdentityTupleRange
	scanIdentityOptionalInt64
)

// ScanIdentityTypeErrorKind identifies why a values.Type cannot safely
// participate in a scan continuation identity. The encoder accepts arbitrary
// implementations of values.Type, including hand-built plans, so it must reject
// interface values that would otherwise panic or recurse without end.
type ScanIdentityTypeErrorKind uint8

const (
	ScanIdentityTypeErrorTypedNil ScanIdentityTypeErrorKind = iota + 1
	ScanIdentityTypeErrorCycle
)

// InvalidScanRangeExecutionIdentityTypeError reports an invalid values.Type
// graph while a continuation fingerprint is being built. It deliberately
// records only concrete implementation names and structural paths: formatting
// the Type itself could invoke String on the same malformed graph and recurse.
type InvalidScanRangeExecutionIdentityTypeError struct {
	Kind           ScanIdentityTypeErrorKind
	Implementation string
	Path           string
	ActivePath     string
}

func (e *InvalidScanRangeExecutionIdentityTypeError) Error() string {
	switch e.Kind {
	case ScanIdentityTypeErrorTypedNil:
		return fmt.Sprintf(
			"scan range execution identity: typed-nil values.Type %s at %s",
			e.Implementation,
			e.Path,
		)
	case ScanIdentityTypeErrorCycle:
		return fmt.Sprintf(
			"scan range execution identity: cyclic values.Type %s at %s (already active at %s)",
			e.Implementation,
			e.Path,
			e.ActivePath,
		)
	default:
		return fmt.Sprintf(
			"scan range execution identity: invalid values.Type %s at %s",
			e.Implementation,
			e.Path,
		)
	}
}

func newScanRangeExecutionIdentityBuilder(domain string) *scanRangeExecutionIdentityBuilder {
	b := &scanRangeExecutionIdentityBuilder{}
	b.stringField("format", "scan-range-execution-identity-v1")
	b.stringField("domain", domain)
	return b
}

func (b *scanRangeExecutionIdentityBuilder) field(name string, kind byte, payload []byte) {
	writeIdentityLength(&b.fields, len(name))
	_, _ = b.fields.WriteString(name)
	_ = b.fields.WriteByte(kind)
	writeIdentityLength(&b.fields, len(payload))
	_, _ = b.fields.Write(payload)
}

func (b *scanRangeExecutionIdentityBuilder) bytesField(name string, value []byte) {
	b.field(name, scanIdentityBytes, value)
}

func (b *scanRangeExecutionIdentityBuilder) stringField(name, value string) {
	b.field(name, scanIdentityString, []byte(value))
}

func (b *scanRangeExecutionIdentityBuilder) boolField(name string, value bool) {
	encoded := byte(0)
	if value {
		encoded = 1
	}
	b.field(name, scanIdentityBool, []byte{encoded})
}

func (b *scanRangeExecutionIdentityBuilder) intField(name string, value int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(int64(value)))
	b.field(name, scanIdentityInt64, encoded[:])
}

func (b *scanRangeExecutionIdentityBuilder) optionalIntField(name string, value *int) {
	if value == nil {
		b.field(name, scanIdentityOptionalInt64, []byte{0})
		return
	}
	var encoded [9]byte
	encoded[0] = 1
	binary.BigEndian.PutUint64(encoded[1:], uint64(int64(*value)))
	b.field(name, scanIdentityOptionalInt64, encoded[:])
}

func (b *scanRangeExecutionIdentityBuilder) stringsField(name string, values []string) {
	var encoded bytes.Buffer
	writeIdentityLength(&encoded, len(values))
	for _, value := range values {
		writeIdentityLength(&encoded, len(value))
		_, _ = encoded.WriteString(value)
	}
	b.field(name, scanIdentityStringList, encoded.Bytes())
}

func (b *scanRangeExecutionIdentityBuilder) typeField(name string, typ values.Type) error {
	encoded, err := newScanIdentityTypeEncoder().encode(typ, name)
	if err != nil {
		return err
	}
	b.field(name, scanIdentityType, encoded)
	return nil
}

func (b *scanRangeExecutionIdentityBuilder) typesField(name string, types []values.Type) error {
	var encoded bytes.Buffer
	writeIdentityLength(&encoded, len(types))
	encoder := newScanIdentityTypeEncoder()
	for i, typ := range types {
		encodedType, err := encoder.encode(typ, fmt.Sprintf("%s[%d]", name, i))
		if err != nil {
			return err
		}
		writeIdentityLength(&encoded, len(encodedType))
		_, _ = encoded.Write(encodedType)
	}
	b.field(name, scanIdentityTypeList, encoded.Bytes())
	return nil
}

func (b *scanRangeExecutionIdentityBuilder) float64sField(name string, values []float64) {
	var encoded bytes.Buffer
	writeIdentityLength(&encoded, len(values))
	for _, value := range values {
		var bits [8]byte
		binary.BigEndian.PutUint64(bits[:], math.Float64bits(value))
		_, _ = encoded.Write(bits[:])
	}
	b.field(name, scanIdentityFloat64List, encoded.Bytes())
}

func (b *scanRangeExecutionIdentityBuilder) tupleRangeField(name string, value recordlayer.TupleRange) {
	var encoded bytes.Buffer
	writeIdentityTuple(&encoded, value.Low)
	writeIdentityTuple(&encoded, value.High)
	writeIdentityLength(&encoded, int(value.LowEndpoint))
	writeIdentityLength(&encoded, int(value.HighEndpoint))
	b.field(name, scanIdentityTupleRange, encoded.Bytes())
}

func (b *scanRangeExecutionIdentityBuilder) sum() string {
	digest := sha256.Sum256(b.fields.Bytes())
	return string(digest[:])
}

func writeIdentityLength(dst *bytes.Buffer, value int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = dst.Write(encoded[:])
}

func writeIdentityTuple(dst *bytes.Buffer, value tuple.Tuple) {
	if value == nil {
		_ = dst.WriteByte(0)
	} else {
		_ = dst.WriteByte(1)
	}
	packed := value.Pack()
	writeIdentityLength(dst, len(packed))
	_, _ = dst.Write(packed)
}

type scanIdentityTypeNode struct {
	implementation reflect.Type
	pointer        uintptr
}

type scanIdentityTypeEncoder struct {
	// active contains only the current recursion path. A completed node is
	// removed, so a shared acyclic subgraph is encoded in full at every use and
	// therefore has the same structural identity as two independently allocated
	// but equal subgraphs.
	active map[scanIdentityTypeNode]string
}

func newScanIdentityTypeEncoder() *scanIdentityTypeEncoder {
	return &scanIdentityTypeEncoder{active: make(map[scanIdentityTypeNode]string)}
}

// encodeScanIdentityType is structural for every built-in values.Type. Key
// schemas normally contain primitive types, but encoding the complete type
// hierarchy keeps the identity correct for hand-built plans and future schema
// extensions. Malformed typed-nil or cyclic built-in graphs return a typed error
// instead of panicking or overflowing the Go stack. The fallback includes the
// concrete implementation name in addition to the Type contract, so an
// out-of-tree implementation cannot alias a built-in type merely by returning
// the same display string.
func encodeScanIdentityType(typ values.Type) ([]byte, error) {
	return newScanIdentityTypeEncoder().encode(typ, "$type")
}

func (e *scanIdentityTypeEncoder) encode(typ values.Type, path string) ([]byte, error) {
	b := newScanRangeExecutionIdentityBuilder("type")
	if typ == nil {
		b.boolField("present", false)
		return b.fields.Bytes(), nil
	}

	implementation := fmt.Sprintf("%T", typ)
	reflected := reflect.ValueOf(typ)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		if reflected.IsNil() {
			return nil, &InvalidScanRangeExecutionIdentityTypeError{
				Kind:           ScanIdentityTypeErrorTypedNil,
				Implementation: implementation,
				Path:           path,
			}
		}
	}

	if node, composite := scanIdentityCompositeTypeNode(typ); composite {
		if activePath, cyclic := e.active[node]; cyclic {
			return nil, &InvalidScanRangeExecutionIdentityTypeError{
				Kind:           ScanIdentityTypeErrorCycle,
				Implementation: implementation,
				Path:           path,
				ActivePath:     activePath,
			}
		}
		e.active[node] = path
		defer delete(e.active, node)
	}

	b.boolField("present", true)
	b.stringField("implementation", implementation)
	b.intField("code", int(typ.Code()))
	b.boolField("nullable", typ.IsNullable())

	switch concrete := typ.(type) {
	case *values.PrimitiveType:
		// Code and nullability fully define a PrimitiveType.
	case *values.RecordType:
		b.stringField("record-name", concrete.RecordName)
		var fields bytes.Buffer
		writeIdentityLength(&fields, len(concrete.Fields))
		for i, field := range concrete.Fields {
			writeIdentityLength(&fields, len(field.Name))
			_, _ = fields.WriteString(field.Name)
			var ordinal [8]byte
			binary.BigEndian.PutUint64(ordinal[:], uint64(int64(field.Ordinal)))
			_, _ = fields.Write(ordinal[:])
			encodedType, err := e.encode(
				field.FieldType,
				fmt.Sprintf("%s.record-fields[%d].field-type", path, i),
			)
			if err != nil {
				return nil, err
			}
			writeIdentityLength(&fields, len(encodedType))
			_, _ = fields.Write(encodedType)
		}
		b.bytesField("record-fields", fields.Bytes())
	case *values.ArrayType:
		encodedElement, err := e.encode(concrete.ElementType, path+".element-type")
		if err != nil {
			return nil, err
		}
		b.field("element-type", scanIdentityType, encodedElement)
	case *values.EnumType:
		b.stringField("enum-name", concrete.EnumName)
		var enumValues bytes.Buffer
		writeIdentityLength(&enumValues, len(concrete.Values))
		for _, enumValue := range concrete.Values {
			writeIdentityLength(&enumValues, len(enumValue.Name))
			_, _ = enumValues.WriteString(enumValue.Name)
			var number [4]byte
			binary.BigEndian.PutUint32(number[:], uint32(enumValue.Number))
			_, _ = enumValues.Write(number[:])
		}
		b.bytesField("enum-values", enumValues.Bytes())
	case *values.RelationType:
		encodedInner, err := e.encode(concrete.InnerType, path+".inner-type")
		if err != nil {
			return nil, err
		}
		b.field("inner-type", scanIdentityType, encodedInner)
	default:
		b.stringField("contract-rendering", typ.String())
	}
	return b.fields.Bytes(), nil
}

func scanIdentityCompositeTypeNode(typ values.Type) (scanIdentityTypeNode, bool) {
	switch typ.(type) {
	case *values.RecordType, *values.ArrayType, *values.RelationType:
		reflected := reflect.ValueOf(typ)
		return scanIdentityTypeNode{
			implementation: reflected.Type(),
			pointer:        reflected.Pointer(),
		}, true
	default:
		return scanIdentityTypeNode{}, false
	}
}

// primaryScanRangeFingerprintSalt identifies every execution-shape input that
// is outside the evaluated primary-key comparison ranges themselves.
func primaryScanRangeFingerprintSalt(plan *plans.RecordQueryScanPlan) (string, error) {
	b := newScanRangeExecutionIdentityBuilder("primary")
	b.stringsField("record-types", plan.GetRecordTypes())
	b.boolField("reverse", plan.IsReverse())
	if err := b.typesField("physical-key-types", plan.GetKeyComponentTypes()); err != nil {
		return "", err
	}
	if err := b.typeField("flowed-type", plan.GetFlowedType()); err != nil {
		return "", err
	}
	return b.sum(), nil
}

// indexScanRangeFingerprintSalt binds an index range continuation to its
// physical index and its post-scan row-shaping path. In particular, a covering
// plan cannot consume a fetch plan's token, and changing covering-column order
// changes the identity because it changes positional row construction.
func indexScanRangeFingerprintSalt(
	plan *plans.RecordQueryIndexPlan,
	scanType recordlayer.IndexScanType,
) (string, error) {
	// A bare index plan is never covering — coveringness is
	// RecordQueryCoveringIndexPlan. The two fields are nonetheless still
	// WRITTEN, at their original positions and with their original names, so
	// the digest of a plan this RFC did not change stays BYTE-IDENTICAL and
	// in-flight continuations keep resuming. They are constants here, not
	// omissions: removing them would silently invalidate every outstanding
	// index-scan continuation.
	return indexScanRangeFingerprintSaltFields(plan, scanType, false, nil)
}

// coveringIndexScanRangeFingerprintSalt is the covering plan's execution
// identity. It reproduces indexScanRangeFingerprintSalt's builder prefix and
// field ORDER exactly, folding the inner plan's index name, reverse flag,
// primary-key columns, record types and key/flowed types directly — the inner
// is a field, not a child, so nothing else in the continuation path reaches it.
//
// Byte-identity is the point: a covering plan that this RFC left unchanged in
// substance must produce the digest the flag-carrying plan produced, or every
// outstanding covering-scan continuation is rejected on upgrade.
//
// The scan COMPARISONS are deliberately not folded here, and their omission is
// not a C2 gap: the salt names everything OUTSIDE the ranges, and it is then
// hashed together with the fully materialized ranges by
// boundScanRangeSet.fingerprint (components, per-component alternatives, every
// tail's endpoints, and the packed low/high bytes). Two covering scans over
// different ranges therefore differ in that digest even though their salts
// agree. Folding the comparisons here as well would change the digest of every
// plan and buy nothing.
func coveringIndexScanRangeFingerprintSalt(
	plan *plans.RecordQueryCoveringIndexPlan,
	scanType recordlayer.IndexScanType,
) (string, error) {
	return indexScanRangeFingerprintSaltFields(
		plan.GetIndexPlan(), scanType, true, plan.GetCoveringColumns())
}

// indexScanRangeFingerprintSaltFields is the ONE field list both index-scan
// salts emit. Sharing it is what makes "byte-identical for an unchanged plan" a
// property of the code rather than of two copies staying in step.
func indexScanRangeFingerprintSaltFields(
	plan *plans.RecordQueryIndexPlan,
	scanType recordlayer.IndexScanType,
	covering bool,
	coveringColumns []string,
) (string, error) {
	b := newScanRangeExecutionIdentityBuilder("index")
	b.stringField("index-name", plan.GetIndexName())
	b.stringField("scan-type", string(scanType))
	b.boolField("reverse", plan.IsReverse())
	b.boolField("covering", covering)
	b.stringsField("covering-columns", coveringColumns)
	b.stringsField("primary-key-columns", plan.GetPKColumnNames())
	b.stringsField("record-types", plan.GetRecordTypes())
	if err := b.typesField("physical-key-types", plan.GetKeyComponentTypes()); err != nil {
		return "", err
	}
	if err := b.typeField("flowed-type", plan.GetFlowedType()); err != nil {
		return "", err
	}
	return b.sum(), nil
}

// aggregateScanRangeFingerprintSalt includes both the logical aggregate row
// layout and the physical BY_VALUE/BY_GROUP layout supplied by metadata. The
// explicit physical counts keep a hand-built or stale permuted plan from
// resuming across the aggregate-value insertion boundary.
func aggregateScanRangeFingerprintSalt(
	plan *plans.RecordQueryAggregateIndexPlan,
	indexType string,
	scanType recordlayer.IndexScanType,
	physicalGroupingCount int,
	physicalGroupingPrefixCount int,
) (string, error) {
	b := newScanRangeExecutionIdentityBuilder("aggregate")
	b.stringField("index-name", plan.GetIndexName())
	b.stringField("index-type", indexType)
	b.stringField("scan-type", string(scanType))
	b.boolField("reverse", plan.IsReverse())
	b.stringField("record-type", plan.GetRecordTypeName())
	b.stringField("aggregate-function", plan.GetAggregateFunction())
	b.stringsField("group-columns", plan.GetGroupCols())
	b.stringField("aggregate-column", plan.GetAggColumn())
	b.stringField("canonical-aggregate-column", plan.CanonicalAggColumnName())
	b.intField("physical-grouping-count", physicalGroupingCount)
	b.intField("physical-grouping-prefix-count", physicalGroupingPrefixCount)
	if err := b.typesField("physical-key-types", plan.GetKeyComponentTypes()); err != nil {
		return "", err
	}
	if err := b.typeField("result-type", plan.GetResultType()); err != nil {
		return "", err
	}
	return b.sum(), nil
}

// vectorScanRangeFingerprintSalt binds the partition-range continuation to
// both the supplied plan mode and the evaluated physical invocation. Raw IEEE
// bits are used for query-vector elements, preserving signed zero and NaN
// payload distinctions. The exact encoded TupleRange is included as a final
// guard against future changes in vector-range construction.
func vectorScanRangeFingerprintSalt(
	plan *plans.RecordQueryVectorIndexPlan,
	indexType string,
	scanType recordlayer.IndexScanType,
	queryVector []float64,
	requestedK int,
	rankCap int,
	effectiveEfSearch int,
	invocationRange recordlayer.TupleRange,
) (string, error) {
	b := newScanRangeExecutionIdentityBuilder("vector")
	b.stringField("index-name", plan.GetIndexName())
	b.stringField("index-type", indexType)
	b.stringField("scan-type", string(scanType))
	b.intField("rank-comparison-type", int(plan.GetRankType()))
	b.boolField("ordered-stream", plan.IsOrderedStream())
	b.boolField("returning-vectors", plan.IsReturningVectors())
	b.optionalIntField("supplied-ef-search", plan.GetEfSearch())
	b.stringsField("record-types", plan.GetRecordTypes())
	if err := b.typesField("physical-partition-key-types", plan.GetPartitionKeyComponentTypes()); err != nil {
		return "", err
	}
	if err := b.typeField("result-type", plan.GetResultType()); err != nil {
		return "", err
	}
	b.float64sField("evaluated-query-vector", queryVector)
	b.intField("evaluated-requested-k", requestedK)
	b.intField("adjusted-rank-cap", rankCap)
	b.intField("effective-ef-search", effectiveEfSearch)
	b.tupleRangeField("physical-invocation-range", invocationRange)
	return b.sum(), nil
}
