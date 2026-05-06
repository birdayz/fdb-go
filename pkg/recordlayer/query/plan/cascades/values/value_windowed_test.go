package values

import "testing"

func TestWindowedValue_Children(t *testing.T) {
	t.Parallel()
	pa := LiteralValue("a")
	pb := LiteralValue("b")
	a1 := LiteralValue(int64(1))
	a2 := LiteralValue(int64(2))
	w := NewWindowedValue([]Value{pa, pb}, []Value{a1, a2})
	cs := w.Children()
	if len(cs) != 4 {
		t.Fatalf("Children len = %d, want 4", len(cs))
	}
	if cs[0] != pa || cs[1] != pb || cs[2] != a1 || cs[3] != a2 {
		t.Fatalf("Children order wrong: %v", cs)
	}
}

func TestWindowedValue_EvaluateReturnsNil(t *testing.T) {
	t.Parallel()
	w := NewWindowedValue([]Value{LiteralValue("x")}, []Value{LiteralValue(int64(1))})
	if got := w.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate = %v, want nil", got)
	}
	row := map[string]any{"x": int64(7)}
	if got := w.Evaluate(row); got != nil {
		t.Fatalf("Evaluate(row) = %v, want nil", got)
	}
}

func TestWindowedValue_SplitNewChildren(t *testing.T) {
	t.Parallel()
	pa := LiteralValue("a")
	pb := LiteralValue("b")
	a1 := LiteralValue(int64(1))
	w := NewWindowedValue([]Value{pa, pb}, []Value{a1})
	// Re-split a flat list of 4 children into (2 partition, 2 args).
	newPa := LiteralValue("A")
	newPb := LiteralValue("B")
	newA1 := LiteralValue(int64(10))
	newA2 := LiteralValue(int64(20))
	part, args := w.SplitNewChildren([]Value{newPa, newPb, newA1, newA2})
	if len(part) != 2 || part[0] != newPa || part[1] != newPb {
		t.Fatalf("partition = %v, want [newPa, newPb]", part)
	}
	if len(args) != 2 || args[0] != newA1 || args[1] != newA2 {
		t.Fatalf("argument = %v, want [newA1, newA2]", args)
	}
}

func TestWindowedValue_SplitNewChildrenShorterThanPartition(t *testing.T) {
	t.Parallel()
	w := NewWindowedValue([]Value{LiteralValue("a"), LiteralValue("b")}, nil)
	// Only one new child — split clamps n to len(newChildren).
	part, args := w.SplitNewChildren([]Value{LiteralValue("X")})
	if len(part) != 1 {
		t.Fatalf("partition len = %d, want 1", len(part))
	}
	if len(args) != 0 {
		t.Fatalf("argument len = %d, want 0", len(args))
	}
}

func TestWindowedValue_DefensiveCopyOnConstruct(t *testing.T) {
	t.Parallel()
	pv := []Value{LiteralValue("a")}
	av := []Value{LiteralValue(int64(1))}
	w := NewWindowedValue(pv, av)
	pv[0] = LiteralValue("MUTATED")
	av[0] = LiteralValue(int64(999))
	if c, _ := w.PartitioningValues[0].Evaluate(nil).(string); c == "MUTATED" {
		t.Fatalf("PartitioningValues aliased caller's slice — not defensively copied")
	}
	if c, _ := w.ArgumentValues[0].Evaluate(nil).(int64); c == 999 {
		t.Fatalf("ArgumentValues aliased caller's slice — not defensively copied")
	}
}

func TestRankValue_Type(t *testing.T) {
	t.Parallel()
	r := NewRankValue(nil)
	if !r.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", r.Type())
	}
}

func TestRankValue_Name(t *testing.T) {
	t.Parallel()
	r := NewRankValue(nil)
	if got := r.Name(); got != "RANK" {
		t.Fatalf("Name = %q, want RANK", got)
	}
}

func TestRankValue_HasNoArgumentValues(t *testing.T) {
	t.Parallel()
	r := NewRankValue([]Value{LiteralValue("p")})
	if len(r.ArgumentValues) != 0 {
		t.Fatalf("ArgumentValues len = %d, want 0 (RANK takes no operands)", len(r.ArgumentValues))
	}
}

func TestRankValue_PartitioningValuesPreserved(t *testing.T) {
	t.Parallel()
	p1 := LiteralValue("region")
	p2 := LiteralValue("year")
	r := NewRankValue([]Value{p1, p2})
	if len(r.PartitioningValues) != 2 {
		t.Fatalf("PartitioningValues len = %d, want 2", len(r.PartitioningValues))
	}
}

func TestRankValue_ChildrenReturnsPartitionsOnly(t *testing.T) {
	t.Parallel()
	p1 := LiteralValue("region")
	r := NewRankValue([]Value{p1})
	cs := r.Children()
	if len(cs) != 1 || cs[0] != p1 {
		t.Fatalf("Children = %v, want [p1] (no args)", cs)
	}
}

func TestRankValue_EvaluateFromHarness(t *testing.T) {
	t.Parallel()
	r := NewRankValue([]Value{LiteralValue("x")})
	row := map[string]any{"_rank": int64(5)}
	if got := r.Evaluate(row); got != int64(5) {
		t.Fatalf("Evaluate = %v, want 5", got)
	}
}

func TestRankValue_EvaluateMissingKeyReturnsNil(t *testing.T) {
	t.Parallel()
	r := NewRankValue(nil)
	if got := r.Evaluate(map[string]any{"x": int64(99)}); got != nil {
		t.Fatalf("Evaluate(no _rank) = %v, want nil", got)
	}
}

func TestRankValue_EvaluateNilCtxReturnsNil(t *testing.T) {
	t.Parallel()
	r := NewRankValue(nil)
	if got := r.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestRankValue_WithChildrenPreservesEmptyArgs(t *testing.T) {
	t.Parallel()
	original := NewRankValue([]Value{LiteralValue("a"), LiteralValue("b")})
	// Replace 2 partition + 1 spurious argument; arg should be dropped.
	newP1 := LiteralValue("A")
	newP2 := LiteralValue("B")
	spurious := LiteralValue("ARG")
	rebuilt := original.WithChildren([]Value{newP1, newP2, spurious})
	if len(rebuilt.ArgumentValues) != 0 {
		t.Fatalf("rebuilt.ArgumentValues len = %d, want 0 (RANK has no args)",
			len(rebuilt.ArgumentValues))
	}
	if len(rebuilt.PartitioningValues) != 2 || rebuilt.PartitioningValues[0] != newP1 {
		t.Fatalf("rebuilt.PartitioningValues = %v, want [newP1, newP2]", rebuilt.PartitioningValues)
	}
}
