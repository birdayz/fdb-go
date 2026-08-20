package recordlayer

import (
	"fmt"

	gen "fdb.dev/gen"
)

// RowNumberWindowDirection is the sort direction of a sliding window's
// ordering field. Matches Java's
// IndexPredicate.RowNumberWindowPredicate.Direction.
type RowNumberWindowDirection int

const (
	// RowNumberWindowAscending keeps the SMALLEST ordering values — the rows
	// with the lowest row numbers in ascending order.
	RowNumberWindowAscending RowNumberWindowDirection = iota
	// RowNumberWindowDescending keeps the LARGEST ordering values.
	RowNumberWindowDescending
)

func (d RowNumberWindowDirection) String() string {
	switch d {
	case RowNumberWindowAscending:
		return "ASC"
	case RowNumberWindowDescending:
		return "DESC"
	default:
		return fmt.Sprintf("RowNumberWindowDirection(%d)", int(d))
	}
}

// RowNumberWindowSpec is the Go form of Java's
// IndexPredicate.RowNumberWindowPredicate: the declaration
// `QualifyRowNumber(orderingField, direction) <= size`, optionally
// `PARTITION BY partitionFieldPaths`.
//
// It is a WINDOWING SPEC, not a row filter. Go's Index.Predicate is a
// `func(proto.Message) bool` that answers "does this record belong in the
// index"; a row-number window cannot be answered per-record, because the
// answer depends on every other record in the partition. So the compiled
// Index.Predicate for this arm accepts every record (matching Java's
// RowNumberWindowPredicate.shouldIndexThisRecord, IndexPredicate.java:739-742)
// and the actual windowing lives in the maintainer, which reads the spec back
// off the stored proto through RowNumberWindowPredicateOf.
type RowNumberWindowSpec struct {
	// OrderingField is the field PATH of the ordering key, outermost first.
	OrderingField []string
	// Direction selects MIN (ASC) or MAX (DESC) extremum semantics.
	Direction RowNumberWindowDirection
	// Size is the window's N — how many records the index keeps per partition.
	Size int
	// PartitionFieldPaths are the PARTITION BY field paths, each outermost
	// first. Empty means unpartitioned.
	PartitionFieldPaths [][]string
}

// OrderingKey builds the KeyExpression for the ordering field.
// Matches Java's RowNumberWindowPredicate.getOrderingKey().
func (s *RowNumberWindowSpec) OrderingKey() (KeyExpression, error) {
	return fieldPathToKeyExpression(s.OrderingField)
}

// PartitionKey builds the KeyExpression for the partition key, or nil when the
// window is unpartitioned. Multiple partition paths are concatenated in
// declaration order.
// Matches Java's RowNumberWindowPredicate.getPartitionKey().
func (s *RowNumberWindowSpec) PartitionKey() (KeyExpression, error) {
	if len(s.PartitionFieldPaths) == 0 {
		return nil, nil
	}
	exprs := make([]KeyExpression, 0, len(s.PartitionFieldPaths))
	for _, path := range s.PartitionFieldPaths {
		e, err := fieldPathToKeyExpression(path)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, e)
	}
	if len(exprs) == 1 {
		return exprs[0], nil
	}
	return Concat(exprs...), nil
}

// PartitionKeyColumnSize returns the total number of columns the partition key
// produces, or 0 when unpartitioned.
// Matches Java's RowNumberWindowPredicate.getPartitionKeyColumnSize().
func (s *RowNumberWindowSpec) PartitionKeyColumnSize() (int, error) {
	pk, err := s.PartitionKey()
	if err != nil {
		return 0, err
	}
	if pk == nil {
		return 0, nil
	}
	return pk.ColumnSize(), nil
}

func (s *RowNumberWindowSpec) String() string {
	out := "QualifyRowNumber("
	if len(s.PartitionFieldPaths) > 0 {
		out += "PARTITION BY "
		for i, p := range s.PartitionFieldPaths {
			if i > 0 {
				out += ", "
			}
			out += joinFieldPath(p)
		}
		out += " ORDER BY "
	}
	out += joinFieldPath(s.OrderingField)
	return fmt.Sprintf("%s, %s) <= %d", out, s.Direction, s.Size)
}

