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
func ToUserIdentifier(protoIdentifier string) string {
	s := strings.ReplaceAll(protoIdentifier, dotEscape, ".")
	s = strings.ReplaceAll(s, dollarEscape, "$")
	return strings.ReplaceAll(s, doubleUnderscoreEscape, "__")
}
