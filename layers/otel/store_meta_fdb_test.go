package otel

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// smHoldTransactor holds the first execution of one transaction, the one
// after skip others, after its body succeeded and before its commit, until
// released.
type smHoldTransactor struct {
	fdb.Database
	held, release chan struct{}
	skip          int32
	calls         atomic.Int32
	executions    atomic.Int32 // of the held transaction
}

func (g *smHoldTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return g.TransactCtx(context.Background(), fn)
}

func (g *smHoldTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if g.calls.Add(1) != g.skip+1 {
		return g.Database.TransactCtx(ctx, fn)
	}
	return g.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		out, err := fn(tx)
		if g.executions.Add(1) == 1 && err == nil {
			close(g.held)
			<-g.release
		}
		return out, err
	})
}

func TestStoreMetaRealFDB(t *testing.T) {
	t.Parallel()
	h := newQRHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	open := func() *tenantStore {
		t.Helper()
		return h.open(t, ctx, nil).store
	}
	load := func(s *tenantStore, name string) (otelsql.StoreMeta, bool) {
		t.Helper()
		var row otelsql.StoreMeta
		var ok bool
		if err := s.read(ctx, "test", func(store *recordlayer.FDBRecordStore) error {
			var err error
			row, ok, err = loadStoreMeta(store, name)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return row, ok
	}

	t.Run("existing_value_wins", func(t *testing.T) {
		s := open()
		if _, ok := load(s, "probe"); ok {
			t.Fatal("a fresh store has a probe row")
		}
		a, err := publishStoreMeta(ctx, s, "probe", otelsql.StoreMeta{Bytes: []byte("a"), Int: proto.Int64(1)})
		if err != nil || string(a.Bytes) != "a" || a.Int == nil || *a.Int != 1 || a.Str != nil || a.Name != "probe" {
			t.Fatalf("first publish: %+v %v", a, err)
		}
		b, err := publishStoreMeta(ctx, s, "probe", otelsql.StoreMeta{Bytes: []byte("b"), Str: proto.String("b")})
		if err != nil || string(b.Bytes) != "a" || b.Str != nil {
			t.Fatalf("second publish replaced the row: %+v %v", b, err)
		}
		if row, ok := load(s, "probe"); !ok || string(row.Bytes) != "a" || *row.Int != 1 {
			t.Fatalf("stored row %+v %v", row, ok)
		}
		// In-transaction saves replace, as a counter's do.
		if err := s.run(ctx, "test", func(store *recordlayer.FDBRecordStore) error {
			return saveStoreMeta(store, otelsql.StoreMeta{Name: "probe", Int: proto.Int64(2)})
		}); err != nil {
			t.Fatal(err)
		}
		if row, ok := load(s, "probe"); !ok || row.Bytes != nil || *row.Int != 2 {
			t.Fatalf("replaced row %+v %v", row, ok)
		}
	})

	// Two publishers race: the first has saved its candidate and waits before
	// its commit while the second commits; the first's commit conflicts on the
	// row it read, and its retry returns the winner.
	t.Run("concurrent_winner_wins", func(t *testing.T) {
		s := open()
		gate := &smHoldTransactor{Database: h.raw, held: make(chan struct{}), release: make(chan struct{})}
		held := *s
		held.db = recordlayer.NewFDBDatabaseWithTransactor(gate, h.raw)
		var loser otelsql.StoreMeta
		var loserErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			loser, loserErr = publishStoreMeta(ctx, &held, "race", otelsql.StoreMeta{Bytes: []byte("loser")})
		}()
		select {
		case <-gate.held:
		case <-ctx.Done():
			t.Fatal("the first publisher never reached its commit")
		}
		winner, err := publishStoreMeta(ctx, s, "race", otelsql.StoreMeta{Bytes: []byte("winner")})
		close(gate.release)
		wg.Wait()
		if err != nil || loserErr != nil || string(winner.Bytes) != "winner" || string(loser.Bytes) != "winner" {
			t.Fatalf("race: winner %q (%v), loser got %q (%v)", winner.Bytes, err, loser.Bytes, loserErr)
		}
		if n := gate.executions.Load(); n != 2 {
			t.Fatalf("the held publisher ran %d times; want a conflict and one retry", n)
		}
		if row, _ := load(s, "race"); string(row.Bytes) != "winner" {
			t.Fatalf("stored %q", row.Bytes)
		}
	})

	t.Run("layout", func(t *testing.T) {
		s := open()
		if err := checkStoreLayout(ctx, s, "messages=none"); err != nil {
			t.Fatal(err)
		}
		if err := checkStoreLayout(ctx, s, "messages=none"); err != nil {
			t.Fatalf("same layout refused: %v", err)
		}
		requireOtelKind(t, checkStoreLayout(ctx, s, "messages=deltas"), "ownership")
		if row, ok := load(s, otelsql.StoreMetaLayout); !ok || row.Str == nil || *row.Str != "messages=none" {
			t.Fatalf("layout row %+v %v", row, ok)
		}
		// A layout row without a string value is refused as well.
		other := open()
		if _, err := publishStoreMeta(ctx, other, otelsql.StoreMetaLayout, otelsql.StoreMeta{Int: proto.Int64(1)}); err != nil {
			t.Fatal(err)
		}
		requireOtelKind(t, checkStoreLayout(ctx, other, "messages=none"), "ownership")
	})
}

func TestRelStoreLayout(t *testing.T) {
	t.Parallel()
	for want, cfg := range map[string]ExporterConfig{
		"messages=none":     {},
		"messages=deltas":   {EnableMessageDeltas: true},
		"messages=fulltext": {EnableMessageDeltas: true, EnableMessageFullText: true},
	} {
		if got := relStoreLayout(cfg); got != want {
			t.Fatalf("%+v: layout %q, want %q", cfg, got, want)
		}
	}
	if relStoreLayout(ExporterConfig{EnableMessageFullText: true}) != "messages=fulltext" {
		t.Fatal("full text implies deltas")
	}
}
