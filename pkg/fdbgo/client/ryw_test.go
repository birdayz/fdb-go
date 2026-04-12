package client

import (
	"bytes"
	"context"
	"testing"
)

// TestRYWGetRange_AllClearedServerMore verifies the fix for the silent
// truncation bug: when serverMore=true and all fetched results are locally
// cleared, getRange must re-fetch instead of returning more=false.
//
// Before the fix: returned ([], false) — silently lost keys D, E.
// After the fix: loops, fetches [D, E], returns them.
func TestRYWGetRange_AllClearedServerMore(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Clear the range [A, D) — covers keys A, B, C but not D, E.
	c.clearRange([]byte("A"), []byte("D"))

	// Mock server: first call returns [A, B, C] with more=true,
	// second call returns [D, E] with more=false.
	callCount := 0
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		callCount++
		switch callCount {
		case 1:
			return []KeyValue{
				{Key: []byte("A"), Value: []byte("a")},
				{Key: []byte("B"), Value: []byte("b")},
				{Key: []byte("C"), Value: []byte("c")},
			}, true, nil
		case 2:
			// Server returns the remaining keys after C.
			return []KeyValue{
				{Key: []byte("D"), Value: []byte("d")},
				{Key: []byte("E"), Value: []byte("e")},
			}, false, nil
		default:
			t.Fatalf("unexpected server call #%d", callCount)
			return nil, false, nil
		}
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d: %v", len(result), result)
	}
	if string(result[0].Key) != "D" || string(result[1].Key) != "E" {
		t.Errorf("expected [D, E], got [%s, %s]", result[0].Key, result[1].Key)
	}
	if callCount != 2 {
		t.Errorf("expected 2 server calls, got %d", callCount)
	}
}

// TestRYWGetRange_WritesAndClears verifies that Set + ClearRange correctly
// merge with server results: writes override, clears remove, boundary is
// respected.
func TestRYWGetRange_WritesAndClears(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Set key "B" to a local value.
	c.set([]byte("B"), []byte("local-b"))
	// Clear key "C".
	c.clear([]byte("C"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("server-a")},
			{Key: []byte("B"), Value: []byte("server-b")},
			{Key: []byte("C"), Value: []byte("server-c")},
			{Key: []byte("D"), Value: []byte("server-d")},
		}, false, nil
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	// A (server), B (local override), D (server). C is cleared.
	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}
	expect := []struct {
		key, val string
	}{
		{"A", "server-a"},
		{"B", "local-b"},
		{"D", "server-d"},
	}
	for i, e := range expect {
		if string(result[i].Key) != e.key || string(result[i].Value) != e.val {
			t.Errorf("result[%d]: got (%s, %s), want (%s, %s)", i, result[i].Key, result[i].Value, e.key, e.val)
		}
	}
}

// TestRYWGetRange_WritesBeyondBoundary verifies that local writes beyond
// the server's knowledge boundary are not included until the next fetch.
func TestRYWGetRange_WritesBeyondBoundary(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Local write at key "F" — beyond any server batch boundary.
	c.set([]byte("F"), []byte("local-f"))

	callCount := 0
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		callCount++
		switch callCount {
		case 1:
			return []KeyValue{
				{Key: []byte("A"), Value: []byte("a")},
				{Key: []byte("C"), Value: []byte("c")},
			}, true, nil // boundary = C, F is beyond it
		case 2:
			return []KeyValue{
				{Key: []byte("E"), Value: []byte("e")},
				{Key: []byte("G"), Value: []byte("g")},
			}, false, nil
		default:
			t.Fatalf("unexpected server call #%d", callCount)
			return nil, false, nil
		}
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	// First batch: A, C (boundary=C, F excluded)
	// Second batch: E, F (local write), G
	// Total: A, C, E, F, G
	if len(result) != 5 {
		keys := make([]string, len(result))
		for i, kv := range result {
			keys[i] = string(kv.Key)
		}
		t.Fatalf("expected 5 results, got %d: %v", len(result), keys)
	}
	if string(result[3].Key) != "F" || string(result[3].Value) != "local-f" {
		t.Errorf("result[3]: got (%s, %s), want (F, local-f)", result[3].Key, result[3].Value)
	}
}

