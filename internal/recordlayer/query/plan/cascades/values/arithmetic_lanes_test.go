package values

import (
	"errors"
	"fmt"
	"sort"
	"testing"
)

// arithmeticOps is every ArithmeticValue operator, the eleven logical
// operators of Java's ArithmeticValue.LogicalOperator (ArithmeticValue.java:
// 366-378).
var arithmeticOps = []ArithmeticOp{
	OpAdd, OpSub, OpMul, OpDiv, OpMod, OpBitOr, OpBitAnd, OpBitXor,
	OpBitmapBucketNumber, OpBitmapBucketOffset, OpBitmapBitPosition,
}

func typedOperand(name string, tc TypeCode) Value {
	return newFieldValueWithResolvedOrdinal(name, 0, &PrimitiveType{TypeCode: tc, Nullable: true})
}

// TestArithmeticLanesAgreeWithThePlanner pins that the planner has ONE lane
// table: every row of Java's PhysicalOperator table (arithmeticLanes, 107
// rows, which the catalog and the DDL generator consult as well) is admitted by
// NewArithmeticValue over its operand types and types the value as the row
// says, for every operator the table names.
func TestArithmeticLanesAgreeWithThePlanner(t *testing.T) {
	t.Parallel()
	lanes := ArithmeticLanes()
	if len(lanes) != 107 {
		t.Fatalf("the table holds %d rows; ArithmeticValue.java:406-522 has 107", len(lanes))
	}
	opOf := map[string]ArithmeticOp{}
	for _, op := range arithmeticOps {
		opOf[op.LogicalOperatorName()] = op
	}
	checked := map[string]int{}
	for _, l := range lanes {
		op, ok := opOf[l.Function]
		if !ok {
			t.Fatalf("lane function %q names no ArithmeticValue operator", l.Function)
		}
		v, err := NewArithmeticValue(op, typedOperand("L", l.Left), typedOperand("R", l.Right))
		if err != nil {
			t.Errorf("%s(%v, %v): the planner refuses a pair the table has: %v", l.Function, l.Left, l.Right, err)
			continue
		}
		if lane, ok := v.Lane(); !ok || lane != l {
			t.Errorf("%s(%v, %v): constructed with lane %v, %v; want the row itself", l.Function, l.Left, l.Right, lane, ok)
		}
		if code := v.Type().Code(); code != l.Result {
			t.Errorf("%s(%v, %v): the planner types it %v, the lane table %v", l.Function, l.Left, l.Right, code, l.Result)
		}
		checked[l.Function]++
	}
	want := map[string]int{
		"add": 25, "sub": 16, "mul": 16, "div": 16, "mod": 16, "bitand": 4, "bitor": 4, "bitxor": 4,
		"bitmap_bit_position": 2, "bitmap_bucket_number": 2, "bitmap_bucket_offset": 2,
	}
	if fmt.Sprint(checked) != fmt.Sprint(want) {
		t.Fatalf("rows driven through the planner: %v, want %v", checked, want)
	}
}

// TestArithmeticPairsJavaRefusesAreRefused is the census of operand pairs
// Java's encapsulate refuses ("unable to encapsulate arithmetic operation due
// to type mismatch(es)": no row names them). It was the gap RFC-257 WS-J
// section 6 closed: before it the planner built the struct directly and typed
// all 156 such pairs of the five arithmetic operators by promotion. Every
// operator's pairs now go through NewArithmeticValue, which refuses them, so
// the list of pairs the planner admits without a row must stay EMPTY: an entry
// is a refusal the planner lost. The admitted count is checked too (the 107
// rows over these codes), so a constructor that refused everything cannot pass.
func TestArithmeticPairsJavaRefusesAreRefused(t *testing.T) {
	t.Parallel()
	codes := []TypeCode{TypeCodeInt, TypeCodeLong, TypeCodeFloat, TypeCodeDouble, TypeCodeString, TypeCodeBoolean, TypeCodeBytes, TypeCodeNull}
	var admittedWithoutRow []string
	admitted, refused := 0, 0
	for _, op := range arithmeticOps {
		for _, l := range codes {
			for _, r := range codes {
				_, hasRow := LookupArithmeticLane(op.LogicalOperatorName(), l, r)
				_, err := NewArithmeticValue(op, typedOperand("L", l), typedOperand("R", r))
				switch {
				case err == nil && hasRow:
					admitted++
				case err == nil:
					admittedWithoutRow = append(admittedWithoutRow, fmt.Sprintf("%s(%v,%v)", op.LogicalOperatorName(), l, r))
				case hasRow:
					t.Errorf("%s(%v,%v) has a row and is refused: %v", op.LogicalOperatorName(), l, r, err)
				default:
					var mismatch *ArithmeticLaneMismatchError
					if !errors.As(err, &mismatch) {
						t.Errorf("%s(%v,%v) refused with %T, want the lane's VerifyException", op.LogicalOperatorName(), l, r, err)
					}
					refused++
				}
			}
		}
	}
	sort.Strings(admittedWithoutRow)
	if len(admittedWithoutRow) != 0 {
		t.Fatalf("the planner admits %d operand pairs Java refuses: %v", len(admittedWithoutRow), admittedWithoutRow)
	}
	// 11 operators over 8x8 pairs = 704; every one of the 107 rows is over
	// these codes.
	if admitted != 107 || refused != 704-107 {
		t.Fatalf("admitted %d and refused %d of 704 pairs, want 107 and 597", admitted, refused)
	}
}

// TestArithmeticComplexOperandIsRefusedFirst pins Java's first step: an operand
// whose type is not primitive is refused with the SemanticException before any
// lane is looked up, on either side, whatever the other operand (a known
// complex operand is refused even beside an operand of unknown type, which
// otherwise defers the lane to run time).
func TestArithmeticComplexOperandIsRefusedFirst(t *testing.T) {
	t.Parallel()
	for _, complexCode := range []TypeCode{TypeCodeEnum, TypeCodeRecord, TypeCodeUuid, TypeCodeArray} {
		for _, other := range []TypeCode{TypeCodeInt, TypeCodeLong, TypeCodeDouble, TypeCodeUnknown, complexCode} {
			for _, op := range []ArithmeticOp{OpAdd, OpBitAnd, OpBitmapBucketOffset} {
				for _, pair := range [][2]TypeCode{{complexCode, other}, {other, complexCode}} {
					_, err := NewArithmeticValue(op, typedOperand("L", pair[0]), typedOperand("R", pair[1]))
					var complexErr *ArithmeticComplexOperandError
					if !errors.As(err, &complexErr) {
						t.Errorf("%s(%v, %v) = %v, want the complex-operand SemanticException", op.LogicalOperatorName(), pair[0], pair[1], err)
					}
				}
			}
		}
	}
}

// TestArithmeticUnknownOperandDefersTheLane pins the one case the constructor
// does not decide: an operand whose type Go's derivation does not know. The
// value is built without a lane, types by the operators' own rule, and picks
// its arithmetic from the runtime operands.
func TestArithmeticUnknownOperandDefersTheLane(t *testing.T) {
	t.Parallel()
	for _, op := range []ArithmeticOp{OpAdd, OpBitAnd} {
		v, err := NewArithmeticValue(op, typedOperand("L", TypeCodeUnknown), typedOperand("R", TypeCodeInt))
		if err != nil {
			t.Fatalf("%s(UNKNOWN, INT): %v", op.LogicalOperatorName(), err)
		}
		if _, ok := v.Lane(); ok {
			t.Fatalf("%s(UNKNOWN, INT) resolved a lane", op.LogicalOperatorName())
		}
	}
}
