package ddl

import (
	"math"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// generatePredicate is the sparse-index (WHERE) arm — the Go port of
// getTopLevelPredicate (MaterializedViewIndexGenerator.java:640-682) plus the
// IndexPredicate.fromQueryPredicate(...).toProto() serialization the caller
// applies to its result (:169-172).
//
// Java walks the reversed topological expression list: an optional leading
// sort, then either the top-level select (whose predicates are the WHERE) or
// a select-having / group-by / select-where sandwich in which the
// select-having must carry no predicates (:651) and the select-where's
// predicates are the WHERE. Go's decomposed plan holds the same facts
// directly: the HAVING lives on the LogicalAggregate (HasHaving), and the
// WHERE is the one LogicalFilter below the aggregate. Inner selects with
// predicates (:659-664) cannot reach this generator — decompose already
// rejects every multi-quantifier shape with the type-filter error.
//
// The pipeline (:669-681): AND the select's predicates (Go's filter carries
// the conjunction as one tree already), DNF-normalize WITHOUT simplification
// (:675 calls normalize, not normalizeAndSimplify — absorption would change
// the stored bytes), require IndexPredicate.isSupported (:676), and store the
// DNF only when it expands to ranges — otherwise the NON-normalized
// conjunction (:677-679), a wire-visible branch.
func generatePredicate(d *decomposed, res storageNames) (*gen.Predicate, error) {
	if d.aggregate != nil && d.aggregate.HasHaving {
		// Java's exact message (MaterializedViewIndexGenerator.java:651).
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, found predicate in select-having")
	}
	f := d.filter
	if f == nil {
		return nil, nil
	}
	conjunction := f.Predicate
	if conjunction == nil {
		// A text-only filter means the resolver declined the WHERE's shape.
		// Building the index anyway would silently drop the predicate — a
		// FULL index holding entries Java's sparse index omits. Fail closed
		// with Java's unsupported-predicate message shape (:676).
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported predicate '%s'", f.PredicateText)
	}
	result := conjunction
	if norm, changed := cascades.NormalizeDNFWithoutSimplification(
		conjunction, cascades.NormalizerDefaultSizeLimit); changed {
		result = norm
	}
	if !indexPredicateIsSupported(result) {
		// Java's message (:676); the rendering of %s is Go's Explain.
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported predicate '%s'", result.Explain())
	}
	if !dnfPredicateHasRanges(result) {
		// IndexPredicateExpansion.dnfPredicateToRanges came back empty: the
		// DNF is not a union of per-value ranges, so Java stores the
		// NON-normalized conjunction instead (:677-679).
		result = conjunction
	}
	return indexPredicateToProto(result, res)
}

// indexPredicateIsSupported ports IndexPredicate.isSupported
// (IndexPredicate.java:212-232) over Go's predicate types. Go's
// ComparisonPredicate carries what Java's ValuePredicate carries — a value
// and a comparison — so it takes that arm: the value must be a fully-named
// FieldValue (getFieldPathNamesMaybe all present) and the comparison one the
// IndexComparison PoJo hierarchy can represent. The RowNumberWindowPredicate
// arm (:223-225) is not constructed by Go DDL (the QUALIFY window form is out
// of scope, RFC-202 §10) and Go's walker never produces the value shape it
// matches.
func indexPredicateIsSupported(p predicates.QueryPredicate) bool {
	switch q := p.(type) {
	case *predicates.ConstantPredicate:
		return true
	case *predicates.NotPredicate:
		return indexPredicateIsSupported(q.Child)
	case *predicates.AndPredicate:
		for _, c := range q.SubPredicates {
			if !indexPredicateIsSupported(c) {
				return false
			}
		}
		return true
	case *predicates.OrPredicate:
		for _, c := range q.SubPredicates {
			if !indexPredicateIsSupported(c) {
				return false
			}
		}
		return true
	case *predicates.ComparisonPredicate:
		fv, ok := values.AsFieldValue(q.Operand)
		if !ok {
			return false
		}
		accessors, ok := fieldAccessors(fv)
		if !ok {
			return false
		}
		for _, acc := range accessors {
			if acc.name == "" {
				return false
			}
		}
		if !indexComparisonIsSupported(q.Comparison) {
			return false
		}
		// A literal STRICTLY WIDER than the column is Java's promoted-COLUMN
		// shape: the planner wraps the column in promote(col AS <wider>), a
		// PromoteValue, and `getValue() instanceof FieldValue` fails
		// (IndexPredicate.java:227) — `WHERE int_col < 5000000000` is
		// REJECTED, measured against the live JVM (the D11 spec's
		// i_sparse_int_big shape). Go's plan tree carries no promote node, so
		// the same shape is detected from the literal/column widths.
		if lit, hasLit := comparisonLiteralOperand(q.Comparison); hasLit {
			if _, rejected, _ := promoteLiteralToColumn(lit, fv.ResultType()); rejected {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// indexComparisonIsSupported ports IndexComparison.isSupported
// (IndexComparison.java:86-96): the comparison must be representable as a
// SimpleComparison or a NullComparison PoJo. In Go terms: the unary null
// checks map to NullComparison; a binary comparison maps to SimpleComparison
// only when its comparand is a plan-time literal (Java's relational front end
// evaluates the constant RHS into a SimpleComparison comparand,
// RelOpValue.java:237-241; a field or parameter RHS arrives as a
// ValueComparison / ParameterComparison, which the PoJo hierarchy rejects).
// IN has no PoJo form (Java's ListComparison), and the comparison kinds
// Java's SimpleComparison serializer cannot store (LIKE, the DISTINCT-FROM
// pair, TEXT_*, SORT, the distance ranks — IndexComparison.java:221-249's
// switch) are rejected here rather than at serialization so the caller gets
// the clean unsupported-predicate error.
func indexComparisonIsSupported(c predicates.Comparison) bool {
	if c.Type.IsUnary() {
		return c.Type == predicates.ComparisonIsNull || c.Type == predicates.ComparisonIsNotNull
	}
	switch c.Type {
	case predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq,
		predicates.ComparisonStartsWith:
	default:
		return false
	}
	if c.ParameterName != "" {
		return false
	}
	_, ok := comparisonLiteralOperand(c)
	return ok
}

// promoteLiteralToColumn applies Java's comparison-promotion lattice to a
// plan-time literal comparand against its column's type.
//
// Java's planner promotes the NARROWER side of a comparison
// (int → long → float → double). When the literal is the narrower side it is
// promoted and the stored SimpleComparison operand carries the COLUMN's type;
// when the COLUMN is the narrower side the planner wraps it in
// promote(col AS <wider>) — a PromoteValue, which IndexPredicate.isSupported
// rejects (IndexPredicate.java:227), failing the whole index definition.
//
// Returns (converted, rejected, applied): rejected=true is the
// promoted-column shape; applied=true means converted carries the literal in
// the column's Go representation (int32 / int64 / float32 / float64).
// Non-numeric literals and unknown column types pass through untouched —
// their comparisons are same-typed or already rejected elsewhere.
func promoteLiteralToColumn(lit any, columnType values.Type) (any, bool, bool) {
	if columnType == nil {
		return lit, false, false
	}
	// The literal's own Java rank: ParseHelpers.parseDecimal narrows an
	// in-int-range decimal literal to Integer (ParseHelpers.java:96-101);
	// a float literal parses Double.
	var litRank int
	switch v := lit.(type) {
	case int64:
		litRank = 2
		if v >= math.MinInt32 && v <= math.MaxInt32 {
			litRank = 1
		}
	case float64:
		litRank = 4
	default:
		return lit, false, false
	}
	var colRank int
	switch columnType.Code() {
	case values.TypeCodeInt:
		colRank = 1
	case values.TypeCodeLong:
		colRank = 2
	case values.TypeCodeFloat:
		colRank = 3
	case values.TypeCodeDouble:
		colRank = 4
	default:
		return lit, false, false
	}
	if litRank > colRank {
		return lit, true, false
	}
	switch columnType.Code() {
	case values.TypeCodeInt:
		if n, ok := lit.(int64); ok {
			return int32(n), false, true
		}
	case values.TypeCodeLong:
		return lit, false, false
	case values.TypeCodeFloat:
		if n, ok := lit.(int64); ok {
			return float32(n), false, true
		}
	case values.TypeCodeDouble:
		switch n := lit.(type) {
		case int64:
			return float64(n), false, true
		case float64:
			return n, false, false
		}
	}
	return lit, false, false
}

// comparisonLiteralOperand extracts the plan-time literal comparand of a
// binary comparison, mirroring the state Java's SimpleComparison carries
// after RelOpValue evaluated the constant RHS (RelOpValue.java:237-241).
func comparisonLiteralOperand(c predicates.Comparison) (any, bool) {
	switch v := c.Operand.(type) {
	case *values.ConstantValue:
		return v.Value, v.Value != nil
	case *values.BooleanValue:
		if v.Value == nil {
			return nil, false
		}
		return *v.Value, true
	default:
		return nil, false
	}
}

// dnfPredicateHasRanges ports IndexPredicateExpansion.dnfPredicateToRanges
// (IndexPredicateExpansion.java:52-114) reduced to the fact the generator
// consumes: whether the DNF expands to a non-empty value→range multimap. A
// successful conversion always yields one range per conjunction group, so
// "converted" and "non-empty" coincide; Optional.empty() — any group that is
// not a single-value conjunction of scan-prefix comparisons — maps to false.
func dnfPredicateHasRanges(p predicates.QueryPredicate) bool {
	if or, ok := p.(*predicates.OrPredicate); ok {
		for _, group := range or.SubPredicates {
			if !conjunctionHasRange(group) {
				return false
			}
		}
		return true
	}
	return conjunctionHasRange(p)
}

// conjunctionHasRange is conjunctionToRange (IndexPredicateExpansion.java:71-114)
// for one DNF group: every term a ValuePredicate (Go: ComparisonPredicate)
// over ONE value (semanticEquals in Java, structural equality here — the same
// identity the generator uses throughout), every comparison admissible in a
// scan prefix (RangeConstraints.Builder.addComparisonMaybe's gate). Java's
// "invalid predicate" throw (:109) — a comparison the builder admitted whose
// built range is nonetheless empty — is unreachable in the Go port because
// admission and emptiness coincide.
func conjunctionHasRange(group predicates.QueryPredicate) bool {
	b := predicates.NewRangeConstraintsBuilder()
	if and, ok := group.(*predicates.AndPredicate); ok {
		var key values.Value
		for _, term := range and.SubPredicates {
			cp, ok := term.(*predicates.ComparisonPredicate)
			if !ok {
				return false
			}
			if key == nil {
				key = cp.Operand
			} else if !values.ValuesStructurallyEqual(key, cp.Operand) {
				return false
			}
			if !b.AddComparisonMaybe(cp.Comparison) {
				return false
			}
		}
		return key != nil
	}
	cp, ok := group.(*predicates.ComparisonPredicate)
	if !ok {
		return false
	}
	return b.AddComparisonMaybe(cp.Comparison)
}

// indexPredicateToProto ports IndexPredicate.fromQueryPredicate
// (IndexPredicate.java:131-150) fused with each PoJo's toProto: the stored
// RecordMetaDataProto.Predicate bytes for the accepted predicate tree.
func indexPredicateToProto(p predicates.QueryPredicate, res storageNames) (*gen.Predicate, error) {
	switch q := p.(type) {
	case *predicates.ConstantPredicate:
		// IndexPredicate.ConstantPredicate (IndexPredicate.java:420-430,
		// toProto :455-475): TRUE / FALSE / NULL.
		v := gen.ConstantPredicate_NULL
		switch q.Value {
		case predicates.TriTrue:
			v = gen.ConstantPredicate_TRUE
		case predicates.TriFalse:
			v = gen.ConstantPredicate_FALSE
		}
		return &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{Value: v.Enum()}}, nil
	case *predicates.NotPredicate:
		child, err := indexPredicateToProto(q.Child, res)
		if err != nil {
			return nil, err
		}
		return &gen.Predicate{NotPredicate: &gen.NotPredicate{Child: child}}, nil
	case *predicates.AndPredicate:
		children, err := indexPredicateChildrenToProto(q.SubPredicates, res)
		if err != nil {
			return nil, err
		}
		return &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: children}}, nil
	case *predicates.OrPredicate:
		children, err := indexPredicateChildrenToProto(q.SubPredicates, res)
		if err != nil {
			return nil, err
		}
		return &gen.Predicate{OrPredicate: &gen.OrPredicate{Children: children}}, nil
	case *predicates.ComparisonPredicate:
		return comparisonPredicateToProto(q, res)
	default:
		// Java's message (IndexPredicate.java:148).
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"attempt to construct index predicate PoJo from unsupported query predicate")
	}
}

func indexPredicateChildrenToProto(children []predicates.QueryPredicate, res storageNames) ([]*gen.Predicate, error) {
	out := make([]*gen.Predicate, 0, len(children))
	for _, c := range children {
		p, err := indexPredicateToProto(c, res)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// comparisonPredicateToProto is the ValuePredicate PoJo
// (IndexPredicate.java:562-566: the value's field path names, the comparison
// via IndexComparison.fromComparison) and its toProto (:587-594).
func comparisonPredicateToProto(q *predicates.ComparisonPredicate, res storageNames) (*gen.Predicate, error) {
	fv, ok := values.AsFieldValue(q.Operand)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"attempt to construct index predicate PoJo from unsupported query predicate")
	}
	// STORAGE names, by accessor ordinal — Java's getFieldPathNames yields
	// the type's field names, and the maintainer resolves the stored path
	// against the record descriptor at indexing time; a folded display name
	// ("COL1" for a quoted "col1") would never match it.
	accessors, ok := fieldAccessors(fv)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"attempt to construct index predicate PoJo from unresolved field access")
	}
	path := make([]string, 0, len(accessors))
	for i := range accessors {
		path = append(path, res.fieldName(accessors, i))
	}
	cmp, err := indexComparisonToProto(q.Comparison, fv.ResultType())
	if err != nil {
		return nil, err
	}
	return &gen.Predicate{ValuePredicate: &gen.ValuePredicate{
		Value:      path,
		Comparison: cmp,
	}}, nil
}