func joinFieldPath(path []string) string {
	out := ""
	for i, p := range path {
		if i > 0 {
			out += "."
		}
		out += p
	}
	return out
}

// fieldPathToKeyExpression turns ["a","b","c"] into Nest("a", Nest("b", Field("c"))).
// Matches Java's RowNumberWindowPredicate.fieldPathToKeyExpression().
func fieldPathToKeyExpression(path []string) (KeyExpression, error) {
	if len(path) == 0 {
		return nil, &MetaDataError{Message: "row-number window field path is empty"}
	}
	result := Field(path[len(path)-1])
	for i := len(path) - 2; i >= 0; i-- {
		result = Nest(path[i], result)
	}
	return result, nil
}

// rowNumberWindowSpecFromProto reads a RowNumberWindowSpec off its stored proto.
// Matches Java's RowNumberWindowPredicate(RecordMetaDataProto.RowNumberWindowPredicate).
//
// `direction` and `size` are both proto2 REQUIRED fields, so no message that
// came off the wire can be missing them — protobuf refuses to parse one that
// is. An in-memory message can be, though, and the nil-safe getters fail OPEN
// there: GetDirection() on an absent field returns the first declared enum
// value, ASC. ASC versus DESC is the difference between evicting the smallest
// and evicting the largest, so absence is refused rather than defaulted.
//
// Java throws on any direction value it does not recognise
// (IndexPredicate.java:657-673); Go refuses absence as well, for the reason
// above.
func rowNumberWindowSpecFromProto(p *gen.RowNumberWindowPredicate) (*RowNumberWindowSpec, error) {
	if p == nil {
		return nil, &MetaDataError{Message: "nil row-number window predicate"}
	}
	if p.Direction == nil {
		return nil, &MetaDataError{Message: "row-number window predicate has no direction; " +
			"ASC and DESC evict from opposite ends of the window, so it must be stated"}
	}
	if p.Size == nil {
		return nil, &MetaDataError{Message: "row-number window predicate has no size"}
	}
	spec := &RowNumberWindowSpec{
		OrderingField: append([]string(nil), p.GetOrderingField()...),
		Size:          int(p.GetSize()),
	}
	switch p.GetDirection() {
	case gen.RowNumberWindowPredicate_ASC:
		spec.Direction = RowNumberWindowAscending
	case gen.RowNumberWindowPredicate_DESC:
		spec.Direction = RowNumberWindowDescending
	default:
		return nil, &MetaDataError{Message: fmt.Sprintf(
			"unknown RowNumberWindowPredicate direction %v", p.GetDirection())}
	}
	for _, fp := range p.GetPartitionFields() {
		spec.PartitionFieldPaths = append(spec.PartitionFieldPaths,
			append([]string(nil), fp.GetField()...))
	}
	return spec, nil
}

// findRowNumberWindowPredicateProto searches a stored predicate tree for a
// row-number window arm, recursing through AND.
// Matches Java's
// SlidingWindowIndexMaintainerFactory.findRowNumberWindowPredicate().
//
// This is the FACTORY's lookup — the one that decides whether an index gets
// decorated at all. It is deliberately NOT the same shape as the MAINTAINER's
// lookup below; see qualifyRowNumberWindowPredicateProto.
//
// AND IS TESTED FIRST, and that order is load-bearing rather than stylistic.
// Java searches the PARSED predicate, so it can only ever see the arm
// IndexPredicate.fromProto chose — and fromProto tests hasAndPredicate before
// hasRowNumberWindowPredicate (IndexPredicate.java:106-118). Go searches the
// RAW proto, where a malformed message can carry both arms at once, so the
// order has to be restated here or the two engines pick different arms from the
// same bytes: Go would decorate from a row-window arm that its own compiled
// evaluator (predicateFromProto, same dispatch order) ignores, and the resulting
// keyspace-10 and HNSW contents would not be Java's.
//
// The same discipline is written down at
// cascades.constantPredicateArmIsTrue: answer only for the arm the evaluator
// would actually run.
func findRowNumberWindowPredicateProto(p *gen.Predicate) *gen.RowNumberWindowPredicate {
	if p == nil {
		return nil
	}
	if and := p.GetAndPredicate(); and != nil {
		for _, child := range and.GetChildren() {
			if found := findRowNumberWindowPredicateProto(child); found != nil {
				return found
			}
		}
		return nil
	}
	return activeRowNumberWindowArm(p)
}

