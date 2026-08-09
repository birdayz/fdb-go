package plans

// IndexScanCarrier is the plan-shaped answer to a hazard RFC-220 created
// structurally: a covering index scan HOLDS its index scan as a plain struct
// field, GetChildren returns nil, and so `plans.Walk` never descends into it.
// Every `case *RecordQueryIndexPlan` / `.(*RecordQueryIndexPlan)` written
// before RFC-220 is therefore blind to a covering scan BY CONSTRUCTION, and
// the access path now emits Fetch(Covering(IndexScan)) for every index-backed
// access — so the blindness is the ordinary case, not an exotic one.
//
// The failure mode is what makes this a type rather than a convention. A miss
// does not raise; it answers "there is no index scan here", which downstream
// reads as "no comparison ranges", i.e. as an UNRESTRICTED scan — the
// expensive and, at two sites already found, the WRONG direction (an unstamped
// RecordConstructorValue evaluating name-keyed; a nil probed outer type
// licensing a name-keyed binding the probe exists to forbid).
//
// The method set is deliberately ONE method. Everything a blind site was
// reading — scan comparisons, key component types, column names, reverseness —
// is a fact ABOUT THE INDEX SCAN, and the covering plan answers each by
// delegating to the very scan this returns. Restating those facts on the
// interface would create a second surface that must be kept in step with the
// first, and the day they drift the miss is silent again. One accessor, and
// every fact read off the thing it returns, keeps them in step by
// construction.
//
// The interface is SEALED by an unexported method. Both implementations live
// in this package, and sealing is not decoration: RecordQueryAggregateIndexPlan
// also carries a `GetIndexPlan() *RecordQueryIndexPlan` and would otherwise
// satisfy this interface structurally — while emitting one row per GROUP, not
// one row per entry. A caller asking "what index scan does this node read
// entries from" and silently getting an aggregate plan's inner would be the
// same class of quiet wrong answer this type exists to remove.
type IndexScanCarrier interface {
	RecordQueryPlan

	// GetIndexPlan returns the index scan this node reads entries from. For a
	// bare index scan that is the node itself; for a covering scan it is the
	// wrapped field.
	GetIndexPlan() *RecordQueryIndexPlan

	// indexScanCarrier seals the interface to this package — see the type
	// comment for why structural satisfaction is a hazard here.
	indexScanCarrier()
}

var (
	_ IndexScanCarrier = (*RecordQueryIndexPlan)(nil)
	_ IndexScanCarrier = (*RecordQueryCoveringIndexPlan)(nil)
)

// GetIndexPlan returns the receiver: a bare index scan IS the index scan it
// reads entries from. Having the bare plan answer the same question as the
// covering wrapper is the whole point — it lets a caller be written once,
// against the carrier, instead of twice against the two concrete types.
func (p *RecordQueryIndexPlan) GetIndexPlan() *RecordQueryIndexPlan { return p }

func (p *RecordQueryIndexPlan) indexScanCarrier() {}

func (p *RecordQueryCoveringIndexPlan) indexScanCarrier() {}

// IndexPlanOf returns the index scan node reads entries from, seeing through a
// covering wrapper, and reports whether node is an index scan at all.
//
// This is the exported form of a helper that had been written twice as an
// unexported test helper in two different packages (`indexScanOf` in
// package embedded, `indexScanOfNode` in package sqldriver_test) precisely
// because there was no importable symbol to share. It is exported for that
// reason as much as for use in the tree: a guard against a structural blindness
// that cannot be shared is a guard that gets re-derived, and re-derived wrong.
//
// A carrier whose inner scan is nil reports FALSE rather than (nil, true). The
// constructors never produce one, but a struct-literal plan can, and a caller
// that gets ok=true proceeds to dereference — turning a malformed test fixture
// into a nil panic far from its cause instead of a clean "not an index scan".
func IndexPlanOf(node RecordQueryPlan) (*RecordQueryIndexPlan, bool) {
	carrier, ok := node.(IndexScanCarrier)
	if !ok {
		return nil, false
	}
	inner := carrier.GetIndexPlan()
	if inner == nil {
		return nil, false
	}
	return inner, true
}
