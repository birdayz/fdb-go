package plandiff

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestJavaHTTPConnections(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		newCall func(string) func(context.Context) error
	}{
		{
			name: "plan",
			newCall: func(url string) func(context.Context) error {
				engine := NewJavaEngineHTTP(url, "cluster")
				return func(ctx context.Context) error {
					return engine.Plan(ctx, Query{SQL: "SELECT 1"}).Err
				}
			},
		},
		{
			name: "run",
			newCall: func(url string) func(context.Context) error {
				runner := NewJavaRunnerHTTP(url, "cluster")
				return func(ctx context.Context) error {
					return runner.Run(ctx, Query{SQL: "SELECT 1"}).Err
				}
			},
		},
		{
			name: "setup",
			newCall: func(url string) func(context.Context) error {
				runner := NewJavaRunnerHTTP(url, "cluster").(SetupRunner)
				return func(ctx context.Context) error {
					return runner.RunWithSetup(ctx, "schema", []string{"INSERT INTO t VALUES (1)"}, "SELECT 1").Err
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, scenario := range []struct {
				name      string
				retained  bool
				dropFirst bool
			}{
				{name: "retained client", retained: true},
				{name: "independent clients"},
				{name: "no replay after response loss", retained: true, dropFirst: true},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					var mu sync.Mutex
					connections := make(map[string]int)
					deliveries := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						mu.Lock()
						connections[r.RemoteAddr]++
						n := connections[r.RemoteAddr]
						deliveries++
						mu.Unlock()
						if r.Method != http.MethodPost || r.URL.Path != "/invoke" {
							t.Errorf("request = %s %s, want POST /invoke", r.Method, r.URL.Path)
						}
						// A reused connection loses its response deterministically.
						// The first-request arm also proves mutating RPCs are not replayed.
						if scenario.dropFirst || n > 1 {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Errorf("hijack: %v", err)
								return
							}
							_ = conn.Close()
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"success":false,"error":"syntax error","exceptionClass":"RelationalException","sqlState":"42601"}`)
					}))
					defer srv.Close()
					calls := 2
					if scenario.dropFirst {
						calls = 1
					}
					invoke := tc.newCall(srv.URL)
					for range calls {
						if !scenario.retained {
							invoke = tc.newCall(srv.URL)
						}
						err := invoke(ctx)
						if scenario.dropFirst {
							if !errors.Is(err, io.EOF) {
								t.Errorf("response loss error = %v, want EOF", err)
							}
						} else {
							var javaErr *JavaError
							if !errors.As(err, &javaErr) || javaErr.SQLState != "42601" || javaErr.ExceptionClass != "RelationalException" {
								t.Errorf("Java response = %v, want typed 42601/RelationalException", err)
							}
						}
					}
					mu.Lock()
					defer mu.Unlock()
					if deliveries != calls || len(connections) != calls {
						t.Errorf("%d calls delivered %d requests over %d connections, want one fresh connection per call and no replay", calls, deliveries, len(connections))
					}
				})
			}
		})
	}
}
