package values

import (
	"fmt"
	"math"
	"testing"
)

// FuzzSimplifyValue_PreservesSemantics is the SEMANTICS differential for
// SimplifyValue: for a randomly shaped tree over four nullable columns, and for
// every row in a fixed battery that includes all-NULL and mixed-NULL rows,
// evaluating the SIMPLIFIED tree must produce what evaluating the ORIGINAL tree
// produces.
//
// The two existing simplifier fuzz targets do not ask this question.
// FuzzSimplifyValue_ArithmeticTree builds an ALL-CONSTANT tree, where the fold
// path is sound by construction — it calls the very Evaluate it is being
// checked against — and FuzzSimplifyValue_CastChain likewise. Neither can reach
// a REWRITE rule, and the rewrites are where a wrong answer can come from:
// simplifyCoalesce drops and reorders arguments, composeFieldOverConstructor
// selects a constructor column by ordinal and discards its siblings,
// composeFieldOverField fuses an accessor chain, and the PromoteValue arm
// re-tags a folded carrier. Each of those changes the tree in a way that a
// constant tree cannot exercise, because each needs a NON-constant leaf to
// survive the fold.
//
// Direction of the assertion, and why it is asymmetric:
//
//   - Original evaluates cleanly -> simplified MUST evaluate cleanly and MUST
//     produce an equal value. Simplification may not introduce an error and may
//     not change an answer.
//   - Original ERRORS -> nothing is asserted. A rewrite is allowed to prune the
//     erroring subtree: `field(RC(1/0, 5), 1)` errors before simplification
//     (RecordConstructorValue.Evaluate evaluates every field) and is 5 after,
//     which is Java's ComposeFieldValueOverRecordConstructorRule behaving
//     correctly, not a divergence.
//
// Equality is CARRIER-SENSITIVE on purpose. int64(5) and float64(5) are not
// interchangeable downstream — a folded constant feeds comparisons, tuple
// encoding and the wire format — so a rewrite that silently widens a carrier is
// a finding, not a rounding detail. NaN compares equal to NaN here because two
// NaNs are the same answer for this question; signed zeros are kept DISTINCT
// for the same reason the carrier is.
func FuzzSimplifyValue_PreservesSemantics(f *testing.F) {
	// Seeds: a bare column, an arithmetic mix, a coalesce chain, a record
	// constructor projection, and a cast chain over a column.
	f.Add([]byte{0x00})
	f.Add([]byte{0x10, 0x00, 0x01, 0x20, 0x02})
	f.Add([]byte{0x50, 0x03, 0x00, 0x21, 0x60})
	f.Add([]byte{0x70, 0x02, 0x00, 0x01})
	f.Add([]byte{0x30, 0x01, 0x00, 0x30, 0x02, 0x00})
	f.Add([]byte{0x40, 0x00, 0x50, 0x02, 0x21, 0x22})

	rows := simplifySemanticsRows()

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) == 0 {
			return
		}
		b := &treeBuilder{script: script}
		tree := b.build(0)
		if tree == nil {
			return
		}

		simplified := SimplifyValue(tree)
		if simplified == nil {
			t.Fatalf("SimplifyValue returned nil for %s", ExplainValue(tree))
		}

		// The STATIC type is part of what a subtree owes its parent, and it is
		// checked FIRST because a parent that dispatches on it turns a type
		// change into a wrong answer only when that parent happens to be
		// present. CastValue.Evaluate dispatches on `c.Child.Type()` and Java's
		// cast table separates INT from LONG, so a rewrite that quietly widened
		// an INT subtree to LONG was observable only under a CAST — here it is
		// observable on its own.
		//
		// NULLABILITY is excluded. Folding a nullable expression to a literal
		// legitimately tightens it (`1+2` is NULLABLE arithmetic and a NOT NULL
		// literal), and that refinement is the fold working, not a defect.
		if !sameDeclaredType(tree.Type(), simplified.Type()) {
			t.Fatalf("simplification changed the static type: %v -> %v\n  original:   %s\n  simplified: %s",
				tree.Type(), simplified.Type(), ExplainValue(tree), ExplainValue(simplified))
		}

		for i, row := range rows {
			want, wantErr := evaluateSafely(tree, row)
			if wantErr != nil {
				// A rewrite is allowed to prune an erroring subtree.
				continue
			}
			got, gotErr := evaluateSafely(simplified, row)
			if gotErr != nil {
				t.Fatalf("row %d: original evaluated cleanly to %#v but simplified errored: %v\n  original:   %s\n  simplified: %s",
					i, want, gotErr, ExplainValue(tree), ExplainValue(simplified))
			}
			if !sameEvaluated(want, got) {
				t.Fatalf("row %d: simplification changed the answer: want %#v (%T), got %#v (%T)\n  original:   %s\n  simplified: %s",
					i, want, want, got, got, ExplainValue(tree), ExplainValue(simplified))
			}
		}
	})
}

