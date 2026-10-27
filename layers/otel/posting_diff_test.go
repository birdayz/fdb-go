package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func clonePostingDiffInput(in []posting) []posting {
	if in == nil {
		return nil
	}
	out := make([]posting, len(in))
	for i, p := range in {
		out[i] = posting{fdb.Key(bytes.Clone(p.Key)), bytes.Clone(p.Value)}
	}
	return out
}

func TestPostingDiffFullKeyAndValue(t *testing.T) {
	t.Parallel()
	p := func(key, value string) posting { return posting{fdb.Key(key), []byte(value)} }
	fresh := []posting{}
	for i := 0; i < 8; i++ {
		fresh = append(fresh, p(fmt.Sprintf("attribute_%d", i), fmt.Sprint(i)))
	}
	for _, test := range []struct {
		name                          string
		before, after, removed, added []posting
	}{
		{"empty", nil, nil, nil, nil},
		{"fresh_eight", nil, fresh, nil, fresh},
		{"delete_eight", fresh, nil, fresh, nil},
		{"identical_eight", fresh, clonePostingDiffInput(fresh), nil, nil},
		{"same_key_changed_value", []posting{p("key", "old")}, []posting{p("key", "new")}, []posting{p("key", "old")}, []posting{p("key", "new")}},
		{"different_full_key_same_value", []posting{p("prefix\x00old", "same")}, []posting{p("prefix\x00new", "same")}, []posting{p("prefix\x00old", "same")}, []posting{p("prefix\x00new", "same")}},
		{"empty_value_equal", []posting{p("key", "")}, []posting{p("key", "")}, nil, nil},
		{"empty_value_changed", []posting{p("key", "")}, []posting{p("key", "new")}, []posting{p("key", "")}, []posting{p("key", "new")}},
		{"duplicate_pair_removed_once", []posting{p("key", "v"), p("key", "v")}, []posting{p("key", "v")}, []posting{p("key", "v")}, nil},
		{"duplicate_pair_added_once", []posting{p("key", "v")}, []posting{p("key", "v"), p("key", "v")}, nil, []posting{p("key", "v")}},
		{"duplicate_key_value_multiset", []posting{p("key", "v"), p("key", "w")}, []posting{p("key", "v"), p("key", "v")}, []posting{p("key", "w")}, []posting{p("key", "v")}},
		{"stable_order", []posting{p("z", "old"), p("a", "keep"), p("b", "remove")}, []posting{p("c", "new"), p("a", "keep"), p("z", "changed")}, []posting{p("z", "old"), p("b", "remove")}, []posting{p("c", "new"), p("z", "changed")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, after := clonePostingDiffInput(test.before), clonePostingDiffInput(test.after)
			removed, added := diffPostings(test.before, test.after)
			if len(removed) != len(test.removed) || len(added) != len(test.added) {
				t.Fatalf("mutation counts clear=%d/set=%d want %d/%d", len(removed), len(added), len(test.removed), len(test.added))
			}
			for i := range removed {
				if !reflect.DeepEqual(removed[i], test.removed[i]) {
					t.Fatalf("removed[%d]=%+v want %+v", i, removed[i], test.removed[i])
				}
			}
			for i := range added {
				if !reflect.DeepEqual(added[i], test.added[i]) {
					t.Fatalf("added[%d]=%+v want %+v", i, added[i], test.added[i])
				}
			}
			if !reflect.DeepEqual(before, test.before) || !reflect.DeepEqual(after, test.after) {
				t.Fatal("posting diff mutated caller input")
			}
		})
	}
}

