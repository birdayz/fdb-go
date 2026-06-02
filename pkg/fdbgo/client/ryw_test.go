package client

import (
	"bytes"
	"context"
	"fmt"
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

// Edge case tests for RYW correctness.

// TestRYWGetRange_EmptyRange verifies that an empty scan range returns nothing.
func TestRYWGetRange_EmptyRange(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	c.set([]byte("A"), []byte("v"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return nil, false, nil
	}

	// begin >= end → empty range (should not return writes either)
	result, more, err := c.getRange(context.Background(), []byte("Z"), []byte("A"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more || len(result) != 0 {
		t.Errorf("empty range: got %d results, more=%v", len(result), more)
	}
}

// TestRYWGetRange_ClearRangeOptimized verifies that clearRange uses
// sorted keys (O(log N + k)) to delete writes in the cleared range.
func TestRYWGetRange_ClearRangeOptimized(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Set 100 keys A00..A99, then ClearRange [A30, A60).
	for i := 0; i < 100; i++ {
		key := []byte(fmt.Sprintf("A%02d", i))
		c.set(key, []byte("v"))
	}
	c.clearRange([]byte("A30"), []byte("A60"))

	// Verify: A30..A59 should be gone from writes.
	c.mu.Lock()
	for i := 30; i < 60; i++ {
		key := fmt.Sprintf("A%02d", i)
		if _, ok := c.writes[key]; ok {
			t.Errorf("key %s should have been cleared from writes", key)
		}
	}
	// A00..A29 and A60..A99 should still exist.
	count := len(c.writes)
	c.mu.Unlock()
	if count != 70 {
		t.Errorf("expected 70 remaining writes, got %d", count)
	}
}

// TestRYWGetRange_WriteAtBoundaryKey verifies that a write at exactly
// the boundary key is included in the merge.
func TestRYWGetRange_WriteAtBoundaryKey(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Write at the exact key the server returns as the last result.
	c.set([]byte("C"), []byte("write-c"))

	callCount := 0
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		callCount++
		switch callCount {
		case 1:
			return []KeyValue{
				{Key: []byte("A"), Value: []byte("a")},
				{Key: []byte("B"), Value: []byte("b")},
				{Key: []byte("C"), Value: []byte("server-c")},
			}, true, nil // boundary = C
		case 2:
			// After advancing past C, no more data.
			return []KeyValue{
				{Key: []byte("D"), Value: []byte("d")},
			}, false, nil
		default:
			t.Fatalf("unexpected server call #%d", callCount)
			return nil, false, nil
		}
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	// Write at C should override server value at C.
	found := false
	for _, kv := range result {
		if string(kv.Key) == "C" {
			found = true
			if string(kv.Value) != "write-c" {
				t.Errorf("write at boundary should override server: got %q", kv.Value)
			}
		}
	}
	if !found {
		t.Error("boundary key C not in results")
	}
}

// TestRYWGetRange_CompareAndClearInRange verifies that CompareAndClear
// atomic mutations that resolve to "clear" are handled correctly.
func TestRYWGetRange_CompareAndClearInRange(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// CompareAndClear: if server value == param, clear the key.
	c.atomic(MutCompareAndClear, []byte("B"), []byte("match"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("a")},
			{Key: []byte("B"), Value: []byte("match")}, // matches → cleared
			{Key: []byte("C"), Value: []byte("c")},
		}, false, nil
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	// B should be cleared (CompareAndClear matched).
	if len(result) != 2 {
		keys := make([]string, len(result))
		for i, kv := range result {
			keys[i] = string(kv.Key)
		}
		t.Fatalf("expected 2 results (B cleared), got %d: %v", len(result), keys)
	}
	if string(result[0].Key) != "A" || string(result[1].Key) != "C" {
		t.Errorf("expected [A, C], got [%s, %s]", result[0].Key, result[1].Key)
	}
}

// TestRYWGetRange_SetThenClearRange verifies that ClearRange after Set
// properly removes the write and the key doesn't appear in results.
func TestRYWGetRange_SetThenClearRange(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	c.set([]byte("B"), []byte("written"))
	c.clearRange([]byte("B"), []byte("C"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("a")},
			{Key: []byte("B"), Value: []byte("server-b")},
			{Key: []byte("C"), Value: []byte("c")},
		}, false, nil
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	// B should be cleared (ClearRange after Set).
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if string(result[0].Key) != "A" || string(result[1].Key) != "C" {
		t.Errorf("expected [A, C], got [%s, %s]", result[0].Key, result[1].Key)
	}
}

// TestRYWGetRange_ClearThenSet verifies that Set after ClearRange wins
// (write takes precedence over clear).
func TestRYWGetRange_ClearThenSet(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	c.clearRange([]byte("A"), []byte("Z"))
	c.set([]byte("B"), []byte("restored"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("a")},
			{Key: []byte("C"), Value: []byte("c")},
		}, false, nil
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	// A and C cleared. B restored by Set after ClearRange.
	if len(result) != 1 {
		keys := make([]string, len(result))
		for i, kv := range result {
			keys[i] = string(kv.Key)
		}
		t.Fatalf("expected 1 result (B restored), got %d: %v", len(result), keys)
	}
	if string(result[0].Key) != "B" || string(result[0].Value) != "restored" {
		t.Errorf("expected (B, restored), got (%s, %s)", result[0].Key, result[0].Value)
	}
}

// TestRYWGetRange_Limit1Reverse verifies the PermutedMinMax pattern:
// reverse scan with limit=1 to find the maximum key.
func TestRYWGetRange_Limit1Reverse(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Local writes only, no server data.
	c.set([]byte("A"), []byte("1"))
	c.set([]byte("B"), []byte("2"))
	c.set([]byte("C"), []byte("3"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return nil, false, nil
	}

	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 1, true, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if string(result[0].Key) != "C" {
		t.Errorf("expected max key C, got %s", result[0].Key)
	}
	if !more {
		t.Error("expected more=true (B, A remain)")
	}
}

// TestRYWClearedRangeMerge verifies the optimized addClearedRangeLocked
// correctly merges overlapping and adjacent ranges.
func TestRYWClearedRangeMerge(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Add non-overlapping ranges: [A,C), [E,G), [I,K)
	c.addClearedRange([]byte("A"), []byte("C"))
	c.addClearedRange([]byte("E"), []byte("G"))
	c.addClearedRange([]byte("I"), []byte("K"))

	c.mu.Lock()
	if len(c.cleared) != 3 {
		t.Fatalf("expected 3 cleared ranges, got %d", len(c.cleared))
	}
	c.mu.Unlock()

	// Merge overlapping: [D,F) overlaps [E,G) → should merge to [D,G)
	c.addClearedRange([]byte("D"), []byte("F"))
	c.mu.Lock()
	if len(c.cleared) != 3 {
		t.Fatalf("expected 3 cleared ranges after overlap merge, got %d", len(c.cleared))
	}
	c.mu.Unlock()

	// Adjacent: [C,D) bridges [A,C) and [D,G) → should merge to [A,G)
	c.addClearedRange([]byte("C"), []byte("D"))
	c.mu.Lock()
	if len(c.cleared) != 2 {
		t.Fatalf("expected 2 cleared ranges after adjacent merge, got %d: %v", len(c.cleared), c.cleared)
	}
	// Verify: [A,G) and [I,K)
	if string(c.cleared[0].begin) != "A" || string(c.cleared[0].end) != "G" {
		t.Errorf("range[0]: expected [A,G), got [%s,%s)", c.cleared[0].begin, c.cleared[0].end)
	}
	if string(c.cleared[1].begin) != "I" || string(c.cleared[1].end) != "K" {
		t.Errorf("range[1]: expected [I,K), got [%s,%s)", c.cleared[1].begin, c.cleared[1].end)
	}
	c.mu.Unlock()

	// Span all: [A,Z) merges everything into one.
	c.addClearedRange([]byte("A"), []byte("Z"))
	c.mu.Lock()
	if len(c.cleared) != 1 {
		t.Fatalf("expected 1 cleared range after spanning merge, got %d", len(c.cleared))
	}
	if string(c.cleared[0].begin) != "A" || string(c.cleared[0].end) != "Z" {
		t.Errorf("range[0]: expected [A,Z), got [%s,%s)", c.cleared[0].begin, c.cleared[0].end)
	}
	c.mu.Unlock()
}

// TestRYWGetRange_AtomicOnClearedKeyInvalidatesSortedKeys reproduces the bug
// where atomic() on a cleared key adds a new write without invalidating
// sortedKeys, causing getRange to miss the write via the fast path.
func TestRYWGetRange_AtomicOnClearedKeyInvalidatesSortedKeys(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	// Step 1: set a key to force sorted keys to be built.
	c.set([]byte("X"), []byte("x"))

	// Step 2: trigger ensureSortedLocked via hasWritesInRangeLocked.
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return nil, false, nil
	}
	c.getRange(context.Background(), []byte("X"), []byte("Z"), 10, false, mockServer)

	// Step 3: clearRange [A, B) — no writes in range, sortedKeys stays valid.
	c.clearRange([]byte("A"), []byte("B"))

	// Step 4: atomic on cleared key "A" — resolves to new write.
	c.atomic(MutAddValue, []byte("A"), []byte{5, 0, 0, 0, 0, 0, 0, 0})

	// Step 5: getRange [A, B) must see the write at "A".
	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("B"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result (atomic on cleared key), got %d", len(result))
	}
	if string(result[0].Key) != "A" {
		t.Errorf("expected key A, got %s", result[0].Key)
	}
}

// Benchmarks for RYW merge performance.

// BenchmarkRYWMergeBatch_FewWrites benchmarks mergeBatch with 10 writes
// in the scan range and 50 server results.
func BenchmarkRYWMergeBatch_FewWrites(b *testing.B) {
	c := &rywCache{}
	for i := 0; i < 10; i++ {
		key := make([]byte, 4)
		key[0] = 'B'
		key[1] = byte('A' + i)
		c.set(key, []byte("value"))
	}

	serverKVs := make([]KeyValue, 50)
	for i := range serverKVs {
		key := make([]byte, 4)
		key[0] = 'A'
		key[1] = byte(i)
		serverKVs[i] = KeyValue{Key: key, Value: []byte("server")}
	}

	b.ResetTimer()
	for b.Loop() {
		c.mergeBatch(serverKVs, []byte("A"), []byte("C"), nil, false)
	}
}

// BenchmarkRYWMergeBatch_ManyWritesOutOfRange benchmarks the O(log N) advantage:
// 10000 writes outside the scan range should not slow down merge.
func BenchmarkRYWMergeBatch_ManyWritesOutOfRange(b *testing.B) {
	c := &rywCache{}
	// 10000 writes in Z* prefix.
	for i := 0; i < 10000; i++ {
		key := make([]byte, 6)
		key[0] = 'Z'
		key[1] = byte(i >> 16)
		key[2] = byte(i >> 8)
		key[3] = byte(i)
		c.set(key, []byte("noise"))
	}
	// 5 writes in scan range.
	for i := 0; i < 5; i++ {
		key := []byte{byte('B'), byte('A' + i)}
		c.set(key, []byte("value"))
	}

	serverKVs := make([]KeyValue, 20)
	for i := range serverKVs {
		key := []byte{byte('A'), byte(i)}
		serverKVs[i] = KeyValue{Key: key, Value: []byte("server")}
	}

	b.ResetTimer()
	for b.Loop() {
		c.mergeBatch(serverKVs, []byte("A"), []byte("C"), nil, false)
	}
}

// BenchmarkRYWMergeBatch_WithBoundary benchmarks merge with a knowledge
// boundary (serverMore=true scenario).
func BenchmarkRYWMergeBatch_WithBoundary(b *testing.B) {
	c := &rywCache{}
	for i := 0; i < 20; i++ {
		key := []byte{byte('A' + i)}
		c.set(key, []byte("value"))
	}

	serverKVs := make([]KeyValue, 10)
	for i := range serverKVs {
		key := []byte{byte('A' + i)}
		serverKVs[i] = KeyValue{Key: key, Value: []byte("server")}
	}
	boundary := []byte{byte('A' + 9)} // boundary at key "J"

	b.ResetTimer()
	for b.Loop() {
		c.mergeBatch(serverKVs, []byte("A"), []byte("Z"), boundary, false)
	}
}

// BenchmarkRYWGetRange_WithClears benchmarks the iterative fetch+merge loop
// when clears consume results.
func BenchmarkRYWGetRange_WithClears(b *testing.B) {
	c := &rywCache{}
	// Clear first half of the range.
	c.clearRange([]byte{0}, []byte{128})

	calls := 0
	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		calls++
		if calls%2 == 1 {
			// First call: return cleared keys.
			kvs := make([]KeyValue, 10)
			for i := range kvs {
				kvs[i] = KeyValue{Key: []byte{byte(i)}, Value: []byte("cleared")}
			}
			return kvs, true, nil
		}
		// Second call: return surviving keys.
		kvs := make([]KeyValue, 10)
		for i := range kvs {
			kvs[i] = KeyValue{Key: []byte{byte(128 + i)}, Value: []byte("live")}
		}
		return kvs, false, nil
	}

	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		calls = 0
		c.getRange(ctx, []byte{0}, []byte{255}, 10, false, mockServer)
	}
}

// BenchmarkSnapshotCacheGetKey benchmarks single-key lookup in a populated cache.
func BenchmarkSnapshotCacheGetKey(b *testing.B) {
	var sc snapshotCache
	// Insert a range with 1000 KVs.
	kvs := make([]KeyValue, 1000)
	for i := range kvs {
		kvs[i] = KeyValue{
			Key:   []byte(fmt.Sprintf("k%04d", i)),
			Value: []byte("v"),
		}
	}
	sc.insert([]byte("k0000"), []byte("k9999"), kvs)
	lookupKey := []byte("k0500")

	b.ResetTimer()
	for b.Loop() {
		sc.getKey(lookupKey)
	}
}

// BenchmarkSnapshotCacheGetRange benchmarks full-range lookup in a populated cache.
func BenchmarkSnapshotCacheGetRange(b *testing.B) {
	var sc snapshotCache
	kvs := make([]KeyValue, 1000)
	for i := range kvs {
		kvs[i] = KeyValue{
			Key:   []byte(fmt.Sprintf("k%04d", i)),
			Value: []byte("v"),
		}
	}
	sc.insert([]byte("k0000"), []byte("k9999"), kvs)

	b.ResetTimer()
	for b.Loop() {
		sc.getRangeKVs([]byte("k0000"), []byte("k9999"))
	}
}

// BenchmarkRYWHasWritesInRange benchmarks the binary search optimization
// for checking if writes exist in a range.
func BenchmarkRYWHasWritesInRange(b *testing.B) {
	c := &rywCache{}
	// 10000 writes spread across the keyspace.
	for i := 0; i < 10000; i++ {
		key := make([]byte, 4)
		key[0] = byte(i >> 8)
		key[1] = byte(i)
		c.set(key, []byte("v"))
	}

	begin := []byte{0x80, 0}
	end := []byte{0x80, 0xFF}

	b.ResetTimer()
	for b.Loop() {
		c.mu.Lock()
		c.hasWritesInRangeLocked(begin, end)
		c.mu.Unlock()
	}
}

// BenchmarkRYWAddClearedRange benchmarks the optimized addClearedRangeLocked
// with many existing non-overlapping ranges.
func BenchmarkRYWAddClearedRange(b *testing.B) {
	// Pre-populate with 1000 non-overlapping ranges.
	c := &rywCache{}
	for i := 0; i < 1000; i++ {
		begin := []byte{byte(i / 256), byte(i % 256), 0}
		end := []byte{byte(i / 256), byte(i % 256), 0xFF}
		c.addClearedRange(begin, end)
	}

	// Benchmark: add a range that doesn't overlap any existing range.
	newBegin := []byte{0xFF, 0, 0}
	newEnd := []byte{0xFF, 0, 0xFF}

	b.ResetTimer()
	for b.Loop() {
		c.addClearedRange(newBegin, newEnd)
		// Remove it to keep the benchmark repeatable.
		c.mu.Lock()
		c.cleared = c.cleared[:len(c.cleared)-1]
		c.mu.Unlock()
	}
}

// TestRYWGetRange_UnlimitedWithClears verifies that limit=0 (unlimited)
// correctly enters the slow path when clears are present. This was a bug:
// remaining=0 caused `for remaining > 0` to skip the entire merge loop.
func TestRYWGetRange_UnlimitedWithClears(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	c.set([]byte("A"), []byte("a"))
	c.set([]byte("B"), []byte("b"))
	c.set([]byte("C"), []byte("c"))
	c.clear([]byte("B"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return nil, false, nil // empty server — all data is local
	}

	// limit=0 means unlimited.
	result, more, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 0, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if more {
		t.Error("expected more=false")
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 results (A, C), got %d", len(result))
	}
	if string(result[0].Key) != "A" || string(result[1].Key) != "C" {
		t.Errorf("unlimited forward with clears: expected [A, C], got [%s, %s]", result[0].Key, result[1].Key)
	}
}

// TestRYWGetRange_UnlimitedWithWrites verifies limit=0 works for the
// writes-only slow path (no clears, just writes that need merging).
func TestRYWGetRange_UnlimitedWithWrites(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	c.set([]byte("B"), []byte("local-b"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{
			{Key: []byte("A"), Value: []byte("server-a")},
			{Key: []byte("C"), Value: []byte("server-c")},
		}, false, nil
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 0, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 results (A, B, C), got %d", len(result))
	}
	if string(result[1].Key) != "B" || string(result[1].Value) != "local-b" {
		t.Errorf("expected B=local-b, got %s=%s", result[1].Key, result[1].Value)
	}
}

// TestRYWGetRange_UnlimitedReverse verifies limit=0 in reverse with clears.
func TestRYWGetRange_UnlimitedReverse(t *testing.T) {
	t.Parallel()
	c := &rywCache{}

	c.set([]byte("A"), []byte("a"))
	c.set([]byte("B"), []byte("b"))
	c.set([]byte("C"), []byte("c"))
	c.clear([]byte("B"))

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		if !reverse {
			t.Fatal("expected reverse=true")
		}
		return nil, false, nil
	}

	result, _, err := c.getRange(context.Background(), []byte("A"), []byte("Z"), 0, true, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 results (C, A), got %d", len(result))
	}
	if string(result[0].Key) != "C" || string(result[1].Key) != "A" {
		t.Errorf("expected [C, A], got [%s, %s]", result[0].Key, result[1].Key)
	}
}

// TestRYWGetRange_V2AtomicOnPresentEmpty pins the RFC-056a fix: an atomic that
// makes a key present-empty (Xor(k,"") → "") followed by a V2 op (MinV2) must treat
// the base as PRESENT (do the min) — not absent (return the operand). The merge
// chain keeps present-empty results non-nil (nil reserved for absent), mirroring
// C++ Optional.present() (Atomic.h doMinV2/doAndV2). Pre-fix the chain returned nil
// after the Xor, so MinV2 took the absent→operand path (0x30 instead of 0x00).
func TestRYWGetRange_V2AtomicOnPresentEmpty(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	c.atomic(MutXor, []byte("k"), []byte(""))    // k → present with empty value
	c.atomic(MutMinV2, []byte("k"), []byte("0")) // 0x30; min("","0")=0x00, NOT operand 0x30

	mockServer := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return nil, false, nil // k absent in storage
	}
	result, _, err := c.getRange(context.Background(), []byte("a"), []byte("z"), 10, false, mockServer)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result (k present-empty after MinV2), got %d: %v", len(result), result)
	}
	if !bytes.Equal(result[0].Value, []byte{0x00}) {
		t.Errorf("MinV2 on present-empty key: got %x, want 0x00 (little-endian min of \"\" and \"0\"); 0x30 means the absent→operand bug", result[0].Value)
	}
}

// TestRYW_VersionstampedAbsentNoPhantom pins the codex RFC-056a finding: an
// unresolved versionstamped mutation on an ABSENT key (applyAtomic → (nil,false),
// the stamp is unknown until commit) must NOT surface as a phantom empty key in a
// pre-commit read. The present-empty normalization is gated to exclude versionstamp
// ops; Get reads absent (nil) and GetRange omits it (Go's approximation of C++
// unreadable), consistently. Pre-fix the normalization made it a phantom {k, ""}.
func TestRYW_VersionstampedAbsentNoPhantom(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	val := make([]byte, 14) // value w/ room for stamp+offset; unresolved client-side anyway
	c.atomic(MutSetVersionstampedValue, []byte("vsk"), val)

	absent := func(ctx context.Context, key []byte) ([]byte, error) { return nil, nil }
	absentRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return nil, false, nil
	}

	// GetRange must omit the unresolved versionstamped key (no phantom).
	result, _, err := c.getRange(context.Background(), []byte("a"), []byte("z"), 10, false, absentRange)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	for _, kv := range result {
		if string(kv.Key) == "vsk" {
			t.Fatalf("versionstamped-pending absent key must NOT appear in GetRange (phantom): %v", result)
		}
	}
	// Get must also read absent (nil).
	got, err := c.get(context.Background(), []byte("vsk"), absent)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("versionstamped-pending absent key: Get must be nil (absent), got %x", got)
	}
}

