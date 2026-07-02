package cmd

import (
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestTupleFromJSON(t *testing.T) {
	t.Parallel()
	got, err := tupleFromJSON(`["myapp", 42, 1.5, {"uuid": "0195c7e8-1111-2222-3333-444455556666"}, {"bytes_hex": "deadbeef"}]`)
	if err != nil {
		t.Fatalf("tupleFromJSON: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d; want 5 (%v)", len(got), got)
	}
	if got[0] != "myapp" || got[1] != int64(42) || got[2] != 1.5 {
		t.Errorf("scalar elements = %v", got[:3])
	}
	if _, ok := got[3].(tuple.UUID); !ok {
		t.Errorf("uuid element = %T; want tuple.UUID", got[3])
	}
	b, ok := got[4].([]byte)
	if !ok || len(b) != 4 || b[0] != 0xde {
		t.Errorf("bytes element = %v (%T); want deadbeef bytes", got[4], got[4])
	}
}

func TestTupleFromJSON_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantSub string
	}{
		{`not json`, "JSON array"},
		{`[]`, "at least one"},
		{`[true]`, "unsupported type"},
		{`[{"uuid": "zz"}]`, "not a valid"},
		{`[{"bytes_hex": "zz"}]`, "not valid hex"},
		{`[{"uuid": "aa", "bytes_hex": "bb"}]`, "exactly one key"},
		{`[{"wat": "x"}]`, "unknown tag"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			_, err := tupleFromJSON(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("tupleFromJSON(%q) err = %v; want substring %q", tc.in, err, tc.wantSub)
			}
		})
	}
}

// int64s beyond float64's exact range survive the flag path (UseNumber),
// and the structpb path renders integral floats as int64.
func TestTupleFromJSON_BigIntPrecision(t *testing.T) {
	t.Parallel()
	got, err := tupleFromJSON(`[9007199254740993]`) // 2^53 + 1
	if err != nil {
		t.Fatalf("tupleFromJSON: %v", err)
	}
	if got[0] != int64(9007199254740993) {
		t.Errorf("big int = %v; want exact 9007199254740993", got[0])
	}
}

func TestTupleFromListValue(t *testing.T) {
	t.Parallel()
	lv, err := structpb.NewList([]any{"a", float64(7), map[string]any{"bytes_hex": "0a0b"}})
	if err != nil {
		t.Fatalf("structpb.NewList: %v", err)
	}
	got, convErr := tupleFromListValue(lv)
	if convErr != nil {
		t.Fatalf("tupleFromListValue: %v", convErr)
	}
	if got[0] != "a" || got[1] != int64(7) {
		t.Errorf("elements = %v", got)
	}
	if b, ok := got[2].([]byte); !ok || len(b) != 2 {
		t.Errorf("bytes element = %v (%T)", got[2], got[2])
	}
}

// tupleToJSON must render in the exact syntax tupleFromJSON accepts —
// write confirmations and `store truncate`'s type-back gate depend on
// the round trip.
func TestTupleToJSON_RoundTrips(t *testing.T) {
	t.Parallel()
	in := `["myapp",42,4.5,{"uuid":"00112233-4455-6677-8899-aabbccddeeff"},{"bytes_hex":"deadbeef"}]`
	tup, err := tupleFromJSON(in)
	if err != nil {
		t.Fatalf("tupleFromJSON: %v", err)
	}
	rendered := tupleToJSON(tup)
	if rendered != in {
		t.Errorf("rendered = %s; want %s", rendered, in)
	}
	tup2, err := tupleFromJSON(rendered)
	if err != nil {
		t.Fatalf("re-parse of rendered tuple: %v", err)
	}
	if !reflect.DeepEqual(tup, tup2) {
		t.Errorf("round trip changed the tuple: %v vs %v", tup, tup2)
	}
}

// A tuple-addressed target must be DESCRIBED as its tuple: write
// confirmations name the store they hit, and truncate's type-back gate
// would otherwise compare against a stale or empty keyspace_path
// (codex P2).
func TestStoreTargetDescribe_KeyspaceTuple(t *testing.T) {
	t.Parallel()
	tup, err := tupleFromJSON(`["frl",7]`)
	if err != nil {
		t.Fatalf("tupleFromJSON: %v", err)
	}
	target := &storeTarget{
		cfgCtx:        &configv1.Context{Name: "x", KeyspacePath: "/stale/path"},
		keyspaceTuple: tup,
	}
	if got := target.describe(); got != `["frl",7]` {
		t.Errorf("describe() = %q; want the tuple form", got)
	}
}
