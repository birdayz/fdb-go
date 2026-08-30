package cascades

import (
	"fmt"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// predicateSimplifySeedScripts is the deterministic corpus
// FuzzSimplifyPredicate_PreservesSemantics runs on every `go test` with no -fuzz
// flag. It is the population that target's coverage floor is written against,
// which is why it is a named slice rather than inline f.Add calls: a floor
// written as a literal stops describing the corpus the first time a seed is
// added.
var predicateSimplifySeedScripts = [][]byte{
	{0x00},
	{0x10, 0x01, 0x02, 0x20, 0x03},
	{0x21, 0x00, 0x11, 0x30, 0x04},
	{0x32, 0x05, 0x00, 0x12, 0x22},
	{0x40, 0x01, 0x41, 0x02, 0x03, 0x04},
}

// FuzzSimplifyPredicate_PreservesSemantics is the SEMANTICS differential for the
// QueryPredicate rule driver: for a randomly shaped boolean tree over four
// nullable columns, and for every row in a battery that includes all-NULL rows,
// the simplified predicate must evaluate to what the original evaluates to —
// under BOTH shipped rule sets.
//
// The existing FuzzSimplify_PredicateTree does not ask this. It asserts
// non-nil-ness and idempotence, which a rule that returns the WRONG predicate
// satisfies perfectly: a stable wrong answer is still idempotent. Everything
// that can actually go wrong here is three-valued — AndDedup and OrDedup remove
// a conjunct, AndAbsorbOr and OrAbsorbAnd remove a whole branch,
// NotComparisonRewrite turns `NOT (a = b)` into `a <> b`, DeMorgan pushes a NOT
// through a connective — and each of those is a step that is valid in
// two-valued logic and has to be re-checked against UNKNOWN.
//
// UNKNOWN is a THIRD outcome here, not an error to be skipped: a row where the
// original is UNKNOWN and the simplified is FALSE is a row the query would drop
// either way, but one where the original is UNKNOWN and the simplified is TRUE
// is a row that appears out of nowhere. Both directions fail.
//
// An EVALUATION ERROR on the original leaves the row unasserted, for the same
// reason the value-level differential does: a rule is allowed to prune a
// subtree that would have errored. The simplified side erroring where the
// original did not IS asserted.
func FuzzSimplifyPredicate_PreservesSemantics(f *testing.F) {
	for _, seed := range predicateSimplifySeedScripts {
		f.Add(seed)
	}

	rows := predicateSemanticsRows()
	ruleSets := []struct {
		name  string
		rules []CascadesRule
	}{
		{name: "default", rules: DefaultSimplifyRules()},
		{name: "normalization", rules: NormalizationRules()},
	}

	// The assertions sit behind three escapes — an empty script, a builder
	// returning nil, and a row whose ORIGINAL evaluation errored — each a bare
	// `continue`/`return`. A builder that stopped producing usable predicates,
	// or an evaluator that started erroring on every row, would leave this target
	// reporting the same green it reports when the simplifier agrees. Count what
	// was actually compared, and floor it over the seed corpus; see
	// activelyFuzzing for why the floor cannot be enforced under -fuzz.
	var builtPredicates, comparedRows atomic.Int64
	f.Cleanup(func() {
		if activelyFuzzing() {
			return
		}
		if builtPredicates.Load() < int64(len(predicateSimplifySeedScripts)) {
			f.Errorf("the builder produced %d usable predicates from %d seeds — it has stopped "+
				"building, and every assertion in this differential ran on nothing",
				builtPredicates.Load(), len(predicateSimplifySeedScripts))
		}
		// EXACT, not a fraction. Unlike the normal-form differential next door this
		// one has no declined-transform escape — Simplify always returns a
		// predicate and it is always compared — so a healthy run compares every
		// seed against every rule set against every row (measured: 5 x 2 x 6 = 60).
		// Anything less means rows started erroring on the ORIGINAL side, which is
		// how this target would go quiet without any visible change.
		if want := int64(len(predicateSimplifySeedScripts) * len(ruleSets) * len(rows)); comparedRows.Load() < want {
			f.Errorf("compared %d (rule set, row) pairs, want %d — rows are being skipped by "+
				"the wantErr escape, so this target is asking less than it looks",
				comparedRows.Load(), want)
		}
	})

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) == 0 {
			return
		}
		b := &predicateBuilder{script: script}
		pred := b.build(0)
		if pred == nil {
			return
		}
		builtPredicates.Add(1)

		for _, rs := range ruleSets {
			simplified, err := Simplify(pred, rs.rules)
			if err != nil {
				t.Fatalf("%s: Simplify returned an error for %s: %v", rs.name, pred.Explain(), err)
			}
			if simplified == nil {
				t.Fatalf("%s: Simplify returned nil for %s", rs.name, pred.Explain())
			}

			for i, row := range rows {
				want, wantErr := evalPredicateSafely(pred, row)
				if wantErr != nil {
					continue
				}
				got, gotErr := evalPredicateSafely(simplified, row)
				if gotErr != nil {
					t.Fatalf("%s row %d: original evaluated cleanly to %s but simplified errored: %v\n  original:   %s\n  simplified: %s",
						rs.name, i, triName(want), gotErr, pred.Explain(), simplified.Explain())
				}
				comparedRows.Add(1)
				if triName(want) != triName(got) {
					t.Fatalf("%s row %d: simplification changed the truth value: want %s, got %s\n  original:   %s\n  simplified: %s",
						rs.name, i, triName(want), triName(got), pred.Explain(), simplified.Explain())
				}
			}
		}
	})
}

