package executor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

var testScanRangeFingerprint = []byte("rfc-205-test-fingerprint")

type rangeSetTestError struct{ message string }

func (e *rangeSetTestError) Error() string { return e.message }

type rangeSetScriptCursor struct {
	items        []int
	pos          int
	pageSize     int
	pageReturned int
	pauseReason  recordlayer.NoNextReason
	stopReason   *recordlayer.NoNextReason
	onNextErr    error
	chargeState  *recordlayer.ScanLimiterState
	chargeBytes  int64
	closeErr     error
	closeCount   *int
	pullCount    *int
	closed       bool
}

func (c *rangeSetScriptCursor) OnNext(
	_ context.Context,
) (recordlayer.RecordCursorResult[int], error) {
	if c.pullCount != nil {
		(*c.pullCount)++
	}
	if c.closed {
		return recordlayer.RecordCursorResult[int]{}, &scanRangeSetCursorClosedError{}
	}
	if c.onNextErr != nil {
		return recordlayer.RecordCursorResult[int]{}, c.onNextErr
	}
	if c.stopReason != nil {
		return recordlayer.NewResultNoNext[int](
			*c.stopReason,
			&recordlayer.StartContinuation{},
		), nil
	}
	if c.pageSize > 0 && c.pageReturned >= c.pageSize && c.pos < len(c.items) {
		return recordlayer.NewResultNoNext[int](
			c.pauseReason,
			recordlayer.NewBytesContinuation(rangeSetPositionBytes(c.pos)),
		), nil
	}
	if c.pos >= len(c.items) {
		return recordlayer.NewResultNoNext[int](
			recordlayer.SourceExhausted,
			&recordlayer.EndContinuation{},
		), nil
	}
	value := c.items[c.pos]
	c.pos++
	c.pageReturned++
	if c.chargeState != nil {
		c.chargeState.AddRecordScanned()
		c.chargeState.AddBytesScanned(c.chargeBytes)
	}
	return recordlayer.NewResultWithValue(
		value,
		recordlayer.NewBytesContinuation(rangeSetPositionBytes(c.pos)),
	), nil
}

func (c *rangeSetScriptCursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	if c.closeCount != nil {
		(*c.closeCount)++
	}
	return c.closeErr
}

func (c *rangeSetScriptCursor) IsClosed() bool { return c.closed }

type rangeSetLiveContinuation struct {
	position *byte
}

func (c *rangeSetLiveContinuation) ToBytes() ([]byte, error) {
	return []byte{*c.position}, nil
}

func (*rangeSetLiveContinuation) IsEnd() bool { return false }

type rangeSetLiveContinuationCursor struct {
	position byte
	done     bool
	closed   bool
}

func (c *rangeSetLiveContinuationCursor) OnNext(
	_ context.Context,
) (recordlayer.RecordCursorResult[int], error) {
	if c.done {
		return recordlayer.NewResultNoNext[int](
			recordlayer.SourceExhausted,
			&recordlayer.EndContinuation{},
		), nil
	}
	c.done = true
	return recordlayer.NewResultWithValue(
		1,
		&rangeSetLiveContinuation{position: &c.position},
	), nil
}

func (c *rangeSetLiveContinuationCursor) Close() error   { c.closed = true; return nil }
func (c *rangeSetLiveContinuationCursor) IsClosed() bool { return c.closed }

type rangeSetFailingContinuation struct{ err error }

func (c *rangeSetFailingContinuation) ToBytes() ([]byte, error) { return nil, c.err }
func (*rangeSetFailingContinuation) IsEnd() bool                { return false }

type rangeSetFailingContinuationCursor struct {
	err    error
	done   bool
	closed bool
}

func (c *rangeSetFailingContinuationCursor) OnNext(
	_ context.Context,
) (recordlayer.RecordCursorResult[int], error) {
	if c.done {
		return recordlayer.NewResultNoNext[int](
			recordlayer.SourceExhausted,
			&recordlayer.EndContinuation{},
		), nil
	}
	c.done = true
	return recordlayer.NewResultWithValue(
		1,
		&rangeSetFailingContinuation{err: c.err},
	), nil
}

func (c *rangeSetFailingContinuationCursor) Close() error   { c.closed = true; return nil }
func (c *rangeSetFailingContinuationCursor) IsClosed() bool { return c.closed }

func rangeSetPositionBytes(position int) []byte {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, uint32(position))
	return data
}

func parseRangeSetPosition(data []byte) (int, error) {
	if data == nil {
		return 0, nil
	}
	if len(data) != 4 {
		return 0, fmt.Errorf("position continuation has length %d, want 4", len(data))
	}
	return int(binary.BigEndian.Uint32(data)), nil
}

