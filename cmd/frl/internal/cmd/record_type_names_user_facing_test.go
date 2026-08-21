package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// metaWithEscapedTypeName builds demo metadata with `Order` stored under an
// ESCAPED name, which is the only way to observe the storage/SQL split: no
// checked-in proto carries one, and a fixture that cannot express the condition
// leaves every assertion below vacuous while reporting green.
func metaWithEscapedTypeName(t *testing.T, storage string) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	// Attached BEFORE the rename so RecordTypesForIndex resolves to the renamed
	// type afterwards -- that is what `frl index describe` renders.
	b.AddIndex("Order", recordlayer.NewIndex("order_by_customer", recordlayer.Field("order_id")))
	base, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	p, err := base.ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	p.JoinedRecordTypes = nil
	for _, msg := range p.GetRecords().GetMessageType() {
		if msg.GetName() == "Order" {
			msg.Name = proto.String(storage)
		}
		// The union addresses record types by the `_TypeName` FIELD-NAME
		// convention, which takes precedence over the field's type reference --
		// renaming only the message drops the type instead of renaming it.
		for _, f := range msg.GetField() {
			if f.GetName() == "_Order" {
				f.Name = proto.String("_" + storage)
			}
			// TypeName is FULLY QUALIFIED (.pkg.Order); match the last segment
			// and preserve the package.
			full := strings.TrimPrefix(f.GetTypeName(), ".")
			short, pkgPrefix := full, ""
			if i := strings.LastIndex(full, "."); i >= 0 {
				short, pkgPrefix = full[i+1:], full[:i+1]
			}
			if short == "Order" {
				f.TypeName = proto.String("." + pkgPrefix + storage)
			}
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if rt.GetName() == "Order" {
			rt.Name = proto.String(storage)
		}
	}
	// Indexes reference their record types BY NAME; leaving these behind makes
	// the rebuild reject the metadata outright.
	for _, idx := range p.GetIndexes() {
		for i, n := range idx.GetRecordType() {
			if n == "Order" {
				idx.RecordType[i] = storage
			}
		}
	}
	md, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	if _, ok := md.RecordTypes()[storage]; !ok {
		t.Fatalf("fixture did not land: no record type stored as %s", storage)
	}
	return md
}

