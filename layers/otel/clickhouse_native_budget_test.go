package otel

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

const nativeDecodedBudgetBytes = 32 << 20

// Count the exhaustion probe as well as scans: a successful result must drain
// through Next=false and check Err before closing its rows.
type nativeBudgetRows struct {
	*fakeCHNativeRows
	nextCalls int
	errCalls  int
}

func (r *nativeBudgetRows) Next() bool {
	r.nextCalls++
	return r.fakeCHNativeRows.Next()
}

func (r *nativeBudgetRows) Err() error {
	r.errCalls++
	return r.fakeCHNativeRows.Err()
}

func nativeBudgetWrapRows(rows *fakeCHNativeRows) *nativeBudgetRows {
	return &nativeBudgetRows{fakeCHNativeRows: rows}
}

func nativeBudgetConfigure(t *testing.T, b *CHBackend) {
	t.Helper()
	b.cfg.K, b.cfg.MaxCandidates, b.cfg.MaxBytes = 4, 4, 128<<20
	q := b.queries.(*chNativeQueries)
	q.cfg = b.cfg
	if b.cfg.K <= 2 || b.cfg.MaxCandidates <= 2 || b.cfg.MaxBytes <= nativeDecodedBudgetBytes+1 || q.cfg.K != b.cfg.K || q.cfg.MaxCandidates != b.cfg.MaxCandidates || q.cfg.MaxBytes != b.cfg.MaxBytes {
		t.Fatal("decoded-byte witness is masked by backend/executor row or server byte budgets")
	}
}

type nativeBudgetPublicResult struct {
	work  QueryResult
	trace []SpanHit
	spans []SpanMatch
}

func nativeBudgetPublicCall(ctx context.Context, b *CHBackend, op string) (out nativeBudgetPublicResult, err error) {
	switch op {
	case "query":
		out.work, err = b.Query(ctx, Attribute{"span", QueryKey, "string", QueryValue})
	case "trace":
		out.trace, err = b.Trace(ctx, "trace-probe")
	case "span":
		out.spans, err = b.Span(ctx, "span-probe")
	case "time":
		out.work, err = b.Time(ctx, TimeRange{Start: 0, End: 100})
	default:
		panic("unknown native budget operation: " + op)
	}
	return out, err
}

func nativeBudgetAssertTypedRows(t *testing.T, op string, got nativeBudgetPublicResult, values [][]any) {
	t.Helper()
	switch op {
	case "query", "time":
		want := make([]Hit, len(values))
		for i, row := range values {
			want[i] = Hit{Trace: row[0].(string), Latest: row[1].(int64)}
		}
		if !reflect.DeepEqual(got.work.Hits, want) {
			t.Fatalf("%s did not return all %d typed hit witnesses (got %d)", op, len(want), len(got.work.Hits))
		}
	case "trace":
		want := make([]SpanHit, len(values))
		for i, row := range values {
			want[i] = SpanHit{ID: row[0].(string), Start: row[1].(int64)}
		}
		if !reflect.DeepEqual(got.trace, want) {
			t.Fatalf("Trace did not return all %d typed span witnesses (got %d)", len(want), len(got.trace))
		}
	case "span":
		want := make([]SpanMatch, len(values))
		for i, row := range values {
			want[i] = SpanMatch{Trace: row[0].(string), ID: row[1].(string), Start: row[2].(int64)}
		}
		if !reflect.DeepEqual(got.spans, want) {
			t.Fatalf("Span did not return all %d typed match witnesses (got %d)", len(want), len(got.spans))
		}
	}
}

func nativeBudgetAssertLifecycle(t *testing.T, rows *nativeBudgetRows, scanned int64, drained bool) {
	t.Helper()
	wantNext, wantErr := int(scanned), 0
	if drained {
		wantNext++
		wantErr = 1
	}
	if rows.scanned.Load() != scanned || rows.closed.Load() != 1 || rows.nextCalls != wantNext || rows.errCalls != wantErr {
		t.Fatalf("native budget lifecycle: scanned=%d closed=%d Next=%d Err=%d, want %d/1/%d/%d", rows.scanned.Load(), rows.closed.Load(), rows.nextCalls, rows.errCalls, scanned, wantNext, wantErr)
	}
}

func nativeBudgetAssertFreshIDs(t *testing.T, calls []fakeCHNativeCall, want int) {
	t.Helper()
	if len(calls) != want {
		t.Fatalf("native budget call population=%d, want %d", len(calls), want)
	}
	seen := make(map[string]bool, want)
	for _, call := range calls {
		if call.options.id == "" || seen[call.options.id] {
			t.Fatal("native budget owner/data calls have empty or reused query IDs")
		}
		seen[call.options.id] = true
	}
}

