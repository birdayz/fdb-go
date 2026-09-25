package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

// restoreEnv is a catalog, a keyspace its schemas' stores live under, and the
// template name the test restores.
type restoreEnv struct {
	t    *testing.T
	cat  *RecordLayerStoreCatalog
	run  func(fn func(txn api.Transaction) error) error
	ks   *keyspace.RelationalKeyspace
	name string
}

func newRestoreEnv(t *testing.T) *restoreEnv {
	t.Helper()
	cat, run := newFDBCatalogInSubspace(t)
	return &restoreEnv{t: t, cat: cat, run: run, ks: keyspace.New(subspace.Sub("restore-ks", t.Name())), name: "r"}
}

// demoMetaData is the demo records' MetaData at metadata version mdVersion,
// with mutate applied, marshalled: a template's stored META_DATA.
func demoMetaData(t *testing.T, mdVersion int32, mutate func(*gen.MetaData)) []byte {
	t.Helper()
	p, err := buildVersionedTemplate(t, "x", 1).(*metadata.RecordLayerSchemaTemplate).Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	p.Version = proto.Int32(mdVersion)
	if mutate != nil {
		mutate(p)
	}
	b, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withPriceIndex adds a value index on Order.price at the given versions, its
// predicate pred when non-nil.
func withPriceIndex(added, lastModified int32, root recordlayer.KeyExpression, pred *gen.Predicate) func(*gen.MetaData) {
	return func(p *gen.MetaData) {
		p.Indexes = append(p.Indexes, &gen.Index{
			Name:                proto.String("Order$price"),
			RecordType:          []string{"Order"},
			RootExpression:      root.ToKeyExpression(),
			Type:                proto.String(recordlayer.IndexTypeValue),
			SubspaceKey:         tuple.Tuple{"Order$price"}.Pack(),
			AddedVersion:        proto.Int32(added),
			LastModifiedVersion: proto.Int32(lastModified),
			Predicate:           pred,
		})
	}
}

func priceAbove(n int32) *gen.Predicate {
	return &gen.Predicate{ValuePredicate: &gen.ValuePredicate{
		Value: []string{"price"},
		Comparison: &gen.Comparison{SimpleComparison: &gen.SimpleComparison{
			Type: gen.ComparisonType_GREATER_THAN.Enum(), Operand: &gen.Value{IntValue: proto.Int32(n)},
		}},
	}}
}

// writeRow writes a template row raw, as the F11 copy and the fixtures do,
// past the version guard.
func (e *restoreEnv) writeRow(version int, md []byte) {
	e.t.Helper()
	mustRun(e.t, e.run, func(tx api.Transaction) error {
		store, err := e.cat.openStore(tx)
		if err != nil {
			return err
		}
		_, err = store.SaveRecord(&gen.Templates{
			TEMPLATE_NAME: proto.String(e.name), TEMPLATE_VERSION: proto.Int32(int32(version)), META_DATA: md,
		})
		return err
	})
}

// storedRow is the stored META_DATA of (name, version), nil when absent.
func (e *restoreEnv) storedRow(version int) []byte {
	e.t.Helper()
	var out []byte
	mustRun(e.t, e.run, func(tx api.Transaction) error {
		store, err := e.cat.openStore(tx)
		if err != nil {
			return err
		}
		rec, err := store.LoadRecord(templateKeyAtVersion(e.name, version))
		if err != nil || rec == nil {
			return err
		}
		out = rec.Record.(*gen.Templates).GetMETA_DATA()
		return nil
	})
	return out
}

// bind binds db/schema to (name, version), and when md is not nil opens the
// schema's store under md in the keyspace (so it has a header) and saves one
// Order.
func (e *restoreEnv) bind(db, schema string, version int, md []byte) {
	e.t.Helper()
	mustRun(e.t, e.run, func(tx api.Transaction) error {
		tmpl, err := e.cat.SchemaTemplateCatalog().LoadSchemaTemplateAtVersion(tx, e.name, version)
		if err != nil {
			return err
		}
		return e.cat.SaveSchema(tx, tmpl.GenerateSchema(db, schema), true)
	})
	if md != nil {
		e.openStore(db, schema, md, func(store *recordlayer.FDBRecordStore) error {
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)})
			return err
		})
	}
}

