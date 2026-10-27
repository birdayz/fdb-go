package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	"github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Ping hides its original dial error. Redial only to distinguish an absent
// daemon from permission, configuration, TLS, and timeout failures.
func requireCHDocker(t *testing.T, ctx context.Context) {
	t.Helper()
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err = cli.Ping(probe); err == nil {
		return
	}
	if client.IsErrConnectionFailed(err) && probe.Err() == nil {
		u, parseErr := url.Parse(cli.DaemonHost())
		if parseErr != nil {
			t.Fatalf("Docker host: %v (ping: %v)", parseErr, err)
		}
		var network, address string
		switch u.Scheme {
		case "unix":
			network, address = "unix", u.Path
		case "tcp", "http", "https":
			network, address = "tcp", u.Host
		default:
			t.Fatalf("Docker probe failed for %s: %v", cli.DaemonHost(), err)
		}
		dialer := net.Dialer{Timeout: 5 * time.Second}
		conn, dialErr := dialer.DialContext(probe, network, address)
		if dialErr == nil {
			conn.Close()
		} else if errors.Is(dialErr, os.ErrNotExist) || errors.Is(dialErr, syscall.ECONNREFUSED) {
			unavailable := &foundationdbtc.DockerUnavailableError{Err: dialErr}
			if foundationdbtc.SkipWithoutDocker(unavailable) {
				t.Skipf("FDB not available (no Docker): %v", unavailable)
			}
		}
	}
	t.Fatalf("Docker probe failed: %v", err)
}

