package recordlayer

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// QueryBuilder provides a fluent API for constructing query plans.
//
// Example:
//
//	plan := NewQuery().
//	    FromIndex("order_price", TupleRangeAll).
//	    Filter("price > 50", func(r *FDBStoredRecord[proto.Message]) bool {
//	        return r.Record.(*gen.Order).GetPrice() > 50
//	    }).
//	    Limit(10).
//	    Build()
//
//	results, err := ExecuteAndCollect(ctx, store, plan)
type QueryBuilder struct {
	plan RecordQueryPlan
}

// NewQuery starts building a query from a full table scan.
func NewQuery() *QueryBuilder {
	return &QueryBuilder{plan: &ScanPlan{}}
}

// NewQueryFrom starts building a query from a specific record type scan.
func NewQueryFrom(recordType string) *QueryBuilder {
	return &QueryBuilder{plan: &ScanPlan{RecordTypeName: recordType}}
}

// NewQueryFromIndex starts building a query from an index scan.
func NewQueryFromIndex(indexName string, scanRange TupleRange) *QueryBuilder {
	return &QueryBuilder{plan: &IndexPlan{IndexName: indexName, Range: scanRange}}
}

// NewQueryInRange starts building a query for a primary key range scan.
func NewQueryInRange(low, high tuple.Tuple) *QueryBuilder {
	return &QueryBuilder{plan: &RangeScanPlan{
		Low: low, High: high,
		LowEndpoint:  EndpointTypeRangeInclusive,
		HighEndpoint: EndpointTypeRangeExclusive,
	}}
}

// NewQueryByPK starts building a query for a single primary key lookup.
func NewQueryByPK(pk tuple.Tuple) *QueryBuilder {
	return &QueryBuilder{plan: &PrimaryKeyLookupPlan{PrimaryKey: pk}}
}

// Filter adds a predicate filter to the query.
func (qb *QueryBuilder) Filter(desc string, pred func(*FDBStoredRecord[proto.Message]) bool) *QueryBuilder {
	qb.plan = &FilterPlan{
		Child:       qb.plan,
		Predicate:   pred,
		Description: desc,
	}
	return qb
}

// Limit caps the number of returned results.
func (qb *QueryBuilder) Limit(n int) *QueryBuilder {
	qb.plan = &LimitPlan{Child: qb.plan, Limit: n}
	return qb
}

// Reverse reverses the scan direction.
func (qb *QueryBuilder) Reverse() *QueryBuilder {
	qb.plan = &ReversePlan{Child: qb.plan}
	return qb
}

// Union merges this query's results with another query's results.
func (qb *QueryBuilder) Union(other *QueryBuilder) *QueryBuilder {
	qb.plan = &UnionPlan{Left: qb.plan, Right: other.plan}
	return qb
}

// Intersect keeps only results present in both this query and another.
func (qb *QueryBuilder) Intersect(other *QueryBuilder) *QueryBuilder {
	qb.plan = &IntersectionPlan{Left: qb.plan, Right: other.plan}
	return qb
}

// Build returns the constructed plan.
func (qb *QueryBuilder) Build() RecordQueryPlan {
	return qb.plan
}

// Explain returns a human-readable description of the built plan.
func (qb *QueryBuilder) Explain() string {
	return qb.plan.Explain(0)
}