// openStore opens db/schema's store under md and runs fn on it.
func (e *restoreEnv) openStore(db, schema string, md []byte, fn func(*recordlayer.FDBRecordStore) error) {
	e.t.Helper()
	p := &gen.MetaData{}
	if err := proto.Unmarshal(md, p); err != nil {
		e.t.Fatal(err)
	}
	rmd, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		e.t.Fatal(err)
	}
	ss, err := e.ks.SchemaSubspace(db, schema)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := testFDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(rmd).SetSubspace(ss).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		return nil, fn(store)
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *restoreEnv) dropTemplate() {
	e.t.Helper()
	mustRun(e.t, e.run, func(tx api.Transaction) error { return e.cat.SchemaTemplateCatalog().DeleteTemplate(tx, e.name, true) })
}

func (e *restoreEnv) restore(version int, md []byte, opts *restoreOptions) error {
	e.t.Helper()
	if opts == nil {
		opts = &restoreOptions{batch: restoreHeaderBatch}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return e.cat.restoreTemplateVersion(ctx, testFDB, e.ks, e.name, version, md, opts)
}

// readsBack loads db/schema through the catalog and reads its Order 1 through
// the store opened under the bound template.
func (e *restoreEnv) readsBack(db, schema string) {
	e.t.Helper()
	var md []byte
	mustRun(e.t, e.run, func(tx api.Transaction) error {
		s, err := e.cat.LoadSchema(tx, db, schema)
		if err != nil {
			return err
		}
		p, err := e.cat.SchemaTemplateCatalog().LoadTemplateProto(tx, e.name, s.SchemaTemplate().Version())
		if err != nil {
			return err
		}
		md, err = proto.Marshal(p)
		return err
	})
	e.openStore(db, schema, md, func(store *recordlayer.FDBRecordStore) error {
		rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
		if err != nil {
			return err
		}
		if rec == nil {
			e.t.Fatalf("%s/%s lost its Order", db, schema)
		}
		return nil
	})
}

func wantRestoreRefused(t *testing.T, err error, code api.ErrorCode, message string) {
	t.Helper()
	wantAPIError(t, err, code, message)
}

// The dropped version comes back from its stored bytes, and its schema loads,
// opens and reads its rows again.
func TestFDB_Restore_DroppedBoundVersion(t *testing.T) {
	t.Parallel()
	e := newRestoreEnv(t)
	v1 := demoMetaData(t, 3, withPriceIndex(2, 3, recordlayer.Field("price"), nil))
	e.writeRow(1, v1)
	e.bind("/db", "s", 1, v1)
	e.dropTemplate()
	err := e.run(func(tx api.Transaction) error { _, err := e.cat.LoadSchema(tx, "/db", "s"); return err })
	wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, "SchemaTemplate=r, version=1 is not in catalog")

	if err := e.restore(1, v1, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.storedRow(1); string(got) != string(v1) {
		t.Fatal("the restored row is not the backup's bytes")
	}
	e.readsBack("/db", "s")
}

// A stored version is not overwritten; a version nothing binds is not
// restored.
func TestFDB_Restore_RefusesStoredOrUnboundVersion(t *testing.T) {
	t.Parallel()
	e := newRestoreEnv(t)
	v1 := demoMetaData(t, 1, nil)
	e.writeRow(1, v1)
	e.bind("/db", "s", 1, v1)
	wantRestoreRefused(t, e.restore(1, v1, nil), api.ErrCodeDuplicateSchemaTemplate,
		"schema template r version 1 cannot be restored: it is stored")

	mustRun(t, e.run, func(tx api.Transaction) error { return e.cat.DeleteSchema(tx, "/db", "s") })
	e.dropTemplate()
	wantRestoreRefused(t, e.restore(1, v1, nil), api.ErrCodeInvalidSchemaTemplate,
		"schema template r version 1 cannot be restored: no schema binds it")
	if e.storedRow(1) != nil {
		t.Fatal("a refused restore wrote the row")
	}
}

// Every bound store's header is read, a batch at a time: a store with no
// header in the keyspace, or with a header above the restored metadata's
// version, refuses; one below is a schema rebound and not opened since.
func TestFDB_Restore_ReadsEveryBoundHeader(t *testing.T) {
	t.Parallel()
	e := newRestoreEnv(t)
	v1 := demoMetaData(t, 2, nil)
	e.writeRow(1, v1)
	above := demoMetaData(t, 5, nil)
	e.bind("/db", "a", 1, v1)
	e.bind("/db", "b", 1, nil) // no store
	e.bind("/db", "c", 1, v1)
	e.openStore("/db", "c", above, func(*recordlayer.FDBRecordStore) error { return nil })
	e.dropTemplate()

	one := &restoreOptions{batch: 1}
	wantRestoreRefused(t, e.restore(1, v1, one), api.ErrCodeInvalidSchemaTemplate,
		"schema template r version 1 cannot be restored: schema /db/b has no store header in the keyspace")
	e.openStore("/db", "b", v1, func(*recordlayer.FDBRecordStore) error { return nil })
	wantRestoreRefused(t, e.restore(1, v1, one), api.ErrCodeInvalidSchemaTemplate,
		"schema template r version 1 cannot be restored: the store of schema /db/c records metadata version 5, above the restored metadata's 2")
	if one.attempts != 0 {
		t.Fatalf("a header refusal ran %d restoring transactions", one.attempts)
	}

	// A header below the restored metadata's: bound to v1 and never opened
	// under the version it was rebound to.
	below := newRestoreEnv(t)
	below.name = "below"
	b1, b2 := demoMetaData(t, 1, nil), demoMetaData(t, 3, withPriceIndex(3, 3, recordlayer.Field("price"), nil))
	below.writeRow(1, b1)
	below.bind("/db", "s", 1, b1)
	below.writeRow(2, b2)
	mustRun(t, below.run, func(tx api.Transaction) error { return below.cat.RepairSchema(tx, "/db", "s") })
	below.dropTemplate()
	if err := below.restore(2, b2, nil); err != nil {
		t.Fatal(err)
	}
	below.readsBack("/db", "s")
}

// The restored bytes must be one history with every stored version.
func TestFDB_Restore_OneHistory(t *testing.T) {
	t.Parallel()
	base := func(t *testing.T) (*restoreEnv, []byte) {
		e := newRestoreEnv(t)
		v3 := demoMetaData(t, 5, withPriceIndex(2, 5, recordlayer.Field("price"), priceAbove(1)))
		e.writeRow(3, v3)
		e.bind("/db", "s", 3, v3)
		e.dropTemplate()
		return e, v3
	}
	for _, c := range []struct {
		name string
		v1   func(t *testing.T) []byte
		want string // "" admits
	}{
		{"agrees in every check", func(t *testing.T) []byte {
			return demoMetaData(t, 5, withPriceIndex(2, 5, recordlayer.Field("price"), priceAbove(1)))
		}, ""},
		{"an inverted history", func(t *testing.T) []byte {
			return demoMetaData(t, 6, withPriceIndex(2, 5, recordlayer.Field("price"), priceAbove(1)))
		}, "template version 1 has metadata version 6, above version 3's 5"},
		{"a record type key that differs", func(t *testing.T) []byte {
			return demoMetaData(t, 5, func(p *gen.MetaData) {
				withPriceIndex(2, 5, recordlayer.Field("price"), priceAbove(1))(p)
				for _, rt := range p.RecordTypes {
					if rt.GetName() == "Customer" {
						rt.ExplicitKey = &gen.Value{LongValue: proto.Int64(99)}
					}
				}
			})
		}, "record type key changed"},
		{"an index predicate that differs", func(t *testing.T) []byte {
			return demoMetaData(t, 5, withPriceIndex(2, 5, recordlayer.Field("price"), priceAbove(2)))
		}, "index Order$price differs in predicate"},
		{"an index H raised above L's metadata version, changed", func(t *testing.T) []byte {
			return demoMetaData(t, 4, withPriceIndex(2, 4, recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("order_id")), priceAbove(1)))
		}, ""},
		{"a record type renamed at its union field", func(t *testing.T) []byte {
			return demoMetaData(t, 5, func(p *gen.MetaData) {
				withPriceIndex(2, 5, recordlayer.Field("price"), priceAbove(1))(p)
				renameMessage(p, "Customer", "Client")
			})
		}, `record type name changed (old="Client", new="Customer")`},
		{"an enum whose values are in another order", func(t *testing.T) []byte {
			return demoMetaData(t, 5, func(p *gen.MetaData) {
				withPriceIndex(2, 5, recordlayer.Field("price"), priceAbove(1))(p)
				for _, e := range p.GetRecords().GetEnumType() {
					if e.GetName() == "Color" {
						v := e.Value
						v[0], v[1] = v[1], v[0]
					}
				}
			})
		}, "type changes are not allowed"},
		{"an index H raised to at most L's metadata version, changed", func(t *testing.T) []byte {
			return demoMetaData(t, 5, withPriceIndex(2, 5, recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("order_id")), priceAbove(1)))
		}, "index key expression changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, v3 := base(t)
			e.writeRow(1, c.v1(t))
			err := e.restore(3, v3, nil)
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				e.readsBack("/db", "s")
				return
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidSchemaTemplate {
				t.Fatalf("err = %v, want 42F59", err)
			}
			const prefix = "schema template r version 3 cannot be restored beside version 1: "
			if len(apiErr.Message) < len(prefix) || apiErr.Message[:len(prefix)] != prefix || !strings.Contains(apiErr.Message, c.want) {
				t.Fatalf("message %q, want %q followed by %q", apiErr.Message, prefix, c.want)
			}
			if e.storedRow(3) != nil {
				t.Fatal("a refused restore wrote the row")
			}
		})
	}
}