func nativeBudgetDataWitness(t *testing.T, op, backing string, extra int) *nativeBudgetRows {
	t.Helper()
	// Deliberately unequal rows keep Query/Time's trace witnesses distinct.
	first, second := nativeDecodedBudgetBytes/2-9, nativeDecodedBudgetBytes/2-7+extra
	var values [][]any
	if op == "span" {
		values = [][]any{{"trace-a", backing[:first-7], int64(10)}, {"trace-b", backing[:second-7], int64(20)}}
	} else {
		values = [][]any{{backing[:first], int64(20)}, {backing[:second], int64(10)}}
	}
	// Independently count the actual typed values, rather than trusting the
	// construction's slice arithmetic or the production decoded-byte counter.
	var sizes []int
	for _, row := range values {
		size, stringsSeen, timesSeen := 0, 0, 0
		for _, field := range row {
			switch value := field.(type) {
			case string:
				size += len(value)
				stringsSeen++
			case int64:
				size += 8
				timesSeen++
			default:
				t.Fatalf("unexpected decoded-byte witness column type %T", field)
			}
		}
		wantStrings := 1
		if op == "span" {
			wantStrings = 2
		}
		if stringsSeen != wantStrings || timesSeen != 1 {
			t.Fatal("decoded-byte witness does not have the public result's column shape")
		}
		sizes = append(sizes, size)
	}
	if len(sizes) != 2 || sizes[0] != 16777215 || sizes[1] != 16777217+extra || sizes[0] >= nativeDecodedBudgetBytes || sizes[1] >= nativeDecodedBudgetBytes || sizes[0]+sizes[1] != 33554432+extra {
		t.Fatalf("decoded-byte witness is not a two-row cumulative boundary: sizes=%v extra=%d", sizes, extra)
	}
	return nativeBudgetWrapRows(&fakeCHNativeRows{values: values})
}

func TestCHNativePublicDecodedByteBoundary(t *testing.T) {
	// No parallel cases: every string field slices this one shared allocation.
	backing := strings.Repeat("x", nativeDecodedBudgetBytes+1)
	for _, op := range []string{"query", "trace", "span", "time"} {
		t.Run(op, func(t *testing.T) {
			b, conn, manifestRows := nativeCHBackendForTest(t)
			nativeBudgetConfigure(t, b)
			owners := []*nativeBudgetRows{
				nativeBudgetWrapRows(&fakeCHNativeRows{values: manifestRows.values}),
				nativeBudgetWrapRows(&fakeCHNativeRows{values: manifestRows.values}),
			}
			data := []*nativeBudgetRows{nativeBudgetDataWitness(t, op, backing, 0), nativeBudgetDataWitness(t, op, backing, 1)}
			conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
				if index >= 4 {
					t.Fatalf("unexpected native budget query %d", index)
				}
				if index%2 == 0 {
					return owners[index/2], nil
				}
				return data[index/2], nil
			}
			got, err := nativeBudgetPublicCall(t.Context(), b, op)
			if err != nil {
				t.Fatalf("exact 33554432 decoded bytes must succeed: %v", err)
			}
			nativeBudgetAssertTypedRows(t, op, got, data[0].values)
			nativeBudgetAssertLifecycle(t, owners[0], 1, true)
			nativeBudgetAssertLifecycle(t, data[0], 2, true)
			calls := conn.snapshot()
			nativeBudgetAssertFreshIDs(t, calls, 2)
			assertCHBudgets(t, calls[1].sql, b.cfg)
			assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
			if op == "query" || op == "time" {
				assertNativeWork(t, got.work, calls[1].options.id)
			}
			firstID := b.LastQueryWork().QueryID
			b.lastWork = QueryResult{QueryID: firstID, QueryTransport: "http", ServerMetricsSource: "stale", ServerSummary: true, ServerReadRows: 7, ServerReadBytes: 13, ServerElapsed: 17, ResponseBytes: 19, Candidates: 23, Bytes: 29}
			got, err = nativeBudgetPublicCall(t.Context(), b, op)
			var budget *QueryBudgetError
			if !errors.As(err, &budget) || budget.Op != "CH native result" {
				t.Fatalf("33554433 decoded bytes must fail with the client result budget: %v", err)
			}
			nativeBudgetAssertLifecycle(t, owners[1], 1, true)
			nativeBudgetAssertLifecycle(t, data[1], 2, false)
			calls = conn.snapshot()
			nativeBudgetAssertFreshIDs(t, calls, 4)
			assertCHBudgets(t, calls[3].sql, b.cfg)
			if calls[1].options.id != firstID || calls[3].options.id == firstID {
				t.Fatal("over-budget result did not replace the preceding positive probe's data ID")
			}
			assertNativeWork(t, b.LastQueryWork(), calls[3].options.id)
			if op == "query" || op == "time" {
				assertNativeWork(t, got.work, calls[3].options.id)
			}
		})
	}
}