// Every connection is to this newly created container, never an existing user's
// ClickHouse server. Layout subtests share only the server, not their databases.
func TestRealClickHouseLayouts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	requireCHDocker(t, ctx)
	setup, setupCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer setupCancel()
	container, err := testcontainers.GenericContainer(setup, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "clickhouse/clickhouse-server:26.10.2.103",
			ExposedPorts: []string{"8123/tcp", "9000/tcp"},
			Env: map[string]string{
				"CLICKHOUSE_USER":                      "otel",
				"CLICKHOUSE_PASSWORD":                  "otel_test_password",
				"CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1",
			},
			WaitingFor: wait.ForHTTP("/ping").WithPort("8123/tcp").WithBasicAuth("otel", "otel_test_password").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if container != nil {
		t.Cleanup(func() {
			cleanup, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			if err := container.Terminate(cleanup); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err) // A reachable daemon's startup/image/readiness failure is not skippable.
	}
	host, err := container.Host(setup)
	if err != nil {
		t.Fatal(err)
	}
	port, err := container.MappedPort(setup, "8123/tcp")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port.Port()), User: url.UserPassword("otel", "otel_test_password")}).String()
	nativePort, err := container.MappedPort(setup, "9000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	nativeAddr := net.JoinHostPort(host, nativePort.Port())
	for _, layout := range []struct {
		name  string
		attrs bool
		order string
	}{{"map", false, "trace"}, {"map_time", false, "time"}, {"attrs_trace", true, "trace"}, {"attrs_time", true, "time"}} {
		t.Run(layout.name, func(t *testing.T) {
			t.Parallel()
			c := testConfig()
			c.Run, c.Order, c.K = freshRun(t), layout.order, 3
			c.CHBatch = 7 // Several synchronous batches must feed both Map and MV destinations.
			b, err := OpenCH(c, endpoint, layout.attrs)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(b.Close)
			// Direct cleanup is safe here only because the test chose and created
			// this fresh namespace; production Drop remains manifest-guarded.
			t.Cleanup(func() {
				cleanup, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cleanupCancel()
				if err := b.exec(cleanup, "DROP DATABASE IF EXISTS "+b.database+" SYNC"); err != nil {
					t.Error(err)
				}
			})
			stats, err := b.Ingest(ctx)
			if err != nil || stats.Rows != int64(c.Count) || stats.Attributes != int64(c.Count*c.Attributes) || stats.LogicalPayloadBytes == 0 {
				t.Fatalf("ingest stats=%+v error=%v", stats, err)
			}
			if _, err := b.Ingest(ctx); err == nil {
				t.Fatal("ingest reused an existing database")
			}
			stats, err = b.Audit(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Rows != int64(c.Count) || stats.Attributes != int64(c.Count*c.Attributes) || stats.Structures["spans"].Keys != int64(c.Count) || stats.Structures["spans"].PhysicalBytes <= 0 {
				t.Fatalf("vacuous source/parts audit: %+v", stats)
			}
			if layout.attrs && stats.Structures["attr_lookup"].Keys != int64(c.Count*c.Attributes) {
				t.Fatalf("MV destination census = %+v", stats.Structures)
			}
			if !strings.HasPrefix(stats.Details["server_version"], "26.10.") || stats.Details["rare_query_explain"] == "" || stats.Details["trace_query_explain"] == "" || stats.Details["span_query_explain"] == "" || stats.Details["time_query_explain"] == "" {
				t.Fatalf("version/plan evidence missing: %+v", stats.Details)
			}
			assertCHProjectionEvidence(t, stats, c.Count)
			if layout.name == "map" {
				t.Run("selected_covering_projections", func(t *testing.T) { assertCHSelectedProjectionPaths(t, ctx, b) })
			}
			native := openCHNativePeer(t, ctx, b, nativeAddr)
			seenNativeIDs := map[string]bool{}
			assertCHNativeMeasurements(t, ctx, native, seenNativeIDs)
			if audited, err := native.Audit(ctx); err != nil || audited.Rows != int64(c.Count) || audited.Details["rare_query_explain"] == "" {
				t.Fatalf("native-configured audit/admin HTTP parity: %+v %v", audited, err)
			}
			responseID := Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + Generate(c, 0).ID}
			if want := Expected(c, responseID.Scope, responseID.Key, responseID.Type, responseID.Value); len(want) != 1 {
				t.Fatalf("GenAI response ID positive population=%v", want)
			}
			for _, a := range []Attribute{
				responseID,
				{"span", QueryKey, "string", QueryValue},
				{"resource", QueryKey, "string", QueryValue},
				{"scope", QueryKey, "string", QueryValue},
				{"span", QueryKey + ".int", "int", QueryValue},
				{"span", QueryKey, "int", QueryValue},
				{"span", QueryKey + ".int", "string", QueryValue},
				{"resource", QueryKey + ".int", "int", QueryValue},
				{"span", QueryKey, "string", "absent_query_v1"},
			} {
				want := Expected(c, a.Scope, a.Key, a.Type, a.Value)
				if a.Type == "int" && a.Scope == "span" && len(want) == 0 {
					t.Fatalf("integer oracle has no positive population: %+v", a)
				}
				got, err := b.Query(ctx, a)
				if err != nil || !reflect.DeepEqual(got.Hits, want) {
					t.Fatalf("query %+v = %+v, want %+v, error %v", a, got.Hits, want, err)
				}
				nativeGot, err := native.Query(ctx, a)
				if err != nil || !reflect.DeepEqual(nativeGot.Hits, want) {
					t.Fatalf("native query %+v = %+v want %+v: %v", a, nativeGot.Hits, want, err)
				}
				assertCHNativeDataReceipt(t, nativeGot, seenNativeIDs)
				if native.LastQueryWork().QueryID != nativeGot.QueryID {
					t.Fatal("native equality work receipt belongs to another query")
				}
				for _, hit := range got.Hits {
					spans, err := b.Trace(ctx, hit.Trace)
					wantSpans := ExpectedTrace(c, hit.Trace)
					if err != nil || len(wantSpans) != 2 || !reflect.DeepEqual(spans, wantSpans) {
						t.Fatalf("trace %s = %+v, want %+v, error %v", hit.Trace, spans, wantSpans, err)
					}
					nativeSpans, err := native.Trace(ctx, hit.Trace)
					if err != nil || !reflect.DeepEqual(nativeSpans, wantSpans) {
						t.Fatalf("native trace %s = %+v want %+v: %v", hit.Trace, nativeSpans, wantSpans, err)
					}
					assertCHNativeDataReceipt(t, native.LastQueryWork(), seenNativeIDs)
				}
			}
			for _, trace := range []string{"absent_trace", Generate(c, 2).Trace} {
				spans, err := b.Trace(ctx, trace)
				if err != nil || len(spans) != 0 {
					t.Fatalf("absent/other-tenant trace %s = %+v, error %v", trace, spans, err)
				}
				nativeSpans, err := native.Trace(ctx, trace)
				if err != nil || len(nativeSpans) != 0 {
					t.Fatalf("native absent/other-tenant trace %s = %+v: %v", trace, nativeSpans, err)
				}
				assertCHNativeDataReceipt(t, native.LastQueryWork(), seenNativeIDs)
			}
			for _, budget := range []string{"bytes", "rows"} {
				tiny := c
				if budget == "bytes" {
					tiny.MaxBytes = 1
				} else {
					tiny.MaxCandidates = 1
				}
				limited, err := OpenCH(tiny, endpoint, layout.attrs)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(limited.Close)
				if _, err := limited.Trace(ctx, Generate(c, 0).Trace); err == nil {
					t.Fatalf("trace accepted insufficient %s read-work budget", budget)
				}
				nativeLimited := openCHNativePeer(t, ctx, limited, nativeAddr)
				if _, err := nativeLimited.Trace(ctx, Generate(c, 0).Trace); err == nil {
					t.Fatalf("native trace accepted insufficient %s read-work budget", budget)
				}
				assertCHNativeDataReceipt(t, nativeLimited.LastQueryWork(), seenNativeIDs)
				if _, err := nativeLimited.Query(ctx, Attribute{"resource", QueryKey, "string", QueryValue}); err == nil {
					t.Fatalf("native equality accepted insufficient %s read-work budget", budget)
				}
				assertCHNativeDataReceipt(t, nativeLimited.LastQueryWork(), seenNativeIDs)
			}
			wrong := c
			wrong.Seed++
			unowned, err := OpenCH(wrong, endpoint, layout.attrs)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(unowned.Close)
			nativeUnowned := openCHNativePeer(t, ctx, unowned, nativeAddr)
			wrongConfig := nativeUnowned.cfg
			nativeUnowned.cfg = native.cfg
			if _, err := nativeUnowned.Query(ctx, responseID); err != nil {
				t.Fatal(err)
			}
			assertCHNativeDataReceipt(t, nativeUnowned.LastQueryWork(), seenNativeIDs)
			nativeUnowned.cfg = wrongConfig
			for _, op := range []string{"query", "trace", "span", "time", "empty_time", "audit", "drop"} {
				if err := chPublicOperation(ctx, unowned, op); err == nil {
					t.Fatalf("wrong-corpus %s authorized", op)
				}
				if err := chPublicOperation(ctx, nativeUnowned, op); err == nil {
					t.Fatalf("native wrong-corpus %s authorized", op)
				}
				assertCHNativeOwnerFailureWork(t, nativeUnowned)
			}
			if stats, err := b.Audit(ctx); err != nil || stats.Rows != int64(c.Count) {
				t.Fatalf("rejected wrong-owner operations damaged source: stats=%+v error=%v", stats, err)
			}
			assertCHUnownedSentinel(t, ctx, c, endpoint, layout.attrs, nativeAddr)
			assertCHIntrinsicQueries(t, ctx, b, native, seenNativeIDs)
			canceled, stop := context.WithCancel(ctx)
			stop()
			for _, op := range []string{"query", "trace", "span", "time", "empty_time"} {
				if err := chPublicOperation(canceled, native, op); !errors.Is(err, context.Canceled) {
					t.Fatalf("native canceled %s: %v", op, err)
				}
				assertCHNativeOwnerFailureWork(t, native)
			}
			if _, err := native.Query(ctx, responseID); err != nil {
				t.Fatal(err)
			}
			assertCHNativeDataReceipt(t, native.LastQueryWork(), seenNativeIDs)
			if err := b.putManifest(ctx, "loading", 3); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"query", "trace", "span", "time", "empty_time", "audit"} {
				if err := chPublicOperation(ctx, b, op); err == nil {
					t.Fatalf("loading %s authorized", op)
				}
				if err := chPublicOperation(ctx, native, op); err == nil {
					t.Fatalf("native loading %s authorized", op)
				}
				if op != "audit" {
					assertCHNativeOwnerFailureWork(t, native)
				}
			}
			if err := native.Drop(ctx); err != nil {
				t.Fatalf("native-configured owned loading run not droppable: %v", err)
			}
			for _, op := range []string{"query", "trace", "span", "time", "empty_time"} {
				if err := chPublicOperation(ctx, native, op); err == nil {
					t.Fatalf("native dropped database %s authorized", op)
				}
				assertCHNativeOwnerFailureWork(t, native)
			}
			wire, err := b.request(ctx, "SELECT count() FROM system.databases WHERE name={db:String} FORMAT TabSeparated", nil, map[string]string{"db": b.database})
			if err != nil || strings.TrimSpace(string(wire)) != "0" {
				t.Fatalf("Drop did not remove owned database: %q, error %v", wire, err)
			}
		})
	}
}