// Two histories that differ only in a literal's carrier are two keys, in either
// order, as Java's evolution validator reads a literal (LiteralKeyExpression's
// equals compares the value object): the restore is refused and writes nothing.
func TestFDB_Restore_RefusesALiteralCarrierChange(t *testing.T) {
	t.Parallel()
	root := func(lit any) recordlayer.KeyExpression {
		return recordlayer.Concat(recordlayer.Field("price"), recordlayer.Literal(lit))
	}
	for _, c := range []struct {
		name          string
		stored, other any
	}{
		{"long stored, int restored", int64(7), int32(7)},
		{"int stored, long restored", int32(7), int64(7)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newRestoreEnv(t)
			v3 := demoMetaData(t, 5, withPriceIndex(2, 5, root(c.other), nil))
			e.writeRow(3, v3)
			e.bind("/db", "s", 3, v3)
			e.dropTemplate()
			e.writeRow(1, demoMetaData(t, 5, withPriceIndex(2, 5, root(c.stored), nil)))
			err := e.restore(3, v3, nil)
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidSchemaTemplate {
				t.Fatalf("err = %v, want 42F59", err)
			}
			const prefix = "schema template r version 3 cannot be restored beside version 1: "
			if !strings.HasPrefix(apiErr.Message, prefix) || !strings.Contains(apiErr.Message, "index key expression changed") {
				t.Fatalf("message %q, want %q and the changed key", apiErr.Message, prefix)
			}
			if e.storedRow(3) != nil {
				t.Fatal("a refused restore wrote the row")
			}
		})
	}
}

