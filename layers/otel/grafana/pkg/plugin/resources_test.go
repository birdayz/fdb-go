package plugin

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"fdb.dev/layers/otel"
)

type captured struct{ resp *backend.CallResourceResponse }

func (c *captured) Send(r *backend.CallResourceResponse) error {
	c.resp = r
	return nil
}

func callResource(t *testing.T, h *harness, method, path, rawQuery string, body []byte) (int, []byte) {
	t.Helper()
	url := path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	var c captured
	if err := h.ds.CallResource(context.Background(), &backend.CallResourceRequest{Method: method, Path: path, URL: url, Body: body}, &c); err != nil {
		t.Fatal(err)
	}
	if c.resp == nil {
		t.Fatal("no response")
	}
	return c.resp.Status, c.resp.Body
}

func schemataAnswer(dsn, query string) fakeResult {
	switch {
	case strings.Contains(query, "SCHEMATA"):
		return fakeResult{cols: []string{"SCHEMA_NAME"}, types: []string{"STRING"}, rows: [][]driver.Value{{"T2"}, {"U1"}, {"T1"}, {"T10"}}}
	case strings.Contains(query, "INFORMATION_SCHEMA.TABLES"):
		return fakeResult{cols: []string{"TABLE_SCHEMA", "TABLE_NAME"}, types: []string{"STRING", "STRING"}, rows: [][]driver.Value{{"T1", "SPANS"}, {"T2", "OTHER"}, {"T1", "LOGS"}}}
	case strings.Contains(query, "INFORMATION_SCHEMA.COLUMNS"):
		return fakeResult{cols: []string{"TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME"}, types: []string{"STRING", "STRING", "STRING"}, rows: [][]driver.Value{
			{"T1", "SPANS", "TRACE_ID"}, {"T1", "SPANS", "START_NS"}, {"T2", "SPANS", "X"}, {"T1", "LOGS", "BODY"},
		}}
	}
	return fakeResult{cols: []string{"1"}, types: []string{"BIGINT"}, rows: [][]driver.Value{{int64(1)}}}
}

func TestTenantsResource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"allowedSchemas": []string{"T*"}})
	h.sql.answer = schemataAnswer
	status, body := callResource(t, h, http.MethodGet, "tenants", "", nil)
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `{"tenants":["T1","T10","T2"]}` {
		t.Fatalf("%d %s", status, body)
	}
	if strings.Contains(h.sql.dsns[0], "schema=") {
		t.Fatalf("tenant listing used a schema-bound connection: %s", h.sql.dsns[0])
	}
	status, body = callResource(t, h, http.MethodPost, "schemas", "", []byte("{}"))
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `["T1","T10","T2"]` {
		t.Fatalf("schemas %d %s", status, body)
	}
}

func TestCompletionResources(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"allowedSchemas": []string{"T1", "T2"}})
	h.sql.answer = schemataAnswer
	status, body := callResource(t, h, http.MethodPost, "tables", "", []byte(`{"schema":"T1"}`))
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `["SPANS","LOGS"]` {
		t.Fatalf("tables %d %s", status, body)
	}
	status, body = callResource(t, h, http.MethodPost, "columns", "", []byte(`{"schema":"T1","table":"spans"}`))
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `["TRACE_ID","START_NS"]` {
		t.Fatalf("columns %d %s", status, body)
	}
	// Default tenant when the body names none.
	status, body = callResource(t, h, http.MethodPost, "tables", "", nil)
	if status != http.StatusOK || !strings.Contains(string(body), "SPANS") {
		t.Fatalf("default tables %d %s", status, body)
	}
	before := h.sql.newCalls
	status, _ = callResource(t, h, http.MethodPost, "tables", "", []byte(`{"schema":"T3"}`))
	if status != http.StatusBadRequest || h.sql.newCalls != before {
		t.Fatalf("disallowed tenant: %d, %d new connectors", status, h.sql.newCalls-before)
	}
}

