package values

import "fmt"

// ResolutionErrorCode is the stable machine-readable failure taxonomy for
// checked type, correlation, value, and ordinal construction.
type ResolutionErrorCode uint16

const (
	TypeNil ResolutionErrorCode = iota + 1
	TypeTypedNil
	TypeCycle
	TypeUnresolved
	TypeErased
	TypeMalformedCode
	TypeMalformedOrdinal
	CorrelationZero
	CorrelationForeignValue
	CorrelationKindMismatch
	CorrelationTypeConflict
	FlowedTypeUnavailable
	FlowedTypeDisagreement
	FieldNilChild
	FieldUnsupportedChild
	FieldEmptyPath
	FieldInvalidRequest
	FieldNegativeOrdinal
	FieldOutOfRange
	FieldNonRecord
	FieldUnknownName
	FieldAmbiguousName
	FieldNameOrdinalMismatch
	FieldIncompatibleRoot
	LayoutForeignValue
	LayoutNonRecordCarrier
	LayoutInvalidTile
	LayoutTileGap
	LayoutTileOverlap
	LayoutInvalidPath
	LayoutInvalidWindow
	LayoutDuplicateSource
	LayoutTypeMismatch
	LayoutNullabilityMismatch
	LayoutCarrierMismatch
	LayoutSourceNotProvided
	LayoutPresenceMissing
	LayoutRuntimeShape
	LayoutNormalizationUnsupported
	LayoutNormalizationTypeMismatch
	ReanchorInvalidValue
	ReanchorTargetMismatch
	ReanchorUnmappedSource
	ReanchorInvalidMappedPath
	ReanchorResultTypeMismatch
	UnboundCorrelation
	AggregateInvalidFunction
	AggregateMissingOperand
	AggregateUnexpectedOperand
	AggregateUnsupportedOperand
	AggregateTypeMismatch
	AggregateLaneMismatch
	AggregateOutputNoMatch
	AggregateOutputAmbiguous
	RewriteNilReplacement
	RewriteValueCycle
	RewriteInvalidCallbackOutput
	RewriteInvalidArity
	RewriteNonComparableNode
	RewriteInvalidTranslation
	UnsupportedValueRebuild
	RuleDirectMutation
	MemoUnsupportedExpression
	MemoResultTypeMismatch
	MemoBatchConflict
	MemoMissingRelationWrapper
	MemoDoubleRelationWrapper
	MemoEmptyReference
	MemoInvalidHandle
	MemoProvisionalEscape
	MemoReferenceCycle
	MemoTransactionClosed
	MemoReentrantTransaction
)

// ResolutionError carries a stable code while retaining enough path context
// for a useful planning diagnostic. It never wraps a partially constructed
// value.
type ResolutionError struct {
	ErrorCode ResolutionErrorCode
	Path      string
	Detail    string
}

func (e *ResolutionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	location := e.Path
	if location == "" {
		location = "value"
	}
	if e.Detail == "" {
		return fmt.Sprintf("resolution error %d at %s", e.ErrorCode, location)
	}
	return fmt.Sprintf("resolution error %d at %s: %s", e.ErrorCode, location, e.Detail)
}

func (e *ResolutionError) Code() ResolutionErrorCode {
	if e == nil {
		return 0
	}
	return e.ErrorCode
}

func resolutionError(code ResolutionErrorCode, path, detail string) error {
	return &ResolutionError{ErrorCode: code, Path: path, Detail: detail}
}
