package rlmetrics_test

import (
	"bytes"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/rlmetrics"
)

// snapshotSource adapts a fixed snapshot to rlmetrics.TimerSource so the
// exposition can be pinned without a live timer.
type snapshotSource map[string]*recordlayer.CounterSnapshot

func (s snapshotSource) Snapshot() map[string]*recordlayer.CounterSnapshot {
	return map[string]*recordlayer.CounterSnapshot(s)
}

func render(t *testing.T, src rlmetrics.TimerSource) string {
	t.Helper()
	var buf bytes.Buffer
	if err := rlmetrics.WriteText(&buf, src.Snapshot()); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return buf.String()
}

// TestWriteText_ExactExposition pins the WHOLE output for one counter of each
// Kind, not a set of substrings. Whole-output is the point: a substring
// assertion cannot catch a stray metric, a duplicated HELP line, or a TYPE that
// disagrees with the sample line below it, and all three break a scraper.
func TestWriteText_ExactExposition(t *testing.T) {
	t.Parallel()

	got := render(t, snapshotSource{
		"save_record": {
			Event:           recordlayer.EventSaveRecord,
			Count:           4,
			CumulativeValue: 2_500_000, // 2.5ms in nanoseconds
		},
		"save_record_key": {
			Event: recordlayer.CountSaveRecordKey,
			Count: 4,
		},
		"save_record_key_bytes": {
			Event: recordlayer.CountSaveRecordKeyBytes,
			Count: 128,
		},
	})

	want := strings.Join([]string{
		"# HELP fdb_recordlayer_save_record_seconds Save Record: cumulative time and occurrence count (record-layer timed event).",
		"# TYPE fdb_recordlayer_save_record_seconds summary",
		"fdb_recordlayer_save_record_seconds_sum 0.0025",
		"fdb_recordlayer_save_record_seconds_count 4",
		"# HELP fdb_recordlayer_save_record_key_total Save Record Key: total (record-layer count event).",
		"# TYPE fdb_recordlayer_save_record_key_total counter",
		"fdb_recordlayer_save_record_key_total 4",
		"# HELP fdb_recordlayer_save_record_key_bytes_total Save Record Key Bytes: cumulative bytes (record-layer size event).",
		"# TYPE fdb_recordlayer_save_record_key_bytes_total counter",
		"fdb_recordlayer_save_record_key_bytes_total 128",
		"",
	}, "\n")

	if got != want {
		t.Errorf("exposition mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestWriteText_KindDecidesTheMetricShape is the dimension that a
// per-event-name test cannot cover: the SAME numbers must render as a duration
// summary or as a counter depending only on the event's Kind. Getting this
// backwards would export a byte total as a duration in seconds — a number that
// looks plausible on a dashboard and is off by nine orders of magnitude.
func TestWriteText_KindDecidesTheMetricShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		event    recordlayer.Event
		wantLine string
		wantType string
	}{
		{
			name:     "timed",
			event:    recordlayer.Event{Name: "e", Title: "E", Kind: recordlayer.KindTimed},
			wantLine: "fdb_recordlayer_e_seconds_sum 3",
			wantType: "# TYPE fdb_recordlayer_e_seconds summary",
		},
		{
			name:     "count",
			event:    recordlayer.Event{Name: "e", Title: "E", Kind: recordlayer.KindCount},
			wantLine: "fdb_recordlayer_e_total 7",
			wantType: "# TYPE fdb_recordlayer_e_total counter",
		},
		{
			name:     "size",
			event:    recordlayer.Event{Name: "e", Title: "E", Kind: recordlayer.KindSize},
			wantLine: "fdb_recordlayer_e_total 7",
			wantType: "# TYPE fdb_recordlayer_e_total counter",
		},
		{
			// Java's StoreTimer.SizeEvent — a count of OBSERVATIONS plus a sum of
			// MAGNITUDES, so a summary whose _sum is the raw magnitude total (not
			// divided by 1e9 the way a duration is) and whose _count is 7.
			//
			// The counter arm would render `_total 7` here, and 7 is the number of
			// observations, not the summed magnitude — a plausible number under a
			// name that reads as the other one. That is why the arm returns early
			// rather than falling through.
			name:     "size distribution",
			event:    recordlayer.Event{Name: "e", Title: "E", Kind: recordlayer.KindSizeDistribution},
			wantLine: "fdb_recordlayer_e_sum 3000000000",
			wantType: "# TYPE fdb_recordlayer_e summary",
		},
		{
			// An event that reached the timer without a declared Kind is still
			// real activity, so it is exported rather than dropped — flagged in
			// the help text so the omission shows up on the scrape.
			name:     "unspecified",
			event:    recordlayer.Event{Name: "e", Title: "E"},
			wantLine: "fdb_recordlayer_e_total 7",
			wantType: "# TYPE fdb_recordlayer_e_total counter",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := render(t, snapshotSource{"e": {
				Event: tc.event, Count: 7, CumulativeValue: 3_000_000_000,
			}})
			if !strings.Contains(got, tc.wantLine) {
				t.Errorf("missing %q in:\n%s", tc.wantLine, got)
			}
			if !strings.Contains(got, tc.wantType) {
				t.Errorf("missing %q in:\n%s", tc.wantType, got)
			}
		})
	}

	t.Run("size_distribution_count_is_observations_not_magnitude", func(t *testing.T) {
		t.Parallel()
		// The pair is what makes the assertion non-vacuous: _sum and _count must
		// carry DIFFERENT numbers from the same snapshot, so a renderer that put
		// the same value in both — or that dropped one — is caught. Asserting
		// only _sum would pass against a renderer that emitted _sum twice.
		got := render(t, snapshotSource{"e": {
			Event: recordlayer.Event{Name: "e", Title: "E", Kind: recordlayer.KindSizeDistribution},
			Count: 3, CumulativeValue: 11,
		}})
		if !strings.Contains(got, "fdb_recordlayer_e_sum 11\n") {
			t.Errorf("_sum must carry the summed magnitude; got:\n%s", got)
		}
		if !strings.Contains(got, "fdb_recordlayer_e_count 3\n") {
			t.Errorf("_count must carry the observation count; got:\n%s", got)
		}
		if strings.Contains(got, "_total") {
			t.Errorf("a size distribution must not be rendered as a monotonic counter; got:\n%s", got)
		}
	})

	t.Run("unspecified_kind_is_visible_in_help", func(t *testing.T) {
		t.Parallel()
		got := render(t, snapshotSource{"e": {
			Event: recordlayer.Event{Name: "e", Title: "E"}, Count: 1,
		}})
		if !strings.Contains(got, "no declared Kind") {
			t.Errorf("an unclassified event must say so in HELP; got:\n%s", got)
		}
	})
}

