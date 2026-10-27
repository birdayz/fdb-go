package otel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func freshRun(t *testing.T) string {
	t.Helper()
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return "test_" + hex.EncodeToString(nonce[:])
}

func otelTransaction(t *testing.T, ctx context.Context, b *FDBBackend, fn func(*recordlayer.FDBRecordStore, *recordlayer.FDBRecordContext) error) {
	t.Helper()
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		return nil, fn(store, rc)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func saveOtelSpan(t *testing.T, ctx context.Context, b *FDBBackend, s Span) {
	t.Helper()
	m, err := toRecord(s)
	if err != nil {
		t.Fatal(err)
	}
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		_, err := store.SaveRecord(m)
		return err
	})
}

func otelPK(s Span) tuple.Tuple { return tuple.Tuple{int64(s.Tenant), s.Trace, s.ID} }

func assertOtelSource(t *testing.T, ctx context.Context, b *FDBBackend, want Span) {
	t.Helper()
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		record, err := store.LoadRecord(otelPK(want))
		if err != nil {
			return err
		}
		if record == nil {
			return errors.New("source disappeared")
		}
		got, err := decodeRecord(record.Record)
		if err != nil {
			return err
		}
		want.Attributes = append([]Attribute(nil), want.Attributes...)
		if !equalSpan(got, want) {
			return fmt.Errorf("source mismatch: got %+v want %+v", got, want)
		}
		return nil
	})
}

// Fixture snapshots are bounded independently of the implementation's census.
func otelSnapshot(t *testing.T, ctx context.Context, b *FDBBackend, ss subspace.Subspace) map[string]string {
	t.Helper()
	var rows []fdb.KeyValue
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		var err error
		rows, err = rc.Transaction().GetRange(ss, fdb.RangeOptions{Limit: 20001}).GetSliceWithError()
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > 20000 {
		t.Fatal("fixture snapshot exceeded 20000-key bound")
	}
	out := make(map[string]string, len(rows))
	bytes := 0
	for _, row := range rows {
		bytes += len(row.Key) + len(row.Value)
		out[string(row.Key)] = string(row.Value)
	}
	if bytes > 16<<20 {
		t.Fatal("fixture snapshot exceeded 16MiB bound")
	}
	return out
}

func otelSecondary(b *FDBBackend) subspace.Subspace {
	return b.store.Sub(recordlayer.IndexSecondarySpaceKey, b.idx.SubspaceTupleKey())
}

func requireOtelKind(t *testing.T, err error, kind string) {
	t.Helper()
	var classified *OtelError
	if !errors.As(err, &classified) || classified.Kind != kind {
		t.Fatalf("want %s error, got %T: %v", kind, err, err)
	}
}

func requireOtelBudget(t *testing.T, err error) {
	t.Helper()
	var budget *QueryBudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("want QueryBudgetError, got %T: %v", err, err)
	}
}

