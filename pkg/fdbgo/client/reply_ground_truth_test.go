package client

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// Reply-direction ground truth: each vector is the EXACT server->client
// payload (ErrorOr<EnsureTable<T>>, AssumeVersion — no version prefix) that
// FDB's networkSender would emit (networksender.actor.h:38,
// FlowTransport.actor.cpp:1932), serialized by the real C++ ObjectWriter in
// the extractor (cmd/fdb-schema-extract). The assertions drive the
// PRODUCTION parse functions and pin the literal field values the C++
// constructor set — never values re-derived from the Go parse.

type replyVectorEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int    `json:"size"`
	Hex  string `json:"hex"`
}

func loadReplyVectors(t *testing.T) []replyVectorEntry {
	t.Helper()
	// Shared with //pkg/fdbgo/wire/types:types_test; declared as a bazel data
	// dep of client_test. Absence is a build bug, never a skip.
	data, err := os.ReadFile("../wire/types/testdata.json")
	if err != nil {
		t.Fatalf("testdata.json not found (missing bazel data dep?): %v", err)
	}
	var vecs []replyVectorEntry
	if err := json.Unmarshal(data, &vecs); err != nil {
		t.Fatalf("parse testdata.json: %v", err)
	}
	var replies []replyVectorEntry
	for _, v := range vecs {
		if v.Kind == "reply" {
			replies = append(replies, v)
		}
	}
	if len(replies) == 0 {
		t.Fatal("no reply vectors in testdata.json")
	}
	return replies
}

