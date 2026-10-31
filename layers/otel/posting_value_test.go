package otel

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

func TestPostingValueFormatOptions(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"", "json", "tuple"} {
		index := recordlayer.NewIndex("posting_value", recordlayer.Field("start_ns"))
		index.Type = indexType
		index.SetOption("hash_bytes", "32")
		index.SetOption("order", "time")
		if format != "" {
			index.SetOption("value_format", format)
		}
		if err := (postingFactory{}).ValidateIndexOptions(index); err != nil {
			t.Fatalf("format %q rejected: %v", format, err)
		}
		if err := (postingFactory{}).ValidateChangedOptions(index, index, map[string]bool{"value_format": true}); err == nil {
			t.Fatalf("format %q allowed a physical-format evolution", format)
		}
	}
	for _, format := range []string{"none", "Tuple", "tuple2", "json "} {
		index := recordlayer.NewIndex("posting_value", recordlayer.Field("start_ns"))
		index.Type = indexType
		index.SetOption("hash_bytes", "32")
		index.SetOption("order", "time")
		index.SetOption("value_format", format)
		var config *ConfigError
		if err := (postingFactory{}).ValidateIndexOptions(index); !errors.As(err, &config) {
			t.Fatalf("unknown format %q admitted: %v", format, err)
		}
		if _, err := encodeCoveringValue(coveringValue{}, format); !errors.As(err, &config) {
			t.Fatalf("unknown encode format %q admitted: %v", format, err)
		}
		if _, err := decodeCoveringValue(nil, format); !errors.As(err, &config) {
			t.Fatalf("unknown decode format %q admitted: %v", format, err)
		}
		if _, err := postingsWithFormat(subspace.Sub("unknown"), 32, "time", format, nil); !errors.As(err, &config) {
			t.Fatalf("nil record bypassed format validation: %v", err)
		}
	}
}

func TestPostingValueByteEncodingAndRoundTrip(t *testing.T) {
	t.Parallel()
	legacy := coveringValue{Attribute{"span", "key", "string", "value"}, 42}
	wantJSON := []byte(`{"a":{"scope":"span","key":"key","type":"string","value":"value"},"t":42}`)
	for _, format := range []string{"", "json"} {
		got, err := encodeCoveringValue(legacy, format)
		if err != nil || !bytes.Equal(got, wantJSON) {
			t.Fatalf("legacy bytes changed: %q %v", got, err)
		}
		back, err := decodeCoveringValue(got, format)
		if err != nil || back != legacy {
			t.Fatalf("legacy round trip: %+v %v", back, err)
		}
	}
	value := coveringValue{Attribute{"s", "k", "string", "v\x00q"}, 42}
	wantTuple := []byte{0x02, 's', 0, 0x02, 'k', 0, 0x02, 's', 't', 'r', 'i', 'n', 'g', 0, 0x02, 'v', 0, 0xff, 'q', 0, 0x15, 42}
	got, err := encodeCoveringValue(value, "tuple")
	if err != nil || !bytes.Equal(got, wantTuple) {
		t.Fatalf("not the standard FDB five-field tuple: %x %v", got, err)
	}
	for _, value := range []coveringValue{
		value,
		{Attribute{"resource", "same.literal.key", "int", "283"}, math.MinInt64},
		{Attribute{"scope", "quote\"slash\\\x00", "string", "unicode_雪_😀"}, math.MaxInt64},
		{Attribute{"span", "", "string", ""}, 0},
	} {
		got, err := encodeCoveringValue(value, "tuple")
		if err != nil {
			t.Fatal(err)
		}
		back, err := decodeCoveringValue(got, "tuple")
		if err != nil || back != value {
			t.Fatalf("tuple lost original strings/time: got=%+v want=%+v err=%v", back, value, err)
		}
		again, err := encodeCoveringValue(back, "tuple")
		if err != nil || sha256.Sum256(got) != sha256.Sum256(again) || !bytes.Equal(got, again) {
			t.Fatalf("tuple round trip lost exact bytes: %v", err)
		}
	}
}

