package otel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func testOtelPersistedIntrinsicCorruption(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	c := testConfig()
	c.Count, c.Tenants, c.K, c.Order = 4, 1, 100, order
	b := openOtelFixture(t, ctx, cluster, c, true)
	source := Generate(b.cfg, 0)
	wantSpan := []SpanMatch{{Trace: source.Trace, ID: source.ID, Start: source.Start}}
	wantTime := []Hit{}
	seen := map[string]bool{}
	for i := 0; i < b.cfg.Count; i++ {
		s := Generate(b.cfg, i)
		assertOtelSource(t, ctx, b, s)
		if s.Tenant == source.Tenant && s.Start == source.Start && !seen[s.Trace] {
			seen[s.Trace] = true
			wantTime = append(wantTime, Hit{Trace: s.Trace, Latest: s.Start})
		}
	}
	sort.Slice(wantTime, func(i, j int) bool { return wantTime[i].Trace > wantTime[j].Trace })
	assertReady := func(t *testing.T) {
		t.Helper()
		otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
			if err := b.readManifest(rc, true); err != nil {
				return err
			}
			for _, name := range []string{spanIndexName, timeIndexName} {
				state, err := store.ReadIndexState(name)
				if err != nil {
					return err
				}
				if state != recordlayer.IndexStateReadable {
					return fmt.Errorf("%s state changed: %v", name, state)
				}
			}
			return nil
		})
	}
	control := func(t *testing.T) {
		t.Helper()
		matches, err := b.Span(ctx, source.ID)
		if err != nil || !reflect.DeepEqual(matches, wantSpan) {
			t.Fatalf("intact public Span: got=%+v want=%+v err=%v", matches, wantSpan, err)
		}
		if work := b.LastQueryWork(); work.Candidates <= 0 || work.Bytes <= 0 {
			t.Fatalf("intact public Span consumed no work: %+v", work)
		}
		got, err := b.Time(ctx, TimeRange{Start: source.Start, End: source.Start + 1})
		if err != nil || !reflect.DeepEqual(got.Hits, wantTime) || got.Candidates <= 0 || got.Bytes <= 0 {
			t.Fatalf("intact public Time: got=%+v want=%+v err=%v", got, wantTime, err)
		}
		assertReady(t)
	}
	control(t)
	for _, name := range []string{spanIndexName, timeIndexName} {
		t.Run(name, func(t *testing.T) {
			ss := b.store.Sub(recordlayer.IndexKey, b.md.GetIndex(name).SubspaceTupleKey())
			intact := otelSnapshot(t, ctx, b, ss)
			if len(intact) != c.Count {
				t.Fatalf("native %s fixture has %d keys, want %d", name, len(intact), c.Count)
			}
			key := tuple.Tuple{int64(source.Tenant), source.ID, source.Start, source.Trace}
			identityLength := 32
			if name == timeIndexName {
				key = tuple.Tuple{int64(source.Tenant), source.Start, source.Trace, source.ID}
				identityLength = 16
			}
			originalKey := ss.Pack(key)
			originalValue, ok := intact[string(originalKey)]
			if !ok || originalValue != "" {
				t.Fatalf("generated source native %s key missing or has nonempty value: present=%t value=%q", name, ok, originalValue)
			}
			badHex := append(tuple.Tuple(nil), key...)
			badHex[3] = strings.Repeat("z", identityLength)
			zeroIdentity := append(tuple.Tuple(nil), key...)
			zeroIdentity[3] = strings.Repeat("0", identityLength)
			cases := []struct {
				name  string
				key   fdb.Key
				value []byte
			}{
				{"nonempty_value", originalKey, tuple.Tuple{int64(7)}.Pack()},
				{"short_key", ss.Pack(key[:3]), nil},
				{"long_key", ss.Pack(append(append(tuple.Tuple(nil), key...), int64(7))), nil},
				{"invalid_hex_pk", ss.Pack(badHex), nil},
				{"zero_pk", ss.Pack(zeroIdentity), nil},
			}
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					if string(test.key) != string(originalKey) {
						if _, exists := intact[string(test.key)]; exists {
							t.Fatal("fault injection would overwrite another native index entry")
						}
					}
					defer otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
						rc.Transaction().Clear(test.key)
						rc.Transaction().Set(originalKey, []byte(originalValue))
						return nil
					})
					// Test-only fault injection replaces an entry maintained by normal ingestion.
					// Valid tuples preserve the lookup predicate and reach intrinsic validation.
					otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
						rc.Transaction().Clear(originalKey)
						rc.Transaction().Set(test.key, test.value)
						return nil
					})
					wantMutated := make(map[string]string, len(intact))
					for k, v := range intact {
						wantMutated[k] = v
					}
					delete(wantMutated, string(originalKey))
					wantMutated[string(test.key)] = string(test.value)
					if !reflect.DeepEqual(otelSnapshot(t, ctx, b, ss), wantMutated) {
						t.Fatal("native index corruption was not persisted exactly")
					}
					assertReady(t)
					var err error
					if name == spanIndexName {
						_, err = b.Span(ctx, source.ID)
					} else {
						_, err = b.Time(ctx, TimeRange{Start: source.Start, End: source.Start + 1})
					}
					var corruption *CorruptionError
					if !errors.As(err, &corruption) {
						t.Fatalf("public %s accepted persisted corruption or lost its type: %T %v", name, err, err)
					}
					if work := b.LastQueryWork(); work.Candidates <= 0 || work.Bytes <= 0 {
						t.Fatalf("public %s corruption did not consume index work: %+v", name, work)
					}
					assertReady(t)
				})
				if !reflect.DeepEqual(otelSnapshot(t, ctx, b, ss), intact) {
					t.Fatal("native index restoration differs from original owned data")
				}
				control(t)
			}
		})
	}
}
