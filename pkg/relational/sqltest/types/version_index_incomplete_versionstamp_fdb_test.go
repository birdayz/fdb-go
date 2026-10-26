// Portions derived from FoundationDB Record Layer (IndexTest.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package sqltest

// Java accepts a VERSION index with the version in its KeyWithValue VALUE
// portion (IndexTest.createVersionIndexWithVersionInValue), but an automatic
// incomplete version then fails during the value's ordinary tuple pack. Go must
// return an error, not panic. In contrast, VERSION(order_desc(version())) is
// rejected during metadata validation: functions declare zero version columns,
// and IndexValidator.validateVersionKey requires exactly one. Pin both stages.

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_VersionIndex_IncompleteVersionstampIsAnErrorNotAPanic(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}

	cases := []struct {
		name string
		ddl  string
		// wantRoot is the key expression Java's generator emits for this
		// declaration. Asserting it here is what stops a future "fix" from
		// quietly rejecting the shape at DDL and diverging on index metadata.
		wantRoot     recordlayer.KeyExpression
		wantDDLError bool
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
			wantDDLError: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fdb.MustAPIVersion(730)
			rawDB, err := fdb.OpenDatabase(testkit.ClusterFile())
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			db := recordlayer.NewFDBDatabase(rawDB)
			ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

			tmpl, err := embedded.BuildSchemaTemplateFromDDL(tc.ddl)
			if tc.wantDDLError {
				var apiErr *api.Error
				var keyErr *recordlayer.KeyExpressionError
				if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnknown ||
					!errors.As(err, &keyErr) || keyErr.Error() != "there must be exactly 1 version entry in index" {
					t.Fatalf("want Java's UNKNOWN metadata-validation error, got %T: %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Java accepts a version in the VALUE portion; DDL failed: %v", err)
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
