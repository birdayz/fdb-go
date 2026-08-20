package cascades

import (
	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// THE INVARIANT, for this function and every caller of it: normalization may
// only ERASE a predicate it can PROVE accepts every record. Anything it cannot
// prove — an arm it does not understand, a malformed message, a child it cannot
// compile — must stay VISIBLE to the sparseness gates, because those gates read
// absence as "this index holds everything". Erasing on doubt turns a partial
// index into a full one and a scan of it into wrong rows; keeping a predicate
// that was in fact harmless costs at worst a declined candidate.
//
// NormalizeIndexPredicateProto applies Java's predicate CONSTRUCTION rules to a
// stored index predicate, returning the equivalent predicate Java would have
// built. It is the single authority on that question: the planner (deciding
// whether a candidate carries a predicate to account for) and the executor
// (deciding whether an index is complete enough to scan) both classify through
// it, so they cannot answer differently about the same stored bytes.
//
// Java never classifies a conjunction of tautologies, because it never
// constructs one. IndexPredicate.AndPredicate.toPredicate delegates to
// AndPredicate.and (IndexPredicate.java:344-345), and that constructor drops
// every tautological conjunct, returns ConstantPredicate.TRUE when none remain,
// and unwraps a lone survivor (AndPredicate.java:188-206). `TRUE AND TRUE` has
// therefore already become the constant before anything asks whether it filters.
//
// OR IS NOT THE DUAL, and deliberately so. OrPredicate.or delegates to
// OrPredicate.of (OrPredicate.java:417-445), which only collapses a
// single-element disjunction — it does no tautology folding — and OrPredicate
// never overrides QueryPredicate.isTautology (default false,
// QueryPredicate.java:311). So `TRUE OR x` is NOT a tautology to Java, and
// folding it here would make Go's completeness proof WIDER than Java's: Go would
// scan an index Java treats as filtering. The asymmetry is Java's, not an
// oversight to be tidied up.
//
// A ROW-NUMBER WINDOW arm is likewise never folded, and that is a DIVERGENCE
// from Java's conversion, taken deliberately. Java's
// RowNumberWindowPredicate.toPredicate returns ConstantPredicate.TRUE
// (IndexPredicate.java:770-772) — but that states only that the constraint is
// not expressible as a QueryPredicate over a record, NOT that it filters
// nothing. It filters maximally: `QualifyRowNumber(score, DESC) <= 100` "keeps
// the 100 records with the highest score values in the index"
// (IndexPredicate.java:608-619). Folding it to the constant here would classify
// a top-N index as COMPLETE and let a scan serve it as the whole table.
//
// Leaving it unfolded keeps it VISIBLE to the sparseness gates, and that alone
// turned out not to be enough: the flat expansion converts the predicate and
// omits a tautological result, so a row-window arm that reached
// indexPredicateToQueryPredicate was mapped to TRUE and dropped, and the
// candidate matched as full whether the arm arrived bare or wrapped in an AND.
// The conversion therefore REFUSES the arm outright, which excludes the
// candidate.
//
// That refusal is LOAD-BEARING rather than belt-and-braces: a store can carry
// such an index, because the record layer maintains one — a vector index whose
// stored predicate declares the window is decorated by
// slidingWindowIndexMaintainer, which keeps only the qualifying records in the
// wrapped HNSW graph. The arm's compiled per-record evaluator answers `true`
// for every record (Java's shouldIndexThisRecord), and it is exactly that
// answer which must never reach the completeness question here.
//
// The result is always a deep clone; the input is never mutated and the two
// trees share no nodes.
func NormalizeIndexPredicateProto(p *gen.Predicate) *gen.Predicate {
	if p == nil {
		return nil
	}
	// Normalize only a message whose shape is unambiguous. predicateFromProto
	// evaluates the FIRST arm it finds, so rewriting one arm of a multi-arm
	// message could produce a predicate the evaluator never runs.
	if indexPredicateProtoArmCount(p) != 1 {
		return proto.Clone(p).(*gen.Predicate)
	}
	// A nil child makes the message malformed, and malformed is not provably
	// true. Normalizing through it would collapse AND(nil) / OR(nil) — a
	// singleton whose only child normalizes to nil — to nil itself, which the
	// candidate boundary reads as "no predicate" and every gate reads as a FULL
	// index. Handing the message back intact keeps it non-nil for the gates and
	// lets the conversion reject it, which excludes the candidate.
	if indexPredicateProtoHasNilChild(p) {
		return proto.Clone(p).(*gen.Predicate)
	}
	switch {
	case p.AndPredicate != nil:
		kept := make([]*gen.Predicate, 0, len(p.AndPredicate.Children))
		for _, child := range p.AndPredicate.Children {
			normalized := NormalizeIndexPredicateProto(child)
			// A conjunct that provably cannot reject a record contributes
			// nothing to the conjunction. Java's `and` keeps Placeholders even
			// when tautological, for the index-matching machinery; a stored
			// index predicate has no placeholder arm, so there is nothing to
			// except here.
			if constantPredicateArmIsTrue(normalized) {
				continue
			}
			kept = append(kept, normalized)
		}
		switch len(kept) {
		case 0:
			return &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
				Value: gen.ConstantPredicate_TRUE.Enum(),
			}}
		case 1:
			return kept[0]
		default:
			return &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: kept}}
		}
	case p.OrPredicate != nil:
		kept := make([]*gen.Predicate, 0, len(p.OrPredicate.Children))
		for _, child := range p.OrPredicate.Children {
			kept = append(kept, NormalizeIndexPredicateProto(child))
		}
		// Java's `of` collapses a singleton and rejects an empty disjunction.
		// An empty one cannot be reconstructed into anything safe, so it is
		// handed back as-is and fails closed as a filtering predicate.
		if len(kept) == 1 {
			return kept[0]
		}
		if len(kept) == 0 {
			return proto.Clone(p).(*gen.Predicate)
		}
		return &gen.Predicate{OrPredicate: &gen.OrPredicate{Children: kept}}
	default:
		return proto.Clone(p).(*gen.Predicate)
	}
}

