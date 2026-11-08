package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/relational/api"
)

func health(t *testing.T, h *harness) (*backend.CheckHealthResult, map[string]string) {
	t.Helper()
	res, err := h.ds.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]string
	if len(res.JSONDetails) > 0 {
		if err := json.Unmarshal(res.JSONDetails, &details); err != nil {
			t.Fatal(err)
		}
	}
	return res, details
}

func ownership() error {
	return &otel.OwnershipError{OtelError: &otel.OtelError{Kind: "ownership", Op: "open reader", Message: "no STORE_META layout"}}
}

func TestHealthOK(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	res, _ := health(t, h)
	if res.Status != backend.HealthStatusOk || !strings.Contains(res.Message, "SELECT 1 ok in /OTEL/R_TEST schema T1") || !strings.Contains(res.Message, otel.StoreLayoutDeltas) {
		t.Fatalf("%v: %s", res.Status, res.Message)
	}
	if q := h.sql.queryLog(); len(q) != 1 || q[0] != "SELECT 1" {
		t.Fatalf("queries %v", q)
	}
}

func TestHealthSteps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		json     map[string]any
		setup    func(*harness)
		status   backend.HealthStatus
		step     string
		code     string
		contains string
	}{
		{"settings", map[string]any{"tlsMode": "bogus"}, nil, backend.HealthStatusError, stepSettings, "", "tlsMode"},
		{"open cluster", nil, func(h *harness) { h.openErr = fdb.Error{Code: 1031} }, backend.HealthStatusError, stepOpenCluster, "FDB error 1031", "open cluster failed"},
		{"select 1", nil, func(h *harness) {
			h.sql.answer = func(string, string) fakeResult {
				return fakeResult{err: api.NewError(api.ErrorCode("08001"), "no catalog")}
			}
		}, backend.HealthStatusError, stepSQL, "SQLSTATE 08001", "SELECT 1 failed"},
		{"otel on, not an OTel store", map[string]any{"otelMode": "on"}, func(h *harness) { h.readerErr["T1"] = ownership() }, backend.HealthStatusError, stepOTel, "", "not an OTEL_V1 store"},
		{"otel auto, not an OTel store", nil, func(h *harness) { h.readerErr["T1"] = ownership() }, backend.HealthStatusOk, "", "", "only SQL queries"},
		{"otel auto, reader fails", nil, func(h *harness) { h.readerErr["T1"] = fdb.Error{Code: 1007} }, backend.HealthStatusError, stepOTel, "FDB error 1007", "OTel store layout failed"},
		{"otel off", map[string]any{"otelMode": "off"}, func(h *harness) { h.readerErr["T1"] = ownership() }, backend.HealthStatusOk, "", "", "OTel queries are off"},
		{"no default schema", map[string]any{"defaultSchema": nil}, nil, backend.HealthStatusOk, "", "", "not checked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, c.json)
			if c.setup != nil {
				c.setup(h)
			}
			res, details := health(t, h)
			if res.Status != c.status || !strings.Contains(res.Message, c.contains) {
				t.Fatalf("%v: %s", res.Status, res.Message)
			}
			if c.status == backend.HealthStatusError && (details["step"] != c.step || details["code"] != c.code) {
				t.Fatalf("details %v", details)
			}
			if c.code != "" && !strings.Contains(res.Message, c.code) {
				t.Fatalf("message lacks %s: %s", c.code, res.Message)
			}
		})
	}
}

// A failed cluster open is not cached: the next check retries.
func TestHealthRetriesOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.openErr = fdb.Error{Code: 1031}
	if res, _ := health(t, h); res.Status != backend.HealthStatusError {
		t.Fatal(res.Message)
	}
	h.mu.Lock()
	h.openErr = nil
	h.mu.Unlock()
	if res, _ := health(t, h); res.Status != backend.HealthStatusOk {
		t.Fatal(res.Message)
	}
}

func TestHealthCatalogConnectionWithoutDefaultSchema(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"defaultSchema": nil})
	health(t, h)
	if len(h.sql.dsns) != 1 || strings.Contains(h.sql.dsns[0], "schema=") {
		t.Fatalf("dsns %v", h.sql.dsns)
	}
}
