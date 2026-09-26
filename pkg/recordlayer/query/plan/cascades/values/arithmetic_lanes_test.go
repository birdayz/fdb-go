package values

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

var arithmeticOpOfLane = map[string]ArithmeticOp{
	"add": OpAdd, "sub": OpSub, "mul": OpMul, "div": OpDiv, "mod": OpMod,
}

func typedOperand(name string, tc TypeCode) Value {
	return newFieldValueWithResolvedOrdinal(name, 0, &PrimitiveType{TypeCode: tc, Nullable: true})
}

// TestArithmeticLanesAgreeWithThePlanner ties the two copies of Java's
// ArithmeticValue.PhysicalOperator table together: arithmeticLanes, which the
// catalog and the DDL generator consult, and the promotion the planner's
// ArithmeticValue.Type() computes. Every row of the five arithmetic operators
// must type in the planner as the row says, so neither copy can drift from the
// other without this failing. The remaining rows (bit and bitmap functions)
// are not ArithmeticValue operators in Go; the population is checked so a row
// cannot be skipped silently.
func TestArithmeticLanesAgreeWithThePlanner(t *testing.T) {
	t.Parallel()
	lanes := ArithmeticLanes()
	if len(lanes) != 107 {
		t.Fatalf("the table holds %d rows; ArithmeticValue.java:406-522 has 107", len(lanes))
	}
	checked := map[string]int{}
	other := map[string]int{}
	for _, l := range lanes {
		op, ok := arithmeticOpOfLane[l.Function]
		if !ok {
			other[l.Function]++
			continue
		}
		got, found := LookupArithmeticLane(strings.ToUpper(l.Function), l.Left, l.Right)
		if !found || got != l {
			t.Errorf("LookupArithmeticLane(%s, %v, %v) = %v, %v; want the row itself", l.Function, l.Left, l.Right, got, found)
		}
		v := &ArithmeticValue{Op: op, Left: typedOperand("L", l.Left), Right: typedOperand("R", l.Right)}
		if code := v.Type().Code(); code != l.Result {
			t.Errorf("%s(%v, %v): the planner types it %v, the lane table %v", l.Function, l.Left, l.Right, code, l.Result)
		}
		checked[l.Function]++
	}
	wantChecked := map[string]int{"add": 25, "sub": 16, "mul": 16, "div": 16, "mod": 16}
	if fmt.Sprint(checked) != fmt.Sprint(wantChecked) {
		t.Fatalf("arithmetic rows driven through the planner: %v, want %v", checked, wantChecked)
	}
	wantOther := map[string]int{"bitand": 4, "bitor": 4, "bitxor": 4, "bitmap_bit_position": 2, "bitmap_bucket_number": 2, "bitmap_bucket_offset": 2}
	if fmt.Sprint(other) != fmt.Sprint(wantOther) {
		t.Fatalf("rows outside the five arithmetic operators: %v, want %v", other, wantOther)
	}
}

// TestArithmeticPairsThePlannerTypesAndJavaRefuses is the census of operand
// pairs Java's encapsulate refuses ("unable to encapsulate arithmetic
// operation due to type mismatch(es)": no row names them) while the planner's
// ArithmeticValue.Type() still gives them a type. It is the gap RFC-257 WS-J
// section 6 closes by resolving the planner's lanes from LookupArithmeticLane;
// until then the list is pinned so it cannot grow unnoticed. Direction of the
// alarm: once section 6 lands this list must be EMPTY, and the guard then
// reverses to "any entry is a refusal the planner lost".
func TestArithmeticPairsThePlannerTypesAndJavaRefuses(t *testing.T) {
	t.Parallel()
	codes := []TypeCode{TypeCodeInt, TypeCodeLong, TypeCodeFloat, TypeCodeDouble, TypeCodeString, TypeCodeBoolean, TypeCodeBytes}
	var gap []string
	for fn, op := range arithmeticOpOfLane {
		for _, l := range codes {
			for _, r := range codes {
				if _, ok := LookupArithmeticLane(fn, l, r); ok {
					continue
				}
				v := &ArithmeticValue{Op: op, Left: typedOperand("L", l), Right: typedOperand("R", r)}
				if v.Type() != nil {
					gap = append(gap, fmt.Sprintf("%s(%v,%v)", fn, l, r))
				}
			}
		}
	}
	sort.Strings(gap)
	// 5 operators over 7x7 pairs = 245; 89 have a row, and the planner types all
	// of the other 156.
	if len(gap) != 156 {
		t.Fatalf("the planner types %d operand pairs Java refuses, pinned at 156 until RFC-257 WS-J "+
			"section 6 resolves its lanes from LookupArithmeticLane (then this must be 0): %v", len(gap), gap)
	}
}
