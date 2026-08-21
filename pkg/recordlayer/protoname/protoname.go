// Package protoname is the protobuf identifier escaping shared by every
// descriptor emitter in the tree.
//
// It is a LEAF package on purpose. The escaping is needed by the DDL-time
// emitter (pkg/relational/core/metadata), by pkg/recordlayer itself, and by
// the plan-time emitter that synthesises a descriptor for a COMPUTED record
// (pkg/recordlayer/query/plan/cascades/values) — and that last one cannot
// import pkg/recordlayer, which already imports it. One escaping, one
// implementation: a second copy is a second chance to diverge from Java on
// bytes that are wire.
package protoname

import (
	"fmt"
	"regexp"
	"strings"
)

// Port of Java's ProtoUtils name escaping (fdb-record-layer-core
// com.apple.foundationdb.record.util.ProtoUtils, ProtoUtils.java:40-90).
//
// A user identifier (table, struct type, or column name) becomes a protobuf
// identifier by escaping the three character sequences protobuf cannot carry
// in a name: "__" -> "__0", "$" -> "__1", "." -> "__2". ToUserIdentifier
// decodes those escape tokens back — but the mapping is NOT injective:
// a single leading underscore followed by a special character collides with
// a literal triple-underscore name ("_$" and "___1" both escape to
// "___1"; "_." and "___2" both escape to "___2"), because the escape scan
// replaces the special character without noticing the preceding "_". This
// is Java's OWN defect (INVALID_START_SEQUENCES guards only name STARTS,
// ProtoUtils.java), reproduced faithfully — the escaped names are WIRE, so
// diverging to fix it would make Go store different bytes than Java for
// the same DDL. The collision witnesses are pinned as reproduced-upstream
// in proto_names_test.go.
//
// These escaped names are WIRE: they are the message and field names inside
// the persisted RecordMetaData descriptor, so the escaping must match Java
// byte-for-byte (e.g. Java stores the struct type "x$$" as message
// "x__1__1" and the table "foo.tableA" as "foo__2tableA" — pinned by the
// RFC-204 descriptor byte-goldens).

const (
	doubleUnderscoreEscape = "__0"
	dollarEscape           = "__1"
	dotEscape              = "__2"
)

// invalidStartSequences mirrors Java's INVALID_START_SEQUENCES: names
// starting with one of these cannot be escaped reversibly.
var invalidStartSequences = []string{".", "$", doubleUnderscoreEscape, dollarEscape, dotEscape}

var validProtoNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// InvalidNameError mirrors Java's ProtoUtils.InvalidNameException: the
// identifier cannot be turned into (or is not) a protobuf-compliant name.
type InvalidNameError struct {
	Message string
}

func (e *InvalidNameError) Error() string { return e.Message }

// ToProtoBufCompliantName escapes a user identifier into a protobuf-compliant
// name. Mirrors ProtoUtils.toProtoBufCompliantName (ProtoUtils.java:51-66):
// a leading "__" is preserved verbatim with only the remainder escaped, and
// names starting with ".", "$", "__0", "__1" or "__2" are rejected because
// the escaping could not be reversed.
func ToProtoBufCompliantName(name string) (string, error) {
	for _, seq := range invalidStartSequences {
		if strings.HasPrefix(name, seq) {
			return "", &InvalidNameError{Message: fmt.Sprintf("name cannot start with %v", invalidStartSequences)}
		}
	}
	var translated string
	if strings.HasPrefix(name, "__") {
		translated = "__" + translateSpecialCharacters(name[2:])
	} else {
		if name == "" {
			return "", &InvalidNameError{Message: "name cannot be empty string"}
		}
		translated = translateSpecialCharacters(name)
	}
	if err := CheckValidProtoBufCompliantName(translated); err != nil {
		return "", err
	}
	return translated, nil
}

// CheckValidProtoBufCompliantName validates that name is a legal protobuf
// identifier ([A-Za-z_][A-Za-z0-9_]*). Mirrors
// ProtoUtils.checkValidProtoBufCompliantName, including Java's exact
// message wording ("it not" verbatim).
func CheckValidProtoBufCompliantName(name string) error {
	if !validProtoNamePattern.MatchString(name) {
		return &InvalidNameError{Message: name + " it not a valid protobuf identifier"}
	}
	return nil
}

// translateSpecialCharacters mirrors ProtoUtils.translateSpecialCharacters:
// sequential left-to-right whole-string replacements, in this exact order.
func translateSpecialCharacters(userIdentifier string) string {
	s := strings.ReplaceAll(userIdentifier, "__", doubleUnderscoreEscape)
	s = strings.ReplaceAll(s, "$", dollarEscape)
	return strings.ReplaceAll(s, ".", dotEscape)
}

// ToUserIdentifier reverses ToProtoBufCompliantName. Mirrors
// ProtoUtils.toUserIdentifier: replacements applied in the exact inverse
// order ("__2" -> ".", "__1" -> "$", "__0" -> "__").
//
// NEITHER DIRECTION IS INJECTIVE, and a caller that assumes either one is will
// bind the wrong field. Both facts are load-bearing and neither is obvious from
// the three substitutions above:
//
//   - ENCODING collides: `___1__2foo` decodes to `_$.foo`, so a DIFFERENT SQL
//     name (`___1.FOO`) encodes to something that case-folds onto it. Matching
//     a SQL name against storage by ENCODING the SQL name therefore accepts
//     fields the identifier does not name. Decode the storage names instead —
//     that is the direction every consumer already uses to answer "what is this
//     column called".
//   - DECODING collides too, which is easy to miss once the encode direction
//     has been rejected for the same reason: `__0_` and `___0` both decode to
//     `___`, so `foo__0_bar` and `foo___0bar` BOTH answer to the SQL name
//     `foo___bar`. A descriptor can hold two fields with one SQL spelling.
//
// So a lookup keyed on decoded names must handle COLLISIONS rather than take
// the first hit: which of two candidates wins is a property of the descriptor's
// field order and not of the query. This is not hypothetical — five successive
// defects in one lookup traced to this single unwritten fact, each fix correct
// about the coordinate it addressed and silent about the next.
//
// A DRAFT OF THIS PARAGRAPH SAID DDL CANNOT PRODUCE SUCH A PAIR. It can:
//
//	CREATE TABLE coll (id BIGINT, "___" BIGINT, "___0" BIGINT, PRIMARY KEY (id))
//
// Both names begin `__`, so both pass through ToProtoBufCompliantName UNCHANGED
// — and `___0` then decodes to `___`, because the decode scan sees the `__0`
// starting at index 1. Two distinct, legal, non-duplicate SQL columns; one
// decoded spelling; a row type that cannot be built. Reading even an UNRELATED
// column of that table fails.
//
// JAVA FAILS ON IT TOO, measured on a live JVM rather than assumed — `Multiple
// entries with same key: ___=…Type$Record$Field@…`, the same cause at the same
// point. So this is the upstream defect reproduced faithfully, not a Go
// divergence, and the escaped names being WIRE means the encoding cannot be
// changed to make the round trip total anyway. Pinned in
// conformance/dotted_and_recursive_seed_java_probe_test.go.
//
// What is Go's alone is that its internal failure is a PANIC, recovered at the
// driver boundary into XX000. A library that panics where it could return an
// error is design-principle 4, and it is tracked in RFC-238 §7 with this
// reproducer.
func ToUserIdentifier(protoIdentifier string) string {
	s := strings.ReplaceAll(protoIdentifier, dotEscape, ".")
	s = strings.ReplaceAll(s, dollarEscape, "$")
	return strings.ReplaceAll(s, doubleUnderscoreEscape, "__")
}
