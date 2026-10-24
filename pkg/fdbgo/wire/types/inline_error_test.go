package types_test

import (
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestInlineErrorUnionRoundTrip is the RFC-115 §6 proof. Real FDB read replies deliver
// wrong_shard_server / future_version / process_behind through the INLINE
// LoadBalancedReply.error field — a flatbuffers UNION (1-byte present tag + a
// RelativeOffset to a nested Error table), NOT a length-prefixed byte vector. Before the
// extractor fix (optionalInnerGoType<Optional<Error>> in cmd/fdb-schema-extract/extract.h)
// the generated reply WRITER mis-encoded this field as Optional<bytes>, so it never matched
// what the readers and C++ emit/expect. This pins it: a reply marshaled WITH a non-empty
// inline error round-trips back to that exact code through the generated decoder (the
// production read path) and wire.ReadInlineReplyError, for all three reply types.
//
// Anti-self-confirming: the injected code is the canonical literal 1001 (wrong_shard_server),
// never a code-under-test constant (fault_test.go P6 rule). Revert-proof: back out the
// extractor specialization + regen → the writer emits Optional<bytes> → ReadInlineReplyError
// mis-decodes the byte vector and this test reddens.
func TestInlineErrorUnionRoundTrip(t *testing.T) {
	t.Parallel()
	const wrongShard = 1001 // canonical wrong_shard_server — NOT a Go constant

	cases := []struct {
		name    string
		marshal func(hasErr bool) []byte
		slot    int
		decode  func(data []byte) (bool, uint16, error) // the generated decoder's HasError/Error
	}{
		{"GetValueReply", func(h bool) []byte {
			return (&types.GetValueReply{HasError: h, Error: types.Error{ErrorCode: wrongShard}}).MarshalFDB()
		}, types.GetValueReplySlotError, func(d []byte) (bool, uint16, error) {
			var m types.GetValueReply
			err := m.UnmarshalFDB(d)
			return m.HasError, m.Error.ErrorCode, err
		}},
		{"GetKeyReply", func(h bool) []byte {
			return (&types.GetKeyReply{HasError: h, Error: types.Error{ErrorCode: wrongShard}}).MarshalFDB()
		}, types.GetKeyReplySlotError, func(d []byte) (bool, uint16, error) {
			var m types.GetKeyReply
			err := m.UnmarshalFDB(d)
			return m.HasError, m.Error.ErrorCode, err
		}},
		{"GetKeyValuesReply", func(h bool) []byte {
			return (&types.GetKeyValuesReply{HasError: h, Error: types.Error{ErrorCode: wrongShard}}).MarshalFDB()
		}, types.GetKeyValuesReplySlotError, func(d []byte) (bool, uint16, error) {
			var m types.GetKeyValuesReply
			err := m.UnmarshalFDB(d)
			return m.HasError, m.Error.ErrorCode, err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/present", func(t *testing.T) {
			t.Parallel()
			r, err := wire.NewReader(tc.marshal(true))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			ferr := wire.ReadInlineReplyError(r, tc.slot)
			if ferr == nil {
				t.Fatal("expected an inline error; got nil (writer/reader disagree on the Optional<Error> union — the mis-marshal)")
			}
			if ferr.Code != wrongShard {
				t.Fatalf("inline error code = %d, want %d", ferr.Code, wrongShard)
			}
			if has, code, err := tc.decode(tc.marshal(true)); err != nil || !has || code != wrongShard {
				t.Fatalf("generated decode: HasError %t, code %d, %v; want %d", has, code, err, wrongShard)
			}
		})
		t.Run(tc.name+"/absent", func(t *testing.T) {
			t.Parallel()
			r, err := wire.NewReader(tc.marshal(false))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			if ferr := wire.ReadInlineReplyError(r, tc.slot); ferr != nil {
				t.Fatalf("no inline error expected, got code %d", ferr.Code)
			}
			if has, _, err := tc.decode(tc.marshal(false)); err != nil || has {
				t.Fatalf("generated decode: HasError %t, %v; want absent", has, err)
			}
		})
	}
}
