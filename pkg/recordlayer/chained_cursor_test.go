package recordlayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

func encodeInt64(v int64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(v))
	return buf
}

func decodeInt64(b []byte) (int64, bool) {
	if len(b) < 8 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(b)), true
}

func TestChainedCursorBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Generate 1, 2, 3, 4, 5
	cursor := Chained(
		func(prev *int64) (*int64, error) {
			var next int64
			if prev == nil {
				next = 1
			} else if *prev >= 5 {
				return nil, nil // exhausted
			} else {
				next = *prev + 1
			}
			return &next, nil
		},
		encodeInt64, decodeInt64, nil,
	)

	var results []int64
	for v, iterErr := range Seq2(cursor, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	expected := []int64{1, 2, 3, 4, 5}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestChainedCursorEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Chained(
		func(prev *int64) (*int64, error) {
			return nil, nil // immediately exhausted
		},
		encodeInt64, decodeInt64, nil,
	)

	result, err := cursor.OnNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.HasNext() {
		t.Fatal("expected no results")
	}
}

func TestChainedCursorContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	gen := func(prev *int64) (*int64, error) {
		var next int64
		if prev == nil {
			next = 1
		} else if *prev >= 5 {
			return nil, nil
		} else {
			next = *prev + 1
		}
		return &next, nil
	}

	// Read first 3 values
	cursor := Chained(gen, encodeInt64, decodeInt64, nil)

	var lastCont RecordCursorContinuation
	for i := 0; i < 3; i++ {
		result, err := cursor.OnNext(ctx)
		if err != nil || !result.HasNext() {
			t.Fatalf("expected result %d", i+1)
		}
		lastCont = result.GetContinuation()
	}

	// Resume from continuation
	contBytes, contBytesErr := lastCont.ToBytes()
	if contBytesErr != nil {
		t.Fatalf("lastCont.ToBytes() error: %v", contBytesErr)
	}
	cursor2 := Chained(gen, encodeInt64, decodeInt64, contBytes)

	var results []int64
	for v, iterErr := range Seq2(cursor2, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
		results = append(results, v)
	}

	// Should get 4, 5
	expected := []int64{4, 5}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

// Java's ChainedCursor invokes the decoder for every non-null continuation,
// including a zero-length byte array. Only nil means a fresh start.
func TestChainedCursorContinuationPresence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cont []byte
		want string
	}{
		{"absent", nil, ""},
		{"empty", []byte{}, "a"},
		{"nonempty", []byte("a"), "aa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			decodeCalls := 0
			cursor := Chained(
				func(prev *string) (*string, error) {
					next := ""
					if prev != nil {
						next = *prev + "a"
					}
					return &next, nil
				},
				func(v string) []byte { return []byte(v) },
				func(b []byte) (string, bool) {
					decodeCalls++
					if b == nil || !bytes.Equal(b, tc.cont) {
						t.Fatalf("decoder received %v, want non-nil %v", b, tc.cont)
					}
					return string(b), true
				}, tc.cont,
			)
			defer func() { _ = cursor.Close() }()
			result, err := cursor.OnNext(context.Background())
			if err != nil || !result.HasNext() {
				t.Fatalf("OnNext: hasNext=%v, err=%v", result.HasNext(), err)
			}
			if got := result.GetValue(); got != tc.want {
				t.Fatalf("OnNext = %q, want %q; ignoring a present continuation silently replays rows", got, tc.want)
			}
			wantCalls := 0
			if tc.cont != nil {
				wantCalls = 1
			}
			if decodeCalls != wantCalls {
				t.Fatalf("decoder called %d times, want %d", decodeCalls, wantCalls)
			}
			encoded, err := result.GetContinuation().ToBytes()
			if err != nil || encoded == nil || string(encoded) != tc.want || result.GetContinuation().IsEnd() {
				t.Fatalf("continuation = %v, err=%v; want non-terminal encoding of %q", encoded, err, tc.want)
			}
		})
	}
}