func openOtelFixture(t *testing.T, ctx context.Context, cluster string, c Config, ingest bool) *FDBBackend {
	t.Helper()
	c.Run = freshRun(t)
	b, err := OpenFDBContext(ctx, c, cluster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if ingest {
			if err := b.Drop(cleanup); err != nil {
				t.Error(err)
			}
			return
		}
		// This namespace was freshly allocated here, not discovered externally.
		_, err := b.db.Run(cleanup, func(rc *recordlayer.FDBRecordContext) (any, error) {
			r, err := fdb.PrefixRange(b.root.Bytes())
			if err == nil {
				rc.Transaction().ClearRange(r)
			}
			return nil, err
		})
		if err != nil {
			t.Error(err)
		}
	})
	if ingest {
		if _, err := b.Ingest(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// One real FDB testcontainer serves only freshly allocated test namespaces.
func TestRealRecordLayerCoveringIndexes(t *testing.T) {
	t.Parallel()
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	wire, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cluster, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"trace", "time"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			c := testConfig()
			c.Count, c.Order, c.HashBytes, c.K = 260, order, 1, 3
			b := openOtelFixture(t, ctx, cluster, c, true)
			c = b.cfg
			if _, err := b.Ingest(ctx); err == nil {
				t.Fatal("reused namespace accepted")
			} else {
				requireOtelKind(t, err, "ownership")
			}
			t.Run("audit_pages_retry_and_corruption", func(t *testing.T) { testOtelAudit(t, ctx, b) })
			t.Run("ownership", func(t *testing.T) { testOtelOwnership(t, ctx, b, cluster) })
			t.Run("typed_scoped_controls", func(t *testing.T) { testOtelScopedQueries(t, ctx, b) })
			t.Run("unreadable_indexes", func(t *testing.T) { testOtelUnreadableIndexes(t, ctx, cluster, c) })
			t.Run("malformed_postings", func(t *testing.T) { testOtelMalformedPostings(t, ctx, b) })
			t.Run("source_identity_admission", func(t *testing.T) { testOtelIdentityAdmission(t, ctx, b) })
			if err := AuditQueries(ctx, b, c); err != nil {
				t.Fatal(err)
			}
			if err := MeasureQueries(ctx, b, c, "selective", &Evidence{}); err != nil {
				t.Fatal(err)
			}
			t.Run("literal_order_and_budgets", func(t *testing.T) { testOtelOrderAndBudgets(t, ctx, b) })
			t.Run("rollback_corrections_and_deletion", func(t *testing.T) { testOtelMutations(t, ctx, b) })
		})
		t.Run(order+"_high_attribute_batches", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			c := testConfig()
			c.Count, c.Attributes, c.Batch, c.Order = 9, 128, 64, order
			b := openOtelFixture(t, ctx, cluster, c, false)
			stats, err := b.Ingest(ctx)
			if err != nil {
				t.Fatal(err)
			}
			batches, err := strconv.Atoi(stats.Details["committed_batches"])
			if err != nil || batches <= 1 || batches > c.Count {
				t.Fatalf("nine sources below batch=64 must split by bytes: %+v (%v)", stats, err)
			}
			if stats.Rows != 9 || stats.Attributes != 1152 {
				t.Fatalf("ingestion population: %+v", stats)
			}
			audited, err := b.Audit(ctx)
			if err != nil || audited.Rows != 9 || audited.Structures["eq_secondary"].Keys != 1152 {
				t.Fatalf("high-attribute ingestion census: %+v %v", audited, err)
			}
			testOtelAdmission(t, ctx, b)
		})
		for _, saveMode := range []string{"serial", "pipelined"} {
			t.Run(order+"_"+saveMode+"_ingest", func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				c := testConfig()
				c.Count, c.Order, c.SaveMode = 7, order, saveMode
				b := openOtelFixture(t, ctx, cluster, c, true)
				if _, err := b.Ingest(ctx); err == nil {
					t.Fatal("duplicate ingest accepted")
				} else {
					var ownership *OwnershipError
					if !errors.As(err, &ownership) {
						t.Fatalf("duplicate ingest: %T %v", err, err)
					}
				}
				stats, err := b.Audit(ctx)
				if err != nil || stats.Rows != 7 || stats.Structures["eq_secondary"].Keys != 56 {
					t.Fatalf("%s ingest census: %+v %v", saveMode, stats, err)
				}
				a := Attribute{"span", QueryKey, "string", QueryValue}
				got, err := b.Query(ctx, a)
				want := Expected(b.cfg, a.Scope, a.Key, a.Type, a.Value)
				if err != nil || len(want) == 0 || !sameJSON(got.Hits, want) {
					t.Fatalf("%s ingest query: %+v want %+v: %v", saveMode, got, want, err)
				}
				first := Generate(b.cfg, 0)
				traced, err := b.Trace(ctx, first.Trace)
				if err != nil || !sameJSON(traced, ExpectedTrace(b.cfg, first.Trace)) {
					t.Fatalf("%s ingest trace: %+v %v", saveMode, traced, err)
				}
			})
		}
		t.Run(order+"_single_source_trace_budget", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			c := testConfig()
			c.Count, c.Tenants, c.Order = 1, 1, order
			b := openOtelFixture(t, ctx, cluster, c, true)
			s := Generate(b.cfg, 0)
			got, err := b.Trace(ctx, s.Trace)
			if err != nil || !sameJSON(got, []SpanHit{{s.ID, s.Start}}) {
				t.Fatalf("sole trace control: %+v %v", got, err)
			}
			bytes := b.LastQueryWork().Bytes
			if bytes <= 1 {
				t.Fatalf("vacuous sole-source byte population: %d", bytes)
			}
			b.cfg.MaxBytes = 1
			_, err = b.Trace(ctx, s.Trace)
			requireOtelBudget(t, err)
			b.cfg.MaxBytes = bytes - 1
			_, err = b.Trace(ctx, s.Trace)
			requireOtelBudget(t, err)
			b.cfg.MaxBytes = bytes
			got, err = b.Trace(ctx, s.Trace)
			if err != nil || !sameJSON(got, []SpanHit{{s.ID, s.Start}}) || b.LastQueryWork().Bytes != bytes {
				t.Fatalf("exact sole-source byte budget: %+v %+v %v", got, b.LastQueryWork(), err)
			}
		})
	}
}