func TestAttributeKeysResource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	r := h.reader("T1")
	r.keys, r.truncated = []string{"http.method", "http.route"}, true
	status, body := callResource(t, h, http.MethodGet, "attribute-keys", "scope=span&type=string&prefix=http.&limit=5", nil)
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `{"keys":["http.method","http.route"],"truncated":true}` {
		t.Fatalf("%d %s", status, body)
	}
	if got := r.lastAttrArgs; got[0] != signalSpan || got[1] != "span" || got[2] != "string" || got[3] != "http." || got[4] != 5 {
		t.Fatalf("reader args %v", got)
	}
	// Cached: the second identical call does not read.
	callResource(t, h, http.MethodGet, "attribute-keys", "scope=span&type=string&prefix=http.&limit=5", nil)
	if n := len(r.callLog()); n != 1 {
		t.Fatalf("%d reads, want 1", n)
	}
	// Log signal, all types, default limit.
	callResource(t, h, http.MethodGet, "attribute-keys", "signal=log&scope=log", nil)
	if got := r.lastAttrArgs; got[0] != signalLog || got[1] != "log" || got[2] != "" || got[4] != maxAttributeKeys {
		t.Fatalf("log args %v", got)
	}
	r.keys = nil
	status, body = callResource(t, h, http.MethodGet, "attribute-keys", "scope=resource", nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"keys":[]`) {
		t.Fatalf("empty keys %d %s", status, body)
	}
}

func TestAttributeValuesResource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	r := h.reader("T1")
	r.values = []string{"GET"}
	status, body := callResource(t, h, http.MethodGet, "attribute-values", "tenant=T1&signal=span&scope=span&key=http.method&type=string&prefix=G", nil)
	var out struct {
		Values    []string `json:"values"`
		Truncated bool     `json:"truncated"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &out) != nil || len(out.Values) != 1 || out.Truncated {
		t.Fatalf("%d %s", status, body)
	}
	if got := r.lastAttrArgs; got[0] != signalSpan || got[1] != (otel.Attribute{Scope: "span", Key: "http.method", Type: "string"}) || got[2] != "G" || got[3] != maxAttributeValues {
		t.Fatalf("reader args %v", got)
	}
}

func TestAttributeResourceRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"allowedSchemas": []string{"T1"}})
	cases := map[string]string{
		"attribute-keys?bad signal":         "signal=metric&scope=span",
		"attribute-keys?log scope on span":  "scope=log",
		"attribute-keys?span scope on log":  "signal=log&scope=span",
		"attribute-keys?event_name scope":   "signal=log&scope=event_name",
		"attribute-keys?bad type":           "scope=span&type=bytes",
		"attribute-keys?limit over":         "scope=span&limit=1001",
		"attribute-keys?limit junk":         "scope=span&limit=x",
		"attribute-keys?disallowed tenant":  "tenant=T2&scope=span",
		"attribute-keys?template tenant":    "tenant=$tenant&scope=span",
		"attribute-values?no key":           "scope=span&type=string",
		"attribute-values?no type":          "scope=span&key=k",
		"attribute-values?limit over 100":   "scope=span&key=k&type=string&limit=101",
		"attribute-values?disallowed scope": "scope=intrinsic&key=k&type=string",
	}
	for name, q := range cases {
		path, _, _ := strings.Cut(name, "?")
		status, body := callResource(t, h, http.MethodGet, path, q, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
	if len(h.readers) != 0 {
		t.Fatal("a refused request opened a reader")
	}
}

func TestTTLCache(t *testing.T) {
	t.Parallel()
	c := newTTLCache(2, time.Minute)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	c.put("a", attrResult{Values: []string{"1"}})
	c.put("b", attrResult{Values: []string{"2"}})
	if _, ok := c.get("a"); !ok {
		t.Fatal("a missing")
	}
	c.put("c", attrResult{})
	if _, ok := c.get("b"); ok {
		t.Fatal("b should be evicted as least recently used")
	}
	now = now.Add(time.Minute)
	if _, ok := c.get("a"); ok {
		t.Fatal("a should have expired")
	}
}

func TestResourcesSettingsError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"database": nil})
	status, body := callResource(t, h, http.MethodGet, "tenants", "", nil)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "database") {
		t.Fatalf("%d %s", status, body)
	}
}
