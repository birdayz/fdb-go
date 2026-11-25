package api

import "testing"

// ---- KeySet ----

func TestKeySetEmptyImmutable(t *testing.T) {
	t.Parallel()
	e := EmptyKeySet()
	if e.NumColumns() != 0 {
		t.Fatalf("empty has %d columns", e.NumColumns())
	}
	_, err := e.SetKeyColumn("k", "v")
	if err == nil {
		t.Fatal("expected error setting on EmptyKeySet")
	}
	got := AsError(err)
	if got == nil || got.Code != ErrCodeUnsupportedOperation {
		t.Errorf("wrong error code: %v", err)
	}
	_, err = e.SetKeyColumns(map[string]any{"x": 1})
	if err == nil {
		t.Fatal("expected error on SetKeyColumns for EmptyKeySet")
	}
}

func TestKeySetEmptyIsSingleton(t *testing.T) {
	t.Parallel()
	a, b := EmptyKeySet(), EmptyKeySet()
	if a != b {
		t.Fatal("EmptyKeySet should be a singleton")
	}
}

func TestKeySetSetKeyColumn(t *testing.T) {
	t.Parallel()
	k := NewKeySet()
	if _, err := k.SetKeyColumn("id", int64(42)); err != nil {
		t.Fatalf("SetKeyColumn: %v", err)
	}
	if _, err := k.SetKeyColumn("name", "alice"); err != nil {
		t.Fatalf("SetKeyColumn: %v", err)
	}
	if k.NumColumns() != 2 {
		t.Errorf("NumColumns = %d", k.NumColumns())
	}
	m := k.ToMap()
	if m["id"] != int64(42) || m["name"] != "alice" {
		t.Errorf("ToMap = %+v", m)
	}
}

func TestKeySetSetKeyColumnOverwrites(t *testing.T) {
	t.Parallel()
	// Setting the same column twice must overwrite, not duplicate.
	// Pins the mutation contract documented on KeySet.
	k := NewKeySet()
	if _, err := k.SetKeyColumn("id", int64(1)); err != nil {
		t.Fatalf("SetKeyColumn: %v", err)
	}
	if _, err := k.SetKeyColumn("id", int64(42)); err != nil {
		t.Fatalf("SetKeyColumn (overwrite): %v", err)
	}
	if k.NumColumns() != 1 {
		t.Errorf("NumColumns after overwrite = %d, want 1", k.NumColumns())
	}
	if v := k.ToMap()["id"]; v != int64(42) {
		t.Errorf("overwritten value = %v, want 42", v)
	}
}

func TestKeySetMutationReturnsSameReceiver(t *testing.T) {
	t.Parallel()
	// Documented contract: SetKeyColumn mutates and returns the
	// receiver (not a copy). Pin that so future refactors don't
	// silently switch to copy-on-write without updating docs.
	k := NewKeySet()
	ret, err := k.SetKeyColumn("id", 1)
	if err != nil {
		t.Fatalf("SetKeyColumn: %v", err)
	}
	if ret != k {
		t.Error("SetKeyColumn should return receiver, not a new value")
	}
}

func TestKeySetSetKeyColumns(t *testing.T) {
	t.Parallel()
	k := NewKeySet()
	_, err := k.SetKeyColumns(map[string]any{"a": 1, "b": 2, "c": 3})
	if err != nil {
		t.Fatalf("SetKeyColumns: %v", err)
	}
	if k.NumColumns() != 3 {
		t.Errorf("NumColumns = %d", k.NumColumns())
	}
}

func TestKeySetToMapIsCopy(t *testing.T) {
	t.Parallel()
	k := NewKeySet()
	_, _ = k.SetKeyColumn("a", 1)
	m := k.ToMap()
	m["a"] = 999
	if k.ToMap()["a"] != 1 {
		t.Error("ToMap returned internal map (aliased)")
	}
}

// ---- Continuation ----

func TestContinuationReasonString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		r    ContinuationReason
		want string
	}{
		{ContinuationUserRequested, "USER_REQUESTED_CONTINUATION"},
		{ContinuationTransactionLimitReached, "TRANSACTION_LIMIT_REACHED"},
		{ContinuationQueryExecutionLimitReached, "QUERY_EXECUTION_LIMIT_REACHED"},
		{ContinuationCursorAfterLast, "CURSOR_AFTER_LAST"},
		// Unknown/out-of-range values fall through to the default branch.
		{ContinuationReason(99), "?"},
		{ContinuationReason(-1), "?"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.r, got, c.want)
		}
	}
}

// fakeCont is a minimal Continuation for testing the helpers.
type fakeCont struct {
	state []byte
	r     ContinuationReason
}

func (f *fakeCont) Serialize() []byte          { return f.state }
func (f *fakeCont) ExecutionState() []byte     { return f.state }
func (f *fakeCont) Reason() ContinuationReason { return f.r }

func TestContinuationAtBeginningAtEnd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state     []byte
		beginning bool
		end       bool
	}{
		{nil, true, false},
		{[]byte{}, false, true},
		{[]byte{1, 2, 3}, false, false},
	}
	for _, c := range cases {
		fc := &fakeCont{state: c.state, r: ContinuationUserRequested}
		if got := AtBeginning(fc); got != c.beginning {
			t.Errorf("AtBeginning(%v) = %v, want %v", c.state, got, c.beginning)
		}
		if got := AtEnd(fc); got != c.end {
			t.Errorf("AtEnd(%v) = %v, want %v", c.state, got, c.end)
		}
	}
}