func testOtelUnreadableIndexes(t *testing.T, ctx context.Context, cluster string, c Config) {
	for _, state := range []string{"write_only", "disabled"} {
		t.Run(state, func(t *testing.T) {
			c.Count = 4
			b := openOtelFixture(t, ctx, cluster, c, true)
			a := Attribute{"span", QueryKey, "string", QueryValue}
			control, err := b.Query(ctx, a)
			if err != nil || len(control.Hits) == 0 {
				t.Fatalf("readable positive control: %+v %v", control, err)
			}
			otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
				var changed bool
				var err error
				if state == "write_only" {
					changed, err = store.MarkIndexWriteOnly(b.idx.Name)
				} else {
					changed, err = store.MarkIndexDisabled(b.idx.Name)
				}
				if err == nil && !changed {
					return errors.New("index state transition did not change readable state")
				}
				return err
			})
			otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
				want := recordlayer.IndexStateWriteOnly
				if state == "disabled" {
					want = recordlayer.IndexStateDisabled
				}
				if got := store.GetIndexState(b.idx.Name); got != want {
					return fmt.Errorf("persisted index state %v want %v", got, want)
				}
				return nil
			})
			_, err = b.Query(ctx, a)
			var ownership *OwnershipError
			if !errors.As(err, &ownership) {
				t.Fatalf("public Query accepted %s index: %T %v", state, err, err)
			}
		})
	}
}

func testOtelMalformedPostings(t *testing.T, ctx context.Context, b *FDBBackend) {
	a := Attribute{"span", "malformed_posting_probe", "string", "probe"}
	s := Span{Tenant: b.cfg.Tenant, Trace: strings.Repeat("e", 32), ID: strings.Repeat("e", 16), Start: 1700000000000000000, Attributes: []Attribute{a}}
	ss := termPrefix(otelSecondary(b), b.cfg.Tenant, a, b.cfg.HashBytes)
	if len(otelSnapshot(t, ctx, b, ss)) != 0 {
		t.Fatal("malformed-posting fixture range already occupied")
	}
	saveOtelSpan(t, ctx, b, s)
	defer otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		_, err := store.DeleteRecord(otelPK(s))
		return err
	})
	intact := otelSnapshot(t, ctx, b, ss)
	if len(intact) != 1 {
		t.Fatalf("malformed-posting setup: %d keys want 1", len(intact))
	}
	var key, value string
	for key, value = range intact {
		break
	}
	cases := []struct {
		name  string
		key   fdb.Key
		value []byte
	}{
		{"json", fdb.Key(key), []byte("[")},
		{"undecodable_suffix", append(append(fdb.Key(nil), ss.Bytes()...), 0x02, 'x'), []byte(value)},
		{"arity", ss.Pack(tuple.Tuple{int64(1)}), []byte(value)},
	}
	if b.cfg.Order == "time" {
		cases = append(cases,
			struct {
				name  string
				key   fdb.Key
				value []byte
			}{"trace_key_type", ss.Pack(tuple.Tuple{-s.Start, s.Trace, s.ID, int64(0)}), []byte(value)},
			struct {
				name  string
				key   fdb.Key
				value []byte
			}{"time_key_type", ss.Pack(tuple.Tuple{"not_an_integer", reverseTrace(s.Trace), s.ID, int64(0)}), []byte(value)},
			struct {
				name  string
				key   fdb.Key
				value []byte
			}{"time_value_mismatch", ss.Pack(tuple.Tuple{-s.Start - 1, reverseTrace(s.Trace), s.ID, int64(0)}), []byte(value)},
		)
	} else {
		cases = append(cases, struct {
			name  string
			key   fdb.Key
			value []byte
		}{"trace_key_type", ss.Pack(tuple.Tuple{[]byte(s.Trace), s.ID, int64(0)}), []byte(value)})
	}
	control := func(t *testing.T) {
		t.Helper()
		got, err := b.Query(ctx, a)
		if err != nil || !sameJSON(got.Hits, []Hit{{s.Trace, s.Start}}) {
			t.Fatalf("intact posting control: %+v %v", got, err)
		}
	}
	control(t)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
				rc.Transaction().Clear(fdb.Key(key))
				rc.Transaction().Set(test.key, test.value)
				return nil
			})
			defer otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
				rc.Transaction().Clear(test.key)
				rc.Transaction().Set(fdb.Key(key), []byte(value))
				return nil
			})
			_, err := b.Query(ctx, a)
			var corruption *CorruptionError
			if !errors.As(err, &corruption) {
				t.Fatalf("malformed posting should fail typed: %T %v", err, err)
			}
		})
		if !reflect.DeepEqual(intact, otelSnapshot(t, ctx, b, ss)) {
			t.Fatal("posting restoration differs")
		}
		control(t)
	}
}