func testRangeSetSpec(
	counts []uint32,
	materialized *[][]uint32,
) scanRangeSetSpec {
	return scanRangeSetSpec{
		fingerprint:       testScanRangeFingerprint,
		alternativeCounts: counts,
		materialize: func(choices []uint32) (recordlayer.TupleRange, error) {
			choiceCopy := append([]uint32(nil), choices...)
			if materialized != nil {
				*materialized = append(*materialized, choiceCopy)
			}
			prefix := make(tuple.Tuple, len(choices))
			for i, choice := range choices {
				prefix[i] = int64(choice)
			}
			return recordlayer.TupleRangeAllOf(prefix), nil
		},
	}
}

func testRangeBranch(scanRange recordlayer.TupleRange) int {
	branch := 0
	for _, component := range scanRange.Low {
		branch = branch*10 + int(component.(int64))
	}
	return branch
}

func marshalRangeSetContinuationForTest(
	t *testing.T,
	fingerprint []byte,
	choices []uint32,
	inner []byte,
	childStarted *bool,
) []byte {
	t.Helper()
	continuation := &gen.ScanRangeSetContinuation{
		Fingerprint:       fingerprint,
		Choices:           choices,
		InnerContinuation: inner,
		ChildStarted:      childStarted,
	}
	data, err := continuation.MarshalVT()
	if err != nil {
		t.Fatalf("marshal continuation: %v", err)
	}
	return data
}

func continuationBytesForTest(
	t *testing.T,
	continuation recordlayer.RecordCursorContinuation,
) []byte {
	t.Helper()
	data, err := continuation.ToBytes()
	if err != nil {
		t.Fatalf("serialize continuation: %v", err)
	}
	return data
}

func drainRangeSetCursor(
	t *testing.T,
	cursor recordlayer.RecordCursor[int],
) ([]int, recordlayer.RecordCursorResult[int]) {
	t.Helper()
	values := make([]int, 0)
	for step := 0; step < 10_000; step++ {
		result, err := cursor.OnNext(context.Background())
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !result.HasNext() {
			return values, result
		}
		values = append(values, result.GetValue())
	}
	t.Fatal("cursor did not stop within progress cap")
	return nil, recordlayer.RecordCursorResult[int]{}
}

func TestScanRangeSetCursorOdometerOrderAndSingleLiveChild(t *testing.T) {
	t.Parallel()

	var materialized [][]uint32
	opened := 0
	active := 0
	maxActive := 0
	closeCounts := map[int]*int{}
	open := func(
		scanRange recordlayer.TupleRange,
		inner []byte,
		_ recordlayer.ScanProperties,
	) (recordlayer.RecordCursor[int], error) {
		if inner != nil {
			t.Fatalf("new branch received inner continuation %x", inner)
		}
		branch := testRangeBranch(scanRange)
		opened++
		active++
		if active > maxActive {
			maxActive = active
		}
		closeCount := 0
		closeCounts[branch] = &closeCount
		return &rangeSetScriptCursor{
			items:      []int{branch},
			closeCount: closeCounts[branch],
		}, nil
	}

	// The close counter is observed after the cursor closes each exhausted
	// child. Decrement active from the materialization boundary, where the prior
	// child is guaranteed closed before the next range is materialized.
	spec := testRangeSetSpec([]uint32{2, 2}, nil)
	originalMaterializer := spec.materialize
	spec.materialize = func(choices []uint32) (recordlayer.TupleRange, error) {
		if len(materialized) > 0 {
			active--
		}
		materialized = append(materialized, append([]uint32(nil), choices...))
		return originalMaterializer(choices)
	}

	cursor, err := newScanRangeSetCursor[int](spec, nil, recordlayer.ForwardScan(), open)
	if err != nil {
		t.Fatalf("construct cursor: %v", err)
	}
	values, terminal := drainRangeSetCursor(t, cursor)
	if terminal.GetNoNextReason() != recordlayer.SourceExhausted || !terminal.GetContinuation().IsEnd() {
		t.Fatalf("terminal = reason %v end=%v", terminal.GetNoNextReason(), terminal.GetContinuation().IsEnd())
	}
	if want := []int{0, 1, 10, 11}; !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %v, want %v", values, want)
	}
	if want := [][]uint32{{0, 0}, {0, 1}, {1, 0}, {1, 1}}; !reflect.DeepEqual(materialized, want) {
		t.Fatalf("materialized = %v, want %v", materialized, want)
	}
	if opened != 4 {
		t.Fatalf("opened = %d, want 4", opened)
	}
	if maxActive != 1 {
		t.Fatalf("max active children = %d, want 1", maxActive)
	}
	for branch, count := range closeCounts {
		if *count != 1 {
			t.Errorf("branch %d close count = %d, want 1", branch, *count)
		}
	}

	// Terminal replay must not reopen or repull any child.
	again, replayErr := cursor.OnNext(context.Background())
	if replayErr != nil || again.GetNoNextReason() != recordlayer.SourceExhausted {
		t.Fatalf("terminal replay = (%v,%v)", again.GetNoNextReason(), replayErr)
	}
	if opened != 4 {
		t.Fatalf("terminal replay opened %d children, want 4", opened)
	}
}

