package client

import (
	"bytes"
	"context"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func TestSampleReadTags(t *testing.T) {
	t.Parallel()
	draws := 0
	scripted := func(v float64) func() float64 {
		return func() float64 { draws++; return v }
	}
	for _, tc := range []struct {
		name  string
		s     readTagSampling
		want  bool
		draws int
	}{
		{"zero value", readTagSampling{}, false, 0},
		{"rate 0 never draws", readTagSampling{rate: 0, draw: scripted(0)}, false, 0},
		{"negative rate never draws", readTagSampling{rate: -1, draw: scripted(0)}, false, 0},
		{"rate 1 always", readTagSampling{rate: 1, draw: scripted(math.Nextafter(1, 0))}, true, 1},
		{"at the rate", readTagSampling{rate: 0.01, draw: scripted(0.01)}, true, 1},
		{"above the rate", readTagSampling{rate: 0.01, draw: scripted(math.Nextafter(0.01, 1))}, false, 1},
	} {
		draws = 0
		db := &database{readTags: tc.s}
		if got := db.sampleReadTags(); got != tc.want || draws != tc.draws {
			t.Errorf("%s: sampled=%v after %d draws, want %v after %d", tc.name, got, draws, tc.want, tc.draws)
		}
	}
	if readTagSampleRate != 0.01 {
		t.Fatalf("READ_TAG_SAMPLE_RATE = %v, want 0.01 (ClientKnobs.cpp:294)", readTagSampleRate)
	}
	if o := applyOptions([]Option{withReadTagSampling(0.5, nil)}); o.readTagSampling == nil || o.readTagSampling.rate != 0.5 {
		t.Fatal("withReadTagSampling did not reach the open options")
	}
}

// Each of the five read request kinds carries a sampled tag set, empty or
// not, and leaves the field absent when the draw missed.
func TestReadRequestsRoundTripTags(t *testing.T) {
	t.Parallel()
	reply := transport.UID{First: 1, Second: 2}
	sel := firstGreaterOrEqual([]byte("a"))
	type decoded struct {
		has  bool
		tags []byte
	}
	kinds := map[string]func(readTagSample) (decoded, error){
		"GetValueRequest": func(s readTagSample) (decoded, error) {
			body, bufp := buildGetValueRequest([]byte("k"), 1, false, 0, types.SpanContext{}, s, reply, transport.UID{})
			defer getValueBufPool.Put(bufp)
			var req types.GetValueRequest
			err := req.UnmarshalFDB(body)
			return decoded{req.HasTags, req.Tags}, err
		},
		"GetKeyRequest": func(s readTagSample) (decoded, error) {
			body, bufp := buildGetKeyRequest([]byte("k"), false, 1, 1, false, 0, types.SpanContext{}, s, reply)
			defer getKeyBufPool.Put(bufp)
			var req types.GetKeyRequest
			err := req.UnmarshalFDB(body)
			return decoded{req.HasTags, req.Tags}, err
		},
		"GetKeyValuesRequest": func(s readTagSample) (decoded, error) {
			body, bufp := buildGetKeyValuesRequest(sel, sel, 1, 10, 100, false, 0, types.SpanContext{}, s, reply, transport.UID{})
			defer getKeyValuesBufPool.Put(bufp)
			var req types.GetKeyValuesRequest
			err := req.UnmarshalFDB(body)
			return decoded{req.HasTags, req.Tags}, err
		},
		"GetMappedKeyValuesRequest": func(s readTagSample) (decoded, error) {
			body, bufp := buildGetMappedKeyValuesRequest([]byte("m"))(sel, sel, 1, 10, 100, false, 0, types.SpanContext{}, s, reply, transport.UID{})
			defer getMappedKeyValuesBufPool.Put(bufp)
			var req types.GetMappedKeyValuesRequest
			err := req.UnmarshalFDB(body)
			return decoded{req.HasTags, req.Tags}, err
		},
		"WatchValueRequest": func(s readTagSample) (decoded, error) {
			var req types.WatchValueRequest
			err := req.UnmarshalFDB(buildWatchValueRequest([]byte("k"), nil, 1, 0, types.SpanContext{}, s, reply))
			return decoded{req.HasTags, req.Tags}, err
		},
	}
	for name, build := range kinds {
		for _, tc := range []struct {
			sample readTagSample
			want   decoded
		}{
			{readTagSample{}, decoded{}},
			{readTagSample{present: true, tags: encodeTagSet(nil)}, decoded{has: true}},
			{readTagSample{present: true, tags: encodeTagSet([]string{"a", "bc"})}, decoded{true, []byte("\x01a\x02bc")}},
		} {
			got, err := build(tc.sample)
			if err != nil || got.has != tc.want.has || !bytes.Equal(got.tags, tc.want.tags) {
				t.Errorf("%s with %+v: decoded HasTags=%v Tags=%q (%v), want %v %q", name, tc.sample, got.has, got.tags, err, tc.want.has, tc.want.tags)
			}
		}
	}
}

// Only C++'s own read loop retries draw again; loadBalance's do not.
func TestReadTagRetry(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]bool{
		errReplyTimeout: false,
		&wire.FDBError{Code: ErrRequestMaybeDelivered}: false,
		&wire.FDBError{Code: ErrBrokenPromise}:         false,
		&wire.FDBError{Code: ErrWrongShardServer}:      true,
		&wire.FDBError{Code: ErrAllAlternativesFailed}: true,
	} {
		if got := readTagRetry(err); got != want {
			t.Errorf("readTagRetry(%v) = %v, want %v", err, got, want)
		}
	}
}

// A watch sends the read tags as of the commit that activated it
// (setupWatches, NativeAPI.actor.cpp:6418-6432), else those captured at setup;
// later option changes reach neither.
func TestWatchReadTags(t *testing.T) {
	t.Parallel()
	setup, committed := string(encodeTagSet([]string{"setup"})), string(encodeTagSet([]string{"setup", "commit"}))
	for _, tc := range []struct {
		name       string
		resolve    func(tx *Transaction)
		wantCommit string
	}{
		{"committed", func(tx *Transaction) { tx.fireWatchActivation(5, false) }, committed},
		{"abandoned", func(tx *Transaction) { tx.fireWatchActivation(0, true) }, setup},
		{"pending", func(*Transaction) {}, setup},
	} {
		tx := &Transaction{}
		tx.readTags = []string{"setup"}
		ctx, cancel := tx.newWatchCtx(context.Background())
		tx.readTags = append(tx.readTags, "commit")
		tc.resolve(tx)
		tx.readTags = append(tx.readTags, "after")
		if got := string(tx.watchCommitTags(ctx)); got != tc.wantCommit {
			t.Errorf("%s: commit tags %q, want %q", tc.name, got, tc.wantCommit)
		}
		if got := string(tx.watchSetupTags(ctx)); got != setup {
			t.Errorf("%s: setup tags %q, want %q", tc.name, got, setup)
		}
		cancel()
	}
}
