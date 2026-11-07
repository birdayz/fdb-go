package otel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigSafety(t *testing.T) {
	t.Parallel()
	c := testConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"", "default", "../../prod", "good;DROP", "UPPER_case", "hello-world"} {
		bad := c
		bad.Run = run
		if bad.Validate() == nil {
			t.Fatalf("unsafe run accepted: %q", run)
		}
	}
	m := manifest(c, "ready")
	if err := checkManifest(c, m, true); err != nil {
		t.Fatal(err)
	}
	other := c
	other.Seed++
	if checkManifest(other, m, true) == nil {
		t.Fatal("different corpus authorized")
	}
	m.State = "loading"
	if checkManifest(c, m, true) == nil {
		t.Fatal("loading run query authorized")
	}
}

func TestCorpusAndLayoutOwnership(t *testing.T) {
	t.Parallel()
	c := testConfig()
	m := manifest(c, "ready")
	other := c
	other.Order = "time"
	other.Backend = "ch-attrs"
	if corpusIdentity(c) != corpusIdentity(other) {
		t.Fatal("physical layout changed source corpus fingerprint")
	}
	if checkManifest(other, m, true) == nil {
		t.Fatal("wrong layout/backend reopened owned run")
	}
	seen := newOrdinalSet(3)
	for _, i := range []int{2, 0, 1} {
		if err := seen.add(i, 3); err != nil {
			t.Fatal(err)
		}
	}
	if seen.count != 3 {
		t.Fatal("incomplete audit population")
	}
	if seen.add(1, 3) == nil || seen.add(3, 3) == nil {
		t.Fatal("duplicate/out-of-range ordinal accepted")
	}
}

func TestOracleIdentityOrderAndScope(t *testing.T) {
	t.Parallel()
	c := testConfig()
	got := Expected(c, "span", QueryKey, "string", QueryValue)
	if len(got) != 4 {
		t.Fatalf("got %d matching traces, want 4", len(got))
	}
	for i, h := range got {
		if len(ExpectedTrace(c, h.Trace)) != 2 {
			t.Fatal("trace pairing lost")
		}
		if i > 0 && (got[i-1].Latest < h.Latest || got[i-1].Latest == h.Latest && got[i-1].Trace < h.Trace) {
			t.Fatal("order wrong")
		}
	}
	if len(Expected(c, "resource", QueryKey, "string", QueryValue)) != 12 {
		t.Fatal("scope lookalike population wrong")
	}
	if len(Expected(c, "span", QueryKey, "string", "absent")) != 0 {
		t.Fatal("negative oracle wrong")
	}
	c.K = 2
	if len(Expected(c, "span", QueryKey, "string", QueryValue)) != 2 {
		t.Fatal("K not honored")
	}
}

func TestCHHTTPParametersAndSummary(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("use_query_cache") != "0" {
			t.Error("query cache enabled")
		}
		if strings.Contains(r.URL.Query().Get("query"), "needle' OR") {
			t.Error("literal interpolated into SQL")
		}
		w.Header().Set("X-ClickHouse-Summary", `{"read_rows":"27","read_bytes":"4096","elapsed_ns":"1200"}`)
		fmt.Fprint(w, "")
	}))
	defer server.Close()
	b, err := OpenCH(testConfig(), server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, summary, err := b.requestWithSummary(t.Context(), "SELECT {value:String}", nil, map[string]string{"value": "needle' OR 1=1"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ReadRows != 27 || summary.ReadBytes != 4096 || summary.Elapsed != 1200 {
		t.Fatalf("missing server accounting: %+v", summary)
	}
}

func TestCHTimeSQLUsesOrderedDistinct(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Order = "time"
	b, err := OpenCH(c, "http://127.0.0.1:8123", true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sql, _ := b.querySQL(Attribute{"span", QueryKey, "string", QueryValue})
	if !strings.Contains(sql, "ORDER BY neg_time,rev_trace,span_id LIMIT 1 BY trace_id LIMIT 100") {
		t.Fatal(sql)
	}
	if strings.Contains(sql, "GROUP BY") {
		t.Fatal("time arm unnecessarily aggregates entire candidate range")
	}
	c.Order = "trace"
	other, err := OpenCH(c, "http://127.0.0.1:8123", true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	sql, _ = other.querySQL(Attribute{})
	if !strings.Contains(sql, "GROUP BY trace_id") {
		t.Fatal("trace-ordered arm must complete max/topK")
	}
}
