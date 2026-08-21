// Package rlmetrics exposes a record-layer StoreTimer (the Go port of Java's
// FDBStoreTimer) in the Prometheus text exposition format, ready to scrape —
// with zero dependencies.
//
// Usage:
//
//	timer := recordlayer.NewStoreTimer()
//	db.SetTimer(timer)                              // every context now records into it
//	http.Handle("/metrics/recordlayer", rlmetrics.Handler(timer))
//
// This is the record-layer counterpart to pkg/fdbgo/fdbmetrics, which covers
// the FDB client's transaction counters. The two are complementary and
// deliberately separate: fdbmetrics answers "what is the cluster doing to my
// transactions" (conflicts, retries, GRV latency), rlmetrics answers "what is
// my application asking the record layer to do" (records saved, indexes
// scanned, time in commit). An operator wants both, on the same handler mux.
//
// Java has no equivalent. Its only StoreTimer export surfaces are
// getKeysAndValues() into log messages (StoreTimer.java:747-780) and the
// subclass hook that fdb-relational uses to mirror events into a Dropwizard
// MetricRegistry (MetricRegistryStoreTimer.java) — and nothing bridges that
// registry to the Prometheus CollectorRegistry the relational server already
// runs (RelationalServer.java:171 wires only gRPC interceptor metrics). So
// there is no Java behaviour to conform to here; the constraint that does
// apply is the one on the events themselves, which are a 1:1 port of
// FDBStoreTimer's taxonomy. StoreTimer.KeysAndValues() is the Java-shaped
// surface if you want the log-key form instead.
//
// Deliberately NOT a prometheus.Collector, for the same reason fdbmetrics is
// not: it would pull github.com/prometheus/client_golang into the module. A
// user who wants a Collector writes a trivial one over the snapshot.
package rlmetrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer"
)

// Namespace prefixes every metric this package emits. Separate from
// fdbmetrics' fdb_client namespace because the two measure different layers of
// the same stack and an operator must be able to tell at a glance which one a
// number came from.
const Namespace = "fdb_recordlayer_"

// TimerSource is the part of *recordlayer.StoreTimer this package consumes —
// accepted as an interface so tests and wrappers can substitute snapshots.
type TimerSource interface {
	Snapshot() map[string]*recordlayer.CounterSnapshot
}

// Handler returns an http.Handler that renders the timer's counters in the
// Prometheus text exposition format (text/plain; version=0.0.4).
//
// A nil *recordlayer.StoreTimer is a valid source: Snapshot returns nil on a
// nil receiver, and an empty exposition is the honest answer for a database
// nobody installed a timer on.
//
// Do not call StoreTimer.Reset on a timer being scraped. Everything here is
// exported as a monotonic counter, and a reset makes the series jump backwards
// — Prometheus reads that as a process restart and discounts the interval.
// Reset belongs to tests and to one-shot measurement, not to a live endpoint;
// a scraper computes its own deltas and needs the running totals to do it.
func Handler(src TimerSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		// A write error here means the scraper hung up mid-response —
		// nothing actionable; the next scrape retries.
		_ = WriteText(w, src.Snapshot())
	})
}

// WriteText renders one snapshot in the Prometheus text exposition format.
// Returns the first writer error (the exposition itself cannot fail).
//
// Only events the timer has ACTUALLY recorded appear. That is a property of
// StoreTimer — counters are created on first use — and it is the right one to
// preserve rather than paper over with a registry of all declared events: Go
// declares several events Java populates from its instrumented transaction
// wrappers (Counts.READS, WRITES, BYTES_READ, BYTES_WRITTEN — see
// InstrumentedReadTransaction.java:123, InstrumentedTransaction.java:91-92)
// that no Go call site increments yet. Emitting those as a flat 0 would tell an
// operator "zero reads happened" when the truth is "reads are not counted",
// and those two claims call for opposite responses.
//
// Output is sorted by metric name so a diff between two scrapes is readable
// and the tests can pin whole-output shape rather than substring presence.
func WriteText(w io.Writer, snap map[string]*recordlayer.CounterSnapshot) error {
	names := make([]string, 0, len(snap))
	for name := range snap {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		cs := snap[name]
		if cs == nil {
			continue
		}
		if err := writeOne(w, name, cs); err != nil {
			return err
		}
	}
	return nil
}

