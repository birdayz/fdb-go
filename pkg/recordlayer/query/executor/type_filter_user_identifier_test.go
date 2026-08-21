package executor

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// `GetRecordTypes()` means a DIFFERENT NAMESPACE on two plan types, and the
// executor compares it against a third.
//
// RecordQueryScanPlan carries the SQL table name straight from the translator
// (cascades_translator.go passes []string{s.Table}); RecordQueryTypeFilterPlan's
// only production origin passes the STORED name off a match candidate; and the
// predicate compares against FDBStoredRecord.RecordType.Name, which is stored.
// The two agree today only because of where the filter's list happens to come
// from -- nothing asserted it, and one plan-side change makes the filter reject
// every row and return zero with a nil error.
//
// LATENT, NOT LIVE, and the distinction is worth keeping straight: no SQL shape
// reaches this predicate with a name needing resolution, because the filter's
// only production origin passes the stored spelling already. Measured -- the
// intermingled escaped-name yamsql scenario stays GREEN with the resolution
// removed from here, and reddens with it removed from ScanRecordsByType. This
// test is the only thing that pins this half.
//
// Resolving the name is the same rule ScanRecordsByType applies, so both
// spellings of one type work and an unresolvable name still matches nothing.
// These arms pin all three cases.
//
// The fixture renames Order to MY__1TABLE because no checked-in proto has an
// escaped record-type name, and without one the SQL and stored spellings
// coincide and the arms test nothing.
func renamedOrderMetaData(t *testing.T, to string) *recordlayer.RecordMetaData {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	p.JoinedRecordTypes = nil
	for _, msg := range p.GetRecords().GetMessageType() {
		if msg.GetName() == "Order" {
			msg.Name = proto.String(to)
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if rt.GetName() == "Order" {
			rt.Name = proto.String(to)
		}
	}
	// The union addresses each record type twice: by the `_TypeName` FIELD-NAME
	// convention, which takes precedence in setRecordsWithUnionName, and by a
	// FULLY QUALIFIED type reference. Renaming only the message unlinks both and
	// the type stops being a record type at all -- a fixture that silently drops
	// the type under test rather than renaming it.
	for _, msg := range p.GetRecords().GetMessageType() {
		for _, f := range msg.GetField() {
			if f.GetName() == "_Order" {
				f.Name = proto.String("_" + to)
			}
			full := strings.TrimPrefix(f.GetTypeName(), ".")
			short, pkgPrefix := full, ""
			if i := strings.LastIndex(full, "."); i >= 0 {
				short, pkgPrefix = full[i+1:], full[:i+1]
			}
			if short == "Order" {
				f.TypeName = proto.String("." + pkgPrefix + to)
			}
		}
	}
	renamed, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	if renamed.GetRecordType(to) == nil {
		t.Fatalf("rename to %q did not take; the fixture cannot express the defect", to)
	}
	return renamed
}

func TestIntegration_TypeFilter_AcceptsEitherSpellingOfTheName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	md := renamedOrderMetaData(t, "MY__1TABLE")
	ks := testSubspace(t)

	rt := md.GetRecordType("MY__1TABLE")
	rec := dynamicpb.NewMessage(rt.Descriptor)
	rec.Set(rt.Descriptor.Fields().ByName("order_id"), protoreflect.ValueOfInt64(10))

	// A SECOND type, and it is load-bearing. With only MY__1TABLE in the store,
	// "want 1" is satisfied by a predicate that matches EVERYTHING, so a
	// regression to match-all would redden only the unknown-name arm. The
	// Customer row makes each count discriminate.
	customerType := md.GetRecordType("Customer")
	if customerType == nil {
		t.Fatal("Customer went missing from the fixture; the arms below stop discriminating")
	}
	cust := dynamicpb.NewMessage(customerType.Descriptor)
	cust.Set(customerType.Descriptor.Fields().ByName("customer_id"), protoreflect.ValueOfInt64(20))

	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		if _, err := s.SaveRecord(rec); err != nil {
			return nil, err
		}
		return s.SaveRecord(cust)
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	t.Cleanup(func() {
		_, cerr := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, oerr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if oerr != nil {
				return nil, oerr
			}
			return nil, s.DeleteAllRecords()
		})
		if cerr != nil {
			t.Errorf("cleanup: %v", cerr)
		}
	})

	for _, tc := range []struct {
		name     string
		filterOn string
		want     int
	}{
		{"storage spelling", "MY__1TABLE", 1},
		{"SQL identifier", "MY$TABLE", 1},
		{"resolves to no type at all", "NoSuchType", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan(nil, values.NewAnyRecordType(false), false))
				filter := mustExecutorConstruct(plans.NewRecordQueryTypeFilterPlan([]string{tc.filterOn}, scan))

				cursor, err := ExecutePlan(ctx, filter, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				if err != nil {
					return nil, err
				}
				defer cursor.Close()
				results, err := CollectAll(ctx, cursor)
				if err != nil {
					return nil, err
				}
				if len(results) != tc.want {
					t.Errorf("type filter on %q returned %d rows, want %d.\n"+
						"The predicate compares the plan's record-type names against the STORED\n"+
						"spelling on each record. Both spellings of one type must resolve to the\n"+
						"same set, and a name that resolves to no type must keep matching nothing\n"+
						"-- resolving must not turn a miss into a match-everything.",
						tc.filterOn, len(results), tc.want)
				}
				for _, r := range results {
					if r.Record.RecordType.Name != "MY__1TABLE" {
						t.Errorf("record type = %q, want MY__1TABLE", r.Record.RecordType.Name)
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