func TestPostingValueRejectsMalformedTuples(t *testing.T) {
	t.Parallel()
	good := tuple.Tuple{"span", "key", "string", "value", int64(1700000000000000000)}
	cases := map[string][]byte{
		"empty":             nil,
		"short tuple":       good[:4].Pack(),
		"extra tuple field": append(append(tuple.Tuple(nil), good...), "extra").Pack(),
		"truncated string":  {0x02, 's'},
		"trailing garbage":  append(good.Pack(), 0xff),
		"legacy JSON":       []byte(`{"a":{"scope":"span"},"t":1}`),
		"key over cap":      (tuple.Tuple{"span", strings.Repeat("k", 1025), "string", "value", int64(1)}).Pack(),
		"value over cap":    (tuple.Tuple{"span", "key", "string", strings.Repeat("v", 64001), int64(1)}).Pack(),
	}
	for i := 0; i < 4; i++ {
		for j, bad := range []tuple.TupleElement{nil, []byte("not string"), int64(1), false, tuple.Tuple{"nested"}, string([]byte{0xff})} {
			fields := append(tuple.Tuple(nil), good...)
			fields[i] = bad
			cases[fmt.Sprintf("field %d type %d", i, j)] = fields.Pack()
		}
	}
	for i, bad := range []tuple.TupleElement{nil, "1", []byte{1}, float64(1), true, uint64(math.MaxUint64)} {
		fields := append(tuple.Tuple(nil), good...)
		fields[4] = bad
		cases[fmt.Sprintf("timestamp type %d", i)] = fields.Pack()
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			var corruption *CorruptionError
			if _, err := decodeCoveringValue(wire, "tuple"); !errors.As(err, &corruption) {
				t.Fatalf("malformed tuple admitted: %x %v", wire, err)
			}
		})
	}
	var corruption *CorruptionError
	if _, err := decodeCoveringValue([]byte("{broken"), "json"); !errors.As(err, &corruption) {
		t.Fatalf("malformed legacy JSON admitted: %v", err)
	}
}

func postingValueRecord(t *testing.T, source Span) *recordlayer.FDBStoredRecord[proto.Message] {
	t.Helper()
	message, err := toRecord(source)
	if err != nil {
		t.Fatal(err)
	}
	return &recordlayer.FDBStoredRecord[proto.Message]{Record: message}
}

func TestPostingValueFormatsPreserveAllKeysAndCoverage(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"trace", "time"} {
		t.Run(order, func(t *testing.T) {
			source := Generate(testConfig(), 0)
			source.Attributes = append(source.Attributes, Attribute{"span", collision(QueryKey), "string", collision(QueryValue)})
			record := postingValueRecord(t, source)
			ss := subspace.Sub("posting_value_keys")
			legacy, err := postings(ss, 1, order, record)
			if err != nil {
				t.Fatal(err)
			}
			projected := map[coveringValue]int{}
			for _, a := range source.Attributes {
				projected[coveringValue{a, source.Start}]++
			}
			for _, format := range []string{"", "json", "tuple"} {
				entries, err := postingsWithFormat(ss, 1, order, format, record)
				if err != nil || len(entries) != len(source.Attributes) {
					t.Fatalf("format=%q dropped covering occurrences: %d %v", format, len(entries), err)
				}
				seenKeys := map[string]bool{}
				covered := map[coveringValue]int{}
				for i, entry := range entries {
					if !bytes.Equal(entry.Key, legacy[i].Key) || seenKeys[string(entry.Key)] {
						t.Fatalf("format=%q changed keys or lost digest-collision ownership", format)
					}
					seenKeys[string(entry.Key)] = true
					value, err := decodeCoveringValue(entry.Value, format)
					if err != nil {
						t.Fatal(err)
					}
					covered[value]++
					if format != "tuple" {
						want, err := json.Marshal(value)
						if err != nil || !bytes.Equal(entry.Value, legacy[i].Value) || !bytes.Equal(entry.Value, want) {
							t.Fatal("legacy JSON wire bytes changed")
						}
					}
				}
				if !reflect.DeepEqual(covered, projected) {
					t.Fatalf("format=%q lost a full original field projection", format)
				}
			}
		})
	}
	for _, tc := range []struct {
		key, value int
		reject     bool
	}{{1024, 1, false}, {1025, 1, true}, {1, 64000, false}, {1, 64001, true}} {
		source := Generate(testConfig(), 0)
		source.Attributes = []Attribute{{"span", strings.Repeat("k", tc.key), "string", strings.Repeat("v", tc.value)}}
		record := postingValueRecord(t, source)
		entries, err := postingsWithFormat(subspace.Sub("tuple_cap"), 32, "trace", "tuple", record)
		var admission *AdmissionError
		if tc.reject {
			if !errors.As(err, &admission) || len(entries) != 0 {
				t.Fatalf("over-cap tuple projection admitted: %v", err)
			}
		} else {
			if err != nil || len(entries) != 1 {
				t.Fatalf("at-cap tuple projection rejected: %v", err)
			}
			if _, err := decodeCoveringValue(entries[0].Value, "tuple"); err != nil {
				t.Fatalf("at-cap tuple projection did not decode: %v", err)
			}
		}
	}
	if entries, err := postingsWithFormat(subspace.Sub("tuple_nil"), 32, "time", "tuple", nil); err != nil || entries != nil {
		t.Fatalf("nil source should have no tuple postings: %v", err)
	}
}