// TestRYW_VersionstampedOverClearedOrPlainNoPhantom pins the codex re-review
// finding on RFC-056a: the no-phantom behavior must hold when the key is absent
// NOT because storage lacks it, but because THIS txn made it absent (a prior local
// clear) — and, symmetrically, when a pending plain Set precedes the versionstamp.
// Both used to be eagerly folded by atomic() into a plain rywEntry (the cleared
// branch stored {value:nil}; the plain-Set branch left the stale value), so the
// versionstamp surfaced as a present key in GetRange/Get despite the read-path gate
// (which only runs on hasAtomics entries). atomic() now refuses to eager-fold a
// versionstamped op, so all three base states (storage-absent, locally cleared,
// pending plain Set) route through the unresolved-atomics path and read as absent.
func TestRYW_VersionstampedOverClearedOrPlainNoPhantom(t *testing.T) {
	t.Parallel()
	val := make([]byte, 14) // operand w/ stamp room; unresolved client-side

	// Server STILL has the key — proves "absent" comes from the txn, not storage.
	withStorage := func(ctx context.Context, key []byte) ([]byte, error) {
		return []byte("storage"), nil
	}
	withStorageRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return []KeyValue{{Key: []byte("vsk"), Value: []byte("storage")}}, false, nil
	}

	assertAbsent := func(t *testing.T, c *rywCache) {
		t.Helper()
		result, _, err := c.getRange(context.Background(), []byte("a"), []byte("z"), 10, false, withStorageRange)
		if err != nil {
			t.Fatalf("getRange: %v", err)
		}
		for _, kv := range result {
			if string(kv.Key) == "vsk" {
				t.Fatalf("versionstamp over cleared/plain must NOT appear in GetRange (phantom): %v", result)
			}
		}
		got, err := c.get(context.Background(), []byte("vsk"), withStorage)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != nil {
			t.Fatalf("versionstamp over cleared/plain: Get must be nil (absent), got %x", got)
		}
	}

	// (1) Cleared earlier in the txn, then versionstamped (codex's exact repro).
	t.Run("cleared_then_versionstamp", func(t *testing.T) {
		t.Parallel()
		c := &rywCache{}
		c.clear([]byte("vsk"))
		c.atomic(MutSetVersionstampedValue, []byte("vsk"), val)
		assertAbsent(t, c)
	})

	// (2) Pending plain Set, then versionstamped — must read absent (unreadable in
	// C++), NOT the stale pre-stamp value.
	t.Run("plain_set_then_versionstamp", func(t *testing.T) {
		t.Parallel()
		c := &rywCache{}
		c.set([]byte("vsk"), []byte("pending"))
		c.atomic(MutSetVersionstampedKey, []byte("vsk"), val)
		assertAbsent(t, c)
	})
}