// activeRowNumberWindowArm returns the row-window arm of a predicate message
// only when it is the arm predicateFromProto would actually EVALUATE.
//
// The message declares "exactly one of the following" and a malformed one can
// set several; predicateFromProto dispatches AND, OR, constant, NOT, value and
// only then row-window, so any of those set alongside a row-window arm shadows
// it. Java is never exposed to the question because it searches the PARSED
// predicate, where the shadowing already happened in fromProto — the precedence
// has to be restated wherever Go reads the raw proto, or the two engines read
// different declarations out of identical bytes.
//
// AND is deliberately NOT handled here: its children have to be searched, which
// is the caller's business and differs between the factory's recursive search
// and the maintainer's one-level lookup. Every caller tests AND first.
func activeRowNumberWindowArm(p *gen.Predicate) *gen.RowNumberWindowPredicate {
	if p == nil {
		return nil
	}
	if p.GetAndPredicate() != nil || p.GetOrPredicate() != nil ||
		p.GetConstantPredicate() != nil || p.GetNotPredicate() != nil ||
		p.GetValuePredicate() != nil {
		return nil
	}
	return p.GetRowNumberWindowPredicate()
}

// qualifyRowNumberWindowPredicateProto is the MAINTAINER's lookup — Java's
// SlidingWindowIndexMaintainer.getQualifyPredicate() (:212-227).
//
// It looks only at the root and at the IMMEDIATE children of a root AND; it
// does not recurse. That is narrower than the factory's search above, and the
// difference is Java's, not an oversight to be tidied up: for a predicate like
// AND(AND(rowWindow)) the factory decorates the index and this lookup then
// FAILS, which is the behaviour a Java store would exhibit. Widening it here
// would make Go accept metadata Java refuses.
//
// AND is tested first for the same reason as in the factory's search above: it
// is the arm predicateFromProto would evaluate, and answering from a shadowed
// one would qualify the window with a declaration the evaluator ignores.
func qualifyRowNumberWindowPredicateProto(p *gen.Predicate) *gen.RowNumberWindowPredicate {
	if p == nil {
		return nil
	}
	if and := p.GetAndPredicate(); and != nil {
		for _, child := range and.GetChildren() {
			// Java's getQualifyPredicate inspects the immediate children with a
			// bare instanceof and does not recurse — but each child is a PARSED
			// predicate there, so a child that Java parsed as an OR is simply
			// not a RowNumberWindowPredicate. Reading the raw proto, the same
			// child can carry both arms, so the child's own precedence has to be
			// resolved rather than its row-window arm read directly.
			if rn := activeRowNumberWindowArm(child); rn != nil {
				return rn
			}
		}
		return nil
	}
	return activeRowNumberWindowArm(p)
}

// HasRowNumberWindowPredicate reports whether this index declares a row-number
// window anywhere on a conjunctive path in its stored predicate. Answers only
// from the SERIALIZED predicate: a programmatic Go predicate is an opaque
// closure and cannot declare a window.
// Matches the predicate half of Java's
// SlidingWindowIndexMaintainerFactory.isSlidingWindowIndex().
func (idx *Index) HasRowNumberWindowPredicate() bool {
	if idx == nil {
		return false
	}
	return findRowNumberWindowPredicateProto(idx.predicateProto) != nil
}