// Only the custom maintainer's mutations are implemented; an unexpected read
// reaches the nil embedded transaction and fails rather than hiding a fetch.
type postingValueTransaction struct {
	fdb.WritableTransaction
	sets   []posting
	clears []fdb.Key
}

func (tx *postingValueTransaction) Set(key fdb.KeyConvertible, value []byte) {
	tx.sets = append(tx.sets, posting{fdb.Key(bytes.Clone(key.FDBKey())), bytes.Clone(value)})
}

func (tx *postingValueTransaction) Clear(key fdb.KeyConvertible) {
	tx.clears = append(tx.clears, fdb.Key(bytes.Clone(key.FDBKey())))
}

func TestPostingValueTupleMutationAndWriteOnlyPaths(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"trace", "time"} {
		t.Run(order, func(t *testing.T) {
			tx := &postingValueTransaction{}
			index := recordlayer.NewIndex("tuple_mutations", recordlayer.Field("start_ns"))
			m := &postingMaintainer{
				StandardIndexMaintainer: *recordlayer.NewStandardIndexMaintainer(recordlayer.IndexMaintainerState{Index: index, Transaction: tx}),
				secondary:               subspace.Sub("tuple_mutations"), width: 1, order: order, valueFormat: "tuple",
			}
			source := Generate(testConfig(), 0)
			original := postingValueRecord(t, source)
			if err := m.Update(nil, original); err != nil || len(tx.sets) != 8 || len(tx.clears) != 0 {
				t.Fatalf("fresh source lost eight synchronous postings: %d/%d %v", len(tx.clears), len(tx.sets), err)
			}
			for _, entry := range tx.sets {
				if _, err := decodeCoveringValue(entry.Value, "tuple"); err != nil {
					t.Fatal(err)
				}
			}
			tx.sets, tx.clears = nil, nil
			if err := m.Update(original, original); err != nil || len(tx.sets)+len(tx.clears) != 0 {
				t.Fatalf("identical replay rewrote tuple postings: %v", err)
			}
			if err := m.UpdateWhileWriteOnly(original, original); err != nil || len(tx.sets) != 8 || len(tx.clears) != 8 {
				t.Fatalf("WRITE_ONLY skipped full materialization: %d/%d %v", len(tx.clears), len(tx.sets), err)
			}
			tx.sets, tx.clears = nil, nil
			changed := Generate(testConfig(), 0)
			changed.Attributes[7].Value = collision(changed.Attributes[7].Value)
			if err := m.Update(original, postingValueRecord(t, changed)); err != nil || len(tx.sets) != 1 || len(tx.clears) != 1 {
				t.Fatalf("changed covering value was skipped: %d/%d %v", len(tx.clears), len(tx.sets), err)
			}
			if !bytes.Equal(tx.sets[0].Key, tx.clears[0]) {
				t.Fatal("collision control did not replace a value under the same full key")
			}
			value, err := decodeCoveringValue(tx.sets[0].Value, "tuple")
			if err != nil || value.Attribute != changed.Attributes[7] {
				t.Fatalf("replacement lost full collision verification fields: %+v %v", value, err)
			}
			tx.sets, tx.clears = nil, nil
			if err := m.Update(original, nil); err != nil || len(tx.sets) != 0 || len(tx.clears) != 8 {
				t.Fatalf("delete lost tuple postings: %d/%d %v", len(tx.clears), len(tx.sets), err)
			}
			tx.sets, tx.clears = nil, nil
			bad := Generate(testConfig(), 0)
			bad.Attributes[7].Value = strings.Repeat("v", 64001)
			invalid := postingValueRecord(t, bad)
			for _, side := range []bool{false, true} {
				old, new := original, invalid
				if side {
					old, new = invalid, original
				}
				var admission *AdmissionError
				if err := m.Update(old, new); !errors.As(err, &admission) || len(tx.sets)+len(tx.clears) != 0 {
					t.Fatalf("invalid old/new source mutated postings: %v", err)
				}
			}
			if m.IsPendingWriteQueueAllowed() {
				t.Fatal("value encoding must not silently enable queued maintenance")
			}
		})
	}
}