// TestRYW_VersionstampUnreadableIsSticky pins that an unresolved versionstamp makes
// the key UNREADABLE for the rest of the chain — a later overwriting atomic
// (MutSetValue, which ignores its base) does NOT make it readable again. This
// matches C++ WriteMap exactly: WriteMap.cpp computes
// `is_unreadable = it.is_unreadable() || <op is versionstamp>` (line 97) and the
// stack-replacing fast path for SetValue is guarded by `!it.is_unreadable()`
// (line 125) — so once a versionstamp poisons the entry, a subsequent SetValue
// only pushes onto the stack and is_unreadable stays true. (A codex re-review
// suggested "continue resolving after a SetValue overwrites the versionstamp";
// that would surface the SetValue value and DIVERGE from C++, which keeps the
// key unreadable. Go approximates unreadable as absent, so the key reads absent
// regardless of a trailing SetValue.) Both orderings are unreadable.
func TestRYW_VersionstampUnreadableIsSticky(t *testing.T) {
	t.Parallel()
	val := make([]byte, 14)
	absent := func(ctx context.Context, key []byte) ([]byte, error) { return nil, nil }
	absentRange := func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error) {
		return nil, false, nil
	}
	assertAbsent := func(t *testing.T, c *rywCache) {
		t.Helper()
		result, _, err := c.getRange(context.Background(), []byte("a"), []byte("z"), 10, false, absentRange)
		if err != nil {
			t.Fatalf("getRange: %v", err)
		}
		for _, kv := range result {
			if string(kv.Key) == "vsk" {
				t.Fatalf("versionstamp poisons the entry: a trailing SetValue must NOT make it readable (C++ keeps it unreadable); got %v", result)
			}
		}
		got, err := c.get(context.Background(), []byte("vsk"), absent)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != nil {
			t.Fatalf("versionstamp-then-SetValue must read absent (C++ unreadable), got %x", got)
		}
	}

	// versionstamp THEN an overwriting SetValue atomic — stays unreadable.
	t.Run("versionstamp_then_setvalue", func(t *testing.T) {
		t.Parallel()
		c := &rywCache{}
		c.atomic(MutSetVersionstampedValue, []byte("vsk"), val)
		c.atomic(MutSetValue, []byte("vsk"), []byte("final"))
		assertAbsent(t, c)
	})

	// SetValue THEN versionstamp — also unreadable (the versionstamp poisons it).
	t.Run("setvalue_then_versionstamp", func(t *testing.T) {
		t.Parallel()
		c := &rywCache{}
		c.atomic(MutSetValue, []byte("vsk"), []byte("first"))
		c.atomic(MutSetVersionstampedValue, []byte("vsk"), val)
		assertAbsent(t, c)
	})
}
