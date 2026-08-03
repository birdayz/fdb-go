package recordlayer

import (
	"fmt"
	"strings"

	gen "fdb.dev/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// predicateFromProto converts a proto Predicate message into an IndexPredicate
// function that can evaluate records at runtime. Matches Java's
// IndexPredicate.shouldIndexThisRecord().
func predicateFromProto(p *gen.Predicate) (IndexPredicate, error) {
	if p == nil {
		return nil, nil
	}
	switch {
	case p.AndPredicate != nil:
		return andPredicateFromProto(p.AndPredicate)
	case p.OrPredicate != nil:
		return orPredicateFromProto(p.OrPredicate)
	case p.ConstantPredicate != nil:
		return constantPredicateFromProto(p.ConstantPredicate)
	case p.NotPredicate != nil:
		return notPredicateFromProto(p.NotPredicate)
	case p.ValuePredicate != nil:
		return valuePredicateFromProto(p.ValuePredicate)
	default:
		return nil, fmt.Errorf("empty predicate message")
	}
}

// NormalizePredicateProto applies Java's predicate CONSTRUCTION rules to a
// stored index predicate, returning the equivalent predicate Java would have
// built. It is the one place that folding lives, so the planner (which decides
// whether a candidate carries a filtering predicate) and the executor (which
// decides whether an index is complete enough to scan) cannot answer the same
// question differently.
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
// The returned message is always freshly built; the input is never mutated.
func NormalizePredicateProto(p *gen.Predicate) *gen.Predicate {
	if p == nil {
		return nil
	}
	// Normalize only a message whose shape is unambiguous. predicateFromProto
	// evaluates the FIRST arm it finds, so rewriting one arm of a multi-arm
	// message could produce a predicate the evaluator never runs. Leave it be
	// and let the classifier below refuse it.
	if predicateProtoArmCount(p) != 1 {
		return p
	}
	switch {
	case p.AndPredicate != nil:
		kept := make([]*gen.Predicate, 0, len(p.AndPredicate.Children))
		for _, child := range p.AndPredicate.Children {
			normalized := NormalizePredicateProto(child)
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
			kept = append(kept, NormalizePredicateProto(child))
		}
		// Java's `of` collapses a singleton and rejects an empty disjunction.
		// An empty one cannot be reconstructed into anything safe, so it is
		// handed back untouched and fails closed as a filtering predicate.
		if len(kept) == 1 {
			return kept[0]
		}
		if len(kept) == 0 {
			return p
		}
		return &gen.Predicate{OrPredicate: &gen.OrPredicate{Children: kept}}
	default:
		return p
	}
}

// predicateProtoArmCount counts the set arms of a predicate message. Exactly
// one is the well-formed case; anything else is ambiguous input.
func predicateProtoArmCount(p *gen.Predicate) int {
	n := 0
	for _, set := range []bool{
		p.AndPredicate != nil,
		p.OrPredicate != nil,
		p.ConstantPredicate != nil,
		p.NotPredicate != nil,
		p.ValuePredicate != nil,
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

// predicateProtoIsTautology reports whether a stored index predicate provably
// rejects no record: normalize the way Java's constructors do, then apply
// Java's narrow tautology test to the result.
func predicateProtoIsTautology(p *gen.Predicate) bool {
	return constantPredicateArmIsTrue(NormalizePredicateProto(p))
}

func andPredicateFromProto(p *gen.AndPredicate) (IndexPredicate, error) {
	children := make([]IndexPredicate, 0, len(p.Children))
	for i, child := range p.Children {
		fn, err := predicateFromProto(child)
		if err != nil {
			return nil, fmt.Errorf("and predicate child %d: %w", i, err)
		}
		children = append(children, fn)
	}
	return func(msg proto.Message) bool {
		for _, child := range children {
			if !child(msg) {
				return false
			}
		}
		return true
	}, nil
}

func orPredicateFromProto(p *gen.OrPredicate) (IndexPredicate, error) {
	children := make([]IndexPredicate, 0, len(p.Children))
	for i, child := range p.Children {
		fn, err := predicateFromProto(child)
		if err != nil {
			return nil, fmt.Errorf("or predicate child %d: %w", i, err)
		}
		children = append(children, fn)
	}
	return func(msg proto.Message) bool {
		for _, child := range children {
			if child(msg) {
				return true
			}
		}
		return false
	}, nil
}

func constantPredicateFromProto(p *gen.ConstantPredicate) (IndexPredicate, error) {
	switch p.GetValue() {
	case gen.ConstantPredicate_TRUE:
		return func(proto.Message) bool { return true }, nil
	case gen.ConstantPredicate_FALSE:
		return func(proto.Message) bool { return false }, nil
	case gen.ConstantPredicate_NULL:
		return func(proto.Message) bool { return false }, nil
	default:
		return nil, fmt.Errorf("unknown constant predicate value: %v", p.GetValue())
	}
}

func notPredicateFromProto(p *gen.NotPredicate) (IndexPredicate, error) {
	child, err := predicateFromProto(p.Child)
	if err != nil {
		return nil, fmt.Errorf("not predicate child: %w", err)
	}
	return func(msg proto.Message) bool {
		return !child(msg)
	}, nil
}

func valuePredicateFromProto(p *gen.ValuePredicate) (IndexPredicate, error) {
	fieldPath := p.Value
	if len(fieldPath) == 0 {
		return nil, fmt.Errorf("value predicate has empty field path")
	}
	cmp := p.Comparison
	if cmp == nil {
		return nil, fmt.Errorf("value predicate has no comparison")
	}

	compareFn, err := comparisonFromProto(cmp)
	if err != nil {
		return nil, err
	}

	return func(msg proto.Message) bool {
		val, has := resolveFieldPath(msg, fieldPath)
		return compareFn(val, has)
	}, nil
}

// comparisonFunc takes (fieldValue, fieldIsPresent) and returns whether the
// comparison holds.
type comparisonFunc func(val any, has bool) bool

func comparisonFromProto(cmp *gen.Comparison) (comparisonFunc, error) {
	switch {
	case cmp.SimpleComparison != nil:
		return simpleComparisonFromProto(cmp.SimpleComparison)
	case cmp.NullComparison != nil:
		return nullComparisonFromProto(cmp.NullComparison)
	default:
		return nil, fmt.Errorf("comparison has neither simple nor null")
	}
}

func nullComparisonFromProto(nc *gen.NullComparison) (comparisonFunc, error) {
	isNull := nc.GetIsNull()
	return func(_ any, has bool) bool {
		if isNull {
			return !has
		}
		return has
	}, nil
}

func simpleComparisonFromProto(sc *gen.SimpleComparison) (comparisonFunc, error) {
	cmpType := sc.GetType()
	operand := extractValueOperand(sc.Operand)

	switch cmpType {
	case gen.ComparisonType_IS_NULL:
		return func(_ any, has bool) bool { return !has }, nil
	case gen.ComparisonType_NOT_NULL:
		return func(_ any, has bool) bool { return has }, nil
	case gen.ComparisonType_EQUALS:
		return func(val any, has bool) bool {
			if !has {
				return false
			}
			return compareValues(val, operand) == 0
		}, nil
	case gen.ComparisonType_NOT_EQUALS:
		return func(val any, has bool) bool {
			if !has {
				return false
			}
			return compareValues(val, operand) != 0
		}, nil
	case gen.ComparisonType_LESS_THAN:
		return func(val any, has bool) bool {
			if !has {
				return false
			}
			return compareValues(val, operand) < 0
		}, nil
	case gen.ComparisonType_LESS_THAN_OR_EQUALS:
		return func(val any, has bool) bool {
			if !has {
				return false
			}
			return compareValues(val, operand) <= 0
		}, nil
	case gen.ComparisonType_GREATER_THAN:
		return func(val any, has bool) bool {
			if !has {
				return false
			}
			return compareValues(val, operand) > 0
		}, nil
	case gen.ComparisonType_GREATER_THAN_OR_EQUALS:
		return func(val any, has bool) bool {
			if !has {
				return false
			}
			return compareValues(val, operand) >= 0
		}, nil
	case gen.ComparisonType_STARTS_WITH:
		return func(val any, has bool) bool {
			if !has {
				return false
			}
			s, ok := val.(string)
			if !ok {
				return false
			}
			prefix, ok := operand.(string)
			if !ok {
				return false
			}
			return strings.HasPrefix(s, prefix)
		}, nil
	default:
		return nil, fmt.Errorf("unknown comparison type: %v", cmpType)
	}
}

// extractValueOperand converts a gen.Value proto to a Go value for comparison.
func extractValueOperand(v *gen.Value) any {
	if v == nil {
		return nil
	}
	// Check each field in priority order matching Java's Value semantics.
	// proto2 optional fields use pointer types; check non-nil.
	if v.LongValue != nil {
		return *v.LongValue
	}
	if v.IntValue != nil {
		return int64(*v.IntValue)
	}
	if v.DoubleValue != nil {
		return *v.DoubleValue
	}
	if v.FloatValue != nil {
		return float64(*v.FloatValue)
	}
	if v.BoolValue != nil {
		return *v.BoolValue
	}
	if v.StringValue != nil {
		return *v.StringValue
	}
	if v.BytesValue != nil {
		return v.BytesValue
	}
	return nil
}

// resolveFieldPath navigates into a proto message following a field path
// (e.g., ["flower", "color"]) and returns the field value and whether it's set.
func resolveFieldPath(msg proto.Message, path []string) (any, bool) {
	if msg == nil {
		return nil, false
	}
	m := msg.ProtoReflect()
	for i, fieldName := range path {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(fieldName))
		if fd == nil {
			return nil, false
		}
		isLast := i == len(path)-1
		if !isLast {
			// Navigate into sub-message
			if fd.Kind() != protoreflect.MessageKind {
				return nil, false
			}
			if !m.Has(fd) {
				return nil, false
			}
			m = m.Get(fd).Message()
		} else {
			// Extract leaf value
			if !m.Has(fd) {
				return nil, false
			}
			return protoFieldToGoValue(m, fd), true
		}
	}
	return nil, false
}

// protoFieldToGoValue extracts a proto field value as a Go type suitable
// for comparison. Normalizes numeric types to int64/float64.
func protoFieldToGoValue(m protoreflect.Message, fd protoreflect.FieldDescriptor) any {
	v := m.Get(fd)
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return v.Int()
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return int64(v.Uint())
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(v.Uint())
	case protoreflect.FloatKind:
		return v.Float()
	case protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return v.Bytes()
	case protoreflect.EnumKind:
		return int64(v.Enum())
	default:
		return nil
	}
}

// compareValues compares two Go values, returning -1, 0, or 1.
// Handles int64, float64, string, bool, and []byte.
func compareValues(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}

	// Normalize numeric types for cross-type comparison
	a = normalizeForCompare(a)
	b = normalizeForCompare(b)

	switch av := a.(type) {
	case int64:
		bv, ok := b.(int64)
		if !ok {
			if bf, ok := b.(float64); ok {
				return compareFloat(float64(av), bf)
			}
			return 0
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
		return 0
	case float64:
		switch bv := b.(type) {
		case float64:
			return compareFloat(av, bv)
		case int64:
			return compareFloat(av, float64(bv))
		}
		return 0
	case string:
		bv, ok := b.(string)
		if !ok {
			return 0
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
		return 0
	case bool:
		bv, ok := b.(bool)
		if !ok {
			return 0
		}
		if av == bv {
			return 0
		}
		if !av {
			return -1
		}
		return 1
	case []byte:
		bv, ok := b.([]byte)
		if !ok {
			return 0
		}
		if string(av) < string(bv) {
			return -1
		}
		if string(av) > string(bv) {
			return 1
		}
		return 0
	}
	return 0
}

func normalizeForCompare(v any) any {
	switch vv := v.(type) {
	case int:
		return int64(vv)
	case int32:
		return int64(vv)
	case uint32:
		return int64(vv)
	case float32:
		return float64(vv)
	default:
		return v
	}
}

func compareFloat(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