func TestScanRangeSetCursorZeroComponentSpecHasOneLeaf(t *testing.T) {
	t.Parallel()

	opened := 0
	spec := testRangeSetSpec(nil, nil)
	cursor, err := newScanRangeSetCursor[int](
		spec,
		nil,
		recordlayer.ForwardScan(),
		func(scanRange recordlayer.TupleRange, _ []byte, _ recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			if len(scanRange.Low) != 0 {
				t.Fatalf("zero-component range low = %v", scanRange.Low)
			}
			return &rangeSetScriptCursor{items: []int{7}}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct cursor: %v", err)
	}
	values, _ := drainRangeSetCursor(t, cursor)
	if !reflect.DeepEqual(values, []int{7}) || opened != 1 {
		t.Fatalf("values/opened = %v/%d, want [7]/1", values, opened)
	}
}

func TestScanRangeSetCursorEmptyOpensNothing(t *testing.T) {
	t.Parallel()

	opened := 0
	spec := scanRangeSetSpec{fingerprint: testScanRangeFingerprint, empty: true}
	cursor, err := newScanRangeSetCursor[int](
		spec,
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetScriptCursor{}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct empty cursor: %v", err)
	}
	_, terminal := drainRangeSetCursor(t, cursor)
	if terminal.GetNoNextReason() != recordlayer.SourceExhausted || opened != 0 {
		t.Fatalf("empty terminal/opened = %v/%d", terminal.GetNoNextReason(), opened)
	}

	started := false
	raw := marshalRangeSetContinuationForTest(
		t,
		testScanRangeFingerprint,
		nil,
		nil,
		&started,
	)
	_, err = newScanRangeSetCursor[int](spec, raw, recordlayer.ForwardScan(), nil)
	var parseErr *recordlayer.ContinuationParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("empty resume error = %T %v, want *ContinuationParseError", err, err)
	}
}

func TestScanRangeSetCursorRejectsInvalidSpecs(t *testing.T) {
	t.Parallel()

	validMaterializer := func([]uint32) (recordlayer.TupleRange, error) {
		return recordlayer.TupleRangeAll, nil
	}
	validFactory := func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
		return &rangeSetScriptCursor{}, nil
	}
	tests := []struct {
		name string
		spec scanRangeSetSpec
		open scanRangeLeafFactory[int]
	}{
		{name: "empty fingerprint", spec: scanRangeSetSpec{materialize: validMaterializer}, open: validFactory},
		{name: "zero alternatives", spec: scanRangeSetSpec{fingerprint: []byte{1}, alternativeCounts: []uint32{2, 0}, materialize: validMaterializer}, open: validFactory},
		{name: "empty with materializer", spec: scanRangeSetSpec{fingerprint: []byte{1}, empty: true, materialize: validMaterializer}, open: validFactory},
		{name: "nonempty without materializer", spec: scanRangeSetSpec{fingerprint: []byte{1}}, open: validFactory},
		{name: "nil factory", spec: scanRangeSetSpec{fingerprint: []byte{1}, materialize: validMaterializer}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := newScanRangeSetCursor[int](test.spec, nil, recordlayer.ForwardScan(), test.open)
			var specErr *invalidScanRangeSetSpecError
			if !errors.As(err, &specErr) {
				t.Fatalf("error = %T %v, want *invalidScanRangeSetSpecError", err, err)
			}
		})
	}
}

func TestScanRangeSetContinuationValidationPrecedesStorage(t *testing.T) {
	t.Parallel()

	started := true
	stopped := false
	tests := []struct {
		name string
		raw  func(*testing.T) []byte
	}{
		{name: "corrupt wire", raw: func(*testing.T) []byte { return []byte{0xff, 0x01} }},
		{name: "missing required fingerprint", raw: func(*testing.T) []byte {
			// packed choices=[0,0], child_started=true, deliberately no
			// required fingerprint field.
			return []byte{0x12, 0x02, 0x00, 0x00, 0x20, 0x01}
		}},
		{name: "wrong fingerprint", raw: func(t *testing.T) []byte {
			return marshalRangeSetContinuationForTest(t, []byte("other"), []uint32{0, 0}, nil, &started)
		}},
		{name: "wrong arity", raw: func(t *testing.T) []byte {
			return marshalRangeSetContinuationForTest(t, testScanRangeFingerprint, []uint32{0}, nil, &started)
		}},
		{name: "out of bounds", raw: func(t *testing.T) []byte {
			return marshalRangeSetContinuationForTest(t, testScanRangeFingerprint, []uint32{0, 2}, nil, &started)
		}},
		{name: "child started absent", raw: func(t *testing.T) []byte {
			return marshalRangeSetContinuationForTest(t, testScanRangeFingerprint, []uint32{0, 0}, nil, nil)
		}},
		{name: "unopened child with inner", raw: func(t *testing.T) []byte {
			return marshalRangeSetContinuationForTest(t, testScanRangeFingerprint, []uint32{0, 0}, []byte{1}, &stopped)
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			materialized := 0
			opened := 0
			spec := testRangeSetSpec([]uint32{2, 2}, nil)
			baseMaterializer := spec.materialize
			spec.materialize = func(choices []uint32) (recordlayer.TupleRange, error) {
				materialized++
				return baseMaterializer(choices)
			}
			_, err := newScanRangeSetCursor[int](
				spec,
				test.raw(t),
				recordlayer.ForwardScan(),
				func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
					opened++
					return &rangeSetScriptCursor{}, nil
				},
			)
			var parseErr *recordlayer.ContinuationParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("error = %T %v, want *ContinuationParseError", err, err)
			}
			if materialized != 0 || opened != 0 {
				t.Fatalf("malformed continuation materialized/opened = %d/%d", materialized, opened)
			}
		})
	}
}