// IndexPredicateProtoIsTautology reports whether a stored index predicate
// provably rejects no record — i.e. whether the index it guards holds an entry
// for every record. Normalize the way Java's constructors do, then apply Java's
// narrow tautology test to the result.
func IndexPredicateProtoIsTautology(p *gen.Predicate) bool {
	return constantPredicateArmIsTrue(NormalizeIndexPredicateProto(p))
}

// indexPredicateProtoHasNilChild reports whether any child slot of a composite
// arm is nil. Only the immediate children are checked; a deeper nil is caught
// when the recursion reaches its parent.
func indexPredicateProtoHasNilChild(p *gen.Predicate) bool {
	if p.AndPredicate != nil {
		for _, c := range p.AndPredicate.Children {
			if c == nil {
				return true
			}
		}
	}
	if p.OrPredicate != nil {
		for _, c := range p.OrPredicate.Children {
			if c == nil {
				return true
			}
		}
	}
	return p.NotPredicate != nil && p.NotPredicate.Child == nil
}

// indexPredicateProtoArmCount counts the set arms of a predicate message.
// Exactly one is the well-formed case; anything else is ambiguous input.
func indexPredicateProtoArmCount(p *gen.Predicate) int {
	n := 0
	for _, set := range []bool{
		p.AndPredicate != nil,
		p.OrPredicate != nil,
		p.ConstantPredicate != nil,
		p.NotPredicate != nil,
		p.ValuePredicate != nil,
		p.RowNumberWindowPredicate != nil,
	} {
		if set {
			n++
		}
	}
	return n
}

// constantPredicateArmIsTrue is the narrow classifier Java's
// QueryPredicate.isTautology() is: only ConstantPredicate.TRUE overrides the
// default `false` (ConstantPredicate.java:98 vs QueryPredicate.java:311). It
// answers about the message AS GIVEN and folds nothing — callers normalize
// first.
//
// The arm-presence test is not redundant with the value test:
// ConstantPredicate.value is a proto2 field whose declared DEFAULT is TRUE, so
// GetValue() on an ABSENT arm returns TRUE. The nil-safe getters fail OPEN
// here, not closed, which is why the arm is checked explicitly — and why
// normalization above must only fold children that carry the arm.
func constantPredicateArmIsTrue(p *gen.Predicate) bool {
	if p == nil {
		return false
	}
	// Dispatch in predicateFromProto's order and answer only for the arm that
	// function would actually evaluate. A message carrying several arms is
	// evaluated by the FIRST one, so reading the constant arm out of turn would
	// prove a tautology about a predicate the evaluator never runs.
	switch {
	case p.AndPredicate != nil, p.OrPredicate != nil:
		return false
	case p.ConstantPredicate != nil:
		return p.ConstantPredicate.GetValue() == gen.ConstantPredicate_TRUE
	default:
		return false
	}
}
