package plans

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// FetchIndexRecords governs how to interpret the primary key of an
// index entry when fetching the base record. Mirrors Java's
// `RecordQueryFetchFromPartialRecordPlan.FetchIndexRecords` enum.
type FetchIndexRecords int

const (
	// FetchIndexRecordsPrimaryKey fetches the base record by its
	// primary key (the standard path).
	FetchIndexRecordsPrimaryKey FetchIndexRecords = iota
	// FetchIndexRecordsSyntheticConstituents fetches synthetic record
	// constituents (for synthetic/joined record types).
	FetchIndexRecordsSyntheticConstituents
)

// RecordQueryFetchFromPartialRecordPlan transforms a stream of partial
// records (index entries from a covering index scan) into full records
// by fetching via primary key. Mirrors Java's
// `RecordQueryFetchFromPartialRecordPlan`.
//
// The plan has:
//   - An inner plan that produces index entries (partial records).
//   - A TranslateValueFunction that maps values from the full-record
//     domain to the partial-record (index) domain — used by push-through
//     rules to determine which predicates/values can be evaluated before
//     the fetch.
//   - A result type (the full record type post-fetch).
//   - A FetchIndexRecords mode.
type RecordQueryFetchFromPartialRecordPlan struct {
	inner                  RecordQueryPlan
	translateValueFunction TranslateValueFunction
	resultType             values.Type
	fetchIndexRecords      FetchIndexRecords
}

// NewRecordQueryFetchFromPartialRecordPlan constructs the plan.
func NewRecordQueryFetchFromPartialRecordPlan(
	inner RecordQueryPlan,
	translateValueFunction TranslateValueFunction,
	resultType values.Type,
	fetchIndexRecords FetchIndexRecords,
) *RecordQueryFetchFromPartialRecordPlan {
	if resultType == nil {
		resultType = values.UnknownType
	}
	if translateValueFunction == nil {
		translateValueFunction = UnableToTranslate
	}
	return &RecordQueryFetchFromPartialRecordPlan{
		inner:                  inner,
		translateValueFunction: translateValueFunction,
		resultType:             resultType,
		fetchIndexRecords:      fetchIndexRecords,
	}
}

// GetInner returns the inner plan (typically a covering index scan).
func (p *RecordQueryFetchFromPartialRecordPlan) GetInner() RecordQueryPlan { return p.inner }

// GetResultType returns the full record type post-fetch.
func (p *RecordQueryFetchFromPartialRecordPlan) GetResultType() values.Type { return p.resultType }

// GetFetchIndexRecords returns the fetch mode.
func (p *RecordQueryFetchFromPartialRecordPlan) GetFetchIndexRecords() FetchIndexRecords {
	return p.fetchIndexRecords
}

// GetTranslateValueFunction returns the push-value function.
func (p *RecordQueryFetchFromPartialRecordPlan) GetTranslateValueFunction() TranslateValueFunction {
	return p.translateValueFunction
}

// PushValue attempts to translate a value from the full-record domain
// (correlated to sourceAlias) to the partial-record domain (correlated
// to targetAlias). Returns the translated value and true on success,
// or nil and false if translation is not possible.
//
// Mirrors Java's `RecordQueryFetchFromPartialRecordPlan.pushValue`.
func (p *RecordQueryFetchFromPartialRecordPlan) PushValue(
	value values.Value,
	sourceAlias values.CorrelationIdentifier,
	targetAlias values.CorrelationIdentifier,
) (values.Value, bool) {
	return p.translateValueFunction(value, sourceAlias, targetAlias)
}

// GetChildren returns the inner plan.
func (p *RecordQueryFetchFromPartialRecordPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares fetch mode (inner is the caller's
// responsibility via children).
func (p *RecordQueryFetchFromPartialRecordPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		return false
	}
	return p.fetchIndexRecords == o.fetchIndexRecords
}

// HashCodeWithoutChildren mixes type discriminator + fetch mode.
func (p *RecordQueryFetchFromPartialRecordPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("fetchfrompartialrecordplan|"))
	h.Write([]byte{byte(p.fetchIndexRecords)})
	return h.Sum64()
}

// Explain renders Fetch(inner).
func (p *RecordQueryFetchFromPartialRecordPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("Fetch(%s)", innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryFetchFromPartialRecordPlan)(nil)
