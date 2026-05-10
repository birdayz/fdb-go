package plans

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

// TranslateValueFunction translates a Value from the domain of a
// fetched full record to the domain of the partial record (index entry)
// that feeds the fetch. Used by RecordQueryFetchFromPartialRecordPlan
// to enable push-through rules (pushing filters, maps, set operations
// below the fetch).
//
// Mirrors Java's `TranslateValueFunction` functional interface.
type TranslateValueFunction func(
	value values.Value,
	sourceAlias values.CorrelationIdentifier,
	targetAlias values.CorrelationIdentifier,
) (values.Value, bool)

// UnableToTranslate is a TranslateValueFunction that always fails
// (returns false). Used when no translation is possible.
func UnableToTranslate(_ values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
	return nil, false
}
