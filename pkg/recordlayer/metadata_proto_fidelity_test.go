package recordlayer

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
)

// The STRUCT-LEVEL round trip: proto → RecordMetaData → proto, asserted field by
// field against the proto we started from.
//
// This test class did not exist, and its absence is why six MetaData fields
// could be dropped in silence. The round trip the suite did have —
// metadata_store_test.go's "saves and loads metadata proto" — is a BYTES round
// trip: it hands a *gen.MetaData to FDBMetaDataStore, reads it back, and
// compares with proto.Equal. That path stores the serialized bytes verbatim and
// never constructs a RecordMetaData at all, so every field survives it by
// construction, including fields no Go code has ever heard of. It can prove the
// STORE is faithful. It cannot see ToProto or RecordMetaDataFromProto, which is
// exactly where the loss was.
//
// The distinction is worth stating because the two tests look interchangeable
// from their names, and the wrong one reads as coverage of the right one.
//
// What the fixture must therefore do is populate EVERY field of MetaData,
// including the ones the Go port does not model, and demand them all back.
func TestMetaDataProtoRoundTripIsFieldComplete(t *testing.T) {
	t.Parallel()

	original := fidelityMetaDataProto(t)

	// Every field of the message must be set in the fixture, or the round trip
	// is only asserted over the subset someone remembered. Checked reflectively
	// so that a field ADDED to the proto in a later Java version fails here
	// rather than being quietly untested — which is the precise failure this
	// whole test exists to catch, one version later.
	assertEveryMetaDataFieldPopulated(t, original)

	md, err := RecordMetaDataFromProto(original)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	got, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}

	// Field by field, so a failure names the field that was dropped rather than
	// dumping two descriptors. Whole-message equality is asserted afterwards, to
	// catch anything the per-field list forgot.
	for _, f := range []struct {
		name string
		want any
		got  any
		why  string
	}{
		{
			"subspace_key_counter", original.GetSubspaceKeyCounter(), got.GetSubspaceKeyCounter(),
			"the counter governs the on-disk subspace key of every index added AFTER this " +
				"metadata is reloaded. Per-index keys round-trip on their own, so existing " +
				"data survives losing this — what is lost is the SCHEME, and the next index " +
				"Java adds gets a NAME-based key with nothing reporting the change",
		},
		{
			"uses_subspace_key_counter", original.GetUsesSubspaceKeyCounter(), got.GetUsesSubspaceKeyCounter(),
			"the counter and its flag must travel together; Java REFUSES to load either " +
				"half alone (RecordMetaDataBuilder.java:279-285), so dropping this one " +
				"produces metadata Java cannot read",
		},
		{
			"joined_record_types", len(original.GetJoinedRecordTypes()), len(got.GetJoinedRecordTypes()),
			"a Go tool that loads a Java application's metadata and saves it back must not " +
				"DELETE the application's joined types. The port does not model them; that " +
				"is a reason not to interpret them, not a reason to drop them",
		},
		{
			"unnested_record_types", len(original.GetUnnestedRecordTypes()), len(got.GetUnnestedRecordTypes()),
			"same as joined_record_types",
		},
		{
			"user_defined_functions", len(original.GetUserDefinedFunctions()), len(got.GetUserDefinedFunctions()),
			"out of scope for the port, which is not the same as safe to discard",
		},
		{
			"views", len(original.GetViews()), len(got.GetViews()),
			"out of scope for the port, which is not the same as safe to discard",
		},
	} {
		if f.got != f.want {
			t.Errorf("field %s did NOT survive the round trip: got %v, want %v\n%s",
				f.name, f.got, f.want, f.why)
		}
	}

	// And the whole message, which is what makes the per-field list above a
	// diagnostic rather than the assertion. Anything the list omits fails here.
	if !proto.Equal(original, got) {
		t.Errorf("the metadata proto did not survive proto → RecordMetaData → proto.\n"+
			"Whatever differs is a field the Go port parsed away and could not put "+
			"back; a Go tool round-tripping another engine's metadata writes this loss "+
			"to the store.\n\nwant:\n%s\ngot:\n%s",
			prototext.Format(original), prototext.Format(got))
	}
}

