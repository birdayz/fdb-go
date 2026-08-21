// Package protoname is the protobuf identifier escaping shared by every
// descriptor emitter in the tree, and the DISPLAY policy that decides when an
// escaped name may safely be shown as the SQL identifier it came from.
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
// The DISPLAY side of the package answers the same fact differently and on
// purpose: SafeDecoderOver goes all-or-nothing across a whole output, because
// under a collision every stored spelling is already a correct label and
// rewriting some of them invents new ones. A LOOKUP cannot take that route — it
// has to resolve one name — so it declines instead. Same fact, two consumers,
// two right answers; neither one is the general policy.
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
// JAVA FAILS ON IT TOO, but with a DIFFERENT BLAST RADIUS, and that difference
// is the defect. Measured on a live JVM:
//
//	SELECT id FROM an_unrelated_table   Java ANSWERS      Go fails
//	SELECT id FROM coll                 Java fails        Go fails
//
// Java's failure is TABLE-LOCAL. Go's takes down every query in the schema,
// including tables sharing nothing with the colliding one. A draft here read
// "both engines fail" as upstream-faithful; that came from a probe whose SETUP
// inserted into the colliding table, so Java was failing on the INSERT and every
// later query inherited it.
//
// The escaped names are WIRE, so the encoding cannot change — reproducing Java
// means failing on `coll` and ANSWERING on everything else. Pinned in
// conformance/dotted_and_recursive_seed_java_probe_test.go.
//
// Go's failure is also a PANIC, recovered at the driver boundary into XX000. A
// library that panics where it could return an error is design principle 4.
//
// Both halves — the panic and the schema-wide scope — are RFC-238 §5 criterion
// (8), with the commands that make them checkable; §7b narrates how they were
// measured. They are TWO mechanisms, not one. The colliding table's own read
// fails table-locally, where it should, at the scan leaf that builds a row type
// from just the table the query names. The schema-wide part is elsewhere:
// buildMatchCandidates builds a positional type for every record type in the
// metadata that has a primary key and a descriptor, so ONE unbuildable table
// aborts the candidate set for all of them, and removing the panic does not
// change that. Java is table-local because MetaDataPlanContext.forRootReference
// narrows to the record types the QUERY names before building anything — not
// because it tolerates a bad table.
func ToUserIdentifier(protoIdentifier string) string {
	s := strings.ReplaceAll(protoIdentifier, dotEscape, ".")
	s = strings.ReplaceAll(s, dollarEscape, "$")
	return strings.ReplaceAll(s, doubleUnderscoreEscape, "__")
}

// DecodeOnceIfReversible returns the SQL identifier for a stored name, but only
// when the decoded spelling provably re-encodes to what was stored.
//
// Record-layer metadata does not only come from the SQL layer —
// RecordMetaDataBuilder.SetRecords copies protobuf identifiers verbatim — so a
// record type may legally be named __0Order having never been escaped from
// anything. Decoding that yields __Order, which re-encodes to __Order and NOT
// to __0Order, so the name shown would resolve to nothing. The round trip is
// the provenance test and it needs no extra bookkeeping.
//
// It does NOT prove the decoded spelling is safe to OFFER: it says the second
// step of a two-step lookup lands on the right entry, and nothing about the
// first. Use SafeDecoderOver when other names are printed alongside.
func DecodeOnceIfReversible(stored string) string {
	user := ToUserIdentifier(stored)
	if user == stored {
		return stored
	}
	back, err := ToProtoBufCompliantName(user)
	if err != nil || back != stored {
		return stored
	}
	return user
}

// SafeDecoderOver decides ONE decoding policy for a whole rendered output, from
// every name that output will print.
//
// Per-value decisions are not enough and the split is easy to miss: decide
// separately for two lists and a colliding pair straddles them, each list is
// individually correct, and the output still shows one label for two different
// stored types.
//
// `decoded` are names this output will render through the returned function.
// `verbatim` are names it prints unchanged — synthetic record types, which Java
// stores exactly as the caller passed them and which must never be decoded.
// They take part in the decision without being subject to it: a decoded name
// that equals one of them is the same one-label-two-things hazard.
//
// Returns DecodeOnceIfReversible when unambiguous, identity otherwise —
// all-or-nothing, because under a collision every stored name is already a
// correct answer and a selective rewrite creates second-order collisions.
func SafeDecoderOver(decoded, verbatim []string) func(string) string {
	identity := func(s string) string { return s }
	printed := make(map[string]struct{}, len(decoded)+len(verbatim))
	for _, s := range decoded {
		printed[s] = struct{}{}
	}
	for _, s := range verbatim {
		printed[s] = struct{}{}
	}
	// Keyed by the DECODED name, valued by the STORED one it came from: a repeat
	// of the same name is not a collision, and a set cannot tell the two apart.
	// Probed -- SafeDecoderOver([]{"MY__1TABLE","MY__1TABLE"}, nil) used to return
	// the stored spelling while the single-element call decoded, so any caller
	// passing overlapping lists silently lost decoding for the whole output.
	seen := make(map[string]string, len(decoded))
	for _, s := range decoded {
		d := DecodeOnceIfReversible(s)
		// The decoded spelling is some OTHER printed name, so the row would carry
		// a label that means a different thing.
		if _, isOthers := printed[d]; isOthers && d != s {
			return identity
		}
		// Two names decoding alike would lose a row outright. Measured never to
		// fire over the valid-proto-name space — but the reason is the arm above,
		// not DecodeOnceIfReversible's round trip, which only covers the case
		// where both names decode. Checked rather than argued.
		if from, dup := seen[d]; dup && from != s {
			return identity
		}
		seen[d] = s
	}
	return DecodeOnceIfReversible
}