func TestPostingDiffCollisionAndOrdinalOwnership(t *testing.T) {
	t.Parallel()
	c := testConfig()
	ss := subspace.Sub("posting_diff_fixture")
	for _, order := range []string{"trace", "time"} {
		t.Run(order, func(t *testing.T) {
			build := func(s Span) []posting {
				m, err := toRecord(s)
				if err != nil {
					t.Fatal(err)
				}
				out, err := postings(ss, 1, order, &recordlayer.FDBStoredRecord[proto.Message]{Record: m})
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			s := Generate(c, 0)
			before := build(s)
			colliding := Attribute{"span", collision(QueryKey), "string", collision(QueryValue)}
			if !bytes.Equal(termPrefix(ss, s.Tenant, s.Attributes[0], 1).Bytes(), termPrefix(ss, s.Tenant, colliding, 1).Bytes()) {
				t.Fatal("collision control did not share the shortened term prefix")
			}
			s.Attributes = append(s.Attributes, colliding)
			after := build(s)
			removed, added := diffPostings(before, after)
			if len(before) != 8 || len(after) != 9 || len(removed) != 0 || len(added) != 1 {
				t.Fatalf("colliding new occurrence lost ordinal ownership: before=%d after=%d clears=%d sets=%d", len(before), len(after), len(removed), len(added))
			}
			for _, old := range before {
				if bytes.Equal(old.Key, added[0].Key) {
					t.Fatal("new collision occurrence reused an existing full key")
				}
			}
			changed := Generate(c, 0)
			changed.Attributes[7].Value = collision(changed.Attributes[7].Value)
			changedPostings := build(changed)
			removed, added = diffPostings(before, changedPostings)
			if len(removed) != 1 || len(added) != 1 || !bytes.Equal(removed[0].Key, added[0].Key) || bytes.Equal(removed[0].Value, added[0].Value) {
				t.Fatalf("same-full-key changed covering value not replaced: clears=%+v sets=%+v", removed, added)
			}
			corrected := Generate(c, 0)
			corrected.Start++
			removed, added = diffPostings(before, build(corrected))
			if len(removed) != 8 || len(added) != 8 {
				t.Fatalf("time correction mutation count: %d/%d", len(removed), len(added))
			}
			if bytes.Equal(removed[0].Key, added[0].Key) != (order == "trace") {
				t.Fatal("time correction did not exercise the declared physical key layout")
			}
			s.Attributes[0], s.Attributes[8] = s.Attributes[8], s.Attributes[0]
			removed, added = diffPostings(after, build(s))
			if len(removed) != 2 || len(added) != 2 {
				t.Fatalf("colliding ordinal reorder skipped changed ownership: %d/%d", len(removed), len(added))
			}
		})
	}
}

type postingDiffMutationSample struct {
	mu           sync.Mutex
	clears, sets int
	size         int64
	sizeErr      error
	measured     bool
}

type postingDiffCapture struct {
	mu      sync.Mutex
	samples []*postingDiffMutationSample
}
type postingDiffTransactor struct {
	fdb.Database
	prefix  []byte
	capture *postingDiffCapture
}
type postingDiffTransaction struct {
	fdb.WritableTransaction
	prefix []byte
	sample *postingDiffMutationSample
}
type postingDiffSizeFuture struct {
	fdb.FutureInt64
	sample *postingDiffMutationSample
}

func (tx postingDiffTransaction) Set(key fdb.KeyConvertible, value []byte) {
	if bytes.HasPrefix(key.FDBKey(), tx.prefix) {
		tx.sample.mu.Lock()
		tx.sample.sets++
		tx.sample.mu.Unlock()
	}
	tx.WritableTransaction.Set(key, value)
}

func (tx postingDiffTransaction) Clear(key fdb.KeyConvertible) {
	if bytes.HasPrefix(key.FDBKey(), tx.prefix) {
		tx.sample.mu.Lock()
		tx.sample.clears++
		tx.sample.mu.Unlock()
	}
	tx.WritableTransaction.Clear(key)
}

func (tx postingDiffTransaction) GetApproximateSize() fdb.FutureInt64 {
	return postingDiffSizeFuture{tx.WritableTransaction.GetApproximateSize(), tx.sample}
}

func (future postingDiffSizeFuture) Get() (int64, error) {
	n, err := future.FutureInt64.Get()
	future.sample.mu.Lock()
	future.sample.size, future.sample.sizeErr, future.sample.measured = n, err, true
	future.sample.mu.Unlock()
	return n, err
}

func (d postingDiffTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d postingDiffTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		sample := &postingDiffMutationSample{}
		d.capture.mu.Lock()
		d.capture.samples = append(d.capture.samples, sample)
		d.capture.mu.Unlock()
		return fn(postingDiffTransaction{tx, d.prefix, sample})
	})
}

func capturePostingDiff(t *testing.T, b *FDBBackend, run func() error) (*postingDiffCapture, error) {
	t.Helper()
	capture := &postingDiffCapture{}
	original := b.db
	b.db = recordlayer.NewFDBDatabaseWithTransactor(postingDiffTransactor{b.raw, otelSecondary(b).Bytes(), capture}, b.raw)
	defer func() { b.db = original }()
	return capture, run()
}

func assertPostingDiffMutations(t *testing.T, capture *postingDiffCapture, clears, sets int, measured bool) int64 {
	t.Helper()
	capture.mu.Lock()
	samples := append([]*postingDiffMutationSample(nil), capture.samples...)
	capture.mu.Unlock()
	if len(samples) == 0 {
		t.Fatal("no actual source transaction was observed")
	}
	var finalSize int64
	for i, sample := range samples {
		sample.mu.Lock()
		actualClear, actualSet, size, sizeErr, gotMeasurement := sample.clears, sample.sets, sample.size, sample.sizeErr, sample.measured
		sample.mu.Unlock()
		if actualClear != clears || actualSet != sets || gotMeasurement != measured {
			t.Fatalf("transaction %d custom mutation count clear=%d/set=%d measured=%t want %d/%d/%t", i, actualClear, actualSet, gotMeasurement, clears, sets, measured)
		}
		if measured && (sizeErr != nil || size <= 0) {
			t.Fatalf("transaction %d real approximate size=%d error=%v", i, size, sizeErr)
		}
		finalSize = size
	}
	return finalSize
}

func testOtelPostingDiff(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	for _, requested := range []int{32, 64} {
		t.Run(fmt.Sprintf("batch_%d", requested), func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Batch, c.Workers, c.SaveMode, c.Order = 64, 1, requested, 1, "batch", order
			probe, err := prepareIngestBatch(ctx, c, 0)
			if err != nil {
				t.Fatal(err)
			}
			wantRows := 32
			if requested == 64 {
				wantRows = 45
			}
			if probe.first != 0 || probe.next != wantRows || len(probe.messages) != wantRows || (requested == 64 && probe.next >= c.Count) {
				t.Fatalf("requested batch=%d did not exercise expected estimate cap: first=%d next=%d count=%d", requested, probe.first, probe.next, c.Count)
			}
			c.Count = wantRows
			b := openOtelFixture(t, ctx, cluster, c, false)
			c = b.cfg
			if len(c.Run) != 29 || len(b.root.Bytes()) != len(tuple.Tuple{"otel_otelbench_v2", strings.Repeat("r", 29)}.Pack()) {
				t.Fatalf("size fixture namespace shape changed: run bytes=%d root bytes=%d", len(c.Run), len(b.root.Bytes()))
			}
			prepareConfig := c
			prepareConfig.Count = 64
			batch, err := prepareIngestBatch(ctx, prepareConfig, 0)
			if err != nil || batch.next != wantRows {
				t.Fatalf("bounded source batch changed after namespace allocation: %+v %v", batch, err)
			}
			otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
				wire, err := json.Marshal(manifest(c, "loading"))
				if err == nil {
					rc.Transaction().Set(b.root.Sub("owner").FDBKey(), wire)
				}
				return err
			})
			freshCapture, err := capturePostingDiff(t, b, func() error { return b.saveIngestBatch(ctx, batch, nil) })
			freshSize := assertPostingDiffMutations(t, freshCapture, 0, 8*wantRows, true)
			if err != nil || freshSize > 500000 {
				t.Fatalf("fresh batch=%d effective=%d rootbytes=%d size=%d: %v", requested, wantRows, len(b.root.Bytes()), freshSize, err)
			}
			otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
				wire, err := json.Marshal(manifest(c, "ready"))
				if err == nil {
					rc.Transaction().Set(b.root.Sub("owner").FDBKey(), wire)
				}
				return err
			})
			sources := make([]Span, wantRows)
			for i := range sources {
				sources[i] = Generate(c, i)
				assertOtelSource(t, ctx, b, sources[i])
			}
			assertOtelCommittedPostings(t, ctx, b, sources)
			before := otelSnapshot(t, ctx, b, b.root)
			replayCapture, err := capturePostingDiff(t, b, func() error { return b.saveIngestBatch(ctx, batch, nil) })
			replaySize := assertPostingDiffMutations(t, replayCapture, 0, 0, true)
			if err != nil || replaySize > 500000 || !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, b.root)) {
				t.Fatalf("identical replay batch=%d effective=%d rootbytes=%d size=%d: %v", requested, wantRows, len(b.root.Bytes()), replaySize, err)
			}
			if replaySize >= freshSize {
				t.Fatalf("zero custom mutations must reduce actual transaction size: fresh=%d replay=%d", freshSize, replaySize)
			}
			audit, err := b.Audit(ctx)
			if err != nil || audit.Rows != int64(wantRows) || audit.Structures["eq_secondary"].Keys != int64(8*wantRows) || audit.Structures[spanIndexName].Keys != int64(wantRows) || audit.Structures[timeIndexName].Keys != int64(wantRows) {
				t.Fatalf("identical replay source/custom/native census: %+v %v", audit, err)
			}
			if err := AuditQueries(ctx, b, c); err != nil {
				t.Fatal(err)
			}
			changed := batch
			changed.messages = make([]proto.Message, wantRows)
			changedSources := make([]Span, wantRows)
			for i := range changedSources {
				changedSources[i] = Generate(c, i)
				changedSources[i].Attributes[7].Value += "_changed"
				changed.messages[i], err = toRecord(changedSources[i])
				if err != nil {
					t.Fatal(err)
				}
			}
			changedCapture, changedErr := capturePostingDiff(t, b, func() error { return b.saveIngestBatch(ctx, changed, nil) })
			changedSize := assertPostingDiffMutations(t, changedCapture, wantRows, wantRows, true)
			if changedErr != nil {
				var budget *QueryBudgetError
				if !errors.As(changedErr, &budget) {
					t.Fatalf("changed old images failed outside explicit ingestion guard: size=%d %T %v", changedSize, changedErr, changedErr)
				}
				if !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, b.root)) {
					t.Fatal("rejected changed-image batch altered source/custom/native keys")
				}
				for _, source := range sources {
					assertOtelSource(t, ctx, b, source)
				}
				assertOtelCommittedPostings(t, ctx, b, sources)
				t.Logf("changed-image batch rejected by intact guard: requested=%d effective=%d rootbytes=%d approximatebytes=%d error=%v", requested, wantRows, len(b.root.Bytes()), changedSize, changedErr)
			} else {
				if changedSize > 500000 {
					t.Fatalf("changed-image batch admitted above unchanged byte guard: %d", changedSize)
				}
				for i, source := range changedSources {
					assertOtelSource(t, ctx, b, source)
					old, err := b.Query(ctx, sources[i].Attributes[7])
					if err != nil || len(old.Hits) != 0 {
						t.Fatalf("changed-image batch left old membership: %+v %v", old, err)
					}
					got, err := b.Query(ctx, source.Attributes[7])
					if err != nil || !sameJSON(got.Hits, []Hit{{source.Trace, source.Start}}) {
						t.Fatalf("changed-image exact new membership: %+v %v", got, err)
					}
				}
				assertOtelCommittedPostings(t, ctx, b, changedSources)
				for _, name := range []string{spanIndexName, timeIndexName} {
					sz, err := b.size(ctx, b.store.Sub(recordlayer.IndexKey, b.md.GetIndex(name).SubspaceTupleKey()))
					if err != nil || sz.Keys != int64(wantRows) {
						t.Fatalf("changed-image %s census: %+v %v", name, sz, err)
					}
				}
				t.Logf("changed-image batch admitted: requested=%d effective=%d rootbytes=%d approximatebytes=%d", requested, wantRows, len(b.root.Bytes()), changedSize)
			}
			t.Logf("actual batch mutation bytes: requested=%d effective=%d rootbytes=%d fresh=%d identicalreplay=%d", requested, wantRows, len(b.root.Bytes()), freshSize, replaySize)
		})
	}
	t.Run("admission_and_write_only", func(t *testing.T) { testOtelPostingDiffAdmissionAndWriteOnly(t, ctx, cluster, order) })
}

