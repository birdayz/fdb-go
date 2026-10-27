package otel

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

func testOtelBatchReplacement(t *testing.T, ctx context.Context, cluster, order string) {
	c := testConfig()
	c.Count, c.Tenants, c.Batch, c.Workers, c.SaveMode, c.Diagnostics, c.Order = 3, 1, 3, 1, "batch", true, order
	b := openOtelFixture(t, ctx, cluster, c, false)
	c = b.cfg
	old := Generate(c, 0)
	old.Start += 1000000
	oldTerm := Attribute{"span", "ingest.replacement.old", "string", "must_be_removed"}
	old.Attributes[0] = oldTerm
	saveOtelSpan(t, ctx, b, old)
	assertOtelSource(t, ctx, b, old)
	assertOtelCommittedPostings(t, ctx, b, []Span{old})
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		maintainer, err := store.GetIndexMaintainer(b.idx)
		if err != nil {
			return err
		}
		scanner, ok := maintainer.(*postingMaintainer)
		if !ok {
			return errors.New("replacement control has wrong custom maintainer")
		}
		got, err := scanner.Query(c, oldTerm)
		if err != nil {
			return err
		}
		if !sameJSON(got.Hits, []Hit{{old.Trace, old.Start}}) {
			return errors.New("replacement control lacks old source membership")
		}
		for _, name := range []string{spanIndexName, timeIndexName} {
			entries, err := recordlayer.AsList(ctx, store.ScanIndex(b.md.GetIndex(name), recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
			if err != nil {
				return err
			}
			if len(entries) != 1 || !sameJSON(entries[0].PrimaryKey(), otelPK(old)) {
				return errors.New(name + ": replacement control lacks exact old native primary key")
			}
			match, err := intrinsicMatch(entries[0], name, old.Tenant)
			if err != nil {
				return err
			}
			if match != (SpanMatch{old.Trace, old.ID, old.Start}) {
				return errors.New(name + ": replacement control lacks old native timestamp/identity")
			}
		}
		return nil
	})
	stats, err := b.Ingest(ctx)
	if err != nil || stats.Rows != 3 || stats.Attributes != 24 {
		t.Fatalf("batch replacement ingestion: %+v %v", stats, err)
	}
	assertOtelIngestDiagnostics(t, stats, 1, 3)
	want := make([]Span, c.Count)
	for i := range want {
		want[i] = Generate(c, i)
		assertOtelSource(t, ctx, b, want[i])
		matches, err := b.Span(ctx, want[i].ID)
		if err != nil || !sameJSON(matches, []SpanMatch{{want[i].Trace, want[i].ID, want[i].Start}}) {
			t.Fatalf("batch replacement native span identity %d: %+v %v", i, matches, err)
		}
	}
	matches, err := b.Span(ctx, old.ID)
	if err != nil || !sameJSON(matches, []SpanMatch{{want[0].Trace, want[0].ID, want[0].Start}}) {
		t.Fatalf("batch replacement retained old span-index timestamp: %+v %v", matches, err)
	}
	oldTime, err := b.Time(ctx, TimeRange{Start: old.Start, End: old.Start + 1})
	if err != nil || len(oldTime.Hits) != 0 {
		t.Fatalf("batch replacement retained old native time entry: %+v %v", oldTime, err)
	}
	assertOtelCommittedPostings(t, ctx, b, want)
	audited, err := b.Audit(ctx)
	if err != nil || audited.Rows != 3 || audited.Structures["eq_secondary"].Keys != 24 || audited.Structures[spanIndexName].Keys != 3 || audited.Structures[timeIndexName].Keys != 3 {
		t.Fatalf("batch replacement exact source/custom/built-in census: %+v %v", audited, err)
	}
	got, err := b.Query(ctx, oldTerm)
	if err != nil || len(got.Hits) != 0 {
		t.Fatalf("batch replacement retained old term: %+v %v", got, err)
	}
	if stale := otelSnapshot(t, ctx, b, termPrefix(otelSecondary(b), c.Tenant, oldTerm, c.HashBytes)); len(stale) != 0 {
		t.Fatalf("batch replacement left %d stale old-term keys", len(stale))
	}
}

func testOtelSaveAPIDispatch(t *testing.T, ctx context.Context, cluster, order string) {
	for _, mode := range []string{"serial", "pipelined", "batch"} {
		t.Run(mode, func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Batch, c.Workers, c.SaveMode, c.Diagnostics, c.Order = 3, 1, 2, 1, mode, true, order
			b := openOtelFixture(t, ctx, cluster, c, false)
			stats, err := b.Ingest(ctx)
			if err != nil || stats.Rows != 3 || stats.Attributes != 24 {
				t.Fatalf("%s public ingest: %+v %v", mode, stats, err)
			}
			assertOtelIngestDiagnostics(t, stats, 2, 3)
			d := stats.Diagnostics
			for _, event := range []recordlayer.Event{recordlayer.EventOpenStore, recordlayer.EventCommit} {
				counter := d.Timer[event.Name]
				if counter == nil || counter.Event.Kind != recordlayer.KindTimed || counter.Count < 2 || counter.CumulativeValue <= 0 {
					t.Fatalf("%s source work lacks positive native %s timer: %+v", mode, event.Name, counter)
				}
			}
			scalarEvents := []recordlayer.Event{recordlayer.EventSaveRecord, recordlayer.CountSaveRecordKey, recordlayer.CountSaveRecordKeyBytes, recordlayer.CountSaveRecordValueBytes}
			for _, event := range scalarEvents {
				counter := d.Timer[event.Name]
				if mode == "batch" {
					if counter != nil && (counter.Count != 0 || counter.CumulativeValue != 0) {
						t.Fatalf("batch mode dispatched scalar %s work: %+v", event.Name, counter)
					}
					continue
				}
				if counter == nil || counter.Event.Kind != event.Kind || counter.Count <= 0 {
					t.Fatalf("%s mode omitted scalar %s work: %+v", mode, event.Name, counter)
				}
				if event.Name == recordlayer.EventSaveRecord.Name && (counter.Count != 3 || counter.CumulativeValue <= 0) {
					t.Fatalf("%s mode must execute three timed scalar saves: %+v", mode, counter)
				}
				if event.Name == recordlayer.CountSaveRecordKey.Name && counter.Count != 3 {
					t.Fatalf("%s scalar save key count: %+v want 3", mode, counter)
				}
			}
			want := make([]Span, b.cfg.Count)
			for i := range want {
				want[i] = Generate(b.cfg, i)
				assertOtelSource(t, ctx, b, want[i])
			}
			assertOtelCommittedPostings(t, ctx, b, want)
			audited, err := b.Audit(ctx)
			if err != nil || audited.Rows != 3 || audited.Structures["eq_secondary"].Keys != 24 || audited.Structures[spanIndexName].Keys != 3 || audited.Structures[timeIndexName].Keys != 3 {
				t.Fatalf("%s dispatch source/custom/built-in census: %+v %v", mode, audited, err)
			}
			a := Attribute{"span", QueryKey, "string", QueryValue}
			got, err := b.Query(ctx, a)
			if err != nil || len(got.Hits) == 0 || !sameJSON(got.Hits, Expected(b.cfg, a.Scope, a.Key, a.Type, a.Value)) {
				t.Fatalf("%s dispatch positive query: %+v %v", mode, got, err)
			}
		})
	}
}