func nativeBudgetPaddedManifest(t *testing.T, payload string) string {
	t.Helper()
	if len(payload) == 0 || len(payload) >= nativeDecodedBudgetBytes {
		t.Fatalf("invalid unpadded owner witness size %d", len(payload))
	}
	wire := make([]byte, nativeDecodedBudgetBytes+1)
	copy(wire, payload)
	for i := len(payload); i < len(wire); i++ {
		wire[i] = ' '
	}
	return string(wire)
}

func TestCHNativePublicOwnerDecodedByteBoundary(t *testing.T) {
	// Share the padded manifest across sequential operations, retaining only
	// one large string; the conversion's temporary byte slice is not retained.
	var padded, original string
	for _, op := range []string{"query", "trace", "span", "time"} {
		t.Run(op, func(t *testing.T) {
			b, conn, manifestRows := nativeCHBackendForTest(t)
			nativeBudgetConfigure(t, b)
			payload := manifestRows.values[0][0].(string)
			if padded == "" {
				original = payload
				padded = nativeBudgetPaddedManifest(t, payload)
			}
			if original != payload || len(padded) != 33554433 || len(padded[:nativeDecodedBudgetBytes]) != 33554432 || padded[nativeDecodedBudgetBytes] != ' ' {
				t.Fatal("owner witnesses are not the same valid manifest padded to 32MiB and 32MiB+1")
			}
			owners := []*nativeBudgetRows{
				nativeBudgetWrapRows(&fakeCHNativeRows{values: [][]any{{padded[:nativeDecodedBudgetBytes]}}}),
				nativeBudgetWrapRows(&fakeCHNativeRows{values: [][]any{{padded}}}),
			}
			data := nativeBudgetWrapRows(nativeTwoRowsForOperation(op))
			conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
				switch index {
				case 0:
					return owners[0], nil
				case 1:
					return data, nil
				case 2:
					return owners[1], nil
				default:
					t.Fatalf("data executed after over-budget owner: call %d", index)
					return nil, errors.New("unexpected data query")
				}
			}
			got, err := nativeBudgetPublicCall(t.Context(), b, op)
			if err != nil {
				t.Fatalf("valid 33554432-byte owner must authorize data: %v", err)
			}
			nativeBudgetAssertTypedRows(t, op, got, data.values)
			nativeBudgetAssertLifecycle(t, owners[0], 1, true)
			nativeBudgetAssertLifecycle(t, data, 2, true)
			calls := conn.snapshot()
			nativeBudgetAssertFreshIDs(t, calls, 2)
			assertCHBudgets(t, calls[1].sql, b.cfg)
			assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
			if op == "query" || op == "time" {
				assertNativeWork(t, got.work, calls[1].options.id)
			}
			firstID := b.LastQueryWork().QueryID
			b.lastWork = QueryResult{QueryID: firstID, QueryTransport: "http", ServerMetricsSource: "stale", ServerSummary: true, ServerReadRows: 7, ServerReadBytes: 13, ServerElapsed: 17, ResponseBytes: 19, Candidates: 23, Bytes: 29}
			got, err = nativeBudgetPublicCall(t.Context(), b, op)
			var budget *QueryBudgetError
			if !errors.As(err, &budget) || budget.Op != "CH native owner" {
				t.Fatalf("33554433-byte owner must fail the client owner budget: %v", err)
			}
			nativeBudgetAssertLifecycle(t, owners[1], 1, false)
			nativeBudgetAssertLifecycle(t, data, 2, true)
			calls = conn.snapshot()
			nativeBudgetAssertFreshIDs(t, calls, 3)
			if calls[1].options.id != firstID || !reflect.DeepEqual(b.LastQueryWork(), QueryResult{}) || !reflect.DeepEqual(got, nativeBudgetPublicResult{}) {
				t.Fatal("over-budget owner retained prior data work or return provenance")
			}
		})
	}
}