// TestWriteText_EscapesHelpText: a newline in a title would terminate the HELP
// line early and leave its tail to be parsed as a metric — which does not
// corrupt one metric, it makes the whole scrape unparseable and takes every
// other metric down with it. Same blast radius as an illegal metric name.
func TestWriteText_EscapesHelpText(t *testing.T) {
	t.Parallel()

	got := render(t, snapshotSource{"e": {
		Event: recordlayer.Event{
			Name:  "e",
			Title: "line one\nfdb_evil_injected 1\nwith a \\ backslash",
			Kind:  recordlayer.KindCount,
		},
		Count: 1,
	}})

	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "fdb_evil_injected") {
			t.Errorf("a newline in a title escaped into a metric line:\n%s", got)
		}
	}
	if !strings.Contains(got, `line one\nfdb_evil_injected 1\nwith a \\ backslash`) {
		t.Errorf("HELP text is not escaped as the text format requires:\n%s", got)
	}
	// Exactly one HELP line and one TYPE line survive.
	if n := strings.Count(got, "# HELP "); n != 1 {
		t.Errorf("got %d HELP lines, want 1:\n%s", n, got)
	}
}

// TestWriteText_IsValidExpositionFormat checks the structural rules a Prometheus
// scraper enforces, over a snapshot containing every Kind: each sample line is
// `name value`, every sample has a preceding TYPE, no TYPE is declared twice,
// and metric names are legal.
func TestWriteText_IsValidExpositionFormat(t *testing.T) {
	t.Parallel()

	timer := recordlayer.NewStoreTimer()
	timer.Record(recordlayer.EventSaveRecord, 1234)
	timer.Record(recordlayer.EventCommit, 999)
	timer.Increment(recordlayer.CountSaveRecordKey)
	timer.IncrementBy(recordlayer.CountSaveRecordKeyBytes, 64)
	timer.Increment(recordlayer.CountSPFreshSplits)

	body := render(t, timer)

	nameRe := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	declaredType := map[string]bool{}
	sampleCount := 0

	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if len(f) != 4 {
				t.Fatalf("malformed TYPE line %q", line)
			}
			if declaredType[f[2]] {
				t.Errorf("metric %q has a duplicate TYPE declaration", f[2])
			}
			declaredType[f[2]] = true
			if f[3] != "counter" && f[3] != "summary" {
				t.Errorf("unexpected metric type %q in %q", f[3], line)
			}
		case strings.HasPrefix(line, "# HELP "):
			f := strings.SplitN(line, " ", 4)
			if len(f) < 4 || strings.TrimSpace(f[3]) == "" {
				t.Errorf("HELP line has no help text: %q", line)
			}
		case line == "":
		default:
			sampleCount++
			f := strings.Fields(line)
			if len(f) != 2 {
				t.Fatalf("sample line must be `name value`, got %q", line)
			}
			if !nameRe.MatchString(f[0]) {
				t.Errorf("illegal metric name %q", f[0])
			}
			if _, err := strconv.ParseFloat(f[1], 64); err != nil {
				t.Errorf("sample value %q is not a number in %q", f[1], line)
			}
			// Every sample belongs to a declared family: exact match for a
			// counter, or the _sum/_count children of a summary.
			base := f[0]
			base = strings.TrimSuffix(base, "_sum")
			base = strings.TrimSuffix(base, "_count")
			if !declaredType[f[0]] && !declaredType[base] {
				t.Errorf("sample %q has no preceding TYPE declaration", f[0])
			}
		}
	}

	// 3 counter samples + 2 summaries × 2 samples.
	if sampleCount != 7 {
		t.Errorf("sample count = %d, want 7 (3 counters + 2 summaries×2)", sampleCount)
	}
}