// The restoring transaction decides for itself: a DROP SCHEMA of the only
// binding, with or without a fresh template after it, committed after the
// listing, refuses the restore; a DROP of the binding its limit-1 read returned,
// or a template write of the name, between its reads and its commit conflicts
// and the restore decides again from the listing; a DROP of a later binding
// does not conflict.
func TestFDB_Restore_ConcurrentWrites(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, schemas ...string) (*restoreEnv, []byte) {
		e := newRestoreEnv(t)
		v3 := demoMetaData(t, 1, nil)
		e.writeRow(3, v3)
		for _, s := range schemas {
			e.bind("/db", s, 3, v3)
		}
		e.dropTemplate()
		return e, v3
	}
	drop := func(e *restoreEnv, schema string) func() {
		return func() {
			mustRun(e.t, e.run, func(tx api.Transaction) error { return e.cat.DeleteSchema(tx, "/db", schema) })
		}
	}
	once := func(f func()) func() {
		done := false
		return func() {
			if !done {
				done = true
				f()
			}
		}
	}

	t.Run("the only binding dropped and a fresh template created after the listing", func(t *testing.T) {
		t.Parallel()
		e, v3 := setup(t, "a")
		opts := &restoreOptions{batch: restoreHeaderBatch, afterListing: once(func() {
			drop(e, "a")()
			mustRun(t, e.run, func(tx api.Transaction) error {
				return e.cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "r", 1))
			})
		})}
		wantRestoreRefused(t, e.restore(3, v3, opts), api.ErrCodeInvalidSchemaTemplate,
			"schema template r version 3 cannot be restored: no schema binds it")
		if e.storedRow(3) != nil || e.storedRow(1) == nil {
			t.Fatal("the fresh v1 must stand alone")
		}
	})

	t.Run("the only binding dropped after the listing", func(t *testing.T) {
		t.Parallel()
		e, v3 := setup(t, "a")
		opts := &restoreOptions{batch: restoreHeaderBatch, afterListing: once(drop(e, "a"))}
		wantRestoreRefused(t, e.restore(3, v3, opts), api.ErrCodeInvalidSchemaTemplate,
			"schema template r version 3 cannot be restored: no schema binds it")
	})

	t.Run("the binding the limit-1 read returned dropped before the commit", func(t *testing.T) {
		t.Parallel()
		e, v3 := setup(t, "a", "b")
		opts := &restoreOptions{batch: restoreHeaderBatch, beforeCommit: once(drop(e, "a"))}
		if err := e.restore(3, v3, opts); err != nil {
			t.Fatal(err)
		}
		if opts.attempts != 2 {
			t.Fatalf("%d restoring transactions, want 2 (the first conflicts)", opts.attempts)
		}
		e.readsBack("/db", "b")
	})

	t.Run("a later binding dropped before the commit", func(t *testing.T) {
		t.Parallel()
		e, v3 := setup(t, "a", "b")
		opts := &restoreOptions{batch: restoreHeaderBatch, beforeCommit: once(drop(e, "b"))}
		if err := e.restore(3, v3, opts); err != nil {
			t.Fatal(err)
		}
		if opts.attempts != 1 {
			t.Fatalf("%d restoring transactions, want 1 (no conflict)", opts.attempts)
		}
	})

	// The restore commits, its result is reported unknown (commit_unknown_result),
	// and the only binding is dropped before the retry: the retry finds exactly
	// its bytes stored and is done, before its listing, which would now find no
	// binding and refuse.
	t.Run("a landed commit reported unknown, the binding dropped after it", func(t *testing.T) {
		t.Parallel()
		e, v3 := setup(t, "a")
		dropOnce := once(drop(e, "a"))
		opts := &restoreOptions{batch: restoreHeaderBatch, afterCommit: func() error {
			dropOnce()
			return fdb.Error{Code: 1021}
		}}
		if err := e.restore(3, v3, opts); err != nil {
			t.Fatalf("the restore landed and reported %v", err)
		}
		if opts.attempts != 1 {
			t.Fatalf("%d restoring transactions, want 1 (the retry reads its bytes first)", opts.attempts)
		}
		if row := e.storedRow(3); row == nil || string(row) != string(v3) {
			t.Fatal("the restored row is not the restored bytes")
		}
	})

	t.Run("a template write of the name before the commit", func(t *testing.T) {
		t.Parallel()
		e, v3 := setup(t, "a")
		opts := &restoreOptions{batch: restoreHeaderBatch, beforeCommit: once(func() { e.writeRow(9, v3) })}
		if err := e.restore(3, v3, opts); err != nil {
			t.Fatal(err)
		}
		if opts.attempts != 2 {
			t.Fatalf("%d restoring transactions, want 2 (the first conflicts)", opts.attempts)
		}
	})
}