// simplifySemanticsRowType is the four-column row every fuzzed tree reads:
// two nullable LONGs, a nullable BOOLEAN and a nullable STRING. All four are
// NULLABLE because the NULL-carrying rows are the ones that separate a correct
// rewrite from one that is only correct on total data.
func simplifySemanticsRowType() *RecordType {
	return NewRecordType("SimplifySemanticsRow", false, []Field{
		{Name: "A", FieldType: NullableLong, Ordinal: 0},
		{Name: "B", FieldType: NullableLong, Ordinal: 1},
		{Name: "C", FieldType: NullableBoolean, Ordinal: 2},
		{Name: "D", FieldType: NullableString, Ordinal: 3},
	})
}

// simplifySemanticsRows is the row battery. Every column is NULL in at least
// one row and non-NULL in at least one other, and the zero/one/negative
// arithmetic edges are present so a division or modulo reaching them is a live
// error path rather than a hypothetical one.
func simplifySemanticsRows() []OrdinalRow {
	return []OrdinalRow{
		&fakeOrdinalRow{names: []string{"A", "B", "C", "D"}, slots: []any{int64(7), int64(3), true, "abc"}},
		&fakeOrdinalRow{names: []string{"A", "B", "C", "D"}, slots: []any{nil, nil, nil, nil}},
		&fakeOrdinalRow{names: []string{"A", "B", "C", "D"}, slots: []any{int64(0), int64(0), false, ""}},
		&fakeOrdinalRow{names: []string{"A", "B", "C", "D"}, slots: []any{int64(-5), nil, true, "Z"}},
		&fakeOrdinalRow{names: []string{"A", "B", "C", "D"}, slots: []any{nil, int64(1), false, "9"}},
		&fakeOrdinalRow{names: []string{"A", "B", "C", "D"}, slots: []any{int64(math.MaxInt64), int64(-1), nil, "x"}},
	}
}

// treeBuilder turns the fuzz byte script into a Value tree. Bytes are consumed
// one at a time and the stream RECYCLES when exhausted, so a short script still
// builds a well-formed (if repetitive) tree rather than collapsing to a single
// leaf — the shapes worth reaching are the deep ones.
type treeBuilder struct {
	script []byte
	pos    int
	nodes  int
}

const (
	treeBuilderMaxDepth = 5
	treeBuilderMaxNodes = 60
)

func (b *treeBuilder) next() byte {
	if len(b.script) == 0 {
		return 0
	}
	v := b.script[b.pos%len(b.script)]
	b.pos++
	return v
}