// TestMetaDataProtoRoundTripPreservesUnmodelledContents is the other half of the
// preservation claim, and without it the counts above are satisfiable by
// re-emitting the right NUMBER of empty messages.
func TestMetaDataProtoRoundTripPreservesUnmodelledContents(t *testing.T) {
	t.Parallel()

	original := fidelityMetaDataProto(t)
	md, err := RecordMetaDataFromProto(original)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	got, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}

	if n := len(got.GetJoinedRecordTypes()); n != 1 {
		t.Fatalf("joined types: got %d, want 1", n)
	}
	jt := got.GetJoinedRecordTypes()[0]
	if jt.GetName() != "OrderWithCustomer" {
		t.Errorf("joined type name: got %q, want %q — the message survived but its "+
			"CONTENTS did not, so the count assertion was passing on an empty shell",
			jt.GetName(), "OrderWithCustomer")
	}
	if n := len(jt.GetJoinConstituents()); n != 2 {
		t.Errorf("joined type constituents: got %d, want 2 — the nested repeated "+
			"fields must survive too, not just the top-level message", n)
	}
	if n := len(got.GetViews()); n != 1 || got.GetViews()[0].GetDefinition() == "" {
		t.Errorf("view definition did not survive: %v", got.GetViews())
	}

	// The carried protos must be COPIES. Mutating the caller's original after
	// the load, or the emitted result afterwards, must not reach the metadata's
	// own state — otherwise "preserved" means "aliased", and the second ToProto
	// returns something the first caller edited.
	original.JoinedRecordTypes[0].Name = proto.String("MUTATED")
	got.JoinedRecordTypes[0].Name = proto.String("ALSO_MUTATED")
	again, err := md.ToProto()
	if err != nil {
		t.Fatalf("second ToProto: %v", err)
	}
	if again.GetJoinedRecordTypes()[0].GetName() != "OrderWithCustomer" {
		t.Errorf("the carried protos are ALIASED, not copied: a mutation through the "+
			"caller's proto reached the metadata's own state (name is now %q). "+
			"ToProto's result is the caller's to modify",
			again.GetJoinedRecordTypes()[0].GetName())
	}
}

// TestSubspaceKeyCounterPairValidation ports Java's refusal of a half-written
// pair (RecordMetaDataBuilder.loadSubspaceKeySettingsFromProto, :279-285).
//
// Leniency here would be a one-way door: Go would accept metadata that Java then
// cannot load at all. And each half alone is ambiguous in a way that decides
// on-disk key assignment — a counter with the flag clear reads as "name-based,
// and here is an unused number"; the flag with no counter restarts assignment
// from zero and hands a new index a key an existing index already owns.
func TestSubspaceKeyCounterPairValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mutate  func(*gen.MetaData)
		wantErr string
	}{
		{
			name: "counter_without_flag",
			mutate: func(md *gen.MetaData) {
				md.SubspaceKeyCounter = proto.Int64(7)
				md.UsesSubspaceKeyCounter = nil
			},
			wantErr: "subspaceKeyCounter is set but usesSubspaceKeyCounter is not set",
		},
		{
			name: "flag_without_counter",
			mutate: func(md *gen.MetaData) {
				md.SubspaceKeyCounter = nil
				md.UsesSubspaceKeyCounter = proto.Bool(true)
			},
			wantErr: "usesSubspaceKeyCounter is set but subspaceKeyCounter is not set",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := fidelityMetaDataProto(t)
			tc.mutate(md)
			_, err := RecordMetaDataFromProto(md)
			if err == nil {
				t.Fatalf("a half-written subspace-key-counter pair was ACCEPTED. Java "+
					"refuses it, so this metadata is loadable by Go and not by Java — "+
					"and the missing half decides how future indexes are keyed. want "+
					"error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name the problem; want it to contain %q",
					err.Error(), tc.wantErr)
			}
		})
	}

	// Neither half present is the ordinary case — the scheme is simply off, and
	// Java emits nothing at all in that state. It must NOT error.
	md := fidelityMetaDataProto(t)
	md.SubspaceKeyCounter = nil
	md.UsesSubspaceKeyCounter = nil
	loaded, err := RecordMetaDataFromProto(md)
	if err != nil {
		t.Fatalf("metadata with the counter scheme OFF was rejected: %v", err)
	}
	if loaded.UsesSubspaceKeyCounter() {
		t.Error("the counter scheme is enabled on metadata that declared neither field")
	}
	// And it must not resurrect the pair on the way out: Java emits both only
	// when the scheme is on, so emitting a zero counter here would turn a
	// scheme-off metadata into a scheme-on one on every save.
	out, err := loaded.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	if out.SubspaceKeyCounter != nil || out.UsesSubspaceKeyCounter != nil {
		t.Errorf("a scheme-OFF metadata emitted the counter pair (counter=%v flag=%v). "+
			"Java emits them only inside `if (usesSubspaceKeyCounter())`, so this would "+
			"silently switch the store's index-key assignment scheme ON",
			out.SubspaceKeyCounter, out.UsesSubspaceKeyCounter)
	}
}

