package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		if err := writeRecordAsJSON(&buf, md, rec); err != nil {
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

// SHELL COMPLETION OFFERS SQL IDENTIFIERS.
//
// Separate from the table above because it drives the real cobra __complete
// path end-to-end (and t.Setenv forbids t.Parallel). Completion is the surface
// where the storage spelling is most harmful: whatever it offers is what the
// operator presses TAB and accepts, so offering MY__1TABLE trains them to type
// a name no other command prints.
//
// Only the record-type completions decode. Index-name completion
// (indexNameCompletion) and context-name completion must NOT, and this test
// deliberately does not touch them -- GetIndex has no escape fallback, so a
// decoded index name would not resolve when accepted.
func TestCompletionOffersSQLIdentifiers(t *testing.T) {
	const storage, sql = "MY__1TABLE", "MY$TABLE"
	if recordlayer.ToUserIdentifier(storage) != sql {
		t.Fatalf("fixture is vacuous: %q does not decode to %q", storage, sql)
	}

	md := metaWithEscapedTypeName(t, storage)
	metaPath := filepath.Join(t.TempDir(), "meta.pb")
	f, err := os.Create(metaPath)
	if err != nil {
		t.Fatalf("create meta file: %v", err)
	}
	if err := recordlayer.WriteRecordMetaData(md, f); err != nil {
		f.Close()
		t.Fatalf("write meta: %v", err)
	}
	f.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf(`current_context: local
contexts:
  - name: local
    cluster_file: /tmp/fake.cluster
    keyspace_path: /test
    metadata:
      meta_file: %s
`, metaPath)
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", cfgPath)

	for _, args := range [][]string{
		{"record", "scan", "--type", ""},
		{"meta", "types", "describe", ""},
	} {
		got := runCompletion(t, args...)
		if len(got) == 0 {
			t.Fatalf("__complete %v offered nothing; the fixture never reached the "+
				"completion path and the assertions below would be vacuous", args)
		}
		var sawSQL, sawStorage bool
		for _, c := range got {
			if c == sql {
				sawSQL = true
			}
			if c == storage {
				sawStorage = true
			}
		}
		if !sawSQL {
			t.Errorf("__complete %v did not offer the SQL identifier %q: %v", args, sql, got)
		}
		if sawStorage {
			t.Errorf("__complete %v offered the storage name %q — pressing TAB would "+
				"accept a name no other command prints: %v", args, storage, got)
		}
	}
}

// COMPLETION FALLS BACK TO STORED NAMES WHEN THE SCHEMA IS AMBIGUOUS.
//
// This is a safety guard, not a cosmetic one. Escaping is not injective across
// the two namespaces: declare SQL types MY$TABLE (stored MY__1TABLE) and
// MY__1TABLE (stored MY__01TABLE) together and decoding offers MY$TABLE and
// MY__1TABLE — but GetRecordType tries the STORED key first, so accepting the
// second candidate resolves to the FIRST type. These completers feed
// `record put` and `record delete`, so that mis-resolution writes to or deletes
// from a table the operator did not name.
//
// Under collision the stored names are offered instead: unhelpful on purpose,
// because they are the only keys that resolve unambiguously.
func TestCompletionFallsBackToStoredNamesWhenAmbiguous(t *testing.T) {
	// MY$TABLE stores as MY__1TABLE; a table whose SQL name IS MY__1TABLE
	// stores as MY__01TABLE. Declaring both storage names is exactly the
	// collision.
	const aStore, bStore = "MY__1TABLE", "MY__01TABLE"
	md := metaWithTwoRenamedTypes(t, aStore, bStore)

	names, ambiguous := md.AmbiguousDeclaredNames()
	if !ambiguous {
		t.Fatalf("fixture is vacuous: the pair does not collide, so the guard under "+
			"test never engages (got %v)", names)
	}

	got := recordTypeCompletionNames(md)
	if len(got) == 0 {
		t.Fatal("completion offered nothing; the assertions below would be vacuous")
	}
	for _, want := range []string{aStore, bStore} {
		var found bool
		for _, c := range got {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("under collision, completion must offer the STORED name %q: %v", want, got)
		}
	}
	// And it must NOT offer the decoded spelling, which is the one that
	// resolves to the wrong type.
	for _, c := range got {
		if c == "MY$TABLE" {
			t.Errorf("completion offered the decoded name MY$TABLE while the schema is "+
				"ambiguous — accepting it resolves to a different table than the one "+
				"whose SQL name that is: %v", got)
		}
	}
}

// metaWithTwoRenamedTypes stores Order and Customer under the two given names.
// Kept separate from metaWithEscapedTypeName rather than generalised: the
// single-rename form is used by eight arms and is clearer read straight.
func metaWithTwoRenamedTypes(t *testing.T, orderTo, customerTo string) *recordlayer.RecordMetaData {
	t.Helper()
	rename := map[string]string{"Order": orderTo, "Customer": customerTo}
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
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
		if to, ok := rename[msg.GetName()]; ok {
			msg.Name = proto.String(to)
		}
		for _, f := range msg.GetField() {
			if to, ok := rename[strings.TrimPrefix(f.GetName(), "_")]; ok &&
				strings.HasPrefix(f.GetName(), "_") {
				f.Name = proto.String("_" + to)
			}
			full := strings.TrimPrefix(f.GetTypeName(), ".")
			short, pkgPrefix := full, ""
			if i := strings.LastIndex(full, "."); i >= 0 {
				short, pkgPrefix = full[i+1:], full[:i+1]
			}
			if to, ok := rename[short]; ok {
				f.TypeName = proto.String("." + pkgPrefix + to)
			}
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if to, ok := rename[rt.GetName()]; ok {
			rt.Name = proto.String(to)
		}
	}
	for _, idx := range p.GetIndexes() {
		for i, n := range idx.GetRecordType() {
			if to, ok := rename[n]; ok {
				idx.RecordType[i] = to
			}
		}
	}
	md, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	for _, want := range []string{orderTo, customerTo} {
		if _, ok := md.RecordTypes()[want]; !ok {
			t.Fatalf("fixture did not land: no record type stored as %s", want)
		}
	}
	return md
}

// `record scan -o json`'s record_type IS GATED, because that field is an input.
//
// The operator guide documents it as feeding --type, so
// `record scan -o json | jq -r .record_type` piped into `record delete` is a
// supported workflow. Under a declared collision the ungated decode printed the
// spelling that resolves to the OTHER type, and the delete landed on the wrong
// table — the same hazard as the completer, at the one site where the output is
// machine-consumed by default.
func TestRecordScanJSONGatesRecordTypeOnAmbiguity(t *testing.T) {
	t.Parallel()

	const aStore, bStore = "MY__1TABLE", "MY__01TABLE"
	md := metaWithTwoRenamedTypes(t, aStore, bStore)
	if _, ambiguous := md.AmbiguousDeclaredNames(); !ambiguous {
		t.Fatal("fixture is vacuous: the pair does not collide, so the gate never engages")
	}
	rt := md.GetRecordType(bStore)
	if rt == nil || rt.Name != bStore {
		t.Fatalf("fixture did not land: no record type stored as %s", bStore)
	}

	var buf bytes.Buffer
	err := writeRecordAsJSON(&buf, md, &recordlayer.FDBStoredRecord[proto.Message]{
		PrimaryKey: tuple.Tuple{int64(1)},
		RecordType: rt,
		Record:     &gen.Order{},
	})
	if err != nil {
		t.Fatalf("writeRecordAsJSON: %v", err)
	}
	var env struct {
		RecordType string `json:"record_type"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.RecordType != bStore {
		t.Errorf("record_type = %q, want the STORED name %q.\n"+
			"Decoding it yields %q, which is the STORED key of the OTHER declared "+
			"type — so piping this value into `record delete --type` deletes the "+
			"wrong table.", env.RecordType, bStore, recordlayer.ToUserIdentifier(bStore))
	}
}

// A DIFF MUST COMPARE STORED NAMES, BECAUSE DECODING IS NOT INJECTIVE.
//
// Stored `A__B` and `A__0B` are different fields that BOTH render as `A__B`:
// `A__0B` is the escaping of a literal `A__B`, and `A__B` decodes to itself.
// So comparing rendered names makes a real index or primary-key change compare
// equal and vanish — silently, from the one tool whose entire job is to report
// what changed.
//
// This pins the reason pkFieldsRaw exists next to pkFieldsOrUnset. Collapsing
// them back into one function is the regression.
func TestDiffComparesStoredNamesNotRenderedOnes(t *testing.T) {
	t.Parallel()

	a := recordlayer.Field("A__B")
	b := recordlayer.Field("A__0B")

	// The premise: these two render identically. If that ever stops being true
	// the test still passes for the wrong reason, so assert it.
	if pkFieldsOrUnset(a) != pkFieldsOrUnset(b) {
		t.Fatalf("fixture is vacuous: %q and %q no longer render alike (%q vs %q), "+
			"so this test cannot observe the collapse it exists to prevent",
			"A__B", "A__0B", pkFieldsOrUnset(a), pkFieldsOrUnset(b))
	}
	// And the comparison form must keep them apart.
	if pkFieldsRaw(a) == pkFieldsRaw(b) {
		t.Errorf("pkFieldsRaw collapses %q and %q to %q — a primary-key change "+
			"between them would be dropped from `frl meta diff` entirely",
			"A__B", "A__0B", pkFieldsRaw(a))
	}
	if pkFieldsRaw(a) != "A__B" || pkFieldsRaw(b) != "A__0B" {
		t.Errorf("pkFieldsRaw must return the STORED spelling: got %q and %q",
			pkFieldsRaw(a), pkFieldsRaw(b))
	}
}

// A RENAME BETWEEN TWO SPELLINGS THAT RENDER ALIKE MUST STILL BE VISIBLE.
//
// Stored A__0B and A__B both decode to A__B. Rename a type from one to the
// other and NEITHER metadata is ambiguous on its own, so the per-metadata gate
// cannot see it — the collision exists only ACROSS the pair being diffed. The
// diff would print `- A__B` / `+ A__B`: a real change the tool cannot express.
func TestDiffShowsStoredNamesWhenRenamedNamesRenderAlike(t *testing.T) {
	t.Parallel()

	const fromStore, toStore = "A__0B", "A__B"
	if userName(fromStore) != userName(toStore) {
		t.Fatalf("fixture is vacuous: %q and %q no longer render alike (%q vs %q)",
			fromStore, toStore, userName(fromStore), userName(toStore))
	}
	oldMeta := metaWithEscapedTypeName(t, fromStore)
	newMeta := metaWithEscapedTypeName(t, toStore)
	// Neither side collides on its own — that is what makes this case invisible
	// to the per-metadata ambiguity gate.
	if _, a := oldMeta.AmbiguousDeclaredNames(); a {
		t.Fatalf("fixture is wrong: the OLD metadata is ambiguous by itself, so this " +
			"test would pass through the ordinary gate rather than the cross-bucket one")
	}
	if _, a := newMeta.AmbiguousDeclaredNames(); a {
		t.Fatal("fixture is wrong: the NEW metadata is ambiguous by itself")
	}

	s := diffRecordTypes(oldMeta, newMeta)
	if len(s.Added) == 0 || len(s.Removed) == 0 {
		t.Fatalf("fixture did not land: added=%d removed=%d, so nothing is compared",
			len(s.Added), len(s.Removed))
	}
	for _, a := range s.Added {
		for _, r := range s.Removed {
			if a.Name == r.Name {
				t.Errorf("the diff reports `- %s` / `+ %s` for a real rename between "+
					"%q and %q — identical text on both sides means the change is "+
					"invisible", r.Name, a.Name, fromStore, toStore)
			}
		}
	}
}

// metaWithRenamedField stores Order's `order_id` under the given name, in the
// proto message, the primary key and the index that references it.
//
// It exists because the diff's raw-vs-rendered split can only be observed on a
// FIELD whose two spellings render alike, and no checked-in proto has one.
func metaWithRenamedField(t *testing.T, to string) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("order_ix", recordlayer.Field("order_id")))
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
		if msg.GetName() != "Order" {
			continue
		}
		for _, f := range msg.GetField() {
			if f.GetName() == "order_id" {
				f.Name = proto.String(to)
			}
		}
	}
	// The key expressions reference the field BY NAME; leaving them behind makes
	// the rebuild reject the metadata.
	rename := func(ke *gen.KeyExpression) {
		if ke == nil {
			return
		}
		if fld := ke.GetField(); fld != nil && fld.GetFieldName() == "order_id" {
			fld.FieldName = proto.String(to)
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if rt.GetName() == "Order" {
			rename(rt.PrimaryKey)
		}
	}
	for _, idx := range p.GetIndexes() {
		if idx.GetName() == "order_ix" {
			rename(idx.RootExpression)
		}
	}
	md, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	rt := md.GetRecordType("Order")
	if rt == nil || pkFieldsRaw(rt.PrimaryKey) != to {
		t.Fatalf("fixture did not land: Order's PK is %q, want %q",
			pkFieldsRaw(rt.PrimaryKey), to)
	}
	return md
}

// THE DIFF ITSELF MUST SEE A FIELD CHANGE THAT RENDERS IDENTICALLY.
//
// TestDiffComparesStoredNamesNotRenderedOnes pins the HELPERS; this drives
// diffRecordTypes and diffIndexes, which is where the bug actually lived —
// reverting the raw comparison left the whole suite green because nothing
// exercised those two functions on colliding spellings.
func TestDiffSeesFieldChangeThatRendersIdentically(t *testing.T) {
	t.Parallel()

	const fromStore, toStore = "A__0B", "A__B"
	if userName(fromStore) != userName(toStore) {
		t.Fatalf("fixture is vacuous: %q and %q no longer render alike", fromStore, toStore)
	}
	oldMeta := metaWithRenamedField(t, fromStore)
	newMeta := metaWithRenamedField(t, toStore)

	t.Run("primary key", func(t *testing.T) {
		s := diffRecordTypes(oldMeta, newMeta)
		var found bool
		for _, e := range s.Changed {
			for _, c := range e.Changes {
				if c.Field == "primary_key" {
					found = true
					if c.Old == c.New {
						t.Errorf("primary_key change reported as %q -> %q — identical text "+
							"means the change is invisible", c.Old, c.New)
					}
				}
			}
		}
		if !found {
			t.Errorf("the PK moved from %q to %q and the diff reported NO primary_key "+
				"change: comparing rendered names collapses them", fromStore, toStore)
		}
	})

	t.Run("index fields", func(t *testing.T) {
		s := diffIndexes(oldMeta, newMeta)
		var found bool
		for _, e := range s.Changed {
			for _, c := range e.Changes {
				if c.Field == "fields" {
					found = true
					if c.Old == c.New {
						t.Errorf("fields change reported as %q -> %q — identical text means "+
							"the change is invisible", c.Old, c.New)
					}
				}
			}
		}
		if !found {
			t.Errorf("the index field moved from %q to %q and the diff reported NO fields "+
				"change: comparing rendered names collapses them", fromStore, toStore)
		}
	})
}

// THE DIFF IS DETERMINISTIC AND COLLISION-FREE AT A 1x2 POPULATION.
//
// The 1x1 case cannot see either hazard. An earlier pairwise fallback compared
// Name fields it had already rewritten, so the result depended on Go map
// iteration order and reproduced the very `+X / -X` collision it removed on
// roughly a quarter of runs; and rewriting only the colliding pair creates a
// fresh collision one step later (old {A__B, A__00B} -> new {A__0B, C__1X}).
//
// Two types per side, run repeatedly, asserting BOTH that no rendered name is
// shared across the buckets and that the whole output is identical every time.
func TestDiffIsDeterministicAndCollisionFreeAcrossBuckets(t *testing.T) {
	t.Parallel()

	// Renders: A__B->A__B, A__00B->A__0B, A__0B->A__B, C__1X->C$X.
	oldMeta := metaWithTwoRenamedTypes(t, "A__B", "A__00B")
	newMeta := metaWithTwoRenamedTypes(t, "A__0B", "C__1X")
	for _, m := range []*recordlayer.RecordMetaData{oldMeta, newMeta} {
		if _, amb := m.AmbiguousDeclaredNames(); amb {
			t.Fatal("fixture is wrong: a side is ambiguous by itself, so this would " +
				"exercise the ordinary gate rather than the cross-bucket one")
		}
	}

	render := func() string {
		s := diffRecordTypes(oldMeta, newMeta)
		sortSection(&s)
		var b strings.Builder
		for _, e := range s.Removed {
			b.WriteString("-" + e.Name + "\n")
		}
		for _, e := range s.Added {
			b.WriteString("+" + e.Name + "\n")
		}
		return b.String()
	}

	first := render()
	if strings.Count(first, "\n") < 4 {
		t.Fatalf("fixture did not land: expected 2 added + 2 removed, got:\n%s", first)
	}
	// No rendered name may appear on both sides — that is the `+X / -X` shape.
	s := diffRecordTypes(oldMeta, newMeta)
	for _, a := range s.Added {
		for _, r := range s.Removed {
			if a.Name == r.Name {
				t.Errorf("`+ %s` and `- %s` name two DIFFERENT stored types "+
					"(%s vs %s):\n%s", a.Name, r.Name, a.raw, r.raw, first)
			}
		}
	}
	// And the output must not depend on map iteration order.
	for i := 0; i < 200; i++ {
		if got := render(); got != first {
			t.Fatalf("diff output varies between runs (iteration %d):\nfirst:\n%s\ngot:\n%s",
				i, first, got)
		}
	}
}

// A CHANGED ENTRY CAN COLLIDE WITH A REMOVED ONE, WITH NO RENAME INVOLVED.
//
// old {Order:A__0B, Customer:A__B} -> new {Order:A__0B changed, Customer:C__1X}
// prints `~ A__B` for the CHANGED Order and `- A__B` for the REMOVED Customer:
// two different stored types under one printed name. Nothing was renamed, so
// reasoning about rename-halves misses it entirely — and an earlier version of
// the fallback both excluded Changed from its tally and ran before the Changed
// bucket was populated, so it could not have seen these entries at all.
func TestDiffChangedBucketParticipatesInCollisionFallback(t *testing.T) {
	t.Parallel()

	oldMeta := metaWithTwoRenamedTypes(t, "A__0B", "A__B")
	// Bump Order so it lands in CHANGED rather than being identical on both
	// sides -- without this the bucket is EMPTY and the assertion below is
	// vacuous, which is how the first version of this test passed under a
	// mutation that removed Changed from the tally entirely.
	newMeta := withSinceVersion(t, metaWithTwoRenamedTypes(t, "A__0B", "C__1X"), "A__0B", 1)
	// NOTE the asymmetry, which is what makes this case interesting: oldMeta IS
	// ambiguous alone (A__B escapes to A__0B, also declared), so its Removed
	// entry renders as the STORED A__B -- while newMeta is not, so its Changed
	// entry renders as the DECODED A__B. Two gates, each correct on its own
	// metadata, producing one printed name for two stored types.
	if _, amb := newMeta.AmbiguousDeclaredNames(); amb {
		t.Fatal("fixture is wrong: newMeta is ambiguous alone, so its Changed entry " +
			"would already render stored and the collision could not arise")
	}

	s := diffRecordTypes(oldMeta, newMeta)
	if len(s.Removed) == 0 || len(s.Changed) == 0 {
		t.Fatalf("fixture did not land: removed=%d changed=%d -- BOTH buckets must be "+
			"non-empty or the uniqueness assertion below is vacuous",
			len(s.Removed), len(s.Changed))
	}
	// Every printed name across all three buckets must be unique — that is the
	// whole claim, and Changed is a bucket like any other.
	seen := map[string][]string{}
	for _, b := range [][]diffEntry{s.Added, s.Removed, s.Changed} {
		for _, e := range b {
			seen[e.Name] = append(seen[e.Name], e.raw)
		}
	}
	for name, raws := range seen {
		if len(raws) > 1 {
			t.Errorf("printed name %q names %d different stored types (%v) — the diff "+
				"is saying something it does not mean", name, len(raws), raws)
		}
	}
}

// withSinceVersion returns md with one record type's since-version bumped, so
// the type lands in the diff's CHANGED bucket rather than Added/Removed.
func withSinceVersion(t *testing.T, md *recordlayer.RecordMetaData, storage string, since int32) *recordlayer.RecordMetaData {
	t.Helper()
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	var hit bool
	for _, rt := range p.GetRecordTypes() {
		if rt.GetName() == storage {
			rt.SinceVersion = proto.Int32(since)
			hit = true
		}
	}
	if !hit {
		t.Fatalf("no record type stored as %s to bump", storage)
	}
	if p.GetVersion() < since {
		p.Version = proto.Int32(since)
	}
	out, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	return out
}
