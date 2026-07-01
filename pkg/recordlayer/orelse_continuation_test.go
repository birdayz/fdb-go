package recordlayer

// Regression tests for OrElseWithContinuation continuation deserialization.
// A continuation token is external wire input: any byte sequence must produce
// either a working cursor or an explicit error — never a panic and never a
// silent restart. Matches Java's OrElseCursor constructor (OrElseCursor.java):
// parse failure → RecordCoreException("error parsing continuation"), unknown
// state → UnknownOrElseCursorStateException. Found by FuzzOrElseContinuation:
// an out-of-range State enum value left the cursor with a nil active cursor
// and OnNext panicked in advanceActive.

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/gen"
)

func marshalOrElseContinuation(t *testing.T, state gen.OrElseContinuation_State, inner []byte) []byte {
	t.Helper()
	cont, err := (&gen.OrElseContinuation{
		State:        &state,
		Continuation: inner,
	}).MarshalVT()
	if err != nil {
		t.Fatalf("marshal OrElseContinuation: %v", err)
	}
	return cont
}

func TestOrElseContinuationInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		continuation []byte
		checkErr     func(t *testing.T, err error)
	}{
		{
			name:         "corrupt bytes fail with ContinuationParseError",
			continuation: []byte{0xff, 0xff, 0xff},
			checkErr: func(t *testing.T, err error) {
				var parseErr *ContinuationParseError
				if !errors.As(err, &parseErr) {
					t.Fatalf("want *ContinuationParseError, got %T: %v", err, err)
				}
				if string(parseErr.RawBytes) != "\xff\xff\xff" {
					t.Errorf("RawBytes = %x, want ffffff", parseErr.RawBytes)
				}
				if parseErr.Unwrap() == nil {
					t.Error("Unwrap() = nil, want wrapped unmarshal error")
				}
			},
		},
		{
			name: "unknown state 3 (first out-of-range) fails with UnknownOrElseCursorStateError",
			continuation: func() []byte {
				return []byte{0x08, 0x03} // field 1 varint = 3
			}(),
			checkErr: func(t *testing.T, err error) {
				var stateErr *UnknownOrElseCursorStateError
				if !errors.As(err, &stateErr) {
					t.Fatalf("want *UnknownOrElseCursorStateError, got %T: %v", err, err)
				}
				if got, want := stateErr.Error(), "unknown state for OrElseCursor"; got != want {
					t.Errorf("Error() = %q, want %q (Java's UnknownOrElseCursorStateException message)", got, want)
				}
			},
		},
		{
			name: "unknown state 99 with inner continuation fails with UnknownOrElseCursorStateError",
			// The FuzzOrElseContinuation crasher: state=99, continuation=[0,0,0,1].
			continuation: []byte{0x08, 0x63, 0x12, 0x04, 0x00, 0x00, 0x00, 0x01},
			checkErr: func(t *testing.T, err error) {
				var stateErr *UnknownOrElseCursorStateError
				if !errors.As(err, &stateErr) {
					t.Fatalf("want *UnknownOrElseCursorStateError, got %T: %v", err, err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			primaryCalled := false
			alternativeCalled := false
			primary := func(_ []byte) RecordCursor[int] {
				primaryCalled = true
				return FromList([]int{1, 2, 3})
			}
			alt := func(_ []byte) RecordCursor[int] {
				alternativeCalled = true
				return FromList([]int{4, 5})
			}

			cursor := OrElseWithContinuation(primary, alt, tt.continuation)

			// Must not panic; must surface an explicit error, not restart.
			_, err := cursor.OnNext(ctx)
			if err == nil {
				t.Fatal("OnNext: want error for invalid continuation, got nil (silent restart is a wrong-results divergence)")
			}
			tt.checkErr(t, err)

			// The error must latch: every subsequent OnNext fails the same way.
			_, err2 := cursor.OnNext(ctx)
			if err2 == nil {
				t.Fatal("second OnNext: want latched error, got nil")
			}
			if err.Error() != err2.Error() {
				t.Errorf("error not latched: first %q, second %q", err, err2)
			}

			if primaryCalled || alternativeCalled {
				t.Errorf("no inner cursor may be built for an invalid continuation (primary=%v alternative=%v)",
					primaryCalled, alternativeCalled)
			}
			if err := cursor.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
}

func TestOrElseContinuationStateRoundTrips(t *testing.T) {
	t.Parallel()

	// FromListWithContinuation continuations are 4-byte big-endian positions.
	pos := func(p int) []byte { return []byte{0x00, 0x00, 0x00, byte(p)} }

	tests := []struct {
		name         string
		continuation func(t *testing.T) []byte
		want         []int
		wantPrimary  bool // primary factory must have been invoked
		wantAlt      bool // alternative factory must have been invoked
	}{
		{
			name: "UNDECIDED resumes primary from continuation",
			continuation: func(t *testing.T) []byte {
				return marshalOrElseContinuation(t, gen.OrElseContinuation_UNDECIDED, pos(2))
			},
			want:        []int{3},
			wantPrimary: true,
			wantAlt:     false,
		},
		{
			name: "UNDECIDED with exhausted primary falls back to alternative",
			continuation: func(t *testing.T) []byte {
				return marshalOrElseContinuation(t, gen.OrElseContinuation_UNDECIDED, pos(3))
			},
			want:        []int{4, 5},
			wantPrimary: true,
			wantAlt:     true,
		},
		{
			// Java proto2: parsed.getState() defaults to UNDECIDED when the
			// field is absent, and the inner continuation is still honored.
			// Go previously dropped the continuation and restarted the primary
			// from scratch in this case.
			name: "absent state defaults to UNDECIDED and keeps continuation",
			continuation: func(t *testing.T) []byte {
				cont, err := (&gen.OrElseContinuation{Continuation: pos(2)}).MarshalVT()
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				return cont
			},
			want:        []int{3},
			wantPrimary: true,
			wantAlt:     false,
		},
		{
			name: "USE_INNER resumes primary",
			continuation: func(t *testing.T) []byte {
				return marshalOrElseContinuation(t, gen.OrElseContinuation_USE_INNER, pos(1))
			},
			want:        []int{2, 3},
			wantPrimary: true,
			wantAlt:     false,
		},
		{
			name: "USE_OTHER resumes alternative without building primary",
			continuation: func(t *testing.T) []byte {
				return marshalOrElseContinuation(t, gen.OrElseContinuation_USE_OTHER, pos(1))
			},
			want:        []int{5},
			wantPrimary: false,
			wantAlt:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			primaryCalled := false
			alternativeCalled := false
			primary := func(cont []byte) RecordCursor[int] {
				primaryCalled = true
				return FromListWithContinuation([]int{1, 2, 3}, cont)
			}
			alt := func(cont []byte) RecordCursor[int] {
				alternativeCalled = true
				return FromListWithContinuation([]int{4, 5}, cont)
			}

			cursor := OrElseWithContinuation(primary, alt, tt.continuation(t))
			got, err := AsList(ctx, cursor)
			if err != nil {
				t.Fatalf("AsList: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
			if primaryCalled != tt.wantPrimary {
				t.Errorf("primary factory called = %v, want %v", primaryCalled, tt.wantPrimary)
			}
			if alternativeCalled != tt.wantAlt {
				t.Errorf("alternative factory called = %v, want %v", alternativeCalled, tt.wantAlt)
			}
		})
	}
}
