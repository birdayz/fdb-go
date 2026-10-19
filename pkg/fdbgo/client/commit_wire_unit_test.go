package client

import (
	"bytes"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func decodeCommit(t *testing.T, body []byte) types.CommitTransactionRequest {
	t.Helper()
	r, err := wire.NewReader(body)
	if err != nil {
		t.Fatal(err)
	}
	var req types.CommitTransactionRequest
	req.UnmarshalFromReader(r)
	return req
}

// A single-key clear goes out as (k\x00, empty), as MutationRef::serialize
// sends it (CommitTransaction.h:318-334); other clears and the captured
// mutations are unchanged. With a tenant, the rule applies to the prefixed keys.
func TestCommitSingleKeyClearEncoding(t *testing.T) {
	for _, tenant := range []int64{NoTenantID, 7} {
		muts := []Mutation{
			{Type: MutClearRange, Key: []byte("k"), Value: []byte("k\x00")},
			{Type: MutClearRange, Key: []byte("a"), Value: []byte("c")},
			{Type: MutSetValue, Key: []byte("s"), Value: []byte("s\x00")},
		}
		input := &commitInput{muts: muts, tenantID: tenant, readVersion: 5}
		body, buf := buildCommitInputRequest(input, transport.UID{})
		req := decodeCommit(t, body)
		marshalBufPool.Put(buf)
		var p []byte
		if tenant >= 0 {
			p = []byte{0, 0, 0, 0, 0, 0, 0, 7}
		}
		pre := func(s string) []byte { return append(bytes.Clone(p), s...) }
		got := req.Transaction.Mutations
		if len(got) != 3 || !bytes.Equal(got[0].Param1, pre("k\x00")) || len(got[0].Param2) != 0 {
			t.Fatalf("WIRE_SINGLE_CLEAR tenant %d: %+v, want (%q, empty)", tenant, got, pre("k\x00"))
		}
		if !bytes.Equal(got[1].Param1, pre("a")) || !bytes.Equal(got[1].Param2, pre("c")) {
			t.Fatalf("a range clear changed: %+v", got[1])
		}
		if !bytes.Equal(got[2].Param1, pre("s")) || !bytes.Equal(got[2].Param2, []byte("s\x00")) {
			t.Fatalf("a set changed: %+v", got[2])
		}
		if string(muts[0].Key) != "k" || string(muts[0].Value) != "k\x00" {
			t.Fatalf("the captured mutation was rewritten: %+v", muts[0])
		}
	}
}