func testOtelIdentityAdmission(t *testing.T, ctx context.Context, b *FDBBackend) {
	s := Generate(b.cfg, 0)
	before := otelSnapshot(t, ctx, b, otelSecondary(b))
	for _, field := range []string{"span_id", "start_ns"} {
		t.Run(field, func(t *testing.T) {
			m, err := toRecord(s)
			if err != nil {
				t.Fatal(err)
			}
			r := m.ProtoReflect()
			otlpField := r.Descriptor().Fields().ByName("otlp")
			var source tracepb.ResourceSpans
			if err := proto.Unmarshal(r.Get(otlpField).Bytes(), &source); err != nil {
				t.Fatal(err)
			}
			span := source.ScopeSpans[0].Spans[0]
			if field == "span_id" {
				span.SpanId[0] ^= 1
			} else {
				span.StartTimeUnixNano++
			}
			wire, err := proto.Marshal(&source)
			if err != nil {
				t.Fatal(err)
			}
			r.Set(otlpField, protoreflect.ValueOfBytes(wire))
			attempted := false
			_, err = b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := b.open(rc)
				if err != nil {
					return nil, err
				}
				attempted = true
				_, err = store.SaveRecord(m)
				return nil, err
			})
			var admission *AdmissionError
			if !attempted || !errors.As(err, &admission) {
				t.Fatalf("source %s mismatch SaveRecord: attempted=%v %T %v", field, attempted, err, err)
			}
			assertOtelSource(t, ctx, b, s)
			if !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, otelSecondary(b))) {
				t.Fatal("identity rejection changed exact old postings")
			}
		})
	}
}

func testOtelAudit(t *testing.T, ctx context.Context, b *FDBBackend) {
	calls := map[int]int{}
	b.auditPageHook = func(_ context.Context, _ *recordlayer.FDBRecordContext, page int) error {
		calls[page]++
		if page == 1 && calls[page] == 1 {
			return fdb.Error{Code: 1020}
		}
		return nil
	}
	t.Cleanup(func() { b.auditPageHook = nil })
	stats, err := b.Audit(ctx)
	b.auditPageHook = nil
	if err != nil || stats.Rows != 260 || stats.Attributes != 2080 || stats.Structures["eq_secondary"].Keys != 2080 {
		t.Fatalf("paginated audit after retry: %+v %v", stats, err)
	}
	if len(calls) != 3 || calls[0] != 1 || calls[1] != 2 || calls[2] != 1 {
		t.Fatalf("expected three pages and one retried second page, got %v", calls)
	}
	original := Generate(b.cfg, 0)
	changed := Generate(b.cfg, 0)
	changed.Attributes[0].Value = "corrupt_source"
	saveOtelSpan(t, ctx, b, changed)
	_, err = b.Audit(ctx)
	requireOtelKind(t, err, "audit")
	saveOtelSpan(t, ctx, b, original)
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		deleted, err := store.DeleteRecord(otelPK(original))
		if err == nil && !deleted {
			return errors.New("missing-source setup did not delete source")
		}
		return err
	})
	_, err = b.Audit(ctx)
	requireOtelKind(t, err, "audit")
	saveOtelSpan(t, ctx, b, original)
	postings := otelSnapshot(t, ctx, b, otelSecondary(b))
	if len(postings) != 2080 {
		t.Fatalf("missing-posting setup needs complete population: %d", len(postings))
	}
	var key, value string
	for key, value = range postings {
		break
	}
	otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
		rc.Transaction().Clear(fdb.Key(key))
		return nil
	})
	_, err = b.Audit(ctx)
	requireOtelKind(t, err, "audit")
	otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
		rc.Transaction().Set(fdb.Key(key), []byte(value))
		return nil
	})
	stats, err = b.Audit(ctx)
	if err != nil || stats.Rows != 260 || stats.Structures["eq_secondary"].Keys != 2080 {
		t.Fatalf("restored audit: %+v %v", stats, err)
	}
}

