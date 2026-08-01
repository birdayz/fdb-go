package executor

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

func FuzzAdvanceScanRangeChoices(f *testing.F) {
	f.Add([]byte{2, 2})
	f.Add([]byte{1, 3, 2})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, rawCounts []byte) {
		if len(rawCounts) > 8 {
			rawCounts = rawCounts[:8]
		}
		counts := make([]uint32, len(rawCounts))
		expectedStates := 1
		for i, rawCount := range rawCounts {
			counts[i] = uint32(rawCount%4) + 1
			expectedStates *= int(counts[i])
		}

		choices := make([]uint32, len(counts))
		seen := make(map[string]struct{}, expectedStates)
		for state := 0; ; state++ {
			if state >= expectedStates {
				t.Fatalf("odometer produced more than %d states", expectedStates)
			}
			encoded := make([]byte, len(choices))
			for i, choice := range choices {
				if choice >= counts[i] {
					t.Fatalf("choice %d at %d is outside count %d", choice, i, counts[i])
				}
				encoded[i] = byte(choice)
			}
			key := string(encoded)
			if _, duplicate := seen[key]; duplicate {
				t.Fatalf("duplicate odometer state %v", choices)
			}
			seen[key] = struct{}{}
			if !advanceScanRangeChoices(choices, counts) {
				break
			}
		}
		if len(seen) != expectedStates {
			t.Fatalf("state count = %d, want %d", len(seen), expectedStates)
		}
	})
}

func FuzzScanRangeSetContinuationParserNeverRestartsCorruptState(f *testing.F) {
	started := true
	valid, err := (&gen.ScanRangeSetContinuation{
		Fingerprint:  testScanRangeFingerprint,
		Choices:      []uint32{1, 0},
		ChildStarted: &started,
	}).MarshalVT()
	if err != nil {
		f.Fatalf("marshal seed: %v", err)
	}
	f.Add(valid)
	f.Add([]byte{0xff, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		opened := 0
		cursor, constructErr := newScanRangeSetCursor[int](
			testRangeSetSpec([]uint32{2, 2}, nil),
			raw,
			recordlayer.ForwardScan(),
			func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
				opened++
				return &rangeSetScriptCursor{}, nil
			},
		)
		if opened != 0 {
			t.Fatalf("constructor opened %d leaves", opened)
		}
		if constructErr != nil {
			var parseErr *recordlayer.ContinuationParseError
			if !errors.As(constructErr, &parseErr) {
				t.Fatalf("error = %T %v, want *ContinuationParseError", constructErr, constructErr)
			}
			return
		}
		// nil is the only non-wrapper start token. A non-nil accepted token must
		// decode to the exact validated position; it may never silently restart.
		concrete := cursor.(*scanRangeSetCursor[int])
		if raw != nil {
			var decoded gen.ScanRangeSetContinuation
			if err := decoded.UnmarshalVT(raw); err != nil {
				t.Fatalf("constructor accepted undecodable bytes: %v", err)
			}
			if !reflect.DeepEqual(concrete.choices, decoded.Choices) {
				t.Fatalf("accepted continuation restarted: choices %v, encoded %v", concrete.choices, decoded.Choices)
			}
		}
	})
}

func FuzzScanRangeSetContinuationRoundTrip(f *testing.F) {
	f.Add([]byte{2, 2}, []byte{1, 0}, []byte{7, 8}, true)
	f.Add([]byte{}, []byte{}, []byte{}, false)
	f.Fuzz(func(t *testing.T, rawCounts, rawChoices, inner []byte, started bool) {
		if len(rawCounts) > 32 {
			rawCounts = rawCounts[:32]
		}
		counts := make([]uint32, len(rawCounts))
		choices := make([]uint32, len(rawCounts))
		for i, rawCount := range rawCounts {
			counts[i] = uint32(rawCount%4) + 1
			if i < len(rawChoices) {
				choices[i] = uint32(rawChoices[i]) % counts[i]
			}
		}
		if !started {
			inner = nil
		}
		wrapper := &scanRangeSetContinuation{
			fingerprint:       testScanRangeFingerprint,
			choices:           choices,
			innerContinuation: bytes.Clone(inner),
			childStarted:      started,
		}
		raw, err := wrapper.ToBytes()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		decodedChoices, decodedInner, err := parseScanRangeSetContinuation(
			raw,
			testScanRangeFingerprint,
			counts,
		)
		if err != nil {
			t.Fatalf("round-trip parse: %v", err)
		}
		if !slices.Equal(decodedChoices, choices) || !bytes.Equal(decodedInner, inner) {
			t.Fatalf("round trip choices/inner = %v/%x, want %v/%x", decodedChoices, decodedInner, choices, inner)
		}
	})
}

func FuzzScanRangeSetPagedSweep(f *testing.F) {
	f.Add([]byte{2, 0, 3}, byte(1))
	f.Add([]byte{0}, byte(3))
	f.Fuzz(func(t *testing.T, rawSizes []byte, rawPageSize byte) {
		if len(rawSizes) == 0 {
			rawSizes = []byte{0}
		}
		if len(rawSizes) > 6 {
			rawSizes = rawSizes[:6]
		}
		pageSize := int(rawPageSize%3) + 1
		itemsByBranch := make(map[int][]int, len(rawSizes))
		expected := make([]int, 0)
		for branch, rawSize := range rawSizes {
			size := int(rawSize % 5)
			items := make([]int, size)
			for i := range items {
				items[i] = branch*100 + i
			}
			itemsByBranch[branch] = items
			expected = append(expected, items...)
		}

		spec := testRangeSetSpec([]uint32{uint32(len(rawSizes))}, nil)
		var continuation []byte
		actual := make([]int, 0, len(expected))
		for page := 0; page < len(expected)+len(rawSizes)+4; page++ {
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
						pageSize:    pageSize,
						pauseReason: recordlayer.ScanLimitReached,
					}, nil
				},
			)
			if err != nil {
				t.Fatalf("page %d construct: %v", page, err)
			}
			for pull := 0; pull < len(expected)+len(rawSizes)+2; pull++ {
				result, nextErr := cursor.OnNext(context.Background())
				if nextErr != nil {
					t.Fatalf("page %d pull %d: %v", page, pull, nextErr)
				}
				if result.HasNext() {
					actual = append(actual, result.GetValue())
					continue
				}
				if result.GetNoNextReason() == recordlayer.SourceExhausted {
					if !reflect.DeepEqual(actual, expected) {
						t.Fatalf("paged output = %v, want %v", actual, expected)
					}
					return
				}
				continuation, err = result.GetContinuation().ToBytes()
				if err != nil {
					t.Fatalf("page %d continuation: %v", page, err)
				}
				break
			}
		}
		t.Fatalf("paged sweep made no terminal progress: got %v want %v", actual, expected)
	})
}
