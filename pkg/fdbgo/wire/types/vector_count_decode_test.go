package types

import (
	"encoding/binary"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// A generated decoder treats a vector whose count cannot fit in the reply as
// absent: it decodes as empty, never as elements read from the bytes after it.
func TestGeneratedDecoderDropsOversizedVector(t *testing.T) {
	t.Parallel()
	reply := GetReadVersionReply{
		Version: 42,
		TagThrottleInfo: []TransactionTagThrottle{
			{Tag: []byte("tag-a"), Limits: ClientTagThrottleLimits{TpsRate: 1}},
			{Tag: []byte("tag-b"), Limits: ClientTagThrottleLimits{TpsRate: 2}},
		},
	}
	data := MarshalErrorOrValueGetReadVersionReply(&reply)
	decode := func(d []byte) GetReadVersionReply {
		t.Helper()
		var r wire.Reader
		if err := wire.ReadErrorOrInto(d, &r); err != nil {
			t.Fatal(err)
		}
		var got GetReadVersionReply
		got.UnmarshalFromReader(&r)
		return got
	}
	if got := decode(data); got.Version != 42 || len(got.TagThrottleInfo) != 2 {
		t.Fatalf("well-formed reply: version %d, %d throttles", got.Version, len(got.TagThrottleInfo))
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
	if got := decode(data); got.Version != 42 || len(got.TagThrottleInfo) != 0 {
		t.Fatalf("oversized vector: version %d, %d throttles; want 42 and none", got.Version, len(got.TagThrottleInfo))
	}
}