// TestWriteText_IsDeterministic pins the sort. Unordered map iteration would
// make every scrape a different byte sequence, which is invisible to a scraper
// and ruins any attempt to diff two scrapes by hand.
func TestWriteText_IsDeterministic(t *testing.T) {
	t.Parallel()

	timer := recordlayer.NewStoreTimer()
	for _, e := range []recordlayer.Event{
		recordlayer.EventScanIndex, recordlayer.EventCommit, recordlayer.EventSaveRecord,
		recordlayer.CountWrites, recordlayer.CountReads, recordlayer.CountBytesRead,
	} {
		timer.Record(e, 1)
	}

	first := render(t, timer)
	for i := 0; i < 20; i++ {
		if got := render(t, timer); got != first {
			t.Fatalf("render %d differs from the first:\n%s\nvs\n%s", i, got, first)
		}
	}

	var names []string
	for _, line := range strings.Split(first, "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			names = append(names, strings.Fields(line)[2])
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("metric families are not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

// TestHandler_ContentTypeAndBody covers the HTTP surface.
func TestHandler_ContentTypeAndBody(t *testing.T) {
	t.Parallel()

	timer := recordlayer.NewStoreTimer()
	timer.IncrementBy(recordlayer.CountBytesWritten, 4096)

	rec := httptest.NewRecorder()
	rlmetrics.Handler(timer).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus text exposition", ct)
	}
	if !strings.Contains(rec.Body.String(), "fdb_recordlayer_bytes_written_total 4096") {
		t.Errorf("body missing the counter:\n%s", rec.Body.String())
	}
}

// TestHandler_NilTimerIsEmptyNotAPanic: a database nobody armed has a nil
// timer, and an operator who wires the handler anyway must get an empty scrape
// rather than a 500 or a crashed process.
func TestHandler_NilTimerIsEmptyNotAPanic(t *testing.T) {
	t.Parallel()

	var timer *recordlayer.StoreTimer
	rec := httptest.NewRecorder()
	rlmetrics.Handler(timer).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("nil timer must render an empty exposition, got:\n%s", body)
	}
}

// TestHandler_ConcurrentScrapeAndRecord is the -race dimension: a scrape runs
// concurrently with the write path by construction (the handler reads the same
// live timer every transaction records into), so the exporter must never be the
// thing that introduces a data race.
func TestHandler_ConcurrentScrapeAndRecord(t *testing.T) {
	t.Parallel()

	timer := recordlayer.NewStoreTimer()
	handler := rlmetrics.Handler(timer)

	const writers, scrapers, iterations = 8, 4, 200
	var writersWG, scrapersWG sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < scrapers; i++ {
		scrapersWG.Add(1)
		go func() {
			defer scrapersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
			}
		}()
	}
	for i := 0; i < writers; i++ {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for j := 0; j < iterations; j++ {
				timer.Record(recordlayer.EventSaveRecord, int64(j))
				timer.Increment(recordlayer.CountSaveRecordKey)
				timer.IncrementBy(recordlayer.CountSaveRecordKeyBytes, 8)
				timer.RecordSince(recordlayer.EventCommit, time.Now())
			}
		}()
	}

	writersWG.Wait()
	close(stop)
	scrapersWG.Wait()

	// Totals must be exact despite the concurrency — the counters are atomic,
	// and a lost update here would be a silent under-count in production.
	if got, want := timer.GetCount(recordlayer.EventSaveRecord), int64(writers*iterations); got != want {
		t.Errorf("save_record count = %d, want %d", got, want)
	}
	if got, want := timer.GetCount(recordlayer.CountSaveRecordKeyBytes), int64(writers*iterations*8); got != want {
		t.Errorf("save_record_key_bytes = %d, want %d", got, want)
	}
}