// TestSubspaceKeyCounterSurvivesReloadAndKeepsAssigning is the BEHAVIOURAL half:
// the counter is not a number to carry around, it is the state that decides the
// on-disk prefix of the next index.
//
// The silent-scheme-change this guards is the whole reason fields 10/11 are not
// a documented scope cut. Reload a counter-keyed metadata, add an index, and the
// new index must get the NEXT counter value — not a name-based key, and not a
// value that collides with an index already in the store.
func TestSubspaceKeyCounterSurvivesReloadAndKeepsAssigning(t *testing.T) {
	t.Parallel()

	builder := fidelityBuilder(t).EnableCounterBasedSubspaceKeys()
	builder.AddIndex("Order", NewIndex("by_price", Field("price")))
	builder.AddIndex("Customer", NewIndex("by_email", Field("email")))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := md.GetIndex("by_price").SubspaceTupleKey(); got != int64(1) {
		t.Fatalf("first counter-keyed index got subspace key %v, want int64(1)", got)
	}
	if got := md.GetIndex("by_email").SubspaceTupleKey(); got != int64(2) {
		t.Fatalf("second counter-keyed index got subspace key %v, want int64(2)", got)
	}
	if md.GetSubspaceKeyCounter() != 2 {
		t.Fatalf("counter after two indexes = %d, want 2", md.GetSubspaceKeyCounter())
	}

	// Round trip.
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	reloaded, err := RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	if !reloaded.UsesSubspaceKeyCounter() {
		t.Fatal("the counter SCHEME did not survive the round trip. Every index added " +
			"from here gets a name-based subspace key instead of a counter one — a " +
			"silent change of on-disk key assignment that no error reports and that " +
			"the per-index keys, which do survive, disguise")
	}
	if reloaded.GetSubspaceKeyCounter() != 2 {
		t.Fatalf("counter after reload = %d, want 2", reloaded.GetSubspaceKeyCounter())
	}

	// The indexes that came back must keep the keys they were stored with. This
	// is the data-safety half: re-numbering them would move each index's on-disk
	// prefix off the entries already written under it.
	if got := reloaded.GetIndex("by_price").SubspaceTupleKey(); got != int64(1) {
		t.Errorf("reloaded by_price subspace key = %v, want int64(1) — the load "+
			"RE-ASSIGNED a key to an index that already owns data at the old one", got)
	}
	if got := reloaded.GetIndex("by_email").SubspaceTupleKey(); got != int64(2) {
		t.Errorf("reloaded by_email subspace key = %v, want int64(2)", got)
	}

	// And a NEW index continues the sequence rather than restarting it or
	// falling back to its name.
	next := fidelityBuilderFrom(t, p)
	next.AddIndex("Order", NewIndex("by_quantity", Field("quantity")))
	nextMD, err := next.Build()
	if err != nil {
		t.Fatalf("build after reload: %v", err)
	}
	got := nextMD.GetIndex("by_quantity").SubspaceTupleKey()
	if got == "by_quantity" {
		t.Fatal("an index added after reload got a NAME-based subspace key. The " +
			"counter scheme was lost in the round trip, so Java and Go now disagree " +
			"about how this store assigns index keys")
	}
	if got != int64(3) {
		t.Fatalf("an index added after reload got subspace key %v, want int64(3) — "+
			"the counter must CONTINUE from the stored value. A restart at 1 hands "+
			"this index the prefix by_price already owns, and two indexes then write "+
			"into one subspace", got)
	}
}