func TestScanRangeSetCursorContinuationSweep(t *testing.T) {
	t.Parallel()

	itemsByBranch := map[int][]int{
		0: {0, 1, 2},
		1: {10, 11},
		2: {20, 21, 22},
	}
	spec := testRangeSetSpec([]uint32{3}, nil)
	var continuation []byte
	got := make([]int, 0)
	for page := 0; page < 20; page++ {
		cursor, err := newScanRangeSetCursor[int](
			spec,
			continuation,
			recordlayer.ForwardScan(),
			func(scanRange recordlayer.TupleRange, inner []byte, _ recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
				position, parseErr := parseRangeSetPosition(inner)
				if parseErr != nil {
					return nil, parseErr
				}
				return &rangeSetScriptCursor{
					items:       itemsByBranch[testRangeBranch(scanRange)],
					pos:         position,
					pageSize:    1,
					pauseReason: recordlayer.ScanLimitReached,
				}, nil
			},
		)
		if err != nil {
			t.Fatalf("page %d construct: %v", page, err)
		}
		values, terminal := drainRangeSetCursor(t, cursor)
		got = append(got, values...)
		if terminal.GetNoNextReason() == recordlayer.SourceExhausted {
			if want := []int{0, 1, 2, 10, 11, 20, 21, 22}; !reflect.DeepEqual(got, want) {
				t.Fatalf("paged values = %v, want %v", got, want)
			}
			return
		}
		if terminal.GetNoNextReason() != recordlayer.ScanLimitReached {
			t.Fatalf("page %d reason = %v", page, terminal.GetNoNextReason())
		}
		continuation = continuationBytesForTest(t, terminal.GetContinuation())
	}
	t.Fatal("continuation sweep exceeded hard progress cap")
}

func TestScanRangeSetCursorContinuationSnapshotsAreImmutable(t *testing.T) {
	t.Parallel()

	child := &rangeSetLiveContinuationCursor{position: 7}
	cursor, err := newScanRangeSetCursor[int](
		testRangeSetSpec([]uint32{1}, nil),
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			return child, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("first result = hasNext %v err %v", result.HasNext(), err)
	}
	wrapped := result.GetContinuation()
	child.position = 99
	data := continuationBytesForTest(t, wrapped)
	var decoded gen.ScanRangeSetContinuation
	if err := decoded.UnmarshalVT(data); err != nil {
		t.Fatalf("decode wrapper: %v", err)
	}
	if !reflect.DeepEqual(decoded.InnerContinuation, []byte{7}) {
		t.Fatalf("inner continuation = %v, want eager snapshot [7]", decoded.InnerContinuation)
	}
}

func TestScanRangeSetCursorOnlySourceExhaustedAdvances(t *testing.T) {
	t.Parallel()

	reasons := []recordlayer.NoNextReason{
		recordlayer.ReturnLimitReached,
		recordlayer.ScanLimitReached,
		recordlayer.ByteLimitReached,
		recordlayer.TimeLimitReached,
	}
	for _, reason := range reasons {
		reason := reason
		t.Run(fmt.Sprintf("reason_%d", reason), func(t *testing.T) {
			t.Parallel()
			var materialized [][]uint32
			opened := 0
			pulls := 0
			cursor, err := newScanRangeSetCursor[int](
				testRangeSetSpec([]uint32{2}, &materialized),
				nil,
				recordlayer.ForwardScan(),
				func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
					opened++
					return &rangeSetScriptCursor{stopReason: &reason, pullCount: &pulls}, nil
				},
			)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			result, err := cursor.OnNext(context.Background())
			if err != nil || result.HasNext() || result.GetNoNextReason() != reason {
				t.Fatalf("stop = hasNext %v reason %v err %v", result.HasNext(), result.GetNoNextReason(), err)
			}
			if opened != 1 || pulls != 1 || !reflect.DeepEqual(materialized, [][]uint32{{0}}) {
				t.Fatalf("opened/pulls/materialized = %d/%d/%v", opened, pulls, materialized)
			}
			data := continuationBytesForTest(t, result.GetContinuation())
			var decoded gen.ScanRangeSetContinuation
			if err := decoded.UnmarshalVT(data); err != nil {
				t.Fatalf("decode continuation: %v", err)
			}
			if !reflect.DeepEqual(decoded.Choices, []uint32{0}) || !decoded.GetChildStarted() {
				t.Fatalf("continuation choices/started = %v/%v", decoded.Choices, decoded.GetChildStarted())
			}

			// Repeated pulls replay the exact stop and cannot advance a branch.
			_, err = cursor.OnNext(context.Background())
			if err != nil || opened != 1 || pulls != 1 {
				t.Fatalf("replay err/opened/pulls = %v/%d/%d", err, opened, pulls)
			}
		})
	}
}

