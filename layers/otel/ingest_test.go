package otel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

func TestPrepareIngestBatchBoundariesAndAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		count      int
		attributes int
		batch      int
		wantBounds [][2]int
	}{
		{"row_bound", 7, 8, 2, [][2]int{{0, 2}, {2, 4}, {4, 6}, {6, 7}}},
		{"byte_bound", 9, 128, 64, [][2]int{{0, 4}, {4, 8}, {8, 9}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := testConfig()
			c.Count, c.Attributes, c.Batch = tc.count, tc.attributes, tc.batch
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			var bounds [][2]int
			var ordinals []int
			var logicalBytes int64
			for lo := 0; lo < c.Count; {
				batch, err := prepareIngestBatch(t.Context(), c, lo)
				if err != nil {
					t.Fatal(err)
				}
				if batch.first != lo || batch.next <= lo || batch.next > c.Count || len(batch.messages) != batch.next-batch.first || len(batch.messages) > c.Batch {
					t.Fatalf("invalid batch bounds first=%d next=%d messages=%d, lo=%d count=%d cap=%d", batch.first, batch.next, len(batch.messages), lo, c.Count, c.Batch)
				}
				if batch.estimate <= 0 || batch.estimate > 300000 {
					t.Fatalf("batch estimate outside admission bound: %d", batch.estimate)
				}
				bounds = append(bounds, [2]int{batch.first, batch.next})
				var wantLogical int64
				wantEstimate := 0
				for i, message := range batch.messages {
					ordinal := lo + i
					want := Generate(c, ordinal)
					got, err := decodeRecord(message)
					if err != nil || !equalSpan(got, want) {
						t.Fatalf("prepared ordinal %d decoded=%+v error=%v", ordinal, got, err)
					}
					expectedMessage, err := toRecord(Generate(c, ordinal))
					if err != nil || !proto.Equal(message, expectedMessage) {
						t.Fatalf("prepared ordinal %d differs from source encoding: %v", ordinal, err)
					}
					wire, err := json.Marshal(want)
					if err != nil {
						t.Fatal(err)
					}
					wantLogical += int64(len(wire))
					wantEstimate += 2048
					for _, attribute := range want.Attributes {
						wantEstimate += 2 * (len(attribute.Key) + len(attribute.Value) + 256)
					}
					ordinals = append(ordinals, got.Ordinal)
				}
				if batch.logicalBytes != wantLogical || batch.estimate != wantEstimate {
					t.Fatalf("batch accounting logical=%d estimate=%d, want logical=%d estimate=%d", batch.logicalBytes, batch.estimate, wantLogical, wantEstimate)
				}
				logicalBytes += batch.logicalBytes
				lo = batch.next
			}
			if !reflect.DeepEqual(bounds, tc.wantBounds) {
				t.Fatalf("batch boundaries=%v, want %v", bounds, tc.wantBounds)
			}
			wantOrdinals := make([]int, c.Count)
			var wantTotal int64
			for i := range wantOrdinals {
				wantOrdinals[i] = i
				wire, err := json.Marshal(Generate(c, i))
				if err != nil {
					t.Fatal(err)
				}
				wantTotal += int64(len(wire))
			}
			if !reflect.DeepEqual(ordinals, wantOrdinals) || logicalBytes != wantTotal || logicalBytes <= 0 {
				t.Fatalf("incomplete/duplicate prepared corpus: ordinals=%v logical=%d, want ordinals=%v logical=%d", ordinals, logicalBytes, wantOrdinals, wantTotal)
			}
		})
	}
}

func TestPrepareIngestBatchCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := prepareIngestBatch(ctx, testConfig(), 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare with canceled context: %v", err)
	}
}

func TestPrepareIngestBatchInvalidOffset(t *testing.T) {
	t.Parallel()
	c := testConfig()
	for _, tc := range []struct {
		name string
		lo   int
	}{{"negative", -1}, {"source_exhausted", c.Count}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			batch, err := prepareIngestBatch(t.Context(), c, tc.lo)
			var config *ConfigError
			if !errors.As(err, &config) {
				t.Fatalf("invalid offset %d: expected ConfigError, got %v", tc.lo, err)
			}
			if batch.first != tc.lo || batch.next != tc.lo || len(batch.messages) != 0 || batch.logicalBytes != 0 || batch.estimate != 0 {
				t.Fatalf("invalid offset performed preparation: %+v", batch)
			}
		})
	}
}

func TestIngestRecorderDisabled(t *testing.T) {
	t.Parallel()
	d := newIngestRecorder(false)
	if d != nil {
		t.Fatal("disabled diagnostics allocated a recorder")
	}
	d.phase("preparation", time.Now())
	d.run(3, time.Now())
	d.size(123)
	if got := d.snapshot(9, 3); got != nil {
		t.Fatalf("disabled diagnostics produced a snapshot: %+v", got)
	}
}

func TestIngestRecorderAggregation(t *testing.T) {
	t.Parallel()
	d := newIngestRecorder(true)
	if d == nil || d.timer == nil {
		t.Fatal("enabled diagnostics did not allocate recorder and timer")
	}
	started := time.Now().Add(-time.Millisecond)
	d.run(0, started)
	d.run(1, started)
	d.run(3, started)
	d.size(17)
	d.size(0)
	d.size(29)
	d.phase("preparation", started)
	d.phase("preparation", started)
	d.timer.IncrementBy(recordlayer.CountSaveRecordKeyBytes, 11)
	d.timer.IncrementBy(recordlayer.CountSaveRecordKeyBytes, 23)
	got := d.snapshot(9, 3)
	if got == nil {
		t.Fatal("enabled diagnostics omitted snapshot")
	}
	if got.Scope != "source_batches_only" || got.CommittedRows != 9 || got.CommittedBatches != 3 {
		t.Fatalf("wrong diagnostic population: %+v", got)
	}
	if got.SourceRuns != 3 || got.SourceAttempts != 4 || got.SourceReplays != 2 || got.ApproximateMutationBytes != 46 {
		t.Fatalf("wrong attempts/replays/size aggregation: %+v", got)
	}
	if len(got.PhaseNanos) != 2 || got.PhaseNanos["run"] < int64(3*time.Millisecond) || got.PhaseNanos["preparation"] < int64(2*time.Millisecond) {
		t.Fatalf("phase durations did not accumulate under expected names: %v", got.PhaseNanos)
	}
	event := got.Timer["save_record_key_bytes"]
	if len(got.Timer) != 1 || event == nil {
		t.Fatalf("timer event population: %+v", got.Timer)
	}
	if event.Event != recordlayer.CountSaveRecordKeyBytes || event.Event.Kind != recordlayer.KindSize || event.Count != 34 || event.CumulativeValue != 0 {
		t.Fatalf("size event lost byte-count semantics: %+v", event)
	}
}
