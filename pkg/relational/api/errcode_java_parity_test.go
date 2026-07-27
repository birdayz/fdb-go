package api

import (
	"sort"
	"strings"
	"testing"
)

// javaErrorCodes is the complete SQLSTATE set of Java's
// com.apple.foundationdb.relational.api.exceptions.ErrorCode enum at the pinned
// tag (4.12.11.0), captured mechanically so the parity check below is a diff
// against Java rather than against someone's recollection of Java.
//
// Regenerate after a Java version bump with:
//
//	grep -oE '^[[:space:]]+[A-Z_0-9]+\("[0-9A-Z]{5}"\)' \
//	  fdb-record-layer/fdb-relational-api/src/main/java/com/apple/foundationdb/\
//	  relational/api/exceptions/ErrorCode.java |
//	  grep -oE '"[0-9A-Z]{5}"' | tr -d '"' | sort
//
// The Java tree is gitignored, so this snapshot — not the tree — is what the
// test reads; that is deliberate, since the check must run in CI and in the
// Bazel sandbox where the Java sources are absent.
var javaErrorCodes = []string{
	"00000", "02F01", "08001", "08003", "08F01", "08F02", "0A000", "0AF00", "0AF01",
	"22000", "2201W", "22023", "2202E", "22F00", "22F03", "22F04", "22F08", "22F3H",
	"23502", "23505", "24000", "24F00", "25000", "25F01", "40001", "42000", "42501",
	"42601", "42602", "42701", "42702", "42703", "42712", "42723", "42803", "42804",
	"42809", "42883", "42F00", "42F01", "42F02", "42F04", "42F06", "42F07", "42F10",
	"42F13", "42F16", "42F18", "42F19", "42F20", "42F21", "42F50", "42F51", "42F54",
	"42F55", "42F56", "42F57", "42F58", "42F59", "42F60", "42F61", "42F62", "42F63",
	"42F64", "42F65", "42F66", "53F00", "54011", "54F01", "55F00", "58F01", "XX000",
	"XXF01", "XXXXX",
}

// TestErrorCodesMatchJava makes the Go/Java enum difference a checked artifact
// rather than a claim in a doc comment.
//
// The comment on ErrorCode used to assert a 1:1 match with a single stated
// exception. It was wrong — there were four Go-only codes, three of them long
// predating the sentence — and nothing could have caught that, because prose
// does not fail a build. This does: the difference between the two enums must
// equal goOnlyErrorCodes exactly, in both directions.
//
// Scope: this reads allErrorCodes, the init() registry, not the const block —
// Go constants cannot be enumerated at runtime, and parsing the source to reach
// them is a worse trade than the gap it would close. The gap is narrow. A
// constant left out of init() is still caught whenever it is a Java code (it
// surfaces as "absent from Go" below) or is listed in goOnlyErrorCodes (it
// surfaces in TestGoOnlyErrorCodesAreRegistered). What slips through is a
// constant that is neither in Java's enum nor documented as Go-only AND is
// unregistered — which is also a code ErrorCodeFromString can never return, so
// nothing can produce it. Registry and const block both hold 78 entries today.
func TestErrorCodesMatchJava(t *testing.T) {
	t.Parallel()

	java := make(map[string]bool, len(javaErrorCodes))
	for _, c := range javaErrorCodes {
		java[c] = true
	}

	// Java codes Go is missing. This set must stay empty: Go tracks Java's enum
	// in full, and a gap means a Java error has no Go representation at all.
	var missing []string
	for c := range java {
		if _, ok := allErrorCodes[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("Java codes absent from Go: %s\n"+
			"Every Java code must exist in Go — add them to the const block.",
			strings.Join(missing, ", "))
	}

	// Go codes Java lacks. This set must equal goOnlyErrorCodes exactly.
	actual := map[ErrorCode]bool{}
	for s, c := range allErrorCodes {
		if !java[s] {
			actual[c] = true
		}
	}
	for c := range actual {
		if _, listed := goOnlyErrorCodes[c]; !listed {
			t.Errorf("%q is Go-only but not listed in goOnlyErrorCodes.\n"+
				"A deliberate divergence needs an entry there saying why, plus a "+
				"DIVERGENCES.md note; an accidental one needs deleting.", c)
		}
	}
	for c, why := range goOnlyErrorCodes {
		if !actual[c] {
			t.Errorf("%q is listed in goOnlyErrorCodes but is not Go-only "+
				"(either Java defines it now, or the code was removed) — drop the "+
				"stale entry. Recorded reason: %s", c, why)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("%q has an empty reason; an unexplained divergence is the "+
				"thing this list exists to prevent", c)
		}
	}
}

// TestErrorCodeClassesMatchConstants keeps the SQLSTATE class list exact in
// both directions.
//
// It replaces a hand-maintained list in ErrorCode's doc comment that had
// drifted both ways at once: it named "01 Warning", a class neither Go nor Java
// has a single code for, and omitted class 21, which the file has defined all
// along and which is one of the four entries in goOnlyErrorCodes. Neither error
// was detectable by anything but reading it — which is the whole argument for
// making it data.
func TestErrorCodeClassesMatchConstants(t *testing.T) {
	t.Parallel()

	// Classes that actually have codes.
	defined := map[string]bool{}
	for code := range allErrorCodes {
		defined[code[:2]] = true
	}

	var undocumented []string
	for class := range defined {
		if _, ok := errorCodeClasses[class]; !ok {
			undocumented = append(undocumented, class)
		}
	}
	if len(undocumented) > 0 {
		sort.Strings(undocumented)
		t.Errorf("classes with codes but no name in errorCodeClasses: %s",
			strings.Join(undocumented, ", "))
	}

	var phantom []string
	for class, name := range errorCodeClasses {
		if !defined[class] {
			phantom = append(phantom, class+" ("+name+")")
		}
		if strings.TrimSpace(name) == "" {
			t.Errorf("class %q has an empty name", class)
		}
	}
	if len(phantom) > 0 {
		sort.Strings(phantom)
		t.Errorf("errorCodeClasses names classes with no codes: %s\n"+
			"Drop them — a class nothing can return is not documentation.",
			strings.Join(phantom, ", "))
	}
}

// TestGoOnlyErrorCodesAreRegistered pins that every listed divergence is a real
// registered code, so the list cannot drift into naming codes that do not
// exist. Without this a typo in the list would silently satisfy the parity test
// by matching nothing.
func TestGoOnlyErrorCodesAreRegistered(t *testing.T) {
	t.Parallel()

	for c := range goOnlyErrorCodes {
		// ErrorCodeFromString falls back to ErrCodeUnknown for anything it does
		// not know, so an unregistered entry shows up as a mismatch here.
		if got := ErrorCodeFromString(string(c)); got != c {
			t.Errorf("goOnlyErrorCodes lists %q, which is not a registered "+
				"ErrorCode (ErrorCodeFromString returned %q)", c, got)
		}
	}
}