func TestPostingValueRealFixtureByteCensus(t *testing.T) {
	t.Parallel()
	const recordsPerCase = 16
	for _, seed := range []uint64{1, 283, 991} {
		for _, attributes := range []int{4, 8, 128} {
			c := testConfig()
			c.Seed, c.Attributes, c.Count = seed, attributes, 100000
			c.Tenants, c.MatchEvery = 1, 100
			var jsonBytes, tupleBytes int
			for n := 0; n < recordsPerCase; n++ {
				i := []int{0, 1, 2, 3, 198, 199, 200, 201, 9998, 9999, 19998, 19999, 49998, 49999, 99998, 99999}[n]
				record := postingValueRecord(t, Generate(c, i))
				legacy, err := postings(subspace.Sub("tuple_census"), 32, "time", record)
				if err != nil {
					t.Fatal(err)
				}
				compact, err := postingsWithFormat(subspace.Sub("tuple_census"), 32, "time", "tuple", record)
				if err != nil || len(legacy) != attributes || len(compact) != attributes {
					t.Fatalf("invalid census population: %d/%d %v", len(legacy), len(compact), err)
				}
				for j := range legacy {
					if !bytes.Equal(legacy[j].Key, compact[j].Key) {
						t.Fatal("value census changed index keys")
					}
					left, err := decodeCoveringValue(legacy[j].Value, "json")
					if err != nil {
						t.Fatal(err)
					}
					right, err := decodeCoveringValue(compact[j].Value, "tuple")
					if err != nil || left != right {
						t.Fatalf("census projections differ: %v", err)
					}
					jsonBytes += len(legacy[j].Value)
					tupleBytes += len(compact[j].Value)
				}
			}
			if jsonBytes-tupleBytes != recordsPerCase*attributes*55 {
				t.Fatalf("fixture field accounting changed: seed=%d attrs=%d json=%d tuple=%d", seed, attributes, jsonBytes, tupleBytes)
			}
			t.Logf("fixture byte census only (not throughput/storage measurement): seed=%d records=%d attrs/record=%d postings=%d json_value_bytes=%d tuple_value_bytes=%d saved_value_bytes=%d", seed, recordsPerCase, attributes, recordsPerCase*attributes, jsonBytes, tupleBytes, jsonBytes-tupleBytes)
		}
	}
}