// replyAsserters maps vector name -> assertion against the production parser.
// The expected literals mirror the C++ constructor sites in
// cmd/fdb-schema-extract/main.cpp generateTestVectors.
var replyAsserters = map[string]func(t *testing.T, data []byte){
	"GetValueReply_present": func(t *testing.T, data []byte) {
		value, penalty, err := parseGetValueReply(data)
		if err != nil {
			t.Fatalf("parseGetValueReply: %v", err)
		}
		if string(value) != "ground_truth_value" {
			t.Errorf("value = %q, want %q", value, "ground_truth_value")
		}
		if penalty != 1.5 {
			t.Errorf("penalty = %v, want 1.5", penalty)
		}
	},
	"GetValueReply_missing": func(t *testing.T, data []byte) {
		value, penalty, err := parseGetValueReply(data)
		if err != nil {
			t.Fatalf("parseGetValueReply: %v", err)
		}
		if value != nil {
			t.Errorf("value = %q, want nil (absent)", value)
		}
		if penalty != 1.0 {
			t.Errorf("penalty = %v, want 1.0", penalty)
		}
	},
	"GetKeyReply_basic": func(t *testing.T, data []byte) {
		key, orEqual, offset, penalty, err := parseGetKeyReply(data)
		if err != nil {
			t.Fatalf("parseGetKeyReply: %v", err)
		}
		if string(key) != "resolved_key" {
			t.Errorf("key = %q, want %q", key, "resolved_key")
		}
		if !orEqual {
			t.Error("orEqual = false, want true")
		}
		if offset != 3 {
			t.Errorf("offset = %d, want 3", offset)
		}
		if penalty != 2.0 {
			t.Errorf("penalty = %v, want 2.0", penalty)
		}
	},
	"GetKeyValuesReply_two_rows": func(t *testing.T, data []byte) {
		kvs, more, penalty, err := parseGetKeyValuesReply(data)
		if err != nil {
			t.Fatalf("parseGetKeyValuesReply: %v", err)
		}
		want := []struct{ k, v string }{
			{"alpha", "value_a"},
			{"beta", "value_b"},
		}
		if len(kvs) != len(want) {
			t.Fatalf("len(kvs) = %d, want %d", len(kvs), len(want))
		}
		for i, w := range want {
			if string(kvs[i].Key) != w.k || string(kvs[i].Value) != w.v {
				t.Errorf("kvs[%d] = (%q, %q), want (%q, %q)", i, kvs[i].Key, kvs[i].Value, w.k, w.v)
			}
		}
		if !more {
			t.Error("more = false, want true")
		}
		if penalty != 1.25 {
			t.Errorf("penalty = %v, want 1.25", penalty)
		}
	},
	"GetKeyValuesReply_empty": func(t *testing.T, data []byte) {
		kvs, more, penalty, err := parseGetKeyValuesReply(data)
		if err != nil {
			t.Fatalf("parseGetKeyValuesReply: %v", err)
		}
		if len(kvs) != 0 {
			t.Errorf("len(kvs) = %d, want 0", len(kvs))
		}
		if more {
			t.Error("more = true, want false")
		}
		if penalty != 1.0 {
			t.Errorf("penalty = %v, want 1.0", penalty)
		}
	},
	"GetReadVersionReply_locked": func(t *testing.T, data []byte) {
		version, rkDefault, rkBatch, _, _, err := parseGetReadVersionReply(data)
		if err != nil {
			t.Fatalf("parseGetReadVersionReply: %v", err)
		}
		if version != 0x123456789a {
			t.Errorf("version = %#x, want 0x123456789a", version)
		}
		if rkDefault || rkBatch {
			t.Errorf("rkDefault/rkBatch = %v/%v, want false/false", rkDefault, rkBatch)
		}
		// The production parser does not surface `locked` — the Go client
		// does not yet ENFORCE database locks on the read path the way C++
		// does (rep.locked && !lockAware → database_locked,
		// NativeAPI.actor.cpp:7425-7426); tracked in TODO.md. Pin the WIRE
		// layer here so the field is provably decodable when that lands.
		var r wire.Reader
		if err := wire.ReadErrorOrInto(data, &r); err != nil {
			t.Fatalf("ReadErrorOrInto: %v", err)
		}
		var reply types.GetReadVersionReply
		reply.UnmarshalFromReader(&r)
		if !reply.Locked {
			t.Error("Locked = false, want true (C++ constructor sets r.locked = true)")
		}
	},
	"CommitID_committed": func(t *testing.T, data []byte) {
		var tx Transaction
		if err := tx.parseCommitReply(data); err != nil {
			t.Fatalf("parseCommitReply: %v", err)
		}
		if tx.committedVersion != 0x0abcdef012 {
			t.Errorf("committedVersion = %#x, want 0xabcdef012", tx.committedVersion)
		}
		if tx.txnBatchId != 7 {
			t.Errorf("txnBatchId = %d, want 7", tx.txnBatchId)
		}
	},
	"CommitID_conflict": func(t *testing.T, data []byte) {
		// A conflict delivered IN-BAND (report_conflicting_keys shape:
		// CommitID{version: invalidVersion, conflictingKRIndices}) must map
		// to not_committed, as C++ does (NativeAPI.actor.cpp:6653,6726) —
		// NOT parse as a successful commit at version -1.
		var tx Transaction
		err := tx.parseCommitReply(data)
		if err == nil {
			t.Fatal("parseCommitReply = nil, want not_committed (1020)")
		}
		assertFDBErrorCode(t, err, ErrNotCommitted)
	},
	"WatchValueReply_basic": func(t *testing.T, data []byte) {
		// Production path: a non-error reply means "key changed".
		if err := parseWatchValueReply(data); err != nil {
			t.Fatalf("parseWatchValueReply: %v", err)
		}
		// The production parser discards the fields — pin the wire layer's
		// decode of them too, so the vector proves more than crash-absence.
		var r wire.Reader
		if err := wire.ReadErrorOrInto(data, &r); err != nil {
			t.Fatalf("ReadErrorOrInto: %v", err)
		}
		var reply types.WatchValueReply
		reply.UnmarshalFromReader(&r)
		if reply.Version != 424242 {
			t.Errorf("Version = %d, want 424242", reply.Version)
		}
		if !reply.Cached {
			t.Error("Cached = false, want true")
		}
	},
	"SplitRangeReply_two_points": func(t *testing.T, data []byte) {
		points, err := parseSplitRangeReply(data)
		if err != nil {
			t.Fatalf("parseSplitRangeReply: %v", err)
		}
		want := [][]byte{[]byte("split_a"), []byte("split_b")}
		if len(points) != len(want) {
			t.Fatalf("len(points) = %d, want %d", len(points), len(want))
		}
		for i := range want {
			if !bytes.Equal(points[i], want[i]) {
				t.Errorf("points[%d] = %q, want %q", i, points[i], want[i])
			}
		}
	},
	"StorageMetrics_basic": func(t *testing.T, data []byte) {
		got, err := parseWaitMetricsReply(data)
		if err != nil {
			t.Fatalf("parseWaitMetricsReply: %v", err)
		}
		if got != 123456789 {
			t.Errorf("bytes = %d, want 123456789", got)
		}
	},
	"GetKeyServerLocationsReply_one_range": func(t *testing.T, data []byte) {
		entries, err := parseGetKeyServerLocationsReply(data)
		if err != nil {
			t.Fatalf("parseGetKeyServerLocationsReply: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		e := entries[0]
		if string(e.begin) != "range_begin" || string(e.end) != "range_end" {
			t.Errorf("range = (%q, %q), want (range_begin, range_end)", e.begin, e.end)
		}
		if len(e.servers) != 1 {
			t.Fatalf("len(servers) = %d, want 1", len(e.servers))
		}
		s := e.servers[0]
		if s.Address != "10.1.2.3:4500" {
			t.Errorf("server address = %q, want 10.1.2.3:4500", s.Address)
		}
		// The extractor pins the SSI getValue endpoint token; the parser
		// derives per-method endpoints from it via getAdjustedEndpoint, so
		// the base token's First word must round-trip exactly.
		wantTok := transport.UID{First: 0x7373737373737373, Second: 0x8484848484848484}
		if s.Token.First != wantTok.First {
			t.Errorf("server token = %#x/%#x, want %#x/%#x",
				s.Token.First, s.Token.Second, wantTok.First, wantTok.Second)
		}
	},
}

func assertFDBErrorCode(t *testing.T, err error, wantCode int) {
	t.Helper()
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("error %v is not a wire.FDBError", err)
	}
	if fdbErr.Code != wantCode {
		t.Fatalf("error code = %d, want %d", fdbErr.Code, wantCode)
	}
}

// TestReplyGroundTruth feeds C++-ObjectWriter-serialized reply payloads to
// the client's production parse functions and asserts the field values.
func TestReplyGroundTruth(t *testing.T) {
	t.Parallel()
	replies := loadReplyVectors(t)

	seen := make(map[string]bool)
	for _, vec := range replies {
		vec := vec
		seen[vec.Name] = true
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()
			assert, ok := replyAsserters[vec.Name]
			if !ok {
				// Every reply vector MUST have an asserter — an unmatched
				// name is a broken net, not a skip (no-skip rule).
				t.Fatalf("no asserter for reply vector %s", vec.Name)
			}
			raw, err := hex.DecodeString(vec.Hex)
			if err != nil {
				t.Fatalf("decode hex: %v", err)
			}
			if len(raw) != vec.Size {
				t.Fatalf("hex decodes to %d bytes, size field says %d", len(raw), vec.Size)
			}
			assert(t, raw)
		})
	}

	// Reverse direction: every asserter must be backed by a vector.
	for name := range replyAsserters {
		if !seen[name] {
			t.Errorf("asserter %s has no vector in testdata.json", name)
		}
	}
}