func testOtelOwnership(t *testing.T, ctx context.Context, b *FDBBackend, cluster string) {
	external := openOtelFixture(t, ctx, cluster, b.cfg, false)
	sentinel := external.root.Sub("external_sentinel").FDBKey()
	otelTransaction(t, ctx, external, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
		rc.Transaction().Set(sentinel, []byte("must survive unauthorized operations"))
		return nil
	})
	outside := otelSnapshot(t, ctx, external, external.root)
	writeManifest := func(m Manifest) {
		t.Helper()
		wire, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		otelTransaction(t, ctx, b, func(_ *recordlayer.FDBRecordStore, rc *recordlayer.FDBRecordContext) error {
			rc.Transaction().Set(b.root.Sub("owner").FDBKey(), wire)
			return nil
		})
	}
	original := manifest(b.cfg, "ready")
	defer writeManifest(original)
	operations := []struct {
		name string
		run  func(*FDBBackend) error
	}{
		{"Query", func(target *FDBBackend) error {
			_, err := target.Query(ctx, Attribute{"span", QueryKey, "string", QueryValue})
			return err
		}},
		{"Trace", func(target *FDBBackend) error { _, err := target.Trace(ctx, Generate(b.cfg, 0).Trace); return err }},
		{"Audit", func(target *FDBBackend) error { _, err := target.Audit(ctx); return err }},
		{"Drop", func(target *FDBBackend) error { return target.Drop(ctx) }},
	}
	mutations := []struct {
		name   string
		change func(*Manifest)
	}{
		{"tool", func(m *Manifest) { m.Tool += "_wrong" }},
		{"run", func(m *Manifest) { m.Run = freshRun(t) }},
		{"corpus", func(m *Manifest) { m.Corpus += "_wrong" }},
		{"order", func(m *Manifest) {
			if m.Order == "time" {
				m.Order = "trace"
			} else {
				m.Order = "time"
			}
		}},
		{"hash_bytes", func(m *Manifest) { m.HashBytes++ }},
		{"backend", func(m *Manifest) { m.Backend = "ch-attrs" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			bad := original
			mutation.change(&bad)
			writeManifest(bad)
			before := otelSnapshot(t, ctx, b, b.root)
			for _, op := range operations {
				t.Run(op.name, func(t *testing.T) { requireOtelKind(t, op.run(b), "ownership") })
			}
			if !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, b.root)) || !reflect.DeepEqual(outside, otelSnapshot(t, ctx, external, external.root)) {
				t.Fatal("unauthorized operation mutated owned data or external sentinel")
			}
			writeManifest(original)
		})
	}
	loading := original
	loading.State = "loading"
	writeManifest(loading)
	for _, op := range operations[:3] {
		requireOtelKind(t, op.run(b), "ownership")
	}
	writeManifest(original)
	for _, op := range operations {
		t.Run("unowned_"+op.name, func(t *testing.T) { requireOtelKind(t, op.run(external), "ownership") })
	}
	if !reflect.DeepEqual(outside, otelSnapshot(t, ctx, external, external.root)) {
		t.Fatal("unowned run or external sentinel changed")
	}
}

func testOtelScopedQueries(t *testing.T, ctx context.Context, b *FDBBackend) {
	predicates := []Attribute{
		{"span", QueryKey, "string", QueryValue},
		{"span", QueryKey, "int", QueryValue},
		{"span", QueryKey + ".int", "int", QueryValue},
		{"resource", QueryKey, "string", QueryValue},
		{"scope", QueryKey, "string", QueryValue},
	}
	for _, a := range predicates {
		want := Expected(b.cfg, a.Scope, a.Key, a.Type, a.Value)
		if len(want) == 0 {
			t.Fatalf("vacuous positive control: %+v", a)
		}
		got, err := b.Query(ctx, a)
		if err != nil || !sameJSON(got.Hits, want) {
			t.Fatalf("typed/scoped positive %+v: %+v want %+v: %v", a, got, want, err)
		}
	}
	for _, a := range []Attribute{
		{"span", QueryKey + ".int", "string", QueryValue},
		{"resource", QueryKey, "int", QueryValue},
		{"scope", QueryKey, "int", QueryValue},
		{"scope", QueryKey, "string", "definitely_absent"},
	} {
		got, err := b.Query(ctx, a)
		if err != nil || len(got.Hits) != 0 {
			t.Fatalf("typed/scoped negative %+v: %+v %v", a, got, err)
		}
	}
}