// TestRYWGetRange_ReverseAllCleared tests the truncation fix for reverse scans.
func TestRYWGetRange_ReverseAllCleared(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Clear [C, Z) — covers C, D, E but not A, B.
	c.clearRange([]byte("C"), []byte("Z"))

	callCount := 0
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		if !reverse {
			t.Fatal("expected reverse=true")
		}
		callCount++
		switch callCount {
		case 1:
			// Reverse: highest keys first. E, D, C — all cleared.
			return []KeyValue{
				{Key: []byte("E"), Value: []byte("e")},
				{Key: []byte("D"), Value: []byte("d")},
				{Key: []byte("C"), Value: []byte("c")},
			}, true, nil
		case 2:
			// Next batch: B, A — not cleared.
			return []KeyValue{
				{Key: []byte("B"), Value: []byte("b")},
				{Key: []byte("A"), Value: []byte("a")},
			}, false, nil
		default:
			t.Fatalf("unexpected server call #%d", callCount)
			return nil, false, nil
		}
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, true, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	// Reverse order: B, A (C, D, E are cleared)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if string(result[0].Key) != "B" || string(result[1].Key) != "A" {
		t.Errorf("expected [B, A], got [%s, %s]", result[0].Key, result[1].Key)
	}
	if callCount != 2 {
		t.Errorf("expected 2 server calls, got %d", callCount)
	}
}

// TestRYWGetRange_ReverseWriteBetweenBatches verifies that a local write
// between two reverse server batches is correctly included once the
// knowledge boundary advances past it.
func TestRYWGetRange_ReverseWriteBetweenBatches(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Local write at "C" — between the two server batches.
	c.set([]byte("C"), []byte("local-c"))

	callCount := 0
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		if !reverse {
			t.Fatal("expected reverse=true")
		}
		callCount++
		switch callCount {
		case 1:
			// Reverse: highest keys first. E, D — boundary at D.
			// C is below boundary, excluded from first batch.
			return []KeyValue{
				{Key: []byte("E"), Value: []byte("e")},
				{Key: []byte("D"), Value: []byte("d")},
			}, true, nil
		case 2:
			// Next batch: B, A. Now C is within [A, D) range.
			return []KeyValue{
				{Key: []byte("B"), Value: []byte("b")},
				{Key: []byte("A"), Value: []byte("a")},
			}, false, nil
		default:
			t.Fatalf("unexpected server call #%d", callCount)
			return nil, false, nil
		}
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, true, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	// Reverse order: E, D, C (local write), B, A
	if len(result) != 5 {
		keys := make([]string, len(result))
		for i, kv := range result {
			keys[i] = string(kv.Key)
		}
		t.Fatalf("expected 5 results, got %d: %v", len(result), keys)
	}
	expect := []string{"E", "D", "C", "B", "A"}
	for i, e := range expect {
		if string(result[i].Key) != e {
			t.Errorf("result[%d]: got %s, want %s", i, result[i].Key, e)
		}
	}
	if string(result[2].Value) != "local-c" {
		t.Errorf("result[2] value: got %q, want %q", result[2].Value, "local-c")
	}
}

// TestRYWGetRange_AtomicResolution verifies that pending atomic mutations
// are resolved against the server base value.
func TestRYWGetRange_AtomicResolution(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Atomic ADD 5 to key "A" (unknown base → resolve from server).
	c.atomic(MutAddValue, []byte("A"), []byte{5, 0, 0, 0, 0, 0, 0, 0})

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte{10, 0, 0, 0, 0, 0, 0, 0}}, // base = 10
		}, false, nil
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	// 10 + 5 = 15
	if !bytes.Equal(result[0].Value, []byte{15, 0, 0, 0, 0, 0, 0, 0}) {
		t.Errorf("expected resolved value 15, got %v", result[0].Value)
	}
}

// TestRYWGetRange_LimitWithMore verifies that the limit is respected and
// more is correctly set when truncating results.
func TestRYWGetRange_LimitWithMore(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Local write at "B".
	c.set([]byte("B"), []byte("local"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("a")},
			{Key: []byte("C"), Value: []byte("c")},
			{Key: []byte("D"), Value: []byte("d")},
		}, false, nil
	}

	// Limit 2: should get A, B and more=true (C, D remain).
	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 2, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if !more {
		t.Error("expected more=true")
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if string(result[0].Key) != "A" || string(result[1].Key) != "B" {
		t.Errorf("expected [A, B], got [%s, %s]", result[0].Key, result[1].Key)
	}
}

