package sqldriver_test

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
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// TestFDB_ArrayCardinalityUniqueIndex pins the uniqueness consequence of keying
// an empty NOT NULL array as the integer 0 rather than as NULL.
//
// The two facts compose into a behaviour change that neither one states on its
// own. StandardIndexMaintainer skips the uniqueness check for an entry whose key
// contains a NULL (Java: StandardIndexMaintainer.java:471,
// `!indexEntry.keyContainsNonUniqueNull()`; Go: index_maintainer.go's
// `!indexKeyContainsNull(entry.key)` guard on checkUniqueness). So while an
// empty NOT NULL array keyed NULL, two such records BYPASSED the check
// entirely and coexisted happily on a UNIQUE cardinality index. Keying 0
// instead puts them on the same non-null key, and they must now COLLIDE.
//
// That is Java's behaviour — Java has no zero special-case on the key
// (CardinalityFunctionKeyExpression.java:115-117) and the same null-bypass on
// the check — so the collision is correct, not a regression. It is also
// invisible to every value-level assertion: the index scan returns the same
// rows either way, and only a second INSERT reveals which key the first one
// occupies. Hence a test at the write path.
//
// The contrast half matters as much as the collision half. A uniqueness check
// that fired for everything would satisfy the first assertion while being
// broken; two arrays of DIFFERENT cardinality must still coexist.
func TestFDB_ArrayCardinalityUniqueIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	b := metadata.NewSchemaTemplateBuilder().SetName("carduniq")
	b.AddTable("T_UNIQ", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
		metadata.NewColumnSpec("INT_ARR", api.NewArrayType(api.NewIntegerType(false), false), 2),
	}, []string{"ID"})
	b.AddCardinalityIndex("T_UNIQ", "T_UNIQ_CARD", "INT_ARR")
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	// The programmatic index builder has no UNIQUE arm for a cardinality index,
	// so the flag is set on the built Index — the same field the index
	// maintainer consults, and the same one a UNIQUE DDL would populate.
	idx := md.GetIndex("T_UNIQ_CARD")
	if idx == nil {
		t.Fatal("index T_UNIQ_CARD not in metadata")
	}
	idx.SetUnique()
	if !idx.IsUnique() {
		t.Fatal("T_UNIQ_CARD is not unique after SetUnique — the rest of this test would assert nothing")
	}
	desc := md.GetRecordType("T_UNIQ").Descriptor

	rec := func(id int32, arr []int32) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt32(id))
		fd := desc.Fields().ByName("INT_ARR")
		pvals := make([]protoreflect.Value, len(arr))
		for i, v := range arr {
			pvals[i] = protoreflect.ValueOfInt32(v)
		}
		setArrayField(m, fd, pvals...)
		return m
	}

	// Each save runs in its own transaction: a uniqueness violation aborts the
	// transaction that raised it, and the surviving records have to be the ones
	// committed before it.
	save := func(id int32, arr []int32, create bool) error {
		_, sErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			sb := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks)
			var store *recordlayer.FDBRecordStore
			var oErr error
			if create {
				store, oErr = sb.CreateOrOpen()
			} else {
				store, oErr = sb.Open()
			}
			if oErr != nil {
				return nil, oErr
			}
			_, e := store.SaveRecord(rec(id, arr))
			return nil, e
		})
		return sErr
	}

	// id1: the first empty array. Nothing to collide with yet.
	if e := save(1, []int32{}, true); e != nil {
		t.Fatalf("first empty-array record must save: %v", e)
	}

	// id2: a SECOND empty array. Both key cardinality 0 — a real, non-NULL key
	// — so the uniqueness check runs and must reject this one. Before the wire
	// fix both keyed NULL, the check was skipped, and this save succeeded.
	e := save(2, []int32{}, false)
	if e == nil {
		t.Fatal("second record with an EMPTY NOT NULL array was accepted on a UNIQUE cardinality index: " +
			"both records key cardinality 0, so the second must violate uniqueness. " +
			"Acceptance means the entries key NULL, which the maintainer's null-bypass excludes from the check")
	}
	var uv *recordlayer.RecordIndexUniquenessViolationError
	if !errors.As(e, &uv) {
		t.Fatalf("second empty-array record failed with %T (%v), want *recordlayer.RecordIndexUniquenessViolationError", e, e)
	}
	if uv.IndexName != "T_UNIQ_CARD" {
		t.Fatalf("uniqueness violation names index %q, want %q", uv.IndexName, "T_UNIQ_CARD")
	}
	// The violated key is the cardinality itself: 0, not NULL.
	if len(uv.IndexKey) != 1 {
		t.Fatalf("violated index key %v has %d columns, want 1 (the cardinality)", uv.IndexKey, len(uv.IndexKey))
	}
	if got, ok := uv.IndexKey[0].(int64); !ok || got != 0 {
		t.Fatalf("violated index key column is %#v (%T), want int64(0) — an empty array's cardinality is 0, never NULL",
			uv.IndexKey[0], uv.IndexKey[0])
	}

	// CONTRAST: different cardinalities do NOT collide. Without this, a check
	// that rejected every insert would satisfy the assertion above.
	if e := save(3, []int32{7}, false); e != nil {
		t.Fatalf("cardinality 1 must not collide with the existing cardinality 0: %v", e)
	}
	if e := save(4, []int32{7, 8}, false); e != nil {
		t.Fatalf("cardinality 2 must not collide with cardinalities 0 and 1: %v", e)
	}

	// And the check is genuinely armed for non-zero keys too, so the collision
	// above is uniqueness working rather than something specific to zero.
	e = save(5, []int32{9}, false)
	if e == nil {
		t.Fatal("second record with cardinality 1 was accepted on a UNIQUE cardinality index")
	}
	if !errors.As(e, &uv) {
		t.Fatalf("cardinality-1 collision failed with %T (%v), want *recordlayer.RecordIndexUniquenessViolationError", e, e)
	}
	if got, ok := uv.IndexKey[0].(int64); !ok || got != 1 {
		t.Fatalf("violated index key column is %#v, want int64(1)", uv.IndexKey[0])
	}
}
