package factorycorpus_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// The exact AFTER snapshot is intentionally not compared with today's corpus
// here: later corpus growth must not invalidate an immutable historical
// retirement. cmd/verify-corpus-retirement-history validates it against the
// sampleLedgerBytes renders a syntactically complete retirement ledger for the
// loader tests below.
//
// The fixture is SYNTHETIC on purpose. These tests exercise the ledger PARSER —
// duplicate keys, trailing content, non-canonical field names, invalid UTF-8 —
// and none of that needs a real retirement to have happened. Reading a
// committed ledger instead would make the parser tests fail the day the
// repository legitimately has no ledger to read, which is the normal state: a
// ledger records a retirement, so no retirement means no file.
func sampleLedgerBytes(t *testing.T) []byte {
	t.Helper()
	data := []byte(`{
  "format_version": 2,
  "rfc": "RFC-208",
  "date": "2026-08-02",
  "reason": "a synthetic ledger used only to exercise the parser",
  "base_commit": "0000000000000000000000000000000000000000",
  "before_census_sha256": "1111111111111111111111111111111111111111111111111111111111111111",
  "after_census_sha256": "2222222222222222222222222222222222222222222222222222222222222222",
  "before_tree_sha256": "3333333333333333333333333333333333333333333333333333333333333333",
  "after_tree_sha256": "4444444444444444444444444444444444444444444444444444444444444444",
  "changes": [
    {
      "name": "single__cmp__none.yamsql",
      "disposition": "replaced",
      "old_sha256": "5555555555555555555555555555555555555555555555555555555555555555",
      "new_sha256": "6666666666666666666666666666666666666666666666666666666666666666"
    }
  ]
}
`)
	return data
}

func TestLoadRetirementLedgerRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	data := sampleLedgerBytes(t)
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, append(data, []byte("{}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := factorycorpus.LoadRetirementLedger(path); err == nil || !strings.Contains(err.Error(), "trailing content") {
		t.Fatalf("LoadRetirementLedger trailing JSON error = %v, want a trailing-content rejection", err)
	}
}

func TestLoadRetirementLedgerRejectsWhitespaceOnlyRFC(t *testing.T) {
	t.Parallel()
	data := sampleLedgerBytes(t)
	data = bytes.Replace(data, []byte(`"rfc": "RFC-208"`), []byte(`"rfc": "   "`), 1)
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := factorycorpus.LoadRetirementLedger(path); err == nil || !strings.Contains(err.Error(), "rfc, date, and reason are required") {
		t.Fatalf("whitespace-only RFC error = %v, want required-field rejection", err)
	}
}

// duplicateJSONKey repeats one object key verbatim wherever it first appears.
// Keying the mutation on the FIELD NAME rather than on its committed value is
// what lets these fixtures survive the next ledger: a hard-coded commit hash
// silently stops mutating anything, and the test then asserts that unmutated
// bytes are rejected — which they are not, so it fails for the wrong reason.
func duplicateJSONKey(t *testing.T, data []byte, key string) []byte {
	t.Helper()
	needle := []byte(`"` + key + `": `)
	at := bytes.Index(data, needle)
	if at < 0 {
		t.Fatalf("ledger fixture has no %q field", key)
	}
	rest := data[at+len(needle):]
	end := bytes.IndexByte(rest, 0x0a)
	if end < 0 {
		t.Fatalf("ledger fixture field %q is not line-terminated", key)
	}
	line := append([]byte(nil), data[at:at+len(needle)+end]...)
	if !bytes.HasSuffix(line, []byte(",")) {
		line = append(line, ',')
	}
	out := append([]byte(nil), data[:at]...)
	out = append(out, line...)
	out = append(out, ' ')
	out = append(out, data[at:]...)
	return out
}

func TestLoadRetirementLedgerRejectsDuplicateJSONKeysRecursively(t *testing.T) {
	t.Parallel()
	data := sampleLedgerBytes(t)
	tests := []struct {
		name string
		data []byte
		key  string
	}{
		{
			name: "top-level endpoint",
			data: duplicateJSONKey(t, data, "base_commit"),
			key:  "base_commit",
		},
		{
			name: "nested change name",
			data: bytes.Replace(data,
				[]byte(`{
      "name":`),
				[]byte(`{
      "name": "duplicate.yamsql",
      "name":`),
				1,
			),
			key: "name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if bytes.Equal(test.data, data) {
				t.Fatalf("test mutation for %s did not change source", test.key)
			}
			path := filepath.Join(t.TempDir(), "ledger.json")
			if err := os.WriteFile(path, test.data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := factorycorpus.LoadRetirementLedger(path); err == nil ||
				!strings.Contains(err.Error(), `duplicate JSON object key "`+test.key+`"`) {
				t.Fatalf("duplicate %s error = %v, want duplicate-key rejection", test.key, err)
			}
		})
	}
}