// TestCounterBasedAssignmentSkipsExplicitSubspaceKeys ports the
// !hasExplicitSubspaceKey() half of Java's addIndexCommon (:1101-1102), which Go
// did not have.
//
// Without it, a caller who chose a subspace key and then added the index to a
// counter-based builder had that key silently OVERWRITTEN — the index's on-disk
// prefix moved off its data with nothing reported. It is also what makes a
// reload safe, since every index deserialized from proto counts as explicit.
func TestCounterBasedAssignmentSkipsExplicitSubspaceKeys(t *testing.T) {
	t.Parallel()

	builder := fidelityBuilder(t).EnableCounterBasedSubspaceKeys()
	explicit := NewIndex("by_price", Field("price")).SetSubspaceKey(int64(42))
	builder.AddIndex("Order", explicit)
	builder.AddIndex("Customer", NewIndex("by_email", Field("email")))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if got := md.GetIndex("by_price").SubspaceTupleKey(); got != int64(42) {
		t.Fatalf("an explicitly-keyed index was RE-KEYED by the counter: got %v, want "+
			"int64(42). Java skips indexes whose key was chosen "+
			"(RecordMetaDataBuilder.java:1101). Overwriting it moves the index's "+
			"on-disk prefix away from the entries already written under it", got)
	}
	// And the explicit key must not consume a counter value either — Java only
	// increments inside the branch it skipped.
	if got := md.GetIndex("by_email").SubspaceTupleKey(); got != int64(1) {
		t.Fatalf("the counter advanced past an index it did not key: by_email got %v, "+
			"want int64(1)", got)
	}
}