func TestScanRangeSetCursorErrorsNeverAdvance(t *testing.T) {
	t.Parallel()

	t.Run("materializer", func(t *testing.T) {
		t.Parallel()
		wantErr := &rangeSetTestError{message: "materialize"}
		opened := 0
		spec := scanRangeSetSpec{
			fingerprint:       testScanRangeFingerprint,
			alternativeCounts: []uint32{2},
			materialize: func([]uint32) (recordlayer.TupleRange, error) {
				return recordlayer.TupleRange{}, wantErr
			},
		}
		cursor, err := newScanRangeSetCursor[int](spec, nil, recordlayer.ForwardScan(), func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetScriptCursor{}, nil
		})
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		_, err = cursor.OnNext(context.Background())
		if !errors.Is(err, wantErr) || opened != 0 {
			t.Fatalf("error/opened = %v/%d", err, opened)
		}
	})

	t.Run("factory", func(t *testing.T) {
		t.Parallel()
		wantErr := &rangeSetTestError{message: "factory"}
		var materialized [][]uint32
		opened := 0
		cursor, err := newScanRangeSetCursor[int](testRangeSetSpec([]uint32{2}, &materialized), nil, recordlayer.ForwardScan(), func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return nil, wantErr
		})
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		_, err = cursor.OnNext(context.Background())
		if !errors.Is(err, wantErr) || opened != 1 || !reflect.DeepEqual(materialized, [][]uint32{{0}}) {
			t.Fatalf("error/opened/materialized = %v/%d/%v", err, opened, materialized)
		}
	})

	t.Run("child on-next", func(t *testing.T) {
		t.Parallel()
		wantErr := &rangeSetTestError{message: "child"}
		var materialized [][]uint32
		opened := 0
		cursor, err := newScanRangeSetCursor[int](testRangeSetSpec([]uint32{2}, &materialized), nil, recordlayer.ForwardScan(), func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetScriptCursor{onNextErr: wantErr}, nil
		})
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		_, err = cursor.OnNext(context.Background())
		if !errors.Is(err, wantErr) || opened != 1 || !reflect.DeepEqual(materialized, [][]uint32{{0}}) {
			t.Fatalf("error/opened/materialized = %v/%d/%v", err, opened, materialized)
		}
	})

	t.Run("child close", func(t *testing.T) {
		t.Parallel()
		wantErr := &rangeSetTestError{message: "close"}
		var materialized [][]uint32
		opened := 0
		closeCount := 0
		cursor, err := newScanRangeSetCursor[int](testRangeSetSpec([]uint32{2}, &materialized), nil, recordlayer.ForwardScan(), func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetScriptCursor{closeErr: wantErr, closeCount: &closeCount}, nil
		})
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		_, err = cursor.OnNext(context.Background())
		if !errors.Is(err, wantErr) || opened != 1 || closeCount != 1 || !reflect.DeepEqual(materialized, [][]uint32{{0}}) {
			t.Fatalf("error/opened/closed/materialized = %v/%d/%d/%v", err, opened, closeCount, materialized)
		}
		_, againErr := cursor.OnNext(context.Background())
		if !errors.Is(againErr, wantErr) || closeCount != 1 || opened != 1 {
			t.Fatalf("replayed close error/count/opened = %v/%d/%d", againErr, closeCount, opened)
		}
		if closeErr := cursor.Close(); !errors.Is(closeErr, wantErr) {
			t.Fatalf("explicit close = %v, want exhausted-child close error %v", closeErr, wantErr)
		}
		if closeErr := cursor.Close(); !errors.Is(closeErr, wantErr) || closeCount != 1 {
			t.Fatalf("replayed explicit close/count = %v/%d, want %v/1", closeErr, closeCount, wantErr)
		}
	})
}