func testOtelPostingDiffAdmissionAndWriteOnly(t *testing.T, ctx context.Context, cluster, order string) {
	c := testConfig()
	c.Count, c.Tenants, c.HashBytes, c.Order = 1, 1, 1, order
	b := openOtelFixture(t, ctx, cluster, c, true)
	s := Generate(b.cfg, 0)
	s.Attributes = append(s.Attributes, Attribute{"span", collision(QueryKey), "string", collision(QueryValue)})
	saveOtelSpan(t, ctx, b, s)
	assertOtelCommittedPostings(t, ctx, b, []Span{s})
	before := otelSnapshot(t, ctx, b, b.root)
	for _, invalidSide := range []string{"old", "new"} {
		capture, err := capturePostingDiff(t, b, func() error {
			_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := b.open(rc)
				if err != nil {
					return nil, err
				}
				old, err := store.LoadRecord(otelPK(s))
				if err != nil {
					return nil, err
				}
				if old == nil {
					return nil, errors.New("admission control source disappeared")
				}
				bad := &recordlayer.FDBStoredRecord[proto.Message]{Record: proto.Clone(old.Record)}
				ref := bad.Record.ProtoReflect()
				ref.Set(ref.Descriptor().Fields().ByName("otlp"), protoreflect.ValueOfBytes([]byte{0xff}))
				maintainer, err := store.GetIndexMaintainer(b.idx)
				if err != nil {
					return nil, err
				}
				if invalidSide == "old" {
					return nil, maintainer.Update(bad, old)
				}
				return nil, maintainer.Update(old, bad)
			})
			return err
		})
		var admission *AdmissionError
		if !errors.As(err, &admission) {
			t.Fatalf("invalid %s image should reject before custom mutation: %T %v", invalidSide, err, err)
		}
		assertPostingDiffMutations(t, capture, 0, 0, false)
		if !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, b.root)) {
			t.Fatalf("invalid %s image mutated persisted data", invalidSide)
		}
	}
	otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
		rangeToClear, err := fdb.PrefixRange(otelSecondary(b).Bytes())
		if err == nil {
			rc.Transaction().ClearRange(rangeToClear)
		}
		return err
	})
	if got := otelSnapshot(t, ctx, b, otelSecondary(b)); len(got) != 0 {
		t.Fatal("WRITE_ONLY missing-index control did not clear custom entries")
	}
	capture, err := capturePostingDiff(t, b, func() error {
		_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := b.open(rc)
			if err != nil {
				return nil, err
			}
			old, err := store.LoadRecord(otelPK(s))
			if err != nil {
				return nil, err
			}
			if old == nil {
				return nil, errors.New("WRITE_ONLY repair source disappeared")
			}
			maintainer, err := store.GetIndexMaintainer(b.idx)
			if err != nil {
				return nil, err
			}
			return nil, maintainer.UpdateWhileWriteOnly(old, old)
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPostingDiffMutations(t, capture, 9, 9, false)
	assertOtelSource(t, ctx, b, s)
	assertOtelCommittedPostings(t, ctx, b, []Span{s})
	if !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, b.root)) {
		t.Fatal("WRITE_ONLY same-image repair did not restore exact custom/native/source data")
	}
	corrected := s
	corrected.Start++
	m, err := toRecord(corrected)
	if err != nil {
		t.Fatal(err)
	}
	capture, err = capturePostingDiff(t, b, func() error {
		_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := b.open(rc)
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(m)
			return nil, err
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPostingDiffMutations(t, capture, 9, 9, false)
	assertOtelSource(t, ctx, b, corrected)
	assertOtelCommittedPostings(t, ctx, b, []Span{corrected})
	for _, a := range []Attribute{s.Attributes[0], s.Attributes[8]} {
		got, err := b.Query(ctx, a)
		if err != nil || !sameJSON(got.Hits, []Hit{{corrected.Trace, corrected.Start}}) {
			t.Fatalf("colliding term lost covering timestamp replacement: %+v %v", got, err)
		}
	}
	matches, err := b.Span(ctx, corrected.ID)
	if err != nil || !sameJSON(matches, []SpanMatch{{corrected.Trace, corrected.ID, corrected.Start}}) {
		t.Fatalf("corrected source native span identity: %+v %v", matches, err)
	}
	oldTime, err := b.Time(ctx, TimeRange{Start: s.Start, End: s.Start + 1})
	if err != nil || len(oldTime.Hits) != 0 {
		t.Fatalf("correction retained old native timestamp: %+v %v", oldTime, err)
	}
	for _, name := range []string{spanIndexName, timeIndexName} {
		sz, err := b.size(ctx, b.store.Sub(recordlayer.IndexKey, b.md.GetIndex(name).SubspaceTupleKey()))
		if err != nil || sz.Keys != 1 {
			t.Fatalf("correction native %s census: %+v %v", name, sz, err)
		}
	}
}