func testOtelOrderAndBudgets(t *testing.T, ctx context.Context, b *FDBBackend) {
	cfg := b.cfg
	defer func() { b.cfg = cfg }()
	predicate := Attribute{"span", "literal_order_probe", "string", "yes"}
	const timestamp int64 = 1700000000000000000
	spans := []Span{
		{Tenant: cfg.Tenant, Trace: "00000000000000000000000000000001", ID: "0000000000000001", Start: timestamp},
		{Tenant: cfg.Tenant, Trace: "00000000000000000000000000000002", ID: "0000000000000001", Start: timestamp},
		{Tenant: cfg.Tenant, Trace: "00000000000000000000000000000003", ID: "0000000000000001", Start: timestamp},
		{Tenant: cfg.Tenant, Trace: "00000000000000000000000000000004", ID: "0000000000000001", Start: timestamp},
		{Tenant: cfg.Tenant, Trace: "00000000000000000000000000000004", ID: "0000000000000004", Start: timestamp + 2},
		{Tenant: cfg.Tenant, Trace: "00000000000000000000000000000004", ID: "0000000000000003", Start: timestamp + 1},
		{Tenant: cfg.Tenant, Trace: "00000000000000000000000000000004", ID: "0000000000000002", Start: timestamp + 1},
		{Tenant: cfg.Tenant, Trace: "00000000000000000000000000000005", ID: "0000000000000001", Start: timestamp},
	}
	for i := range spans {
		spans[i].Attributes = []Attribute{predicate}
		saveOtelSpan(t, ctx, b, spans[i])
	}
	defer otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		for _, s := range spans {
			deleted, err := store.DeleteRecord(otelPK(s))
			if err != nil {
				return err
			}
			if !deleted {
				return errors.New("literal fixture cleanup missed source")
			}
		}
		return nil
	})
	want := []Hit{{"00000000000000000000000000000004", timestamp + 2}, {"00000000000000000000000000000005", timestamp}, {"00000000000000000000000000000003", timestamp}}
	got, err := b.Query(ctx, predicate)
	if err != nil || !sameJSON(got.Hits, want) {
		t.Fatalf("literal top-K tie boundary: %+v want %+v: %v", got, want, err)
	}
	// This full candidate census is independent of the scanner's work receipt.
	all := otelSnapshot(t, ctx, b, termPrefix(otelSecondary(b), cfg.Tenant, predicate, cfg.HashBytes))
	if len(all) != 8 {
		t.Fatalf("candidate population (including shortened-hash collisions) = %d want 8", len(all))
	}
	exhaustiveBytes := int64(0)
	for key, value := range all {
		exhaustiveBytes += int64(len(key) + len(value))
	}
	if cfg.Order == "time" {
		if got.Candidates != 6 || got.Bytes <= 0 || got.Bytes >= exhaustiveBytes {
			t.Fatalf("time frontier must consume exactly six of eight postings: %+v exhaustive_bytes=%d", got, exhaustiveBytes)
		}
	} else if got.Candidates != 8 || got.Bytes != exhaustiveBytes {
		t.Fatalf("trace layout must exhaust eight candidates: %+v bytes=%d", got, exhaustiveBytes)
	}
	baseline := got
	b.cfg.MaxCandidates = baseline.Candidates - 1
	_, err = b.Query(ctx, predicate)
	requireOtelBudget(t, err)
	b.cfg.MaxCandidates = baseline.Candidates
	got, err = b.Query(ctx, predicate)
	if err != nil || !sameJSON(got, baseline) {
		t.Fatalf("exact candidate budget: %+v %v", got, err)
	}
	b.cfg.MaxBytes = baseline.Bytes - 1
	_, err = b.Query(ctx, predicate)
	requireOtelBudget(t, err)
	b.cfg.MaxBytes = baseline.Bytes
	got, err = b.Query(ctx, predicate)
	if err != nil || !sameJSON(got, baseline) {
		t.Fatalf("exact byte budget: %+v %v", got, err)
	}
	b.cfg = cfg
	trace := spans[3].Trace
	wantSpans := []SpanHit{{"0000000000000001", timestamp}, {"0000000000000002", timestamp + 1}, {"0000000000000003", timestamp + 1}, {"0000000000000004", timestamp + 2}}
	traced, err := b.Trace(ctx, trace)
	if err != nil || !sameJSON(traced, wantSpans) {
		t.Fatalf("literal trace order: %+v want %+v: %v", traced, wantSpans, err)
	}
	work := b.LastQueryWork()
	var sourceBytes int64
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		sourceBytes = 0
		for _, s := range spans {
			if s.Trace != trace {
				continue
			}
			record, err := store.LoadRecord(otelPK(s))
			if err != nil {
				return err
			}
			if record == nil {
				return errors.New("trace source missing")
			}
			sourceBytes += int64(record.KeySize + record.ValueSize)
		}
		return nil
	})
	if work.Candidates != 4 || work.Bytes != sourceBytes || sourceBytes <= 0 {
		t.Fatalf("trace work %+v want four rows/%d bytes", work, sourceBytes)
	}
	b.cfg.MaxCandidates = 3
	_, err = b.Trace(ctx, trace)
	requireOtelBudget(t, err)
	b.cfg.MaxCandidates = 4
	traced, err = b.Trace(ctx, trace)
	if err != nil || !sameJSON(traced, wantSpans) {
		t.Fatalf("exact trace row budget: %+v %v", traced, err)
	}
	b.cfg.MaxBytes = 1
	_, err = b.Trace(ctx, trace)
	requireOtelBudget(t, err)
	b.cfg.MaxBytes = sourceBytes - 1
	_, err = b.Trace(ctx, trace)
	requireOtelBudget(t, err)
	if work := b.LastQueryWork(); work.Candidates != 4 || work.Bytes != sourceBytes {
		t.Fatalf("final source must cross remaining byte budget: %+v want 4/%d", work, sourceBytes)
	}
	b.cfg.MaxBytes = sourceBytes
	traced, err = b.Trace(ctx, trace)
	if err != nil || !sameJSON(traced, wantSpans) || b.LastQueryWork().Bytes != sourceBytes {
		t.Fatalf("exact trace byte budget: %+v %+v %v", traced, b.LastQueryWork(), err)
	}
}

