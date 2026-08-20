package properties

// CollectedStatistics is RFC-236's statistics provider: per-record-type row
// counts gathered by the offline collector.
//
// It is deliberately NOT MapStatistics. Two differences, both load-bearing.
//
// COMPLETENESS. It is constructed only when every record type in the schema has
// a fresh entry, so a named lookup can never miss. MapStatistics is built from
// whatever was available and answers a miss with LeafScanCardinality; that is
// fine for its callers and fatal here, because a miss is not merely wrong, it is
// wrong in the INVERTING direction. LeafScanCardinality is 1e6 — larger than
// almost any real count — so one missing type standing beside a real 150-row
// count makes the missing one the largest table in the schema and drives the
// join from the wrong side. Statistics that do that are worse than none, which
// at least ties.
//
// This inversion is reachable under ONE provider, which is what makes it real:
// MapStatistics' fallback is a CONSTANT, so the miss and the hit are drawn from
// the same map and still differ by four orders of magnitude. It is a different
// mechanism from the empty-NAME case below, and the two are easy to conflate —
// an earlier revision of the FullUnorderedScan comment described a
// universal-vs-typed comparison as inverting when, under one provider, it is
// not. Keep them apart: a MISSING name inverts because the fallback is a
// constant; the EMPTY name would invert only for a store larger than
// LeafScanCardinality.
//
// THE EMPTY NAME IS NOT A RECORD TYPE. Production sites ask for it when a leaf's
// record types are unknown — a nil plan, or a scan carrying no type list
// (planning_cost_model.go, plans/cost.go, and FullUnorderedScanExpression with an
// empty type list). The population is deliberately NOT written as a number here:
// an earlier revision said "four", RFC-236 then added the fifth, and the stale
// count sat in the file that states the rule about comments outrunning code. To
// see the current set:
//
//	grep -rn --include='*.go' 'RecordTypeCardinality("")' . |
//	  grep -v _test.go | grep -v '^[^:]*:[0-9]*:\s*//'
//
// Semantically that leaf could read anything in the store, so the honest
// answer is the whole store: the SUM of every type. That keeps the value on
// the same scale as the data, so comparing
// it against a real count still means something, whatever the store's size. A
// magic constant does not.
type CollectedStatistics struct {
	perType map[string]float64
	total   float64
}

// NewCollectedStatistics builds a provider over a COMPLETE per-type map. The
// caller is responsible for completeness (RFC-236 §5.2); this type cannot check
// it, because it does not know the schema.
func NewCollectedStatistics(perType map[string]float64) CollectedStatistics {
	cp := make(map[string]float64, len(perType))
	total := 0.0
	for name, c := range perType {
		// A zero or negative count is not a small table; it is a collector bug
		// or a corrupt entry. Clamp to 1 rather than 0: a zero-cardinality leaf
		// makes every cost above it zero, which does not rank a plan, it
		// flattens the whole comparison.
		if c < 1 {
			c = 1
		}
		cp[name] = c
		total += c
	}
	return CollectedStatistics{perType: cp, total: total}
}

// RecordTypeCardinality returns the collected count for name.
//
// For the EMPTY name — an unknown-type leaf — it returns the whole store. For a
// name that is not in the map at all it also returns the whole store rather
// than LeafScanCardinality: an over-estimate on the store's own scale is the
// safe direction, where the 1e6 default is an over-estimate on nobody's scale.
func (s CollectedStatistics) RecordTypeCardinality(name string) float64 {
	if name == "" {
		return s.storeTotal()
	}
	if c, ok := s.perType[name]; ok {
		return c
	}
	return s.storeTotal()
}

// storeTotal is the sum over every known type, floored at LeafScanCardinality's
// smallest sensible value so an empty schema cannot produce a zero-cost plan.
func (s CollectedStatistics) storeTotal() float64 {
	if s.total < 1 {
		return 1
	}
	return s.total
}