func TestLoadRetirementLedgerRequiresCanonicalKeysAndUTF8(t *testing.T) {
	t.Parallel()
	data := sampleLedgerBytes(t)
	reasonPrefix := []byte(`"reason": "`)
	reasonIndex := bytes.Index(data, reasonPrefix)
	if reasonIndex < 0 {
		t.Fatal("test ledger has no reason field")
	}
	invalidUTF8 := append([]byte(nil), data...)
	insertAt := reasonIndex + len(reasonPrefix)
	invalidUTF8 = append(invalidUTF8[:insertAt], append([]byte{0xff}, invalidUTF8[insertAt:]...)...)
	tests := []struct {
		name, want string
		data       []byte
	}{
		{
			name: "case-variant top-level field",
			data: bytes.Replace(data, []byte(`"rfc": "RFC-208"`),
				[]byte(`"RFC": "RFC-208"`), 1),
			want: `non-canonical JSON object key "RFC" at $`,
		},
		{
			name: "case-variant nested field",
			data: bytes.Replace(data, []byte(`"name":`),
				[]byte(`"Name":`), 1),
			want: `non-canonical JSON object key "Name" at $.changes[0]`,
		},
		{name: "invalid UTF-8", data: invalidUTF8, want: "ledger is not valid UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if bytes.Equal(test.data, data) {
				t.Fatal("test mutation did not change source ledger")
			}
			path := filepath.Join(t.TempDir(), "ledger.json")
			if err := os.WriteFile(path, test.data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := factorycorpus.LoadRetirementLedger(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("canonical ledger error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPortableCorpusFilename(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "single__cmp__none.yamsql", want: true},
		{name: "nested/file.yamsql"},
		{name: `nested\file.yamsql`},
		{name: `C:\file.yamsql`},
		{name: "../file.yamsql"},
		{name: "CON.yamsql"},
		{name: "lpt9.anything.yamsql"},
		{name: "trailing.yamsql "},
		{name: "control\x1f.yamsql"},
		{name: "less<than.yamsql"},
		{name: "greater>than.yamsql"},
		{name: `quote"name.yamsql`},
		{name: "pipe|name.yamsql"},
		{name: "question?.yamsql"},
		{name: "star*.yamsql"},
		{name: "café.yamsql"},
		{name: "upper.YAMSQL"},
		{name: "Upper.yamsql"},
	} {
		if got := factorycorpus.IsPortableCorpusFilename(test.name); got != test.want {
			t.Errorf("IsPortableCorpusFilename(%q) = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestRetirementLedgerRejectsWrongEndpointAndFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	beforeDir := t.TempDir()
	path := filepath.Join(dir, "replacement.yamsql")
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stable.yamsql"), []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beforeDir, "replacement.yamsql"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beforeDir, "stable.yamsql"), []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := factorycorpus.Census{Scenarios: 2, Tests: 8, ByFeature: map[string]int{"old": 2}, ByBlessing: map[string]int{"metamorphic": 2}, ByKeyBlessing: map[string]string{"a": "metamorphic", "b": "metamorphic"}}
	after := factorycorpus.Census{Scenarios: 1, Tests: 4, ByFeature: map[string]int{"new": 1}, ByBlessing: map[string]int{"metamorphic": 1}, ByKeyBlessing: map[string]string{"c": "metamorphic"}}
	beforeDigest, err := factorycorpus.CensusSHA256(before)
	if err != nil {
		t.Fatal(err)
	}
	afterDigest, err := factorycorpus.CensusSHA256(after)
	if err != nil {
		t.Fatal(err)
	}
	fileDigest := digest([]byte("new"))
	beforeTreeDigest, err := factorycorpus.CorpusTreeSHA256(beforeDir)
	if err != nil {
		t.Fatal(err)
	}
	afterTreeDigest, err := factorycorpus.CorpusTreeSHA256(dir)
	if err != nil {
		t.Fatal(err)
	}
	ledger := factorycorpus.RetirementLedger{
		FormatVersion:      2,
		RFC:                "RFC-test",
		Date:               "2026-08-01",
		Reason:             "a measured test retirement",
		BaseCommit:         strings.Repeat("a", 40),
		BeforeCensusSHA256: beforeDigest,
		AfterCensusSHA256:  afterDigest,
		BeforeTreeSHA256:   beforeTreeDigest,
		AfterTreeSHA256:    afterTreeDigest,
		Changes: []factorycorpus.RetirementChange{{
			Name: "replacement.yamsql", Disposition: factorycorpus.DispositionReplaced,
			OldSHA256: digest([]byte("old")), NewSHA256: fileDigest,
		}},
	}
	if err := factorycorpus.ValidateRetirementTransition(ledger, before, after, dir); err != nil {
		t.Fatalf("valid transition: %v", err)
	}
	if err := factorycorpus.ValidateRetirementLedgerBefore(ledger, before, beforeDir); err != nil {
		t.Fatalf("valid historical before tree: %v", err)
	}

	tampered := ledger
	tampered.AfterCensusSHA256 = strings.Repeat("2", 64)
	if err := factorycorpus.ValidateRetirementTransition(tampered, before, after, dir); err == nil {
		t.Fatal("wrong after census fingerprint was accepted")
	}
	tampered = ledger
	tampered.Changes = append([]factorycorpus.RetirementChange(nil), ledger.Changes...)
	tampered.Changes[0].OldSHA256 = strings.Repeat("1", 64)
	if err := factorycorpus.ValidateRetirementTransition(tampered, before, after, dir); err == nil {
		t.Fatal("wrong old file fingerprint was accepted")
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := factorycorpus.ValidateRetirementTransition(ledger, before, after, dir); err == nil {
		t.Fatal("wrong replacement bytes were accepted")
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stable.yamsql"), []byte("unlisted edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := factorycorpus.ValidateRetirementTransition(ledger, before, after, dir); err == nil {
		t.Fatal("unlisted corpus edit was accepted")
	}
	if err := os.WriteFile(filepath.Join(beforeDir, "replacement.yamsql"), []byte("invented old bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := factorycorpus.ValidateRetirementLedgerBefore(ledger, before, beforeDir); err == nil {
		t.Fatal("ledger accepted old bytes that do not exist in the anchored before tree")
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