func testOtelMutations(t *testing.T, ctx context.Context, b *FDBBackend) {
	c := b.cfg
	original := Generate(c, 0)
	before := otelSnapshot(t, ctx, b, otelSecondary(b))
	rejected := Generate(c, 0)
	rejected.Attributes[0].Value = "abort_only"
	abort := errors.New("intentional transaction abort")
	saved := false
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		saved = false
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		m, err := toRecord(rejected)
		if err != nil {
			return nil, err
		}
		if _, err := store.SaveRecord(m); err != nil {
			return nil, err
		}
		saved = true
		return nil, abort
	})
	if !errors.Is(err, abort) || !saved {
		t.Fatalf("save did not complete before deliberate abort: saved=%v err=%v", saved, err)
	}
	assertOtelSource(t, ctx, b, original)
	if !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, otelSecondary(b))) {
		t.Fatal("abort changed exact old posting keys/values")
	}
	got, err := b.Query(ctx, Attribute{"span", QueryKey, "string", "abort_only"})
	if err != nil || len(got.Hits) != 0 {
		t.Fatalf("aborted posting visible: %+v %v", got, err)
	}
	collisionSpan := Generate(c, 0)
	collisionAttribute := Attribute{"span", collision(QueryKey), "string", collision(QueryValue)}
	collisionSpan.Attributes = append(collisionSpan.Attributes, collisionAttribute)
	saveOtelSpan(t, ctx, b, collisionSpan)
	got, err = b.Query(ctx, Attribute{"span", QueryKey, "string", QueryValue})
	if err != nil || !sameJSON(got.Hits, Expected(c, "span", QueryKey, "string", QueryValue)) {
		t.Fatalf("collision changed exact matches: %+v %v", got, err)
	}
	got, err = b.Query(ctx, collisionAttribute)
	if err != nil || !sameJSON(got.Hits, []Hit{{collisionSpan.Trace, collisionSpan.Start}}) {
		t.Fatalf("collision owner not independently queryable: %+v %v", got, err)
	}
	// A committed replacement changes value, removes a term, and moves time keys.
	corrected := collisionSpan
	corrected.Attributes = append([]Attribute(nil), collisionSpan.Attributes...)
	corrected.Attributes[0].Value = "committed_replacement"
	removed := corrected.Attributes[4]
	corrected.Attributes = append(corrected.Attributes[:4], corrected.Attributes[5:]...)
	corrected.Start += 100000000000000
	saveOtelSpan(t, ctx, b, corrected)
	assertOtelSource(t, ctx, b, corrected)
	for _, a := range []Attribute{original.Attributes[0], removed, corrected.Attributes[0], collisionAttribute} {
		want := otelExpectedAfter(c, a, map[int]*Span{0: &corrected}, c.K)
		got, err := b.Query(ctx, a)
		if err != nil || !sameJSON(got.Hits, want) {
			t.Fatalf("committed correction query %+v: %+v want %+v: %v", a, got, want, err)
		}
	}
	if size, err := b.size(ctx, otelSecondary(b)); err != nil || size.Keys != int64(c.Count*c.Attributes) {
		t.Fatalf("correction left stale keys: %+v %v", size, err)
	}
	// Restoring also verifies the inverse timestamp/value/removal transition.
	saveOtelSpan(t, ctx, b, original)
	if !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, otelSecondary(b))) {
		t.Fatal("correction/restore left stale old or replacement postings")
	}
	saveOtelSpan(t, ctx, b, collisionSpan)
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		old, err := store.LoadRecord(otelPK(collisionSpan))
		if err != nil {
			return err
		}
		if old == nil {
			return errors.New("WRITE_ONLY source missing")
		}
		maintainer, err := store.GetIndexMaintainer(b.idx)
		if err != nil {
			return err
		}
		if err := maintainer.UpdateWhileWriteOnly(old, nil); err != nil {
			return err
		}
		scanner, ok := maintainer.(*postingMaintainer)
		if !ok {
			return errors.New("wrong custom maintainer")
		}
		q, err := scanner.Query(c, collisionAttribute)
		if err != nil {
			return err
		}
		if len(q.Hits) != 0 {
			return errors.New("WRITE_ONLY override did not clear custom posting")
		}
		if err := maintainer.UpdateWhileWriteOnly(nil, old); err != nil {
			return err
		}
		err = store.DeleteRecordsWhere(tuple.Tuple{int64(c.Tenant)})
		if err == nil {
			return errors.New("unsafe prefix delete accepted")
		}
		still, err := store.LoadRecord(old.PrimaryKey)
		if err != nil {
			return err
		}
		if still == nil {
			return errors.New("rejected delete mutated record")
		}
		return nil
	})
	assertOtelSource(t, ctx, b, collisionSpan)
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		deleted, err := store.DeleteRecord(otelPK(collisionSpan))
		if err == nil && !deleted {
			return errors.New("delete missed source")
		}
		return err
	})
	b.cfg.K = 100
	defer func() { b.cfg = c }()
	got, err = b.Query(ctx, Attribute{"span", QueryKey, "string", QueryValue})
	want := otelExpectedAfter(c, Attribute{"span", QueryKey, "string", QueryValue}, map[int]*Span{0: nil}, 100)
	if err != nil || !sameJSON(got.Hits, want) {
		t.Fatalf("delete lost another span's membership: %+v want %+v: %v", got, want, err)
	}
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		return store.DeleteAllRecords()
	})
	got, err = b.Query(ctx, Attribute{"span", QueryKey, "string", QueryValue})
	if err != nil || len(got.Hits) != 0 {
		t.Fatalf("DeleteAllRecords left secondary postings: %+v %v", got, err)
	}
	if n, err := b.size(ctx, otelSecondary(b)); err != nil || n.Keys != 0 {
		t.Fatalf("secondary cleanup: %+v %v", n, err)
	}
}

