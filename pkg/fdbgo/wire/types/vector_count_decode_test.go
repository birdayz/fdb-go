package types

import (
	"encoding/binary"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// A generated decoder fails on a vector whose count cannot fit in the reply,
// instead of decoding it as absent.
func TestGeneratedDecoderRejectsOversizedVector(t *testing.T) {
	t.Parallel()
	reply := GetReadVersionReply{
		Version: 42,
		TagThrottleInfo: []TransactionTagThrottle{
			{Tag: []byte("tag-a"), Limits: ClientTagThrottleLimits{TpsRate: 1}},
			{Tag: []byte("tag-b"), Limits: ClientTagThrottleLimits{TpsRate: 2}},
		},
	}
	data := MarshalErrorOrValueGetReadVersionReply(&reply)
	decode := func(d []byte) (GetReadVersionReply, error) {
		t.Helper()
		var r wire.Reader
		if err := wire.ReadErrorOrInto(d, &r); err != nil {
			t.Fatal(err)
		}
		var got GetReadVersionReply
		err := got.UnmarshalFromReader(&r)
		return got, err
	}
	if got, err := decode(data); err != nil || got.Version != 42 || len(got.TagThrottleInfo) != 2 {
		t.Fatalf("well-formed reply: version %d, %d throttles, %v", got.Version, len(got.TagThrottleInfo), err)
	}

	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		t.Fatal(err)
	}
	field := r.ObjectPos() + r.FieldOffset(GetReadVersionReplySlotTagThrottleInfo)
	count := field + int(binary.LittleEndian.Uint32(data[field:]))
	if binary.LittleEndian.Uint32(data[count:]) != 2 {
		t.Fatal("did not locate the throttle vector's count")
	}
	binary.LittleEndian.PutUint32(data[count:], 1000)
	_, err := decode(data)
	var decodeErr *wire.DecodeError
	var countErr *wire.VectorCountError
	if !errors.As(err, &decodeErr) || decodeErr.Type != "GetReadVersionReply" || decodeErr.Field != "TagThrottleInfo" ||
		!errors.As(err, &countErr) || countErr.Count != 1000 {
		t.Fatalf("oversized vector: %v; want a DecodeError for TagThrottleInfo wrapping a VectorCountError", err)
	}
}