// TestRYWGetRange_NoWritesOrClears verifies the fast path: when there are
// no local writes or clears, getRange goes straight to the server.
func TestRYWGetRange_NoWritesOrClears(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	called := false
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		called = true
		return []KeyValue{{Key: []byte("X"), Value: []byte("x")}}, false, nil
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if !called {
		t.Fatal("expected server to be called (fast path)")
	}
	if more {
		t.Error("expected more=false")
	}
	if len(result) != 1 || string(result[0].Key) != "X" {
		t.Errorf("unexpected result: %v", result)
	}
}

// TestRYWGetRange_MultipleClearedBatches tests that the loop correctly handles
// multiple consecutive cleared batches before finding live data.
func TestRYWGetRange_MultipleClearedBatches(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Clear a large range.
	c.clearRange([]byte("A"), []byte("M"))

	callCount := 0
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		callCount++
		switch callCount {
		case 1:
			return []KeyValue{
				{Key: []byte("A"), Value: []byte("a")},
				{Key: []byte("B"), Value: []byte("b")},
			}, true, nil
		case 2:
			return []KeyValue{
				{Key: []byte("C"), Value: []byte("c")},
				{Key: []byte("D"), Value: []byte("d")},
			}, true, nil
		case 3:
			return []KeyValue{
				{Key: []byte("L"), Value: []byte("l")},
			}, true, nil
		case 4:
			// Finally past the cleared range.
			return []KeyValue{
				{Key: []byte("N"), Value: []byte("n")},
				{Key: []byte("P"), Value: []byte("p")},
			}, false, nil
		default:
			t.Fatalf("unexpected server call #%d", callCount)
			return nil, false, nil
		}
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	// All keys A-L are cleared. N, P survive.
	if len(result) != 2 {
		keys := make([]string, len(result))
		for i, kv := range result {
			keys[i] = string(kv.Key)
		}
		t.Fatalf("expected 2 results, got %d: %v", len(result), keys)
	}
	if string(result[0].Key) != "N" || string(result[1].Key) != "P" {
		t.Errorf("expected [N, P], got [%s, %s]", result[0].Key, result[1].Key)
	}
}

// TestRYWGetRange_InterleavedWritesAndServer verifies the two-pointer merge
// correctly interleaves sorted server results with sorted writes.
func TestRYWGetRange_InterleavedWritesAndServer(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Writes at B, D, F — interleaved with server results at A, C, E, G.
	c.set([]byte("B"), []byte("write-b"))
	c.set([]byte("D"), []byte("write-d"))
	c.set([]byte("F"), []byte("write-f"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("server-a")},
			{Key: []byte("C"), Value: []byte("server-c")},
			{Key: []byte("E"), Value: []byte("server-e")},
			{Key: []byte("G"), Value: []byte("server-g")},
		}, false, nil
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	expect := []struct{ key, val string }{
		{"A", "server-a"},
		{"B", "write-b"},
		{"C", "server-c"},
		{"D", "write-d"},
		{"E", "server-e"},
		{"F", "write-f"},
		{"G", "server-g"},
	}
	if len(result) != len(expect) {
		keys := make([]string, len(result))
		for i, kv := range result {
			keys[i] = string(kv.Key)
		}
		t.Fatalf("expected %d results, got %d: %v", len(expect), len(result), keys)
	}
	for i, e := range expect {
		if string(result[i].Key) != e.key || string(result[i].Value) != e.val {
			t.Errorf("result[%d]: got (%s, %s), want (%s, %s)", i, result[i].Key, result[i].Value, e.key, e.val)
		}
	}
}