// RowNumberWindowSpec returns the window declaration the MAINTAINER will use,
// or an error when the index carries no usable one. Uses Java's narrower
// maintainer-side lookup — see qualifyRowNumberWindowPredicateProto.
func (idx *Index) RowNumberWindowSpec() (*RowNumberWindowSpec, error) {
	if idx == nil {
		return nil, &MetaDataError{Message: "sliding window index requires a RowNumberWindowPredicate"}
	}
	rn := qualifyRowNumberWindowPredicateProto(idx.predicateProto)
	if rn == nil {
		return nil, &MetaDataError{Message: fmt.Sprintf(
			"sliding window index requires a RowNumberWindowPredicate (index %s)", idx.Name)}
	}
	return rowNumberWindowSpecFromProto(rn)
}

// validateRowNumberWindowPlacement checks that a row-number window arm only
// appears on a pure conjunctive (AND-only) path from the root — never below an
// OR or a NOT.
// Matches Java's IndexPredicate.validateRowNumberWindowPlacement() (:241-246).
//
// The constraint is not cosmetic. The maintainer keeps exactly the rows the
// window admits, so a window reached only on one branch of a disjunction would
// describe an index whose contents depend on a choice the maintainer never
// makes.
func validateRowNumberWindowPlacement(p *gen.Predicate) error {
	if p == nil {
		return nil
	}
	if !rowNumberWindowValidInConjunctivePath(p) {
		return &MetaDataError{Message: "RowNumberWindowPredicate must not appear under a disjunction (OR)"}
	}
	return nil
}

// rowNumberWindowValidInConjunctivePath is Java's isValidInConjunctivePath
// (IndexPredicate.java:247-268).
// The arms are tested in predicateFromProto's ORDER — composites first — not in
// Java's source order. Java's version switches on the PARSED predicate, where
// only one arm can exist; reading the raw proto, a malformed-but-decodable
// message can set several, and answering from a shadowed one accepts a
// placement Java rejects (or rejects one it accepts). Same precedence rule as
// activeRowNumberWindowArm, applied to the whole tree rather than one node.
func rowNumberWindowValidInConjunctivePath(p *gen.Predicate) bool {
	switch {
	case p == nil:
		return true
	case p.GetAndPredicate() != nil:
		for _, c := range p.GetAndPredicate().GetChildren() {
			if !rowNumberWindowValidInConjunctivePath(c) {
				return false
			}
		}
		return true
	case p.GetOrPredicate() != nil:
		// Under an OR, no row-number window is allowed anywhere below.
		for _, c := range p.GetOrPredicate().GetChildren() {
			if !rowNumberWindowAbsent(c) {
				return false
			}
		}
		return true
	case p.GetNotPredicate() != nil:
		return rowNumberWindowAbsent(p.GetNotPredicate().GetChild())
	case p.GetConstantPredicate() != nil, p.GetValuePredicate() != nil:
		return true
	case p.GetRowNumberWindowPredicate() != nil:
		return true
	default:
		return true
	}
}

// rowNumberWindowAbsent is Java's hasNoRowNumberWindow
// (IndexPredicate.java:270-289) — note the Java name states the opposite of
// what it returns; this one is named for its return value.
// Arms are tested in predicateFromProto's order, for the reason given on
// rowNumberWindowValidInConjunctivePath.
func rowNumberWindowAbsent(p *gen.Predicate) bool {
	switch {
	case p == nil:
		return true
	case p.GetAndPredicate() != nil:
		for _, c := range p.GetAndPredicate().GetChildren() {
			if !rowNumberWindowAbsent(c) {
				return false
			}
		}
		return true
	case p.GetOrPredicate() != nil:
		for _, c := range p.GetOrPredicate().GetChildren() {
			if !rowNumberWindowAbsent(c) {
				return false
			}
		}
		return true
	case p.GetNotPredicate() != nil:
		return rowNumberWindowAbsent(p.GetNotPredicate().GetChild())
	case p.GetConstantPredicate() != nil, p.GetValuePredicate() != nil:
		return true
	case p.GetRowNumberWindowPredicate() != nil:
		return false
	default:
		return true
	}
}
