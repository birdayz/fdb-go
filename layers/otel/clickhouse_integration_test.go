package otel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"reflect"
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
			ExposedPorts: []string{"8123/tcp"},
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
	for _, layout := range []struct {
		name  string
		attrs bool
		order string
	}{{"map", false, "trace"}, {"attrs_trace", true, "trace"}, {"attrs_time", true, "time"}} {
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
			if !strings.HasPrefix(stats.Details["server_version"], "26.10.") || stats.Details["rare_query_explain"] == "" || stats.Details["trace_query_explain"] == "" {
				t.Fatalf("version/plan evidence missing: %+v", stats.Details)
			}
			for _, a := range []Attribute{
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
				for _, hit := range got.Hits {
					spans, err := b.Trace(ctx, hit.Trace)
					wantSpans := ExpectedTrace(c, hit.Trace)
					if err != nil || len(wantSpans) != 2 || !reflect.DeepEqual(spans, wantSpans) {
						t.Fatalf("trace %s = %+v, want %+v, error %v", hit.Trace, spans, wantSpans, err)
					}
				}
			}
			for _, trace := range []string{"absent_trace", Generate(c, 2).Trace} {
				spans, err := b.Trace(ctx, trace)
				if err != nil || len(spans) != 0 {
					t.Fatalf("absent/other-tenant trace %s = %+v, error %v", trace, spans, err)
				}
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
			}
			wrong := c
			wrong.Seed++
			unowned, err := OpenCH(wrong, endpoint, layout.attrs)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(unowned.Close)
			for _, op := range []string{"query", "trace", "audit", "drop"} {
				if err := chPublicOperation(ctx, unowned, op); err == nil {
					t.Fatalf("wrong-corpus %s authorized", op)
				}
			}
			if stats, err := b.Audit(ctx); err != nil || stats.Rows != int64(c.Count) {
				t.Fatalf("rejected wrong-owner operations damaged source: stats=%+v error=%v", stats, err)
			}
			assertCHUnownedSentinel(t, ctx, c, endpoint, layout.attrs)
			if err := b.putManifest(ctx, "loading", 3); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"query", "trace", "audit"} {
				if err := chPublicOperation(ctx, b, op); err == nil {
					t.Fatalf("loading %s authorized", op)
				}
			}
			if err := b.Drop(ctx); err != nil {
				t.Fatalf("owned loading run not droppable: %v", err)
			}
			wire, err := b.request(ctx, "SELECT count() FROM system.databases WHERE name={db:String} FORMAT TabSeparated", nil, map[string]string{"db": b.database})
			if err != nil || strings.TrimSpace(string(wire)) != "0" {
				t.Fatalf("Drop did not remove owned database: %q, error %v", wire, err)
			}
		})
	}
}

func assertCHUnownedSentinel(t *testing.T, ctx context.Context, c Config, endpoint string, attrs bool) {
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
	for _, op := range []string{"query", "trace", "audit", "drop"} {
		if err := chPublicOperation(ctx, b, op); err == nil {
			t.Fatalf("manifest-free %s authorized", op)
		}
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
