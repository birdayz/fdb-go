package recordlayer

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

// renameRecordTypeTo rebuilds md with the record type named `from` stored under
// `to`. It exists because no checked-in proto has an ESCAPED record-type name,
// and the arm under test only fires for one -- a fixture that cannot express
// the condition would leave the arm undriven while reporting green.
func renameRecordTypeTo(t *testing.T, from, to string) *RecordMetaData {
	t.Helper()
	p, err := testMetaData(t).ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	p.JoinedRecordTypes = nil
	for _, msg := range p.GetRecords().GetMessageType() {
		if msg.GetName() == from {
			msg.Name = proto.String(to)
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if rt.GetName() == from {
			rt.Name = proto.String(to)
		}
	}
	// The union addresses record types by the `_TypeName` FIELD-NAME convention,
	// which takes precedence over the field's type reference in
	// setRecordsWithUnionName -- so renaming only the message makes the lookup of
	// the old name miss and the type is dropped entirely rather than renamed.
	for _, msg := range p.GetRecords().GetMessageType() {
		for _, f := range msg.GetField() {
			if f.GetName() == "_"+from {
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
			if short == from {
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
	md := renameRecordTypeTo(t, "Order", storage)

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