// TestRYWGetRange_InterleavedReverse verifies two-pointer merge in reverse.
func TestRYWGetRange_InterleavedReverse(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	c.set([]byte("B"), []byte("write-b"))
	c.set([]byte("D"), []byte("write-d"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		if !reverse {
			t.Fatal("expected reverse")
		}
		return []KeyValue{
			{Key: []byte("E"), Value: []byte("server-e")},
			{Key: []byte("C"), Value: []byte("server-c")},
			{Key: []byte("A"), Value: []byte("server-a")},
		}, false, nil
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, true, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	// Reverse order: E, D, C, B, A
	expect := []string{"E", "D", "C", "B", "A"}
	if len(result) != len(expect) {
		keys := make([]string, len(result))
		for i, kv := range result {
			keys[i] = string(kv.Key)
		}
		t.Fatalf("expected %d results, got %d: %v", len(expect), len(result), keys)
	}
	for i, e := range expect {
		if string(result[i].Key) != e {
			t.Errorf("result[%d]: got %s, want %s", i, result[i].Key, e)
		}
	}
}

// TestRYWGetRange_WriteOverridesServer verifies that writes at the same key
// as server results take precedence (two-pointer merge shadow logic).
func TestRYWGetRange_WriteOverridesServer(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Set the same keys the server will return.
	c.set([]byte("A"), []byte("write-a"))
	c.set([]byte("C"), []byte("write-c"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("server-a")},
			{Key: []byte("B"), Value: []byte("server-b")},
			{Key: []byte("C"), Value: []byte("server-c")},
		}, false, nil
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	expect := []struct{ key, val string }{
		{"A", "write-a"}, {"B", "server-b"}, {"C", "write-c"},
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}
	for i, e := range expect {
		if string(result[i].Key) != e.key || string(result[i].Value) != e.val {
			t.Errorf("result[%d]: got (%s, %s), want (%s, %s)", i, result[i].Key, result[i].Value, e.key, e.val)
		}
	}
}

// TestRYWGetRange_ManyWritesFewInRange exercises the sorted-keys optimization:
// 1000 writes outside the scan range should not slow down the range query.
func TestRYWGetRange_ManyWritesFewInRange(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// 1000 writes in "Z" prefix (outside scan range).
	for i := 0; i < 1000; i++ {
		key := []byte("Z" + string(rune('A'+i%26)) + string(rune('A'+i/26)))
		c.set(key, []byte("noise"))
	}
	// 2 writes in scan range.
	c.set([]byte("B"), []byte("write-b"))
	c.set([]byte("D"), []byte("write-d"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("a")},
			{Key: []byte("C"), Value: []byte("c")},
			{Key: []byte("E"), Value: []byte("e")},
		}, false, nil
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("F"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	expect := []string{"A", "B", "C", "D", "E"}
	if len(result) != len(expect) {
		t.Fatalf("expected %d results, got %d", len(expect), len(result))
	}
	for i, e := range expect {
		if string(result[i].Key) != e {
			t.Errorf("result[%d]: got %s, want %s", i, result[i].Key, e)
		}
	}
}

// TestRYWGetRange_HasWritesInRangeBinarySearch verifies the O(log N)
// hasWritesInRangeLocked optimization.
func TestRYWGetRange_HasWritesInRangeBinarySearch(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Only writes outside the scan range — fast path should trigger.
	c.set([]byte("Z1"), []byte("out"))
	c.set([]byte("Z2"), []byte("out"))
	c.set([]byte("Z3"), []byte("out"))

	serverCalled := false
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		serverCalled = true
		return []KeyValue{{Key: []byte("B"), Value: []byte("b")}}, false, nil
	}

	// Scan [A, F) — no writes in range, but clears check needed.
	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("F"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if !serverCalled {
		t.Error("expected fast-path server call")
	}
	if len(result) != 1 || string(result[0].Key) != "B" {
		t.Errorf("expected [B], got %v", result)
	}
}

// TestRYWGetRange_ServerMoreWithLimit tests that when we hit the limit from
// merged results of a batch where server had more, more is set correctly.
func TestRYWGetRange_ServerMoreWithLimit(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Clear key "B" to create a gap. Local write at "A\x01".
	c.clear([]byte("B"))
	c.set([]byte("A\x01"), []byte("inserted"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("a")},
			{Key: []byte("B"), Value: []byte("b")},
			{Key: []byte("C"), Value: []byte("c")},
		}, true, nil
	}

	// Limit 2: A, A\x01 (B cleared, C not taken). Server had more → more=true.
	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 2, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if !more {
		t.Error("expected more=true (server had more and batch had excess)")
	}
	if len(result) != 2 {
		keys := make([]string, len(result))
		for i, kv := range result {
			keys[i] = string(kv.Key)
		}
		t.Fatalf("expected 2 results, got %d: %v", len(result), keys)
	}
}
