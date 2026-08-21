package recordlayer

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

// renameRecordTypesTo rebuilds md with each record type named by a key of
// rename stored under its value. It exists because no checked-in proto has an
// ESCAPED record-type name, and the arms under test only fire for one -- a
// fixture that cannot express the condition leaves them undriven while green.
func renameRecordTypesTo(t *testing.T, rename map[string]string) *RecordMetaData {
	t.Helper()
	p, err := testMetaData(t).ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	p.JoinedRecordTypes = nil
	for _, msg := range p.GetRecords().GetMessageType() {
		if to, ok := rename[msg.GetName()]; ok {
			msg.Name = proto.String(to)
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if to, ok := rename[rt.GetName()]; ok {
			rt.Name = proto.String(to)
		}
	}
	// The union addresses record types by the `_TypeName` FIELD-NAME convention,
	// which takes precedence over the field's type reference in
	// setRecordsWithUnionName -- so renaming only the message makes the lookup of
	// the old name miss and the type is dropped entirely rather than renamed.
	for _, msg := range p.GetRecords().GetMessageType() {
		for _, f := range msg.GetField() {
			if to, ok := rename[strings.TrimPrefix(f.GetName(), "_")]; ok && strings.HasPrefix(f.GetName(), "_") {
				f.Name = proto.String("_" + to)
			}
		}
	}
	// The UNION message addresses each record type by field TypeName, which is
	// FULLY QUALIFIED (.pkg.Order). Renaming only the messages unlinks them and
	// they stop being record types at all -- so rewrite the references, matching
	// the last segment and preserving the package.
	for _, msg := range p.GetRecords().GetMessageType() {
		for _, f := range msg.GetField() {
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
	md, err := RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	return md
}

// TestGetRecordTypeResolvesAUserIdentifier pins the escape fallback in
// GetRecordType: a caller holding the SQL identifier `MY$TABLE` resolves the
// type stored as MY__1TABLE. Every other test in this package addresses
// GetRecordType with a name that is already in storage form, so the direct-hit
// arm is exercised heavily and the fallback arm was not exercised at all.
//
// The fallback is what lets an operator-facing surface print the SQL identifier
// while the metadata stays keyed by the storage name. Measured at 832 Test
// functions in this package: deleting the fallback reddens THIS test and no
// other, so without it the loss would surface only at a caller, at a distance.
func TestGetRecordTypeResolvesAUserIdentifier(t *testing.T) {
	t.Parallel()

	const storage, sql = "MY__1TABLE", "MY$TABLE"
	md := renameRecordTypesTo(t, map[string]string{"Order": storage})

	// Fixture guard: if the rename silently failed, every assertion below would
	// be about a type that is not there, and a miss would read as a fallback bug.
	if _, ok := md.RecordTypes()[storage]; !ok {
		names := make([]string, 0, len(md.RecordTypes()))
		for n := range md.RecordTypes() {
			names = append(names, n)
		}
		t.Fatalf("fixture did not land: no record type stored as %s; have %v", storage, names)
	}

	// The metadata map is keyed by the STORAGE name -- this is the reason an
	// operator-facing renderer that walks RecordTypes() raw prints MY__1TABLE.
	if _, ok := md.RecordTypes()[sql]; ok {
		t.Fatalf("RecordTypes() is keyed by the user identifier %s; the escape boundary has moved", sql)
	}

	byStorage := md.GetRecordType(storage)
	if byStorage == nil {
		t.Fatalf("GetRecordType(%q) missed on the direct key", storage)
	}
	bySQL := md.GetRecordType(sql)
	if bySQL == nil {
		t.Fatalf("GetRecordType(%q) missed: the escape fallback is gone, so a caller "+
			"holding the SQL identifier can no longer resolve the type stored as %s", sql, storage)
	}
	if byStorage != bySQL {
		t.Fatalf("GetRecordType(%q) and GetRecordType(%q) resolved to different types", storage, sql)
	}

	// A name that escapes to nothing stored must still miss -- the fallback may
	// not become a fuzzy match.
	if rt := md.GetRecordType("NO$SUCH"); rt != nil {
		t.Fatalf("GetRecordType(\"NO$SUCH\") resolved to %s; the fallback is matching too broadly", rt.Name)
	}
}

// TestGetRecordTypeMisResolvesAnAmbiguousPair pins the LIMIT of the fallback
// above, and it is deliberately a test of wrong-looking behaviour.
//
// GetRecordType's comment says the fallback "can never shadow a real type",
// which is true and stops one step short of the hazard. The escaping is not
// injective across the two namespaces: MY$TABLE is stored as MY__1TABLE, while
// a table whose SQL name IS MY__1TABLE is stored as MY__01TABLE. Declare both
// and the direct hit answers first, so a caller holding the SQL identifier
// MY__1TABLE is handed the type whose SQL name is MY$TABLE. Nothing is
// shadowed; the answer is simply the wrong entry, and — as
// AmbiguousDeclaredNames' own doc says — no ordering fixes it, because either
// order resolves one of the pair wrong.
//
// This is exactly why AmbiguousDeclaredNames exists and why the statistics
// gate REFUSES on an ambiguous schema rather than reporting numbers keyed by a
// name it cannot resolve. If someone later "fixes" GetRecordType by reordering
// its two lookups, this test fails and says why that does not help.
func TestGetRecordTypeMisResolvesAnAmbiguousPair(t *testing.T) {
	t.Parallel()

	// Order becomes the storage form of SQL `MY$TABLE`; Customer becomes the
	// storage form of SQL `MY__1TABLE`. The two SQL names are distinct, and one
	// of them equals the OTHER's storage name — that is the collision.
	const aStorage, aSQL = "MY__1TABLE", "MY$TABLE"
	const bStorage, bSQL = "MY__01TABLE", "MY__1TABLE"
	if ToUserIdentifier(aStorage) != aSQL || ToUserIdentifier(bStorage) != bSQL {
		t.Fatalf("fixture is wrong: %q->%q, %q->%q", aStorage, ToUserIdentifier(aStorage),
			bStorage, ToUserIdentifier(bStorage))
	}
	if aStorage != bSQL {
		t.Fatalf("fixture does not collide: %q must equal %q", aStorage, bSQL)
	}

	md := renameRecordTypesTo(t, map[string]string{"Order": aStorage, "Customer": bStorage})
	for _, want := range []string{aStorage, bStorage} {
		if _, ok := md.RecordTypes()[want]; !ok {
			t.Fatalf("fixture did not land: no record type stored as %s", want)
		}
	}

	// The metadata itself reports the collision — this is the signal callers are
	// expected to consult INSTEAD of trusting a lookup.
	names, ambiguous := md.AmbiguousDeclaredNames()
	if !ambiguous {
		t.Fatalf("AmbiguousDeclaredNames did not report the collision; it is the "+
			"guard that makes the mis-resolution below detectable, and it reported %v", names)
	}

	// The mis-resolution itself: asking by the SQL identifier MY__1TABLE returns
	// the type stored under it, whose SQL name is MY$TABLE — not the type the
	// caller named, which is stored as MY__01TABLE.
	got := md.GetRecordType(bSQL)
	if got == nil {
		t.Fatalf("GetRecordType(%q) missed entirely; expected the mis-resolution, not a miss", bSQL)
	}
	if got.Name != aStorage {
		// The reorder case gets its OWN message, and it has to live inside this
		// branch: `!= aStorage` already covers `== bStorage`, so a separate
		// if-block below would be unreachable and the explanation it carries
		// would never print.
		why := "the ambiguity hazard has changed shape, and AmbiguousDeclaredNames' " +
			"reasoning needs re-reading"
		if got.Name == bStorage {
			why = "the lookup now resolves the SQL name to its own type -- reordering " +
				"GetRecordType's two lookups does NOT fix this class, it only moves " +
				"which of the pair resolves wrong. If the escaping became injective " +
				"or the catalog became user-keyed, the ambiguity gates can be " +
				"revisited; until then they are load-bearing"
		}
		t.Fatalf("GetRecordType(%q) resolved to %q, want the mis-resolution to %q: %s",
			bSQL, got.Name, aStorage, why)
	}
}
