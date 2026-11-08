package plugin

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// metrics are the RFC §9 counters, exported through the registry the SDK
// serves (prometheus.DefaultRegisterer). Labels: datasource UID, query type
// and tenant; SQL counters cover every statement on the tenant's pool.
type metrics struct {
	sqlScannedRows  *prometheus.CounterVec
	sqlScannedBytes *prometheus.CounterVec
	sqlPages        *prometheus.CounterVec
	sqlFailures     *prometheus.CounterVec
	candidates      *prometheus.CounterVec
	readBytes       *prometheus.CounterVec
	decodedBytes    *prometheus.CounterVec
	truncations     *prometheus.CounterVec
	readerReopens   *prometheus.CounterVec
}

var baseLabels = []string{"datasource", "query_type", "tenant"}

func newMetrics(reg prometheus.Registerer) *metrics {
	counter := func(name, help string, extra ...string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "fdb_datasource", Name: name, Help: help}, append(append([]string(nil), baseLabels...), extra...))
		reg.MustRegister(c)
		return c
	}
	return &metrics{
		sqlScannedRows:  counter("sql_scanned_rows_total", "Records the SQL scan limiter charged, every page and retry included."),
		sqlScannedBytes: counter("sql_scanned_bytes_total", "Key and value bytes the SQL scan limiter charged."),
		sqlPages:        counter("sql_pages_total", "SQL page fetches; each page is its own FDB transaction."),
		sqlFailures:     counter("sql_failures_total", "Failed SQL statements by SQLSTATE.", "sqlstate"),
		candidates:      counter("otel_candidates_total", "Candidate index entries the trace searches examined."),
		readBytes:       counter("otel_read_bytes_total", "Logical bytes the OTel reads took from FDB."),
		decodedBytes:    counter("otel_decoded_bytes_total", "Protobuf bytes of the spans returned to Grafana."),
		truncations:     counter("trace_truncations_total", "Trace reads cut by a budget, by reason.", "reason"),
		readerReopens:   counter("reader_reopens_total", "OTel readers re-opened after stale store metadata."),
	}
}

var (
	defaultMetricsOnce sync.Once
	defaultMetricsVal  *metrics
)

// processMetrics registers the counters once per process: every datasource
// instance shares them, told apart by the datasource label.
func processMetrics() *metrics {
	defaultMetricsOnce.Do(func() { defaultMetricsVal = newMetrics(prometheus.DefaultRegisterer) })
	return defaultMetricsVal
}

// otelRead counts one reader call's work.
func (m *metrics) otelRead(l prometheus.Labels, candidates int, bytes, decoded int64) {
	m.candidates.With(l).Add(float64(candidates))
	m.readBytes.With(l).Add(float64(bytes))
	m.decodedBytes.With(l).Add(float64(decoded))
}

// traceRead counts a TraceOTLP call: bytes and one truncation per reason.
func (m *metrics) traceRead(l prometheus.Labels, s otel.TraceReadStats) {
	m.otelRead(l, 0, s.Bytes, s.DecodedBytes)
	for _, r := range strings.Split(s.Reason, ",") {
		if r = strings.TrimSpace(r); r != "" {
			m.truncations.With(withLabel(l, "reason", r)).Inc()
		}
	}
}

// withLabel is l plus one label.
func withLabel(l prometheus.Labels, k, v string) prometheus.Labels {
	out := prometheus.Labels{k: v}
	for lk, lv := range l {
		out[lk] = lv
	}
	return out
}

// sqlstate is the error's SQLSTATE, or a class for errors without one.
func sqlstate(err error) string {
	var ae *api.Error
	switch {
	case errors.As(err, &ae) && ae.Code != "":
		return string(ae.Code)
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "unknown"
}

// statsConnector installs the execution-statistics hook on every connection
// of one tenant's pool.
type statsConnector struct {
	driver.Connector
	m      *metrics
	labels prometheus.Labels
}

func (c *statsConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	ec, ok := conn.(*embedded.EmbeddedConnection)
	if !ok {
		return conn, nil
	}
	sc := &statsConn{EmbeddedConnection: ec, c: c}
	ec.SetExecutionStatsLogger(sc)
	return sc, nil
}

// statsConn is the driver's connection with its execution statistics
// counted. ResetSession drops the logger at each pool return, so it is put
// back; a statement that fails before executing emits no statistics, so
// QueryContext counts its SQLSTATE.
type statsConn struct {
	*embedded.EmbeddedConnection
	c      *statsConnector
	logged bool // the current statement emitted ExecutionStats
}

func (s *statsConn) LogExecutionStats(_ context.Context, st embedded.ExecutionStats) {
	s.logged = true
	s.c.m.sqlScannedRows.With(s.c.labels).Add(float64(st.RecordsScanned))
	s.c.m.sqlScannedBytes.With(s.c.labels).Add(float64(st.BytesScanned))
	s.c.m.sqlPages.With(s.c.labels).Add(float64(st.Pages))
	if st.Err != nil {
		s.failure(st.Err)
	}
}

func (s *statsConn) failure(err error) {
	s.c.m.sqlFailures.With(withLabel(s.c.labels, "sqlstate", sqlstate(err))).Inc()
}

func (s *statsConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	s.logged = false
	rows, err := s.EmbeddedConnection.QueryContext(ctx, query, args)
	if err != nil && !s.logged {
		s.failure(err)
	}
	return rows, err
}

func (s *statsConn) ResetSession(ctx context.Context) error {
	err := s.EmbeddedConnection.ResetSession(ctx)
	s.EmbeddedConnection.SetExecutionStatsLogger(s)
	return err
}