// Restore(1), then a new v2, which the dangling (3) binding refuses; Restore(3),
// then v4, which is accepted: no carried version is ever stored beside a
// dangling binding above it (WS-J section 2's first sequence).
// The other two sequences of WS-J section 2: Restore(3), then v4, which the
// dangling (5) binding refuses; Restore(5), then v6, accepted. And Restore(1),
// then v5, which the dangling (3) binding refuses, the save skipping the
// versions between; Restore(3), then v5, accepted. Each ends with every bound
// schema opened and its rows read back.
func TestFDB_Restore_GuardSequences(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name             string
		stored           []int // versions stored, each bound by schema "s<v>", then dropped
		first, refused   int   // the restore, then the version it refuses
		dangling         int   // the dangling bound version the refusal names
		second, accepted int   // the restore of the dangling version, then the version accepted
	}{
		{"a restore below two dangling versions", []int{3, 5}, 3, 4, 5, 5, 6},
		{"a save that skips versions", []int{1, 3}, 1, 5, 3, 3, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newRestoreEnv(t)
			e.name = "seq"
			mds := map[int][]byte{}
			for _, v := range c.stored {
				mds[v] = demoMetaData(t, 1, nil)
				e.writeRow(v, mds[v])
				e.bind("/db", fmt.Sprintf("s%d", v), v, mds[v])
			}
			e.dropTemplate()
			create := func(v int) error {
				return e.run(func(tx api.Transaction) error {
					return e.cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "seq", v))
				})
			}
			if err := e.restore(c.first, mds[c.first], nil); err != nil {
				t.Fatal(err)
			}
			wantAPIError(t, create(c.refused), api.ErrCodeInvalidSchemaTemplate,
				fmt.Sprintf("schema template seq version %d cannot be created: schemas are still bound to its dropped version %d (/db/s%d)",
					c.refused, c.dangling, c.dangling))
			if err := e.restore(c.second, mds[c.second], nil); err != nil {
				t.Fatal(err)
			}
			if err := create(c.accepted); err != nil {
				t.Fatal(err)
			}
			for _, v := range c.stored {
				e.readsBack("/db", fmt.Sprintf("s%d", v))
			}
		})
	}
}