// indexComparisonToProto builds the Comparison proto: a NullComparison for
// the unary null checks (IndexComparison.java:324-327), otherwise a
// SimpleComparison whose type maps per IndexComparison.java:221-249 and whose
// operand is stored in LiteralKeyExpression.toProtoValue's encoding (:253).
func indexComparisonToProto(c predicates.Comparison, columnType values.Type) (*gen.Comparison, error) {
	if c.Type.IsUnary() {
		switch c.Type {
		case predicates.ComparisonIsNull:
			return &gen.Comparison{NullComparison: &gen.NullComparison{IsNull: protoBool(true)}}, nil
		case predicates.ComparisonIsNotNull:
			return &gen.Comparison{NullComparison: &gen.NullComparison{IsNull: protoBool(false)}}, nil
		}
	}
	var typ gen.ComparisonType
	switch c.Type {
	case predicates.ComparisonEquals:
		typ = gen.ComparisonType_EQUALS
	case predicates.ComparisonNotEquals:
		typ = gen.ComparisonType_NOT_EQUALS
	case predicates.ComparisonLessThan:
		typ = gen.ComparisonType_LESS_THAN
	case predicates.ComparisonLessThanOrEq:
		typ = gen.ComparisonType_LESS_THAN_OR_EQUALS
	case predicates.ComparisonGreaterThan:
		typ = gen.ComparisonType_GREATER_THAN
	case predicates.ComparisonGreaterThanEq:
		typ = gen.ComparisonType_GREATER_THAN_OR_EQUALS
	case predicates.ComparisonStartsWith:
		typ = gen.ComparisonType_STARTS_WITH
	default:
		// Java's message (IndexComparison.java:76).
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"attempt to create PoJo index comparison from unsupported comparison")
	}
	lit, ok := comparisonLiteralOperand(c)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"attempt to create PoJo index comparison from unsupported comparison")
	}
	// The stored comparand follows the COMPARISON's promoted type — the
	// COLUMN's — never the raw literal's own width, and a comparison whose
	// COLUMN side would need the promotion is REJECTED outright. Both facts
	// MEASURED against the live 4.12.11.0 JVM through the D11 stored-bytes
	// comparison (conformance/index_ddl_metadata_conformance_test.go):
	//
	//   - `WHERE bigint_col > 10` stores long_value:10 (the literal is
	//     promoted to the column type; ParseHelpers.parseDecimal's Integer
	//     narrowing describes only the literal as parsed);
	//   - `WHERE int_col > 10` stores int_value:10;
	//   - `WHERE int_col < 5000000000` FAILS: the planner wraps the COLUMN in
	//     promote(col AS LONG), a PromoteValue, and IndexPredicate.isSupported
	//     requires a bare FieldValue (IndexPredicate.java:227) — Java throws
	//     "Unsupported predicate '…'" (MaterializedViewIndexGenerator.java:676).
	//
	// Go's plan predicates carry no promote wrapper, so the same lattice is
	// applied here: a literal STRICTLY WIDER than the column rejects (Java's
	// promoted-column shape); otherwise the literal converts to the column's
	// type for storage.
	if converted, rejected, applied := promoteLiteralToColumn(lit, columnType); rejected {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"attempt to create PoJo index comparison from unsupported comparison")
	} else if applied {
		lit = converted
	}
	operand, err := recordlayer.LiteralToProtoValue(lit)
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"attempt to create PoJo index comparison from unsupported comparison: %v", err)
	}
	return &gen.Comparison{SimpleComparison: &gen.SimpleComparison{
		Type:    typ.Enum(),
		Operand: operand,
	}}, nil
}

func protoBool(b bool) *bool { return &b }