func TestChainedCursorRejectsUndecodableContinuation(t *testing.T) {
	t.Parallel()
	for _, missingDecoder := range []bool{false, true} {
		for _, raw := range [][]byte{{}, {0xff}} {
			for _, concat := range []bool{false, true} {
				t.Run(fmt.Sprintf("missing_decoder=%v/bytes=%x/concat=%v", missingDecoder, raw, concat), func(t *testing.T) {
					t.Parallel()
					generatorCalls, decoderCalls := 0, 0
					decode := func([]byte) (int, bool) {
						decoderCalls++
						return 99, false
					}
					if missingDecoder {
						decode = nil
					}
					factory := func(cont []byte) RecordCursor[int] {
						return Chained(
							func(prev *int) (*int, error) {
								generatorCalls++
								v := 1
								return &v, nil
							},
							func(v int) []byte { return []byte{byte(v)} }, decode, cont,
						)
					}
					var cursor RecordCursor[int]
					if concat {
						wrapped, err := (&gen.ConcatContinuation{Second: proto.Bool(true), Continuation: raw}).MarshalVT()
						if err != nil {
							t.Fatal(err)
						}
						cursor = ConcatCursors(
							func([]byte) RecordCursor[int] {
								t.Fatal("continuation points at the second cursor; must not restart the first")
								return Empty[int]()
							}, factory, wrapped,
						)
					} else {
						cursor = factory(raw)
					}
					ctx := context.Background()
					result, err := cursor.OnNext(ctx)
					if err == nil || result.HasNext() {
						t.Fatalf("undecodable continuation must fail, not silently restart: hasNext=%v, err=%v", result.HasNext(), err)
					}
					var parseErr *ContinuationParseError
					if !errors.As(err, &parseErr) || parseErr.RawBytes == nil || !bytes.Equal(parseErr.RawBytes, raw) || parseErr.Cause == nil {
						t.Fatalf("want ContinuationParseError carrying bytes and cause, got %T: %v", err, err)
					}
					requireLatched(t, ctx, cursor, err)
					if generatorCalls != 0 {
						t.Fatalf("invalid continuation invoked generator %d times", generatorCalls)
					}
					wantDecoderCalls := 1
					if missingDecoder {
						wantDecoderCalls = 0
					}
					if decoderCalls != wantDecoderCalls {
						t.Fatalf("decoder called %d times, want %d", decoderCalls, wantDecoderCalls)
					}
				})
			}
		}
	}
}

func TestChainedCursorError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Chained(
		func(prev *int64) (*int64, error) {
			return nil, fmt.Errorf("generator error")
		},
		encodeInt64, decodeInt64, nil,
	)

	_, err := cursor.OnNext(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestChainedCursorSeq2(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := Chained(
		func(prev *int64) (*int64, error) {
			var next int64
			if prev == nil {
				next = 10
			} else if *prev >= 12 {
				return nil, nil
			} else {
				next = *prev + 1
			}
			return &next, nil
		},
		encodeInt64, decodeInt64, nil,
	)

	var results []int64
	for v, err := range Seq2(cursor, ctx) {
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, v)
	}

	expected := []int64{10, 11, 12}
	if len(results) != len(expected) {
		t.Fatalf("got %v, want %v", results, expected)
	}
}

// An exhausted generator needs no encoded position. Java never invokes the
// continuation encoder on this path, even if the encoder is missing.
func TestChainedCursorEmptyWithNilEncode(t *testing.T) {
	t.Parallel()
	cursor := Chained[int](func(*int) (*int, error) { return nil, nil }, nil, nil, nil)
	defer func() { _ = cursor.Close() }()
	result, err := cursor.OnNext(context.Background())
	if err != nil || result.HasNext() || result.GetNoNextReason() != SourceExhausted || !result.GetContinuation().IsEnd() {
		t.Fatalf("empty generator must exhaust without encoding: result=%+v, err=%v", result, err)
	}
}

func TestChainedCursorNilEncode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		encode func(int) []byte
	}{
		{"missing_encoder", nil},
		{"nil_bytes", func(int) []byte { return nil }},
	} {
		for _, concat := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/concat=%v", tc.name, concat), func(t *testing.T) {
				t.Parallel()
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("invalid continuation encoding must return an error, not panic: %v", r)
					}
				}()
				generatorCalls := 0
				factory := func(cont []byte) RecordCursor[int] {
					return Chained(
						func(prev *int) (*int, error) {
							generatorCalls++
							v := 1
							if prev != nil {
								v = *prev + 1
							}
							return &v, nil
						}, tc.encode, nil, cont,
					)
				}
				cursor := factory(nil)
				if concat {
					_ = cursor.Close()
					cursor = ConcatCursors(
						func(cont []byte) RecordCursor[int] { return FromListWithContinuation([]int{100}, cont) },
						factory, nil,
					)
					first, err := cursor.OnNext(ctx)
					if err != nil || !first.HasNext() || first.GetValue() != 100 {
						t.Fatalf("concat's first cursor: got %+v, err=%v", first, err)
					}
				}
				defer func() { _ = cursor.Close() }()
				result, err := cursor.OnNext(ctx)
				if err == nil || result.HasNext() {
					t.Fatalf("a row without a position must fail, not carry a restart token: hasNext=%v, err=%v", result.HasNext(), err)
				}
				var encodeErr *ContinuationEncodeError
				if !errors.As(err, &encodeErr) {
					t.Fatalf("want ContinuationEncodeError, got %T: %v", err, err)
				}
				wantMessage := "chained continuation requires an encoder"
				if tc.encode != nil {
					wantMessage = "cannot return end continuation with next value"
				}
				if encodeErr.Error() != wantMessage {
					t.Fatalf("encoding error = %q, want %q", encodeErr.Error(), wantMessage)
				}
				requireLatched(t, ctx, cursor, err)
				if generatorCalls != 1 {
					t.Fatalf("generator called %d times; encoding failure must not consume another row", generatorCalls)
				}
			})
		}
	}
}
