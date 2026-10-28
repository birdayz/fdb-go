package otel

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

func testOtelIngestFailedPreparation(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	c := testConfig()
	c.Count, c.Tenants, c.Batch, c.Workers, c.Diagnostics, c.Order = 2, 1, 1, 1, true, order
	b := openOtelFixture(t, ctx, cluster, c, false)
	seedOtelProfileClientMetrics(t, ctx, b)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started, prepared := 0, 0
	b.ingestProbe = func(event ingestEvent) {
		if event.kind == "preparation_started" {
			started++
			cancel()
		}
		if event.kind == "prepared" {
			prepared++
		}
	}
	defer func() { b.ingestProbe = nil }()
	before := b.raw.Metrics()
	stats, err := b.Ingest(runCtx)
	after := b.raw.Metrics()
	if !errors.Is(err, context.Canceled) || started != 1 || prepared != 0 || stats.Rows != 0 || stats.Attributes != 0 || stats.LogicalPayloadBytes != 0 {
		t.Fatalf("failed preparation population: started=%d prepared=%d stats=%+v err=%v", started, prepared, stats, err)
	}
	if stats.Diagnostics == nil || stats.Diagnostics.SourceRuns != 0 || stats.Diagnostics.SourceAttempts != 0 || stats.Diagnostics.CommittedBatches != 0 {
		t.Fatalf("failed preparation invented source work: %+v", stats.Diagnostics)
	}
	assertOtelProfileMoments(t, stats, map[string]int64{"preparation": 1, "manifest": 0, "open": 0, "save": 0, "guard": 0, "run": 0, "timeout_backoff": 0, "commit": 0})
	assertOtelProfileClientMetrics(t, stats, before, after)
	assertSourceTimeoutRawPopulation(t, ctx, b, "loading", 0)
}

func assertOtelProfileMoments(t *testing.T, stats Statistics, want map[string]int64) {
	t.Helper()
	d := stats.Diagnostics
	if d == nil {
		t.Fatal("source receipt omitted profiling diagnostics")
	}
	for phase, count := range want {
		moment, exists := d.PhaseLatencies[phase]
		if count == 0 {
			if exists {
				t.Fatalf("phase %s invented samples: %+v", phase, moment)
			}
			continue
		}
		if !exists || moment.Count != count || moment.MinNanos < 0 || moment.MaxNanos < moment.MinNanos || moment.TotalNanos < moment.MaxNanos || moment.TotalNanos < moment.MinNanos*count || moment.TotalNanos > moment.MaxNanos*count {
			t.Fatalf("phase %s population/moments=%+v, want count=%d", phase, moment, count)
		}
		if count == 1 && (moment.MinNanos != moment.TotalNanos || moment.MaxNanos != moment.TotalNanos) {
			t.Fatalf("phase %s singleton extrema differ: %+v", phase, moment)
		}
		if phase != "commit" && moment.TotalNanos != d.PhaseNanos[phase] {
			t.Fatalf("phase %s lost failed/replayed legacy samples: moments=%+v legacy=%d", phase, moment, d.PhaseNanos[phase])
		}
	}
	for phase := range d.PhaseLatencies {
		if _, specified := want[phase]; !specified {
			t.Fatalf("unexpected phase %s: %+v", phase, d.PhaseLatencies[phase])
		}
	}
	for phase := range d.PhaseNanos {
		if phase == "commit" {
			t.Fatal("commit sample leaked into legacy phase totals")
		}
		if _, specified := want[phase]; !specified {
			t.Fatalf("legacy phase %s was not accounted for", phase)
		}
	}
	moment := d.PhaseLatencies["commit"]
	counter := d.Timer[recordlayer.EventCommit.Name]
	if want["commit"] == 0 {
		if counter != nil && (counter.Count != 0 || counter.CumulativeValue != 0) {
			t.Fatalf("failed Runs recorded commit timing: %+v", counter)
		}
	} else if counter == nil || counter.Count != moment.Count || counter.CumulativeValue != moment.TotalNanos {
		t.Fatalf("commit moments lost/cross-assigned samples: moments=%+v native=%+v", moment, counter)
	}
}

func seedOtelProfileClientMetrics(t *testing.T, ctx context.Context, b *FDBBackend) {
	t.Helper()
	_, err := b.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		return tx.Get(b.root.Sub("profile_probe").FDBKey()).Get()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertOtelProfileClientMetrics(t *testing.T, stats Statistics, before, after client.ClientMetricsSnapshot) {
	t.Helper()
	if stats.Diagnostics == nil || stats.Diagnostics.ClientMetrics == nil {
		t.Fatal("source receipt omitted whole-handle client snapshots")
	}
	if before.GRVLatency.Count == 0 {
		t.Fatal("zero before snapshot cannot prove undifferenced lifetime metrics")
	}
	metrics := stats.Diagnostics.ClientMetrics
	if metrics.Scope != "whole_database_handle_including_initialization_and_ready" || !reflect.DeepEqual(metrics.Before, before) || !reflect.DeepEqual(metrics.After, after) {
		t.Fatalf("client snapshots were missing, differenced or narrowed: got=%+v before=%+v after=%+v", metrics, before, after)
	}
}