func TestFDB_Restore_GuardSequence(t *testing.T) {
	t.Parallel()
	e := newRestoreEnv(t)
	e.name = "seq"
	v1 := demoMetaData(t, 1, nil)
	v3 := demoMetaData(t, 1, nil)
	e.writeRow(1, v1)
	e.writeRow(3, v3)
	e.bind("/db", "one", 1, v1)
	e.bind("/db", "three", 3, v3)
	e.dropTemplate()

	if err := e.restore(1, v1, nil); err != nil {
		t.Fatal(err)
	}
	err := e.run(func(tx api.Transaction) error {
		return e.cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "seq", 2))
	})
	wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate,
		"schema template seq version 2 cannot be created: schemas are still bound to its dropped version 3 (/db/three)")
	if err := e.restore(3, v3, nil); err != nil {
		t.Fatal(err)
	}
	mustRun(t, e.run, func(tx api.Transaction) error {
		return e.cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "seq", 4))
	})
	e.readsBack("/db", "one")
	e.readsBack("/db", "three")
}

// renameMessage renames a record message in the records file, its union field's
// type and its RecordType entry: the same union field under another name.
func renameMessage(p *gen.MetaData, from, to string) {
	pkg := p.GetRecords().GetPackage()
	for _, m := range p.GetRecords().GetMessageType() {
		if m.GetName() == from {
			m.Name = proto.String(to)
		}
		for _, f := range m.GetField() {
			if f.GetTypeName() == "."+pkg+"."+from {
				f.TypeName = proto.String("." + pkg + "." + to)
			}
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if rt.GetName() == from {
			rt.Name = proto.String(to)
		}
	}
}