// build produces one node. Past the depth or node budget it produces a leaf, so
// the recursion always terminates.
func (b *treeBuilder) build(depth int) Value {
	b.nodes++
	if depth >= treeBuilderMaxDepth || b.nodes > treeBuilderMaxNodes {
		return b.leaf()
	}
	switch b.next() % 10 {
	case 0, 1:
		return b.leaf()
	case 2:
		return &ArithmeticValue{
			Op:    ArithmeticOp(b.next() % 5),
			Left:  b.build(depth + 1),
			Right: b.build(depth + 1),
		}
	case 3:
		return NewCastValue(b.build(depth+1), b.castTarget())
	case 4:
		// A PROMOTE is only well formed along the promotion lattice, so the
		// target is CHECKED against the child rather than picked freely. A
		// producer never mints `PROMOTE(<boolean> TO FLOAT)`, and a finding on
		// one is a finding about this generator.
		child := b.build(depth + 1)
		target := b.promoteTarget()
		if child.Type() == nil || !IsPromotable(child.Type(), target) {
			return child
		}
		return NewPromoteValue(child, target)
	case 5:
		return &NotValue{Child: b.build(depth + 1)}
	case 6:
		op := AndOrAnd
		if b.next()%2 == 1 {
			op = AndOrOr
		}
		return NewAndOrValue(op, b.build(depth+1), b.build(depth+1))
	case 7:
		n := 2 + int(b.next()%2) // 2 or 3 arguments
		args := make([]Value, n)
		argTypes := make([]Type, n)
		for i := range args {
			args[i] = b.build(depth + 1)
			argTypes[i] = args[i].Type()
		}
		// COALESCE's declared type is the MAXIMUM of its arguments — that is
		// what the catalog's scalarFunctionCommonResult computes, so a declared
		// type unrelated to the arguments is a tree no producer builds.
		// Incompatible arguments have no common type at all and the producer
		// would have rejected the call, so the node is dropped rather than
		// built ill-typed.
		declared := MaximumTypeOfMany(argTypes...)
		if declared == nil {
			return args[0]
		}
		return NewScalarFunctionValue("COALESCE", declared, args...)
	case 8:
		// field(RecordConstructor(...), ordinal) — the compose rewrite.
		n := 2 + int(b.next()%2)
		fields := make([]RecordConstructorField, n)
		for i := range fields {
			fields[i] = RecordConstructorField{
				Name:  fmt.Sprintf("F%d", i),
				Value: b.build(depth + 1),
			}
		}
		rc := NewRecordConstructorValue(fields...)
		ord := int(b.next()) % n
		fv, err := ResolveFieldOrdinals(rc, []int{ord})
		if err != nil {
			return b.leaf()
		}
		return fv
	default:
		return NewEvaluatesToValue(b.build(depth+1), b.evaluatesTo())
	}
}

func (b *treeBuilder) leaf() Value {
	switch b.next() % 8 {
	case 0, 1, 2, 3:
		return b.column(int(b.next()) % 4)
	case 4:
		return &ConstantValue{Value: int64(int8(b.next())), Typ: NullableLong}
	case 5:
		return &ConstantValue{Value: string(rune('a' + b.next()%26)), Typ: NullableString}
	case 6:
		tf := b.next()%2 == 1
		return &BooleanValue{Value: &tf}
	default:
		return &NullValue{Typ: NullableLong}
	}
}

// column returns a BAKED FieldValue over the fuzz row's ordinal `ord`. A baked
// reference is the only shape that resolves against an OrdinalRow; a lazy one
// fails loud by design, which would make every tree error and the differential
// vacuous.
func (b *treeBuilder) column(ord int) Value {
	rowType := simplifySemanticsRowType()
	root, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("simplify_semantics_fuzz"), rowType)
	if err != nil {
		return &NullValue{Typ: NullableLong}
	}
	fv, err := ResolveFieldOrdinals(root, []int{ord})
	if err != nil {
		return &NullValue{Typ: NullableLong}
	}
	return fv
}

func (b *treeBuilder) castTarget() Type {
	pool := []Type{NullableLong, NullableString, NullableBoolean, NullableDouble}
	return pool[int(b.next())%len(pool)]
}

func (b *treeBuilder) promoteTarget() Type {
	pool := []Type{NullableLong, NullableDouble, NullableFloat}
	return pool[int(b.next())%len(pool)]
}

func (b *treeBuilder) evaluatesTo() EvaluatesTo {
	pool := []EvaluatesTo{EvaluatesToTrue, EvaluatesToFalse, EvaluatesToNull, EvaluatesToNotNull}
	return pool[int(b.next())%len(pool)]
}

// evaluateSafely evaluates v, converting a panic into an error. A panic is a
// finding in its own right, but it is one the NoPanic targets already own; here
// it must not be allowed to abort the differential before the comparison runs.
func evaluateSafely(v Value, row OrdinalRow) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return v.Evaluate(row)
}

// sameEvaluated compares two evaluated results for "same answer", carrier
// included. Two NaNs are the same answer; +0.0 and -0.0 are not, because the
// tuple encoding distinguishes them.
func sameEvaluated(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	af, aIsFloat := a.(float64)
	bf, bIsFloat := b.(float64)
	if aIsFloat && bIsFloat {
		if math.IsNaN(af) && math.IsNaN(bf) {
			return true
		}
		return math.Signbit(af) == math.Signbit(bf) && af == bf
	}
	if aIsFloat != bIsFloat {
		return false
	}
	return a == b
}
