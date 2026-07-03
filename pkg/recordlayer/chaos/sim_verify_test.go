package chaos

import (
	"context"
	mrand "math/rand/v2"
	"sort"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
)

// TestSimFDB_RunRandomVerify is RFC-179 Tier 2's first deliverable and a Tier 1 fidelity
// validation in one: a serial, seed-reproducible save/overwrite/delete workload runs against
// SimFDB (no Docker) and is checked by the chaos Verify() oracle (14 store<->model invariant
// families). The MAX_EVER + record-count metadata exercises the COUNT, VALUE, MAX_EVER, and
// record-integrity invariants — so a clean pass proves SimFDB correctly maintains the record
// layer's derived state end-to-end, single-goroutine, from one seed.
func TestSimFDB_RunRandomVerify(t *testing.T) {
	t.Parallel()
	const seed = 7
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier() // clean run: proves correctness, not fault-tolerance
	db := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	md := buildMaxEverMetadata()
	sub := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	model := NewStoreModel(md)
	rng := mrand.New(mrand.NewPCG(seed, 0))
	ctx := context.Background()

	open := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
		return recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
	}
	run := func(fn func(*recordlayer.FDBRecordStore) error) error {
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := open(rtx)
			if err != nil {
				return nil, err
			}
			return nil, fn(store)
		})
		return err
	}
	verify := func(op int) {
		if err := run(func(store *recordlayer.FDBRecordStore) error {
			if v := Verify(store, model); len(v) > 0 {
				t.Fatalf("SimFDB Verify violation at op %d (seed=%d): %+v", op, seed, v)
			}
			return nil
		}); err != nil {
			t.Fatalf("verify at op %d: %v", op, err)
		}
	}
	order := func(pk int64) *gen.Order {
		return &gen.Order{
			OrderId:  proto.Int64(pk),
			Price:    proto.Int32(rng.Int32N(10000)),
			Quantity: proto.Int32(rng.Int32N(100)),
		}
	}

	var alive []int64 // sorted set of currently-live PKs
	nextPK := int64(0)
	const maxPKs = 30
	const numOps = 250
	for i := 0; i < numOps; i++ {
		switch roll := rng.IntN(10); {
		case roll < 5 || len(alive) == 0: // save new (may wrap to an existing PK → overwrite)
			pk := nextPK % maxPKs
			nextPK++
			o := order(pk)
			if err := run(func(s *recordlayer.FDBRecordStore) error { _, e := s.SaveRecord(o); return e }); err != nil {
				t.Fatalf("save op %d (seed=%d): %v", i, seed, err)
			}
			model.Save(o)
			alive = insertPK(alive, pk)
		case roll < 8: // overwrite an existing PK
			pk := alive[rng.IntN(len(alive))]
			o := order(pk)
			if err := run(func(s *recordlayer.FDBRecordStore) error { _, e := s.SaveRecord(o); return e }); err != nil {
				t.Fatalf("overwrite op %d: %v", i, err)
			}
			model.Save(o)
		default: // delete an existing PK
			idx := rng.IntN(len(alive))
			pk := alive[idx]
			if err := run(func(s *recordlayer.FDBRecordStore) error { _, e := s.DeleteRecord(tuple.Tuple{pk}); return e }); err != nil {
				t.Fatalf("delete op %d: %v", i, err)
			}
			model.Delete(tuple.Tuple{pk})
			alive = append(alive[:idx], alive[idx+1:]...)
		}
		if i%25 == 24 {
			verify(i)
		}
	}
	verify(numOps)
	if len(alive) == 0 {
		t.Fatal("workload left no live records — op mix produced nothing to verify")
	}
}

// insertPK adds pk to the sorted set a (no-op if already present).
func insertPK(a []int64, pk int64) []int64 {
	i := sort.Search(len(a), func(i int) bool { return a[i] >= pk })
	if i < len(a) && a[i] == pk {
		return a
	}
	a = append(a, 0)
	copy(a[i+1:], a[i:])
	a[i] = pk
	return a
}