// helpEscaper applies the two escapes the text format requires in HELP text.
// An unescaped newline in a title would end the HELP line early and leave the
// remainder to be parsed as a metric, which does not corrupt one metric — it
// rejects the whole scrape. Titles are hand-written today, so this guards the
// next one rather than a current bug; that is the cheap moment to do it.
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

// writeOne renders a single counter according to its event Kind.
func writeOne(w io.Writer, name string, cs *recordlayer.CounterSnapshot) error {
	title := cs.Event.Title
	if title == "" {
		title = name
	}
	title = helpEscaper.Replace(title)

	if cs.Event.Kind == recordlayer.KindTimed {
		// A timed event is a count of occurrences plus a sum of durations, which
		// is exactly a Prometheus summary with no quantiles — the type exists for
		// this shape, and StoreTimer keeps no distribution to derive quantiles
		// from. Seconds, not Java's micros (StoreTimer.java:757), because
		// Prometheus base units are seconds; the integer-truncating micros form
		// is available from StoreTimer.KeysAndValues() for log parity.
		metric := Namespace + name + "_seconds"
		_, err := fmt.Fprintf(w,
			"# HELP %s %s: cumulative time and occurrence count (record-layer timed event).\n"+
				"# TYPE %s summary\n%s_sum %g\n%s_count %d\n",
			metric, title, metric, metric, float64(cs.CumulativeValue)/1e9, metric, cs.Count)
		return err
	}

	if cs.Event.Kind == recordlayer.KindSizeDistribution {
		// Java's StoreTimer.SizeEvent: a count of observations plus a sum of
		// magnitudes. That is the same shape as a timed event — and therefore
		// the same Prometheus type, a summary with no quantiles — but the sum
		// is a dimensionless magnitude, not seconds, so it is emitted raw
		// rather than divided.
		//
		// It must NOT fall through to the counter arm below. That arm renders
		// Count alone, which for a distribution is the number of OBSERVATIONS,
		// under a `_total` name that reads as the summed magnitude — a plausible
		// number answering a different question.
		metric := Namespace + name
		_, err := fmt.Fprintf(w,
			"# HELP %s %s: summed magnitude and observation count (record-layer size event).\n"+
				"# TYPE %s summary\n%s_sum %d\n%s_count %d\n",
			metric, title, metric, metric, cs.CumulativeValue, metric, cs.Count)
		return err
	}

	// KindCount, KindSize, and any unclassified event are monotonic counters
	// whose whole payload is in Count — for KindSize that payload is a byte
	// total, which is why those event names already end in _bytes and the
	// rendered metric reads fdb_recordlayer_save_record_key_bytes_total.
	//
	// KindUnspecified lands here rather than being dropped: an event that
	// reached the timer was recorded by real code, and silently withholding it
	// from the exposition would hide activity. It is reported as a counter and
	// flagged in the help text, so a missing classification is visible on the
	// scrape instead of invisible.
	help := fmt.Sprintf("%s: total (record-layer count event).", title)
	switch cs.Event.Kind {
	case recordlayer.KindSize:
		help = fmt.Sprintf("%s: cumulative bytes (record-layer size event).", title)
	case recordlayer.KindUnspecified:
		help = fmt.Sprintf("%s: total (record-layer event with no declared Kind; reported as a counter).", title)
	case recordlayer.KindTimed, recordlayer.KindCount, recordlayer.KindSizeDistribution:
		// KindTimed and KindSizeDistribution returned above; KindCount uses
		// the default help.
	}
	metric := Namespace + name + "_total"
	_, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n",
		metric, help, metric, metric, cs.Count)
	return err
}