func otelExpectedAfter(c Config, a Attribute, replacements map[int]*Span, k int) []Hit {
	latest := map[string]int64{}
	for i := 0; i < c.Count; i++ {
		s := Generate(c, i)
		if replacement, ok := replacements[i]; ok {
			if replacement == nil {
				continue
			}
			s = *replacement
		}
		if s.Tenant != c.Tenant {
			continue
		}
		for _, attribute := range s.Attributes {
			if attribute == a {
				if previous, ok := latest[s.Trace]; !ok || s.Start > previous {
					latest[s.Trace] = s.Start
				}
				break
			}
		}
	}
	return oracleTopK(latest, k)
}

func testOtelAdmission(t *testing.T, ctx context.Context, b *FDBBackend) {
	original := Generate(b.cfg, 0)
	atCap := Generate(b.cfg, 0)
	atCap.Attributes[0] = Attribute{"span", strings.Repeat("k", 1024), "string", strings.Repeat("v", 64000)}
	saveOtelSpan(t, ctx, b, atCap)
	assertOtelSource(t, ctx, b, atCap)
	got, err := b.Query(ctx, atCap.Attributes[0])
	if err != nil || !sameJSON(got.Hits, []Hit{{atCap.Trace, atCap.Start}}) {
		t.Fatalf("at-cap membership: %+v %v", got, err)
	}
	for _, boundary := range []string{"key", "value"} {
		t.Run("over_"+boundary, func(t *testing.T) {
			before := otelSnapshot(t, ctx, b, otelSecondary(b))
			over := atCap
			over.Attributes = append([]Attribute(nil), atCap.Attributes...)
			if boundary == "key" {
				over.Attributes[0].Key += "x"
			} else {
				over.Attributes[0].Value += "x"
			}
			m, err := toRecord(over)
			if err != nil {
				t.Fatal(err)
			}
			attempted := false
			_, err = b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := b.open(rc)
				if err != nil {
					return nil, err
				}
				attempted = true
				_, err = store.SaveRecord(m)
				return nil, err
			})
			var admission *AdmissionError
			if !attempted || !errors.As(err, &admission) {
				t.Fatalf("over-cap SaveRecord should reach admission rejection: attempted=%v %T %v", attempted, err, err)
			}
			assertOtelSource(t, ctx, b, atCap)
			if !reflect.DeepEqual(before, otelSnapshot(t, ctx, b, otelSecondary(b))) {
				t.Fatal("failed admission changed exact source memberships")
			}
			got, err := b.Query(ctx, atCap.Attributes[0])
			if err != nil || !sameJSON(got.Hits, []Hit{{atCap.Trace, atCap.Start}}) {
				t.Fatalf("failed admission lost old posting: %+v %v", got, err)
			}
		})
	}
	saveOtelSpan(t, ctx, b, original)
	stats, err := b.Audit(ctx)
	if err != nil || stats.Rows != 9 || stats.Structures["eq_secondary"].Keys != 1152 {
		t.Fatalf("admission restoration census: %+v %v", stats, err)
	}
}