// triName renders a TriBool as one of three names so UNKNOWN compares as its
// own outcome rather than collapsing into FALSE. Comparing the raw *bool
// pointers would compare ADDRESSES and pass on any two independently allocated
// results.
func triName(t predicates.TriBool) string {
	if t == nil {
		return "UNKNOWN"
	}
	if *t {
		return "TRUE"
	}
	return "FALSE"
}

func evalPredicateSafely(p predicates.QueryPredicate, row values.OrdinalRow) (result predicates.TriBool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return p.Eval(row)
}

// predicateSemanticsRowType is the row the fuzzed leaves read. Every column is
// NULLABLE: a three-valued bug cannot be reached on total data.
func predicateSemanticsRowType() *values.RecordType {
	return values.NewRecordType("PredicateSemanticsRow", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "C", FieldType: values.NullableBoolean, Ordinal: 2},
		{Name: "D", FieldType: values.NullableString, Ordinal: 3},
	})
}

func predicateSemanticsRows() []values.OrdinalRow {
	return []values.OrdinalRow{
		&predicateFuzzRow{slots: []any{int64(7), int64(3), true, "abc"}},
		&predicateFuzzRow{slots: []any{nil, nil, nil, nil}},
		&predicateFuzzRow{slots: []any{int64(0), int64(0), false, ""}},
		&predicateFuzzRow{slots: []any{int64(-5), nil, true, "Z"}},
		&predicateFuzzRow{slots: []any{nil, int64(1), false, "9"}},
		&predicateFuzzRow{slots: []any{int64(3), int64(3), nil, "abc"}},
	}
}

type predicateFuzzRow struct{ slots []any }

func (r *predicateFuzzRow) Get(ord int) (any, bool) {
	if ord < 0 || ord >= len(r.slots) {
		return nil, false
	}
	return r.slots[ord], true
}

// predicateBuilder turns the fuzz byte script into a boolean tree. The script
// RECYCLES when exhausted so a short input still reaches a deep shape.
type predicateBuilder struct {
	script []byte
	pos    int
	nodes  int
}

const (
	predicateBuilderMaxDepth = 4
	predicateBuilderMaxNodes = 40
)

func (b *predicateBuilder) next() byte {
	if len(b.script) == 0 {
		return 0
	}
	v := b.script[b.pos%len(b.script)]
	b.pos++
	return v
}

func (b *predicateBuilder) build(depth int) predicates.QueryPredicate {
	b.nodes++
	if depth >= predicateBuilderMaxDepth || b.nodes > predicateBuilderMaxNodes {
		return b.leaf()
	}
	switch b.next() % 8 {
	case 0, 1, 2:
		return b.leaf()
	case 3, 4:
		return predicates.NewAnd(b.buildList(depth)...)
	case 5, 6:
		return predicates.NewOr(b.buildList(depth)...)
	default:
		return predicates.NewNot(b.build(depth + 1))
	}
}

// buildList produces two or three children. Three matters: the absorption and
// dedup rules operate on a conjunct LIST, and a rule that mishandles the third
// element is invisible at arity two.
func (b *predicateBuilder) buildList(depth int) []predicates.QueryPredicate {
	n := 2 + int(b.next()%2)
	out := make([]predicates.QueryPredicate, n)
	for i := range out {
		out[i] = b.build(depth + 1)
	}
	return out
}

func (b *predicateBuilder) leaf() predicates.QueryPredicate {
	switch b.next() % 10 {
	case 0:
		return predicates.NewConstantPredicate(predicates.TriTrue)
	case 1:
		return predicates.NewConstantPredicate(predicates.TriFalse)
	case 2:
		return predicates.NewConstantPredicate(nil) // UNKNOWN
	case 3:
		// A boolean COLUMN read as a predicate — the shape whose value is
		// genuinely three-valued rather than a comparison that happens to be.
		return predicates.NewValuePredicate(b.column(2))
	default:
		return b.comparison()
	}
}

// comparison builds `<column> <op> <literal>` over a column whose type suits
// the operand, so the leaf is one a producer could build. The operator pool is
// the eight the simplification rules actually reason about — the six ordering
// and equality forms plus the two unary null tests; the TEXT_* and
// DISTANCE_RANK families are not boolean-simplification territory.
func (b *predicateBuilder) comparison() predicates.QueryPredicate {
	ops := []predicates.ComparisonType{
		predicates.ComparisonEquals,
		predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
		predicates.ComparisonIsNull,
		predicates.ComparisonIsNotNull,
	}
	op := ops[int(b.next())%len(ops)]

	// Column 0/1 are LONG, column 3 is STRING. Pair each with a literal of its
	// own type so the comparison is well typed.
	var operand values.Value
	var literal any
	if b.next()%4 == 0 {
		operand = b.column(3)
		literal = string(rune('a' + b.next()%4))
	} else {
		operand = b.column(int(b.next()) % 2)
		literal = int64(int8(b.next()) % 8)
	}
	if op.IsUnary() {
		return predicates.NewComparisonPredicate(operand, predicates.Comparison{Type: op})
	}
	return predicates.NewComparisonPredicate(operand,
		predicates.NewLiteralComparison(op, literal))
}

// column returns a BAKED FieldValue over the fuzz row's ordinal. A baked
// reference is the only shape that resolves against an OrdinalRow; a lazy one
// fails loud, which would make every leaf error and the differential vacuous.
func (b *predicateBuilder) column(ord int) values.Value {
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("predicate_semantics_fuzz"), predicateSemanticsRowType())
	if err != nil {
		return &values.NullValue{Typ: values.NullableLong}
	}
	fv, err := values.ResolveFieldOrdinals(root, []int{ord})
	if err != nil {
		return &values.NullValue{Typ: values.NullableLong}
	}
	return fv
}