func TestScanRangeSetCursorCloseIsIdempotentAndActiveOnly(t *testing.T) {
	t.Parallel()

	wantErr := &rangeSetTestError{message: "close active"}
	opened := 0
	closeCount := 0
	cursor, err := newScanRangeSetCursor[int](
		testRangeSetSpec([]uint32{2}, nil),
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetScriptCursor{items: []int{1, 2}, closeErr: wantErr, closeCount: &closeCount}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("first pull = hasNext %v err %v", result.HasNext(), err)
	}
	if err := cursor.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("first close = %v, want %v", err, wantErr)
	}
	if err := cursor.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("second close = %v, want latched %v", err, wantErr)
	}
	if opened != 1 || closeCount != 1 || !cursor.IsClosed() {
		t.Fatalf("opened/closed-count/is-closed = %d/%d/%v", opened, closeCount, cursor.IsClosed())
	}
	_, err = cursor.OnNext(context.Background())
	var closedErr *scanRangeSetCursorClosedError
	if !errors.As(err, &closedErr) {
		t.Fatalf("OnNext after Close error = %T %v", err, err)
	}
}

func TestScanRangeSetCursorCloseBeforeFirstPullOpensNothing(t *testing.T) {
	t.Parallel()

	materialized := 0
	opened := 0
	spec := testRangeSetSpec([]uint32{2}, nil)
	base := spec.materialize
	spec.materialize = func(choices []uint32) (recordlayer.TupleRange, error) {
		materialized++
		return base(choices)
	}
	cursor, err := newScanRangeSetCursor[int](
		spec,
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetScriptCursor{}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if materialized != 0 || opened != 0 || !cursor.IsClosed() {
		t.Fatalf("materialized/opened/closed = %d/%d/%v", materialized, opened, cursor.IsClosed())
	}
}

func TestScanRangeSetCursorContextCancellationPrecedesOpening(t *testing.T) {
	t.Parallel()

	materialized := 0
	opened := 0
	spec := testRangeSetSpec([]uint32{2}, nil)
	base := spec.materialize
	spec.materialize = func(choices []uint32) (recordlayer.TupleRange, error) {
		materialized++
		return base(choices)
	}
	cursor, err := newScanRangeSetCursor[int](
		spec,
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetScriptCursor{}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = cursor.OnNext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OnNext error = %v, want context.Canceled", err)
	}
	if materialized != 0 || opened != 0 {
		t.Fatalf("canceled pull materialized/opened = %d/%d", materialized, opened)
	}
}

func TestScanRangeSetCursorRejectsNilChildWithoutAdvancing(t *testing.T) {
	t.Parallel()

	var materialized [][]uint32
	opened := 0
	cursor, err := newScanRangeSetCursor[int](
		testRangeSetSpec([]uint32{2}, &materialized),
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = cursor.OnNext(context.Background())
	var specErr *invalidScanRangeSetSpecError
	if !errors.As(err, &specErr) || opened != 1 || !reflect.DeepEqual(materialized, [][]uint32{{0}}) {
		t.Fatalf("error/opened/materialized = %T(%v)/%d/%v", err, err, opened, materialized)
	}
	_, againErr := cursor.OnNext(context.Background())
	if !errors.As(againErr, &specErr) || opened != 1 {
		t.Fatalf("terminal replay error/opened = %T(%v)/%d", againErr, againErr, opened)
	}
}

func TestScanRangeSetCursorInnerContinuationErrorDoesNotAdvance(t *testing.T) {
	t.Parallel()

	wantErr := &rangeSetTestError{message: "serialize inner"}
	var materialized [][]uint32
	opened := 0
	cursor, err := newScanRangeSetCursor[int](
		testRangeSetSpec([]uint32{2}, &materialized),
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetFailingContinuationCursor{err: wantErr}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = cursor.OnNext(context.Background())
	if !errors.Is(err, wantErr) || opened != 1 || !reflect.DeepEqual(materialized, [][]uint32{{0}}) {
		t.Fatalf("error/opened/materialized = %v/%d/%v", err, opened, materialized)
	}
}

func TestScanRangeSetCursorNormalizesAndSharesChildProperties(t *testing.T) {
	t.Parallel()

	input := recordlayer.ScanProperties{
		Reverse:             true,
		CursorStreamingMode: recordlayer.StreamingModeSmall,
		ExecuteProperties: recordlayer.ExecuteProperties{
			Skip:                       7,
			ReturnedRowLimit:           11,
			ScannedRecordsLimit:        13,
			ScannedBytesLimit:          17,
			TimeLimit:                  time.Hour,
			FailOnScanLimitReached:     true,
			DefaultCursorStreamingMode: recordlayer.StreamingModeMedium,
		},
	}
	var states []*recordlayer.ScanLimiterState
	cursor, err := newScanRangeSetCursor[int](
		testRangeSetSpec([]uint32{2}, nil),
		nil,
		input,
		func(_ recordlayer.TupleRange, _ []byte, properties recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			states = append(states, properties.ExecuteProperties.ScanState)
			if properties.ExecuteProperties.Skip != 0 || properties.ExecuteProperties.ReturnedRowLimit != 0 {
				t.Fatalf("child skip/row-limit = %d/%d, want 0/0", properties.ExecuteProperties.Skip, properties.ExecuteProperties.ReturnedRowLimit)
			}
			if properties.ExecuteProperties.ScannedRecordsLimit != 13 ||
				properties.ExecuteProperties.ScannedBytesLimit != 17 ||
				properties.ExecuteProperties.TimeLimit != time.Hour ||
				!properties.ExecuteProperties.FailOnScanLimitReached {
				t.Fatalf("child resource properties were not preserved: %+v", properties.ExecuteProperties)
			}
			if !properties.Reverse || properties.CursorStreamingMode != recordlayer.StreamingModeSmall {
				t.Fatalf("child scan shape = reverse %v stream %v", properties.Reverse, properties.CursorStreamingMode)
			}
			return &rangeSetScriptCursor{}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, _ = drainRangeSetCursor(t, cursor)
	if len(states) != 2 || states[0] == nil || states[0] != states[1] {
		t.Fatalf("child ScanStates = %v, want same non-nil pointer", states)
	}
	if input.ExecuteProperties.ScanState != nil {
		t.Fatal("constructor mutated caller's value-copied properties")
	}
}

func TestScanRangeSetCursorWholeRangeInitialPassGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		properties func() recordlayer.ScanProperties
		wantReason recordlayer.NoNextReason
	}{
		{
			name: "records",
			properties: func() recordlayer.ScanProperties {
				properties := recordlayer.ForwardScan()
				properties.ExecuteProperties.ScannedRecordsLimit = 1
				properties.ExecuteProperties.ScanState.AddRecordScanned()
				return properties
			},
			wantReason: recordlayer.ScanLimitReached,
		},
		{
			name: "bytes",
			properties: func() recordlayer.ScanProperties {
				properties := recordlayer.ForwardScan()
				properties.ExecuteProperties.ScannedBytesLimit = 1
				properties.ExecuteProperties.ScanState.AddBytesScanned(1)
				return properties
			},
			wantReason: recordlayer.ByteLimitReached,
		},
		{
			name: "time",
			properties: func() recordlayer.ScanProperties {
				properties := recordlayer.ForwardScan()
				properties.ExecuteProperties.TimeLimit = time.Nanosecond
				return properties
			},
			wantReason: recordlayer.TimeLimitReached,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			opened := make([]int, 0)
			cursor, err := newScanRangeSetCursor[int](
				testRangeSetSpec([]uint32{2}, nil),
				nil,
				test.properties(),
				func(scanRange recordlayer.TupleRange, _ []byte, _ recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
					opened = append(opened, testRangeBranch(scanRange))
					return &rangeSetScriptCursor{}, nil // the first physical branch is empty
				},
			)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			result, err := cursor.OnNext(context.Background())
			if err != nil || result.HasNext() || result.GetNoNextReason() != test.wantReason {
				t.Fatalf("result = hasNext %v reason %v err %v", result.HasNext(), result.GetNoNextReason(), err)
			}
			if !reflect.DeepEqual(opened, []int{0}) {
				t.Fatalf("opened branches = %v, want only empty first branch", opened)
			}
			data := continuationBytesForTest(t, result.GetContinuation())
			var continuation gen.ScanRangeSetContinuation
			if err := continuation.UnmarshalVT(data); err != nil {
				t.Fatalf("decode branch-start continuation: %v", err)
			}
			if !reflect.DeepEqual(continuation.Choices, []uint32{1}) || continuation.GetChildStarted() {
				t.Fatalf("branch-start continuation choices/started = %v/%v", continuation.Choices, continuation.GetChildStarted())
			}
		})
	}
}

func TestScanRangeSetCursorWholeRangeGateFailMode(t *testing.T) {
	t.Parallel()

	properties := recordlayer.ForwardScan()
	properties.ExecuteProperties.ScannedRecordsLimit = 1
	properties.ExecuteProperties.FailOnScanLimitReached = true
	properties.ExecuteProperties.ScanState.AddRecordScanned()
	opened := 0
	cursor, err := newScanRangeSetCursor[int](
		testRangeSetSpec([]uint32{2}, nil),
		nil,
		properties,
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return &rangeSetScriptCursor{}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = cursor.OnNext(context.Background())
	var limitErr *recordlayer.ScanLimitReachedError
	if !errors.As(err, &limitErr) || limitErr.Reason != recordlayer.ScanLimitReached {
		t.Fatalf("error = %T %v, want scan-limit failure", err, err)
	}
	if opened != 1 {
		t.Fatalf("opened = %d, want first branch only", opened)
	}
}

func TestScanRangeSetCursorLimitAtBranchBoundaryAndFreshPagePass(t *testing.T) {
	t.Parallel()

	properties := recordlayer.ForwardScan()
	properties.ExecuteProperties.ScannedRecordsLimit = 1
	state := properties.ExecuteProperties.ScanState
	opened := make([]int, 0)
	spec := testRangeSetSpec([]uint32{2}, nil)
	first, err := newScanRangeSetCursor[int](
		spec,
		nil,
		properties,
		func(scanRange recordlayer.TupleRange, _ []byte, _ recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			branch := testRangeBranch(scanRange)
			opened = append(opened, branch)
			return &rangeSetScriptCursor{items: []int{branch}, chargeState: state}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct first page: %v", err)
	}
	firstValue, err := first.OnNext(context.Background())
	if err != nil || !firstValue.HasNext() || firstValue.GetValue() != 0 {
		t.Fatalf("first value = %+v err %v", firstValue, err)
	}
	stop, err := first.OnNext(context.Background())
	if err != nil || stop.GetNoNextReason() != recordlayer.ScanLimitReached {
		t.Fatalf("boundary stop = reason %v err %v", stop.GetNoNextReason(), err)
	}
	if !reflect.DeepEqual(opened, []int{0}) {
		t.Fatalf("first page opened = %v, want [0]", opened)
	}

	// Even with a pre-exhausted state, a resumed logical range-set cursor gets
	// one fresh page attempt at the unopened second branch. It must not inherit
	// initialPassUsed from the prior cursor.
	continuation := continuationBytesForTest(t, stop.GetContinuation())
	resumedProperties := recordlayer.ForwardScan()
	resumedProperties.ExecuteProperties.ScannedRecordsLimit = 1
	resumedProperties.ExecuteProperties.ScanState.AddRecordScanned()
	resumedOpened := make([]int, 0)
	resumed, err := newScanRangeSetCursor[int](
		spec,
		continuation,
		resumedProperties,
		func(scanRange recordlayer.TupleRange, _ []byte, _ recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			branch := testRangeBranch(scanRange)
			resumedOpened = append(resumedOpened, branch)
			return &rangeSetScriptCursor{items: []int{branch}}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct resumed page: %v", err)
	}
	resumedValue, err := resumed.OnNext(context.Background())
	if err != nil || !resumedValue.HasNext() || resumedValue.GetValue() != 1 {
		t.Fatalf("resumed value = hasNext %v value %v err %v", resumedValue.HasNext(), func() any {
			if resumedValue.HasNext() {
				return resumedValue.GetValue()
			}
			return nil
		}(), err)
	}
	if !reflect.DeepEqual(resumedOpened, []int{1}) {
		t.Fatalf("resumed opened = %v, want [1]", resumedOpened)
	}
}

func TestScanRangeSetCursorStartContinuationPresence(t *testing.T) {
	t.Parallel()

	cursor, err := newScanRangeSetCursor[int](
		testRangeSetSpec([]uint32{1}, nil),
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			return &rangeSetScriptCursor{stopReason: func() *recordlayer.NoNextReason {
				reason := recordlayer.ScanLimitReached
				return &reason
			}()}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	result, err := cursor.OnNext(context.Background())
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	data := continuationBytesForTest(t, result.GetContinuation())
	var continuation gen.ScanRangeSetContinuation
	if err := continuation.UnmarshalVT(data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !continuation.GetChildStarted() || continuation.InnerContinuation != nil {
		t.Fatalf("started/inner = %v/%v, want true/absent", continuation.GetChildStarted(), continuation.InnerContinuation)
	}
}

func TestScanRangeSetContinuationSizeIsLinear(t *testing.T) {
	t.Parallel()

	sizeFor := func(componentCount int) int {
		choices := make([]uint32, componentCount)
		for i := range choices {
			choices[i] = uint32(i & 1)
		}
		continuation := &scanRangeSetContinuation{
			fingerprint:       testScanRangeFingerprint,
			choices:           choices,
			innerContinuation: []byte{1, 2, 3},
			childStarted:      true,
		}
		return len(continuationBytesForTest(t, continuation))
	}
	small := sizeFor(2_000)
	large := sizeFor(4_000)
	if large > 2*small+32 {
		t.Fatalf("continuation growth is super-linear: size(2000)=%d size(4000)=%d", small, large)
	}

	materialized := 0
	counts := make([]uint32, 50_000)
	for i := range counts {
		counts[i] = 2
	}
	spec := testRangeSetSpec(counts, nil)
	base := spec.materialize
	spec.materialize = func(choices []uint32) (recordlayer.TupleRange, error) {
		materialized++
		return base(choices)
	}
	cursor, err := newScanRangeSetCursor[int](
		spec,
		nil,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			return &rangeSetScriptCursor{}, nil
		},
	)
	if err != nil {
		t.Fatalf("construct large cursor: %v", err)
	}
	if materialized != 0 {
		t.Fatalf("constructor materialized %d branches, want zero", materialized)
	}
	concrete := cursor.(*scanRangeSetCursor[int])
	if len(concrete.choices) != len(counts) || len(concrete.alternativeCounts) != len(counts) {
		t.Fatalf("live odometer sizes = %d/%d, want %d", len(concrete.choices), len(concrete.alternativeCounts), len(counts))
	}
}
