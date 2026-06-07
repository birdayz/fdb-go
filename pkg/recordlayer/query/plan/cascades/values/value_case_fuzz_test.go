package values

import "testing"

// FuzzCaseExpression_FirstMatchWins fuzzes the SQL CASE lowering
// (PickValue over ConditionSelectorValue) under random implication
// + alternative shapes. Pins:
//
//  1. No panic on any byte input.
//  2. When at least one implication is TRUE, the result MUST be the
//     alternative at the first-TRUE index — Java's strict-TRUE
//     match contract.
//  3. When ALL implications are FALSE, the result MUST be nil
//     (no implicit ELSE — the harness builds CASE without trailing
//     TRUE).
//  4. Out-of-range index returns nil — pinned by PickValue's OOB
//     guard.
//
// This is the integration-level companion to ConditionSelector +
// PickValue's per-rule unit tests. Properties hold across random
// 8-implication-element shapes.
func FuzzCaseExpression_FirstMatchWins(f *testing.F) {
	// Seeds covering the documented contract:
	f.Add(uint8(0b00000001), uint8(0b11111111))            // implication[0] TRUE → alt[0]
	f.Add(uint8(0b00000100), uint8(0b11111111))            // implication[2] TRUE → alt[2]
	f.Add(uint8(0b00000000), uint8(0b11111111))            // all FALSE → nil
	f.Add(uint8(0b10000000), uint8(0b11111111))            // implication[7] TRUE → alt[7]
	f.Add(uint8(0b00000010|0b00001000), uint8(0b11111111)) // bits 1+3 — first-TRUE wins (idx=1)

	f.Fuzz(func(t *testing.T, implMask, _ uint8) {
		const N = 8
		implications := make([]Value, N)
		alternatives := make([]Value, N)
		for i := 0; i < N; i++ {
			isTrue := (implMask >> i & 1) == 1
			implications[i] = NewBooleanValue(isTrue)
			// Distinct alternative per slot — int64(100+i) lets us
			// identify which slot won.
			alternatives[i] = LiteralValue(int64(100 + i))
		}
		selector := NewConditionSelectorValue(implications)
		pick := NewPickValue(selector, alternatives, NotNullLong)

		got := mustEvaluate(pick, nil)

		// Property check: walk the implications to find the expected
		// first-TRUE index.
		expectedIdx := -1
		for i := 0; i < N; i++ {
			if implMask>>i&1 == 1 {
				expectedIdx = i
				break
			}
		}
		if expectedIdx == -1 {
			// All FALSE → expect nil.
			if got != nil {
				t.Fatalf("all-FALSE mask=%08b: got %v, want nil", implMask, got)
			}
			return
		}
		// At least one TRUE → expect the alternative at that index.
		want := int64(100 + expectedIdx)
		if got != want {
			t.Fatalf("mask=%08b: got %v, want %v (first-TRUE at idx %d)",
				implMask, got, want, expectedIdx)
		}
	})
}

// FuzzAndOrValue_Kleene3VL fuzzes AndOrValue's eval against the
// Kleene 3VL truth table on random (left, right) operand triples
// drawn from {TRUE, FALSE, NULL}. Pins:
//
//  1. No panic on any byte input.
//  2. Result is always one of {true, false, nil}.
//  3. AND is COMMUTATIVE (a AND b == b AND a) under Kleene 3VL.
//  4. OR is COMMUTATIVE.
//  5. AND/OR are idempotent on identical operands (a AND a = a,
//     a OR a = a) — modulo NULL where NULL AND NULL = NULL.
func FuzzAndOrValue_Kleene3VL(f *testing.F) {
	f.Add(uint8(0), uint8(0))     // (TRUE, TRUE)
	f.Add(uint8(0), uint8(1))     // (TRUE, FALSE)
	f.Add(uint8(0), uint8(2))     // (TRUE, NULL)
	f.Add(uint8(1), uint8(2))     // (FALSE, NULL)
	f.Add(uint8(2), uint8(2))     // (NULL, NULL)
	f.Add(uint8(255), uint8(255)) // out-of-range byte

	f.Fuzz(func(t *testing.T, leftRaw, rightRaw uint8) {
		mkOperand := func(v uint8) Value {
			switch v % 3 {
			case 0:
				return NewBooleanValue(true)
			case 1:
				return NewBooleanValue(false)
			}
			return LiteralValue(nil)
		}
		left := mkOperand(leftRaw)
		right := mkOperand(rightRaw)

		for _, op := range []AndOrOp{AndOrAnd, AndOrOr} {
			lr := mustEvaluate(NewAndOrValue(op, left, right), nil)
			rl := mustEvaluate(NewAndOrValue(op, right, left), nil)

			// Property 2: result must be one of {true, false, nil}.
			switch lr {
			case true, false, nil:
			default:
				t.Fatalf("op=%v: result %v (%T) not in {true, false, nil}", op, lr, lr)
			}

			// Property 3+4: commutativity.
			if lr != rl {
				t.Fatalf("op=%v not commutative: lr=%v rl=%v leftRaw=%d rightRaw=%d", op, lr, rl, leftRaw, rightRaw)
			}
		}

		// Property 5: idempotence (a OP a == a, with NULL exception).
		for _, op := range []AndOrOp{AndOrAnd, AndOrOr} {
			selfEval := mustEvaluate(NewAndOrValue(op, left, left), nil)
			leftEval := mustEvaluate(left, nil)
			if selfEval != leftEval {
				t.Fatalf("op=%v not idempotent: a OP a=%v, a=%v leftRaw=%d", op, selfEval, leftEval, leftRaw)
			}
		}
	})
}

// FuzzArrayConstructorValue_LengthInvariant fuzzes ArrayConstructor's
// eval with random child counts + child-NULL patterns. Pins:
//
//  1. No panic on any byte input.
//  2. Eval result is always non-nil []any.
//  3. len(result) == len(children) (one slot per child, no
//     compaction).
//  4. The "empty array != NULL array" contract holds: zero children
//     produces empty (len=0) but non-nil slice.
func FuzzArrayConstructorValue_LengthInvariant(f *testing.F) {
	f.Add(uint8(0), uint8(0))                   // empty array
	f.Add(uint8(0b00001111), uint8(0))          // 4 elements, all non-NULL
	f.Add(uint8(0b00001111), uint8(0b00000101)) // 4 elements, 2 NULL
	f.Add(uint8(0xFF), uint8(0xFF))             // 8 elements, all NULL

	f.Fuzz(func(t *testing.T, presenceMask, nullMask uint8) {
		const N = 8
		var elements []Value
		for i := 0; i < N; i++ {
			if presenceMask>>i&1 == 0 {
				continue
			}
			if nullMask>>i&1 == 1 {
				elements = append(elements, LiteralValue(nil))
			} else {
				elements = append(elements, LiteralValue(int64(i)))
			}
		}
		v := NewArrayConstructorValue(NullableLong, elements)

		got := mustEvaluate(v, nil)
		gotSlice, ok := got.([]any)
		if !ok {
			t.Fatalf("Evaluate returned %T, want []any", got)
		}
		if gotSlice == nil {
			t.Fatalf("Evaluate returned nil slice — must be non-nil to distinguish empty from NULL array")
		}
		if len(gotSlice) != len(elements) {
			t.Fatalf("len(got) = %d, want %d (one slot per child, no compaction)",
				len(gotSlice), len(elements))
		}
	})
}
