package cascades

import "testing"

// Example rule: "constant folding for ADD" — when we see
// ArithmeticValue(Add, ConstantValue(a), ConstantValue(b)), yield
// a single ConstantValue(a+b) as the replacement. This is a tiny
// but realistic example of what Batch A rules will look like.
type addConstantFoldRule struct {
	matcher BindingMatcher
	lhs     *Instance
	rhs     *Instance
}

func newAddConstantFoldRule() *addConstantFoldRule {
	lhs := NewConstantMatcher()
	rhs := NewConstantMatcher()
	return &addConstantFoldRule{
		lhs:     lhs,
		rhs:     rhs,
		matcher: &ArithmeticMatcher{Op: OpAdd, Left: lhs, Right: rhs},
	}
}

func (r *addConstantFoldRule) Matcher() BindingMatcher { return r.matcher }

func (r *addConstantFoldRule) OnMatch(call *RuleCall) {
	l := Get[*ConstantValue](call.Bindings, r.lhs)
	rv := Get[*ConstantValue](call.Bindings, r.rhs)
	li, ok1 := l.Value.(int64)
	ri, ok2 := rv.Value.(int64)
	if !ok1 || !ok2 {
		// Non-integer constant — decline to fold.
		return
	}
	call.Yield(&ConstantValue{Value: li + ri, Typ: TypeInt})
}

var _ CascadesRule = (*addConstantFoldRule)(nil)

func TestRuleCall_Yield(t *testing.T) {
	t.Parallel()
	call := &RuleCall{Bindings: NewBindings()}
	if got := call.Yielded(); len(got) != 0 {
		t.Fatalf("fresh call: expected 0 yields, got %d", len(got))
	}
	call.Yield("first")
	call.Yield("second")
	got := call.Yielded()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("yields: got %v", got)
	}
}

func TestFireRule_AddConstantFold(t *testing.T) {
	t.Parallel()
	rule := newAddConstantFoldRule()

	// Matching shape: Constant + Constant → should fold.
	expr := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(3), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(4), Typ: TypeInt},
	}
	replacements := FireRule(rule, expr)
	if len(replacements) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(replacements))
	}
	cv, ok := replacements[0].(*ConstantValue)
	if !ok {
		t.Fatalf("replacement not ConstantValue: %T", replacements[0])
	}
	if cv.Value != int64(7) {
		t.Fatalf("folded value: got %v, want 7", cv.Value)
	}

	// Non-matching shape: Constant + Field → rule does not fire.
	nonMatch := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(3), Typ: TypeInt},
		Right: &FieldValue{Field: "x", Typ: TypeInt},
	}
	if got := FireRule(rule, nonMatch); len(got) != 0 {
		t.Fatalf("non-matching shape: expected 0 replacements, got %d", len(got))
	}

	// Wrong op: Mul instead of Add → rule does not fire.
	wrongOp := &ArithmeticValue{
		Op:    OpMul,
		Left:  &ConstantValue{Value: int64(3), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(4), Typ: TypeInt},
	}
	if got := FireRule(rule, wrongOp); len(got) != 0 {
		t.Fatalf("wrong op: expected 0 replacements, got %d", len(got))
	}
}

// Rule body can decline to yield even on a successful match.
type declineRule struct {
	matcher BindingMatcher
}

func (r *declineRule) Matcher() BindingMatcher { return r.matcher }
func (r *declineRule) OnMatch(*RuleCall)       { /* deliberately empty */ }

func TestFireRule_DeclineToYield(t *testing.T) {
	t.Parallel()
	rule := &declineRule{matcher: NewAnyValue()}
	got := FireRule(rule, &ConstantValue{Value: int64(1), Typ: TypeInt})
	if len(got) != 0 {
		t.Fatalf("expected 0 (decline), got %d", len(got))
	}
}
