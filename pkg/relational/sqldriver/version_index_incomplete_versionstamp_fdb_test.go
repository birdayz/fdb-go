package sqldriver_test

// Two index shapes that ordinary DDL accepts put an INCOMPLETE versionstamp in
// front of a vanilla tuple pack:
//
//   - the version column landing in the VALUE portion of a KeyWithValue split
//     (`... ORDER BY col2` with the version later in the projection), and
//   - an ordering wrapper over the version (`ORDER BY "__ROW_VERSION" DESC`).
//
// Neither can ever be written: the versionstamp mutation substitutes into the
// KEY only, so a versionstamp anywhere else has nothing that could complete it.
// Java reaches the identical dead end — VersionIndexMaintainer.java:107 packs
// the value after testing only the entry KEY for incompleteness (:90), and
// TupleOrdering.packNullsLast raises IllegalArgumentException("Incomplete
// Versionstamp included in vanilla tuple pack") (TupleOrdering.java:174-180).
//
// Java THROWS. Go used to PANIC, because tuple.Pack panics where Java's throws,
// so the failure crossed the library boundary as a crash instead of an error —
// a direct violation of the no-panic invariant. Measured before the fix:
//
//	p13_version_in_value: PANIC on insert: Incomplete Versionstamp included in vanilla tuple pack
//	p14_version_desc:     PANIC on insert: Incomplete Versionstamp included in vanilla tuple pack
//
// The METADATA is deliberately unchanged. Both shapes are what Java's own index
// generator emits and its DDL tests pin (IndexTest.java:897-905
// createVersionIndexWithVersionInValue), and index metadata is wire, so
// rejecting them at DDL would diverge. The failure belongs at the write, which
// is exactly where Java puts it. This test therefore asserts BOTH halves: the
// DDL still builds the Java key expression, AND the insert fails with an error
// rather than a panic.

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_VersionIndex_IncompleteVersionstampIsAnErrorNotAPanic(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	cases := []struct {
		name string
		ddl  string
		// wantRoot is the key expression Java's generator emits for this
		// declaration. Asserting it here is what stops a future "fix" from
		// quietly rejecting the shape at DDL and diverging on index metadata.
		wantRoot recordlayer.KeyExpression
	}{
		{
			name: "version_in_value_portion",
			ddl: `CREATE SCHEMA TEMPLATE ivs_val_tpl
				CREATE TABLE T (id bigint, col2 string, col3 bigint, col4 bigint, PRIMARY KEY(id))
				CREATE INDEX value_v AS SELECT col2, "__ROW_VERSION", col3, col4 FROM t ORDER BY col2
				WITH OPTIONS(store_row_versions=true)`,
			wantRoot: recordlayer.KeyWithValue(recordlayer.Concat(
				recordlayer.Field("COL2"), recordlayer.VersionKey(),
				recordlayer.Field("COL3"), recordlayer.Field("COL4")), 1),
		},
		{
			name: "ordering_wrapper_over_version",
			ddl: `CREATE SCHEMA TEMPLATE ivs_desc_tpl
				CREATE TABLE T (id bigint, col2 string, col3 bigint, col4 bigint, PRIMARY KEY(id))
				CREATE INDEX desc_v AS SELECT "__ROW_VERSION" FROM t ORDER BY "__ROW_VERSION" DESC
				WITH OPTIONS(store_row_versions=true)`,
			wantRoot: recordlayer.FunctionExpr("order_desc_nulls_last", recordlayer.VersionKey()),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fdb.MustAPIVersion(730)
			rawDB, err := fdb.OpenDatabase(clusterFilePath)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			db := recordlayer.NewFDBDatabase(rawDB)
			ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

			tmpl, err := embedded.BuildSchemaTemplateFromDDL(tc.ddl)
			if err != nil {
				t.Fatalf("the DDL must still be ACCEPTED — Java's index generator emits "+
					"this shape and its own DDL tests pin it, so rejecting it here would "+
					"diverge on index metadata: %v", err)
			}
			md := tmpl.Underlying()
			var found *recordlayer.Index
			for _, idx := range md.GetAllIndexes() {
				if idx.Type == recordlayer.IndexTypeVersion {
					found = idx
				}
			}
			if found == nil {
				t.Fatal("no VERSION index was built")
			}
			if got, want := found.RootExpression.ToKeyExpression(), tc.wantRoot.ToKeyExpression(); !proto.Equal(got, want) {
				t.Fatalf("index metadata drifted from Java's shape\n got: %v\nwant: %v", got, want)
			}

			desc := md.GetRecordType("T").Descriptor
			m := dynamicpb.NewMessage(desc)
			m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(1))
			m.Set(desc.Fields().ByName("COL2"), protoreflect.ValueOfString("a"))
			m.Set(desc.Fields().ByName("COL3"), protoreflect.ValueOfInt64(30))
			m.Set(desc.Fields().ByName("COL4"), protoreflect.ValueOfInt64(40))

			// A panic here is the defect. Recovering and reporting it keeps the
			// failure message readable instead of taking the whole test binary
			// down with the rest of the package's parallel tests.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("SaveRecord PANICKED: %v\nJava THROWS here (TupleOrdering.java:178); "+
						"the Go port must return an error, never cross the library "+
						"boundary as a crash", r)
				}
			}()
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, sErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if sErr != nil {
					return nil, sErr
				}
				_, e := store.SaveRecord(proto.Message(m))
				return nil, e
			})
			if err == nil {
				t.Fatal("SaveRecord SUCCEEDED — an incomplete versionstamp cannot be " +
					"written through a vanilla pack, so this must fail")
			}
			var ive *recordlayer.IncompleteVersionstampError
			if !errors.As(err, &ive) {
				t.Fatalf("want *recordlayer.IncompleteVersionstampError (Java's "+
					"IllegalArgumentException as a Go error), got %T: %v", err, err)
			}
		})
	}
}