// TestDeclaredSyntheticTypesAreVisibleAndIndexesOverThemRefused is the loud
// half of carrying fields 12/13.
//
// Carrying a joined type verbatim must not make it look SUPPORTED. Two things
// are asserted, and they are different claims:
//
//   - the metadata reports that it declares synthetic types, so a caller
//     computing over "all record types" can tell that its view is partial rather
//     than complete; and
//   - an index defined OVER a synthetic type is REFUSED at load, loudly, because
//     that index's record type is one this port cannot build or maintain.
//     Accepting it and silently treating the type as absent is the failure mode
//     — the index would be listed, planned against, and maintained against
//     nothing.
func TestDeclaredSyntheticTypesAreVisibleAndIndexesOverThemRefused(t *testing.T) {
	t.Parallel()

	md, err := RecordMetaDataFromProto(fidelityMetaDataProto(t))
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	if !md.DeclaresSyntheticRecordTypes() {
		t.Fatal("metadata carrying a joined record type reports that it declares none. " +
			"IsSynthetic() is hardcoded false on every RecordType — sound only because " +
			"no synthetic type ever BECOMES one — so this is the only way a caller can " +
			"learn that the record-type set it is about to compute over is partial")
	}
	names := md.SyntheticRecordTypeNames()
	if len(names) != 2 || names[0] != "OrderWithCustomer" {
		t.Errorf("synthetic type names = %v, want [OrderWithCustomer UnnestedOrder]", names)
	}

	// An index over the joined type must be refused rather than dropped.
	withIndex := fidelityMetaDataProto(t)
	withIndex.Indexes = append(withIndex.Indexes, &gen.Index{
		Name:           proto.String("joined_idx"),
		Type:           proto.String(IndexTypeValue),
		RecordType:     []string{"OrderWithCustomer"},
		RootExpression: Field("order_id").ToKeyExpression(),
	})
	if _, err := RecordMetaDataFromProto(withIndex); err == nil {
		t.Fatal("an index over a JOINED record type was accepted. The port cannot build " +
			"or maintain that index — its record type does not exist here — so accepting " +
			"it yields metadata whose index list contains an index backed by nothing. " +
			"Fail closed instead")
	} else if !strings.Contains(err.Error(), "OrderWithCustomer") {
		t.Errorf("the refusal does not name the type it refused for: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func fidelityBuilder(t *testing.T) *RecordMetaDataBuilder {
	t.Helper()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	return b
}

// fidelityBuilderFrom rebuilds a builder from a stored proto, the way a tool
// that loads metadata and then edits it does.
func fidelityBuilderFrom(t *testing.T, p *gen.MetaData) *RecordMetaDataBuilder {
	t.Helper()
	md, err := RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	// Carry the scheme forward exactly as a reload does.
	if md.UsesSubspaceKeyCounter() {
		b.EnableCounterBasedSubspaceKeys()
		if c := md.GetSubspaceKeyCounter(); c > 0 {
			b.SetSubspaceKeyCounter(c)
		}
	}
	for _, idx := range md.GetAllIndexes() {
		for _, rt := range md.RecordTypesForIndex(idx) {
			b.AddIndex(rt.Name, idx)
		}
	}
	return b
}

// fidelityMetaDataProto builds a MetaData proto with EVERY field populated,
// including the four the Go port does not model. It is written in proto form
// rather than through the builder precisely because the builder cannot express
// fields 12-15 — a fixture built through the Go API could never have caught
// them being dropped, which is how they stayed dropped.
func fidelityMetaDataProto(t *testing.T) *gen.MetaData {
	t.Helper()

	builder := fidelityBuilder(t).EnableCounterBasedSubspaceKeys()
	builder.AddIndex("Order", NewIndex("by_price", Field("price")))
	builder.SetStoreRecordVersions(true)
	builder.SetSplitLongRecords(true)
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("fixture ToProto: %v", err)
	}

	// Fields 10 and 11 are set EXPLICITLY here rather than inherited from the
	// ToProto call above, and the distinction decides what this fixture can
	// prove. The builder path reaches them through the very function under test,
	// so a fixture that took them from there would vanish along with the code
	// that emits them — the round-trip assertion would then pass vacuously (both
	// sides absent) and only the fixture-completeness check would notice, which
	// reports "your fixture is incomplete" for what is actually "the port
	// dropped a field". Writing them in by hand makes the two sides independent.
	//
	// The value is deliberately AHEAD of the one index built above (which holds
	// counter key 1): a store whose counter has run ahead of its current indexes
	// is the ordinary state after any index is dropped, and it is the state where
	// losing the counter does the damage.
	p.SubspaceKeyCounter = proto.Int64(5)
	p.UsesSubspaceKeyCounter = proto.Bool(true)

	// A former index, so field 6 is non-empty.
	p.FormerIndexes = append(p.FormerIndexes, &gen.FormerIndex{
		FormerName:     proto.String("gone_idx"),
		RemovedVersion: proto.Int32(3),
		AddedVersion:   proto.Int32(1),
	})
	// Field 7 (deprecated but still a field).
	p.RecordCountKey = Field("order_id").ToKeyExpression()

	// Fields 12-15: what a Java application writes and the Go port does not model.
	p.JoinedRecordTypes = append(p.JoinedRecordTypes, &gen.JoinedRecordType{
		Name: proto.String("OrderWithCustomer"),
		JoinConstituents: []*gen.JoinedRecordType_JoinConstituent{
			{Name: proto.String("o"), RecordType: proto.String("Order")},
			{Name: proto.String("c"), RecordType: proto.String("Customer"), OuterJoined: proto.Bool(true)},
		},
		Joins: []*gen.JoinedRecordType_Join{{
			Left:            proto.String("o"),
			LeftExpression:  Field("customer_id").ToKeyExpression(),
			Right:           proto.String("c"),
			RightExpression: Field("customer_id").ToKeyExpression(),
		}},
	})
	p.UnnestedRecordTypes = append(p.UnnestedRecordTypes, &gen.UnnestedRecordType{
		Name: proto.String("UnnestedOrder"),
		NestedConstituents: []*gen.UnnestedRecordType_NestedConstituent{
			{Name: proto.String("parent"), TypeName: proto.String("Order")},
		},
	})
	p.UserDefinedFunctions = append(p.UserDefinedFunctions, &gen.PUserDefinedFunction{
		SpecificFunction: &gen.PUserDefinedFunction_SqlFunction{
			SqlFunction: &gen.PRawSqlFunction{
				Name:       proto.String("double_it"),
				Definition: proto.String("CREATE FUNCTION double_it(x BIGINT) AS SELECT x * 2"),
			},
		},
	})
	p.Views = append(p.Views, &gen.PView{
		Name:       proto.String("big_orders"),
		Definition: proto.String("SELECT * FROM Order WHERE price > 100"),
	})

	return p
}

// exemptFromFidelityFixture names the fields the completeness check below does
// NOT require the fixture to populate, with the reason. An entry here is a
// stated fact about the field, not a place to park an inconvenient one — adding
// to this map is how the check stops catching the defect it exists for, so each
// entry has to say why the field cannot be populated rather than why it was not.
var exemptFromFidelityFixture = map[string]bool{
	// `dependencies` (9) is DERIVED, not carried: ToProto recomputes it by
	// walking the records descriptor's transitive imports, minus Java's
	// defaultExcludedDependencies. The demo schema this fixture is built on
	// imports exactly one file, record_metadata_options.proto, and that file is
	// on the exclusion list — so an empty `dependencies` is the CORRECT output
	// here, and a fixture that forced a value into it would be asserting that
	// ToProto echoes a field it is supposed to recompute.
	//
	// Populating it honestly needs a records schema importing a non-excluded
	// proto, which does not exist in this repo's test fixtures. The field is not
	// untested — it is derived from the descriptor, which the whole-message
	// comparison covers — it is only not COVERED BY THIS CHECK.
	"dependencies": true,
}

// assertEveryMetaDataFieldPopulated fails if any field of MetaData is unset in
// the fixture. Reflective rather than a hand-written list, so that a field added
// to the proto by a later Java version arrives here as a FAILURE rather than as
// silence — which is the same defect this file exists to close, one version on.
func assertEveryMetaDataFieldPopulated(t *testing.T, p *gen.MetaData) {
	t.Helper()
	desc := p.ProtoReflect().Descriptor()
	fields := desc.Fields()
	var missing []string
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if exemptFromFidelityFixture[string(f.Name())] {
			continue
		}
		if f.IsList() {
			if p.ProtoReflect().Get(f).List().Len() == 0 {
				missing = append(missing, string(f.Name()))
			}
			continue
		}
		if !p.ProtoReflect().Has(f) {
			missing = append(missing, string(f.Name()))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the round-trip fixture leaves these MetaData fields UNSET: %v.\n"+
			"An unset field is a field this test cannot prove survives ToProto/FromProto, "+
			"and every field dropped by the port was dropped in exactly that silence. "+
			"Populate them in fidelityMetaDataProto.", missing)
	}
}