func openCHNativePeer(t *testing.T, ctx context.Context, httpBackend *CHBackend, nativeAddr string) *CHBackend {
	t.Helper()
	c := httpBackend.cfg
	c.CHQueryTransport, c.CHNativeAddr = "native", nativeAddr
	peer, err := OpenCHContext(ctx, c, httpBackend.endpoint.String(), httpBackend.attrs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	if peer.database != httpBackend.database {
		t.Fatal("native parity peer did not reuse HTTP-loaded tables")
	}
	if peer.LastQueryWork().QueryID != "" {
		t.Fatal("native constructor unexpectedly executed a data query")
	}
	return peer
}

func assertCHNativeDataReceipt(t *testing.T, work QueryResult, seen map[string]bool) {
	t.Helper()
	if work.QueryID == "" || seen[work.QueryID] {
		t.Fatalf("native data query ID missing/reused: %+v", work)
	}
	seen[work.QueryID] = true
	if work.QueryTransport != "native" || work.ServerMetricsSource != "unavailable" || work.ServerSummary || work.ServerReadRows != 0 || work.ServerReadBytes != 0 || work.ServerElapsed != 0 || work.ResponseBytes != 0 || work.Candidates != 0 || work.Bytes != 0 {
		t.Fatalf("native fast-query work must be unavailable, not measured zero: %+v", work)
	}
}

func assertCHNativeOwnerFailureWork(t *testing.T, b *CHBackend) {
	t.Helper()
	work := b.LastQueryWork()
	if !reflect.DeepEqual(work, QueryResult{}) {
		t.Fatalf("ownership/cancellation failure retained or attributed a data query: %+v", work)
	}
}

func assertCHNativeMeasurements(t *testing.T, ctx context.Context, native *CHBackend, seen map[string]bool) {
	t.Helper()
	c := native.cfg
	c.Iterations = 16
	var evidence Evidence
	if err := MeasureQueries(ctx, native, c, "all", &evidence); err != nil {
		t.Fatalf("native all-suite source-oracle parity: %v", err)
	}
	if len(evidence.Samples) != 16 || evidence.Failures != 0 {
		t.Fatalf("native parity has incomplete samples: %+v", evidence)
	}
	classes := map[string]bool{}
	for _, sample := range evidence.Samples {
		if sample.Error != "" || sample.Digest == "" || sample.Nanoseconds <= 0 {
			t.Fatalf("native parity sample failed/empty: %+v", sample)
		}
		classes[sample.Class] = true
		assertCHNativeDataReceipt(t, QueryResult{QueryID: sample.QueryID, QueryTransport: sample.QueryTransport, ServerMetricsSource: sample.ServerMetricsSource, ServerSummary: sample.ServerSummary, ServerReadRows: sample.ServerReadRows, ServerReadBytes: sample.ServerReadBytes, ServerElapsed: sample.ServerElapsed, ResponseBytes: sample.ResponseBytes, Candidates: sample.Candidates, Bytes: sample.LogicalBytes}, seen)
	}
	for _, class := range []string{"trace", "trace_absent", "span", "span_absent", "genai_response", "genai_scope_resource", "genai_scope_scope", "genai_type", "genai_absent", "genai_operation", "genai_provider", "genai_model", "time_narrow", "time_empty", "time_absent", "time_broad"} {
		if !classes[class] {
			t.Fatalf("native all-suite omitted %s", class)
		}
	}
}

func assertCHUnownedSentinel(t *testing.T, ctx context.Context, c Config, endpoint string, attrs bool, nativeAddr string) {
	t.Helper()
	c.Run = freshRun(t)
	b, err := OpenCH(c, endpoint, attrs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	if err := b.exec(ctx, "CREATE DATABASE "+b.database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := b.exec(cleanup, "DROP DATABASE "+b.database+" SYNC"); err != nil {
			t.Error(err)
		}
	})
	if err := b.exec(ctx, "CREATE TABLE "+b.database+".sentinel (v String) ENGINE=TinyLog"); err != nil {
		t.Fatal(err)
	}
	if err := b.exec(ctx, "INSERT INTO "+b.database+".sentinel VALUES ('do_not_drop')"); err != nil {
		t.Fatal(err)
	}
	native := openCHNativePeer(t, ctx, b, nativeAddr)
	for _, op := range []string{"query", "trace", "span", "time", "empty_time", "audit", "drop"} {
		if err := chPublicOperation(ctx, b, op); err == nil {
			t.Fatalf("manifest-free %s authorized", op)
		}
		if err := chPublicOperation(ctx, native, op); err == nil {
			t.Fatalf("native manifest-free %s authorized", op)
		}
		assertCHNativeOwnerFailureWork(t, native)
	}
	wire, err := b.request(ctx, "SELECT v FROM "+b.database+".sentinel FORMAT TabSeparated", nil, nil)
	if err != nil || string(wire) != "do_not_drop\n" {
		t.Fatalf("unowned sentinel changed: %q, error %v", wire, err)
	}
	// Ingest must not claim or clean up a pre-existing, manifest-free database.
	if _, err := b.Ingest(ctx); err == nil {
		t.Fatal("ingest claimed an unowned database")
	}
	wire, err = b.request(ctx, fmt.Sprintf("SELECT v FROM %s.sentinel FORMAT TabSeparated", b.database), nil, nil)
	if err != nil || string(wire) != "do_not_drop\n" {
		t.Fatalf("failed ingest removed pre-existing sentinel: %q, error %v", wire, err)
	}
}

type chExplainPlanNode struct {
	Type        string               `json:"Node Type"`
	Description string               `json:"Description"`
	Plans       []*chExplainPlanNode `json:"Plans"`
}

func requireCHSelectedProjection(wire []byte, projection string) error {
	var document []struct {
		Plan *chExplainPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(wire, &document); err != nil {
		return fmt.Errorf("decode EXPLAIN plan: %w", err)
	}
	if len(document) != 1 || document[0].Plan == nil {
		return errors.New("EXPLAIN must contain exactly one nonempty plan")
	}
	reads := 0
	var visit func(*chExplainPlanNode) error
	visit = func(node *chExplainPlanNode) error {
		if node == nil {
			return errors.New("EXPLAIN contains null plan node")
		}
		if node.Type == "ReadFromMergeTree" {
			reads++
			if node.Description != projection {
				return fmt.Errorf("selected MergeTree read %q, want covering projection %q", node.Description, projection)
			}
		}
		for _, child := range node.Plans {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(document[0].Plan); err != nil {
		return err
	}
	if reads == 0 {
		return errors.New("EXPLAIN contains no selected MergeTree read")
	}
	return nil
}

func TestCHSelectedProjectionPlan(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, wire, projection string
		valid                  bool
	}{
		{"selected_span", `[{"Plan":{"Node Type":"Expression","Plans":[{"Node Type":"ReadFromMergeTree","Description":"by_span"}]}}]`, "by_span", true},
		{"selected_time", `[{"Plan":{"Node Type":"ReadFromMergeTree","Description":"by_time"}}]`, "by_time", true},
		{"analyzed_only", `[{"Plan":{"Node Type":"ReadFromMergeTree","Description":"db.spans","Projections":[{"Name":"by_span","Description":"Projection by_span is usable"}]}}]`, "by_span", false},
		{"stats_claim_selected", `[{"Plan":{"Node Type":"ReadFromMergeTree","Description":"db.spans","Projections":[{"Name":"by_span","Description":"Projection by_span is selected as the best"}]}}]`, "by_span", false},
		{"wrong_selected_projection", `[{"Plan":{"Node Type":"ReadFromMergeTree","Description":"by_time"}}]`, "by_span", false},
		{"base_fallback_union", `[{"Plan":{"Node Type":"Union","Plans":[{"Node Type":"ReadFromMergeTree","Description":"by_span"},{"Node Type":"ReadFromMergeTree","Description":"db.spans"}]}}]`, "by_span", false},
		{"name_without_read", `[{"Plan":{"Node Type":"Expression","Description":"by_span"}}]`, "by_span", false},
		{"empty", `[]`, "by_span", false},
		{"missing_plan", `[{}]`, "by_span", false},
		{"null_child", `[{"Plan":{"Node Type":"Expression","Plans":[null]}}]`, "by_span", false},
		{"multiple_plans", `[{"Plan":{}},{"Plan":{}}]`, "by_span", false},
		{"malformed", `[`, "by_span", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireCHSelectedProjection([]byte(test.wire), test.projection)
			if (err == nil) != test.valid {
				t.Fatalf("selection classifier valid=%t: %v", test.valid, err)
			}
		})
	}
}

func assertCHSelectedProjectionPaths(t *testing.T, ctx context.Context, schema *CHBackend) {
	t.Helper()
	c := schema.cfg
	c.Run, c.Count, c.Tenants, c.Attributes = freshRun(t), 8192, 1, 4
	c.MaxCandidates, c.MaxBytes = 2*c.Count, 64<<20
	c.CHQueryTransport, c.CHNativeAddr = "http", ""
	profile, err := OpenCHContext(ctx, c, schema.endpoint.String(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(profile.Close)
	if err := profile.exec(ctx, "CREATE DATABASE "+profile.database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := profile.exec(cleanup, "DROP DATABASE IF EXISTS "+profile.database+" SYNC"); err != nil {
			t.Error(err)
		}
	})
	// Clone the production columns/indexes/projections; 128 granules make the
	// alternate keys observable without forcing a projection or changing SQL.
	ddl := "CREATE TABLE " + profile.database + ".spans AS " + schema.database + ".spans ENGINE=MergeTree PARTITION BY day ORDER BY (tenant,service,ts_ns,trace_id,span_id) SETTINGS index_granularity=64"
	if err := profile.exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	if err := profile.exec(ctx, "CREATE TABLE "+profile.database+".owner (sequence UInt32,payload String) ENGINE=TinyLog"); err != nil {
		t.Fatal(err)
	}
	if err := profile.putManifest(ctx, "loading", 1); err != nil {
		t.Fatal(err)
	}
	for lo := 0; lo < c.Count; lo += 1024 {
		var payload strings.Builder
		for ordinal := lo; ordinal < lo+1024 && ordinal < c.Count; ordinal++ {
			wire, err := json.Marshal(chRow(Generate(c, ordinal)))
			if err != nil {
				t.Fatal(err)
			}
			payload.Write(wire)
			payload.WriteByte('\n')
		}
		if _, err := profile.request(ctx, "INSERT INTO "+profile.database+".spans SETTINGS async_insert=0 FORMAT JSONEachRow", []byte(payload.String()), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := profile.putManifest(ctx, "ready", 2); err != nil {
		t.Fatal(err)
	}
	population, err := profile.request(ctx, "SELECT count() FROM "+profile.database+".spans FORMAT TabSeparated", nil, nil)
	if err != nil || strings.TrimSpace(string(population)) != strconv.Itoa(c.Count) {
		t.Fatalf("projection profile source population=%q want %d: %v", population, c.Count, err)
	}
	marks, err := profile.request(ctx, "SELECT sum(marks) FROM system.parts WHERE database={db:String} AND table='spans' AND active FORMAT TabSeparated", nil, map[string]string{"db": profile.database})
	granules, parseErr := strconv.ParseInt(strings.TrimSpace(string(marks)), 10, 64)
	if err != nil || parseErr != nil || granules < 128 {
		t.Fatalf("projection profile lacks representative granules: %q %v %v", marks, err, parseErr)
	}
	probe := Generate(c, c.Count/2)
	spanSQL, spanParams := profile.spanSQL(probe.ID)
	window := TimeRange{Start: probe.Start, End: probe.Start + 1}
	timeSQL, timeParams := profile.timeSQL(window)
	for _, query := range []struct {
		name, sql string
		params    map[string]string
	}{{"by_span", spanSQL, spanParams}, {"by_time", timeSQL, timeParams}} {
		// The selected read step's exact description identifies its data source;
		// projection-analysis statistics are not evidence of a covering read.
		explain := "EXPLAIN PLAN json=1,description=1,actions=0 " + strings.TrimSuffix(query.sql, " FORMAT JSONEachRow") + " FORMAT TSVRaw"
		wire, err := profile.request(ctx, explain, nil, query.params)
		if err != nil {
			t.Fatalf("%s representative EXPLAIN failed: %v", query.name, err)
		}
		if err := requireCHSelectedProjection(wire, query.name); err != nil {
			t.Fatalf("%s not the selected covering path for %d rows/%d granules: %v\nEXPLAIN=%s", query.name, c.Count, granules, err, wire)
		}
	}
	matches, err := profile.Span(ctx, probe.ID)
	if err != nil || !reflect.DeepEqual(matches, []SpanMatch{{probe.Trace, probe.ID, probe.Start}}) {
		t.Fatalf("selected span path result: %+v %v", matches, err)
	}
	want, err := ExpectedTimeContext(ctx, c, window)
	if err != nil || len(want) == 0 {
		t.Fatalf("projection profile time oracle lacks population: %+v %v", want, err)
	}
	got, err := profile.Time(ctx, window)
	if err != nil || !reflect.DeepEqual(got.Hits, want) {
		t.Fatalf("selected time path result: %+v want %+v: %v", got, want, err)
	}
}

func assertCHProjectionEvidence(t *testing.T, stats Statistics, count int) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(stats.Details["projection_parts_bytes"]))
	seen := map[string]bool{}
	for decoder.More() {
		var row struct{ Name, Rows, Disk, Logical string }
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		if seen[row.Name] {
			t.Fatalf("duplicate projection evidence: %s", row.Name)
		}
		seen[row.Name] = true
		if row.Rows != strconv.Itoa(count) {
			t.Fatalf("projection rows=%s, want %d: %+v", row.Rows, count, row)
		}
		for _, value := range []string{row.Disk, row.Logical} {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 {
				t.Fatalf("projection byte evidence missing: %+v error=%v", row, err)
			}
		}
	}
	for _, name := range []string{"by_trace", "by_span", "by_time"} {
		if !seen[name] {
			t.Fatalf("projection not materialized/accounted: %s in %+v", name, stats.Details)
		}
	}
}

func assertCHIntrinsicQueries(t *testing.T, ctx context.Context, b, native *CHBackend, seenNativeIDs map[string]bool) {
	t.Helper()
	const base int64 = 1700000000000000000
	const shared = "ffffffffffffffff"
	trace := func(n int) string { return fmt.Sprintf("%032x", n) }
	id := func(n int) string { return fmt.Sprintf("%016x", n) }
	if got, err := b.Span(ctx, shared); err != nil || len(got) != 0 {
		t.Fatalf("standalone span miss control=%+v error=%v", got, err)
	}
	if got, err := native.Span(ctx, shared); err != nil || len(got) != 0 {
		t.Fatalf("native standalone span miss control=%+v error=%v", got, err)
	}
	assertCHNativeDataReceipt(t, native.LastQueryWork(), seenNativeIDs)
	// These timestamps are between generated whole-second timestamps. A shared
	// span ID across traces proves the intrinsic lookup needs no trace hint.
	spans := []Span{
		{Tenant: b.cfg.Tenant, Trace: trace(1), ID: shared, Start: base + 10},
		{Tenant: b.cfg.Tenant, Trace: trace(2), ID: shared, Start: base + 10},
		{Tenant: b.cfg.Tenant, Trace: trace(3), ID: shared, Start: base + 9},
		{Tenant: b.cfg.Tenant, Trace: trace(1), ID: id(2), Start: base + 20},
		{Tenant: b.cfg.Tenant, Trace: trace(4), ID: id(3), Start: base + 19},
		{Tenant: b.cfg.Tenant, Trace: trace(4), ID: id(4), Start: base + 18},
		{Tenant: b.cfg.Tenant, Trace: trace(5), ID: id(5), Start: base + 10},
		{Tenant: (b.cfg.Tenant + 1) % b.cfg.Tenants, Trace: trace(99), ID: shared, Start: base + 11},
	}
	var payload strings.Builder
	for _, span := range spans {
		span.Attributes = []Attribute{{Scope: "span", Key: "intrinsic.fixture", Type: "string", Value: "v2"}}
		wire, err := json.Marshal(chRow(span))
		if err != nil {
			t.Fatal(err)
		}
		payload.Write(wire)
		payload.WriteByte('\n')
	}
	if _, err := b.request(ctx, "INSERT INTO "+b.database+".spans SETTINGS async_insert=0 FORMAT JSONEachRow", []byte(payload.String()), nil); err != nil {
		t.Fatal(err)
	}
	k := b.cfg.K
	b.cfg.K = 1
	got, err := b.Span(ctx, shared)
	b.cfg.K = k
	want := []SpanMatch{{Trace: trace(3), ID: shared, Start: base + 9}, {Trace: trace(1), ID: shared, Start: base + 10}, {Trace: trace(2), ID: shared, Start: base + 10}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("standalone span matches/order with K=1: got=%+v want=%+v error=%v", got, want, err)
	}
	nativeK := native.cfg.K
	native.cfg.K = 1
	nativeSpans, err := native.Span(ctx, shared)
	native.cfg.K = nativeK
	if err != nil || !reflect.DeepEqual(nativeSpans, want) {
		t.Fatalf("native same-table standalone span matches/order with K=1: got=%+v want=%+v error=%v", nativeSpans, want, err)
	}
	assertCHNativeDataReceipt(t, native.LastQueryWork(), seenNativeIDs)
	work := b.LastQueryWork()
	if !work.ServerSummary || work.ServerReadRows < 3 || work.ServerReadBytes <= 0 {
		t.Fatalf("span server read-work evidence unavailable: %+v", work)
	}
	for _, tc := range []struct {
		window TimeRange
		want   []Hit
	}{
		{TimeRange{Start: base + 10, End: base + 20}, []Hit{{Trace: trace(4), Latest: base + 19}, {Trace: trace(5), Latest: base + 10}, {Trace: trace(2), Latest: base + 10}}},
		{TimeRange{Start: base + 10, End: base + 11}, []Hit{{Trace: trace(5), Latest: base + 10}, {Trace: trace(2), Latest: base + 10}, {Trace: trace(1), Latest: base + 10}}},
		{TimeRange{Start: base + 9, End: base + 10}, []Hit{{Trace: trace(3), Latest: base + 9}}},
		{TimeRange{Start: base + 20, End: base + 21}, []Hit{{Trace: trace(1), Latest: base + 20}}},
		{TimeRange{Start: base + 20, End: base + 20}, []Hit{}},
		{TimeRange{Start: base + 21, End: base + 22}, []Hit{}},
	} {
		result, err := b.Time(ctx, tc.window)
		if err != nil || !reflect.DeepEqual(result.Hits, tc.want) {
			t.Fatalf("intrinsic time %+v: got=%+v want=%+v error=%v", tc.window, result.Hits, tc.want, err)
		}
		if len(tc.want) > 0 && (!result.ServerSummary || result.ServerReadRows <= 0 || result.ServerReadBytes <= 0) {
			t.Fatalf("time server read-work evidence unavailable: %+v", result)
		}
		nativeResult, err := native.Time(ctx, tc.window)
		if err != nil || !reflect.DeepEqual(nativeResult.Hits, tc.want) {
			t.Fatalf("native same-table intrinsic time %+v: got=%+v want=%+v error=%v", tc.window, nativeResult.Hits, tc.want, err)
		}
		assertCHNativeDataReceipt(t, nativeResult, seenNativeIDs)
		if native.LastQueryWork().QueryID != nativeResult.QueryID {
			t.Fatal("native time work receipt belongs to another query")
		}
	}
	for _, budget := range []string{"bytes", "rows"} {
		limitedConfig := b.cfg
		if budget == "bytes" {
			limitedConfig.MaxBytes = 1
		} else {
			limitedConfig.MaxCandidates = 1
		}
		limited, err := OpenCH(limitedConfig, b.endpoint.String(), b.attrs)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(limited.Close)
		if _, err := limited.Span(ctx, shared); err == nil {
			t.Fatalf("span accepted insufficient %s budget", budget)
		}
		if _, err := limited.Time(ctx, TimeRange{Start: base + 10, End: base + 20}); err == nil {
			t.Fatalf("time accepted insufficient %s budget", budget)
		}
		nativeLimited := openCHNativePeer(t, ctx, limited, native.cfg.CHNativeAddr)
		if _, err := nativeLimited.Span(ctx, shared); err == nil {
			t.Fatalf("native span accepted insufficient %s budget", budget)
		}
		assertCHNativeDataReceipt(t, nativeLimited.LastQueryWork(), seenNativeIDs)
		if _, err := nativeLimited.Time(ctx, TimeRange{Start: base + 10, End: base + 20}); err == nil {
			t.Fatalf("native time accepted insufficient %s budget", budget)
		}
		assertCHNativeDataReceipt(t, nativeLimited.LastQueryWork(), seenNativeIDs)
	}
}