// EVERY COMMAND THAT PRINTS A RECORD-TYPE NAME PRINTS THE SQL IDENTIFIER.
//
// RecordTypes() is keyed by the ESCAPED storage name, so a renderer copying
// those keys names a table the operator does not have. `frl stats` was fixed
// first and the rest of the CLI was not, which meant one table was MY$TABLE
// under one command and MY__1TABLE under another -- worse than either spelling
// alone, because a script crossing two commands silently missed.
//
// Round-tripping is safe: GetRecordType resolves EITHER namespace (see
// pkg/recordlayer's TestGetRecordTypeResolvesAUserIdentifier), so a name copied
// out of this output still resolves when passed back in via --type.
//
// NOT covered here, deliberately, because they are different namespaces:
// INDEX names (GetIndex is a raw map lookup with no escape fallback), context
// names, and SYNTHETIC type names (stored verbatim by Java, never created by
// this port -- see TestStats_SyntheticTypeNamesAreRenderedVerbatim).
func TestRecordTypeNamesRenderAsSQLIdentifiers(t *testing.T) {
	t.Parallel()

	const storage, sql = "MY__1TABLE", "MY$TABLE"
	if recordlayer.ToUserIdentifier(storage) != sql {
		t.Fatalf("fixture is vacuous: %q does not decode to %q", storage, sql)
	}
	md := metaWithEscapedTypeName(t, storage)

	// assert renders `sql` and never `storage`. Checking BOTH matters: a
	// renderer that emits both spellings would satisfy a contains-check alone.
	assert := func(t *testing.T, what, got string) {
		t.Helper()
		if !strings.Contains(got, sql) {
			t.Errorf("%s does not name the table by its SQL identifier %q:\n%s", what, sql, got)
		}
		if strings.Contains(got, storage) {
			t.Errorf("%s leaks the storage name %q:\n%s", what, storage, got)
		}
	}

	t.Run("meta types (text)", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := writeTypesList(&buf, md); err != nil {
			t.Fatalf("writeTypesList: %v", err)
		}
		assert(t, "meta types", buf.String())
	})

	t.Run("meta types (json)", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := writeTypesListJSON(&buf, md); err != nil {
			t.Fatalf("writeTypesListJSON: %v", err)
		}
		assert(t, "meta types -o json", buf.String())
	})

	t.Run("index describe", func(t *testing.T) {
		t.Parallel()
		idx := md.GetIndex("order_by_customer")
		if idx == nil {
			t.Fatal("fixture did not land: the index survived neither the rename nor the rebuild")
		}
		got := strings.Join(recordTypeNames(md, idx), ", ")
		if got == "*" {
			t.Fatal("fixture is vacuous: the index reads as universal, so no record-type name is rendered")
		}
		assert(t, "index describe record types", got)
	})

	t.Run("not-found message", func(t *testing.T) {
		t.Parallel()
		// The "available:" list is what an operator copies a name out of after a
		// typo, so it has to offer names they can type back.
		_, err := lookupRecordType(md, "NoSuchType")
		if err == nil {
			t.Fatal("expected a lookup miss")
		}
		assert(t, "lookupRecordType error", err.Error())
	})

	t.Run("meta types describe (text)", func(t *testing.T) {
		t.Parallel()
		rt := md.GetRecordType(storage)
		if rt == nil {
			t.Fatal("fixture did not land: the escaped type is not resolvable")
		}
		var buf bytes.Buffer
		if err := writeRecordTypeDescription(&buf, md, rt); err != nil {
			t.Fatalf("writeRecordTypeDescription: %v", err)
		}
		// This output deliberately carries TWO namespaces: `Name:` is the SQL
		// identifier, `Proto message:` is the descriptor's full name and is
		// CORRECTLY the storage spelling. So assert per line, not over the blob --
		// a whole-output check would either miss the Name regression or forbid the
		// proto name that is supposed to be there.
		got := buf.String()
		var nameLine string
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, "Name:") {
				nameLine = l
				break
			}
		}
		if nameLine == "" {
			t.Fatalf("no Name: line in the description:\n%s", got)
		}
		if !strings.Contains(nameLine, sql) || strings.Contains(nameLine, storage) {
			t.Errorf("the Name line is not the SQL identifier %q: %s", sql, nameLine)
		}
		if !strings.Contains(got, "Proto message:") || !strings.Contains(got, storage) {
			t.Errorf("the Proto message line should still carry the descriptor name %q "+
				"(it is a proto fact, not a SQL one):\n%s", storage, got)
		}
	})

	t.Run("meta types describe (json)", func(t *testing.T) {
		t.Parallel()
		rt := md.GetRecordType(storage)
		if rt == nil {
			t.Fatal("fixture did not land: the escaped type is not resolvable")
		}
		var buf bytes.Buffer
		if err := writeRecordTypeDescriptionJSON(&buf, md, rt); err != nil {
			t.Fatalf("writeRecordTypeDescriptionJSON: %v", err)
		}
		// Same two-namespace split as the text form, checked on the typed fields
		// so the assertion cannot drift onto the wrong one.
		var desc struct {
			Name         string `json:"name"`
			ProtoMessage string `json:"proto_message"`
		}
		if err := json.Unmarshal(buf.Bytes(), &desc); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, buf.String())
		}
		if desc.Name != sql {
			t.Errorf("name = %q, want the SQL identifier %q", desc.Name, sql)
		}
		if !strings.Contains(desc.ProtoMessage, storage) {
			t.Errorf("proto_message = %q, want it to carry the descriptor name %q",
				desc.ProtoMessage, storage)
		}
	})

	t.Run("record scan (json)", func(t *testing.T) {
		t.Parallel()
		// `stats show -o json` keys per_type by the DECODED name, so a script
		// joining scan output to statistics needs the same spelling here.
		rt := md.GetRecordType(storage)
		if rt == nil {
			t.Fatal("fixture did not land: the escaped type is not resolvable")
		}
		var buf bytes.Buffer
		rec := &recordlayer.FDBStoredRecord[proto.Message]{
			PrimaryKey: tuple.Tuple{int64(1)},
			RecordType: rt,
			Record:     &gen.Order{},
		}
		if err := writeRecordAsJSON(&buf, rec); err != nil {
			t.Fatalf("writeRecordAsJSON: %v", err)
		}
		var env struct {
			RecordType string `json:"record_type"`
		}
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, buf.String())
		}
		if env.RecordType != sql {
			t.Errorf("record_type = %q, want the SQL identifier %q", env.RecordType, sql)
		}
	})

	t.Run("meta diff", func(t *testing.T) {
		t.Parallel()
		// Diffed against metadata where the type is NOT renamed, so the escaped
		// type lands in Removed -- which is the bucket that prints its name.
		mb := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		mb.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		mb.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		mb.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		unrenamed, err := mb.Build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		s := diffRecordTypes(md, unrenamed)
		if len(s.Removed) == 0 {
			t.Fatal("fixture is vacuous: nothing was removed, so no name is rendered")
		}
		var b strings.Builder
		for _, e := range s.Removed {
			b.WriteString(e.Name + "\n")
		}
		for _, e := range s.Added {
			b.WriteString(e.Name + "\n")
		}
		assert(t, "meta diff", b.String())
	})
}