// TestMetaDataExtensionRangeSurvivesRoundTrip covers the half of the message the
// field-by-field test above cannot see: the declared extension range
// (`extensions 1000 to 2000`), which the generated Go type has no fields for.
//
// These are genuinely unknown fields, and the intuition that protobuf therefore
// preserves them is wrong HERE specifically. Unknown-field preservation keeps
// the bytes attached to the message they were parsed into; ToProto constructs a
// fresh MetaData and copies modelled fields onto it, so the original's unknown
// bytes have no route to the result. They were being dropped exactly as fields
// 12-15 were — the same defect reached from the opposite direction, one assumed
// unknown and not, the other genuinely unknown and still not carried.
//
// The extension range is where applications and downstream layers hang their own
// metadata, so a Go tool round-tripping another engine's metadata deleted it.
//
// The fixture-completeness check cannot catch this: it walks the descriptor's
// declared FIELDS, and an extension is by construction not one of them.
func TestMetaDataExtensionRangeSurvivesRoundTrip(t *testing.T) {
	t.Parallel()

	original := fidelityMetaDataProto(t)

	// Two extension fields inside the declared 1000-2000 range, of different
	// wire types, so a carry that mishandles length-delimited data is caught.
	var raw []byte
	raw = protowire.AppendTag(raw, 1001, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 12345)
	raw = protowire.AppendTag(raw, 1002, protowire.BytesType)
	raw = protowire.AppendBytes(raw, []byte("tenant-owned metadata"))
	original.ProtoReflect().SetUnknown(protoreflect.RawFields(raw))

	md, err := RecordMetaDataFromProto(original)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	got, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}

	gotRaw := []byte(got.ProtoReflect().GetUnknown())
	if len(gotRaw) == 0 {
		t.Fatalf("the MetaData extension range did NOT survive the round trip: %d bytes "+
			"in, 0 bytes out. ToProto builds a fresh message, so protobuf's unknown-field "+
			"preservation never reaches these — a Go tool that loads and saves another "+
			"application's metadata deletes everything it hung on the extension range",
			len(raw))
	}
	if !bytes.Equal(gotRaw, raw) {
		t.Errorf("extension bytes changed across the round trip:\n got %x\nwant %x", gotRaw, raw)
	}

	// And the whole message, which is what the round trip actually promises.
	if !proto.Equal(original, got) {
		t.Errorf("metadata with extension fields did not survive the round trip.\nwant:\n%s\ngot:\n%s",
			prototext.Format(original), prototext.Format(got))
	}

	// The carried bytes must be a COPY, on both sides — the same requirement the
	// carried messages have, and easier to get wrong for a slice.
	original.ProtoReflect().SetUnknown(protoreflect.RawFields(nil))
	again, err := md.ToProto()
	if err != nil {
		t.Fatalf("second ToProto: %v", err)
	}
	if !bytes.Equal([]byte(again.ProtoReflect().GetUnknown()), raw) {
		t.Errorf("clearing the caller's unknown fields reached the metadata's own state: "+
			"second ToProto returned %x, want %x",
			[]byte(again.ProtoReflect().GetUnknown()), raw)
	}
}
