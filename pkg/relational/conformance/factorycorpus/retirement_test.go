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
// Git commit that first added this ledger (and against the worktree while the
// ledger is new to the trusted branch). This hermetic test pins RFC-205's
// reviewed transaction shape while the generic validator tests endpoint logic.
func TestRFC205RetirementLedgerShape(t *testing.T) {
	t.Parallel()
	ledger, err := factorycorpus.LoadRetirementLedger(filepath.Join("retirements", "2026-08-01-rfc205.json"))
	if err != nil {
		t.Fatalf("LoadRetirementLedger: %v", err)
	}
	counts := map[string]int{}
	for _, change := range ledger.Changes {
		counts[change.Disposition]++
	}
	if counts[factorycorpus.DispositionRetired] != 24 ||
		counts[factorycorpus.DispositionReplaced] != 27 ||
		counts[factorycorpus.DispositionAdded] != 3 || len(ledger.Changes) != 54 {
		t.Fatalf("RFC-205 ledger dispositions = %v across %d files, want retired=24 replaced=27 added=3 across 54", counts, len(ledger.Changes))
	}
}

func TestLoadRetirementLedgerRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	source := filepath.Join("retirements", "2026-08-01-rfc205.json")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
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
	data, err := os.ReadFile(filepath.Join("retirements", "2026-08-01-rfc205.json"))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"rfc": "RFC-205"`), []byte(`"rfc": "   "`), 1)
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := factorycorpus.LoadRetirementLedger(path); err == nil || !strings.Contains(err.Error(), "rfc, date, and reason are required") {
		t.Fatalf("whitespace-only RFC error = %v, want required-field rejection", err)
	}
}

func TestLoadRetirementLedgerRejectsDuplicateJSONKeysRecursively(t *testing.T) {
	t.Parallel()
	source := filepath.Join("retirements", "2026-08-01-rfc205.json")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		key  string
	}{
		{
			name: "top-level endpoint",
			data: bytes.Replace(data,
				[]byte(`"base_commit": "51d9e9701bbcb959ae09e472fa9e6bb2c9e84169",`),
				[]byte(`"base_commit": "51d9e9701bbcb959ae09e472fa9e6bb2c9e84169", "base_commit": "51d9e9701bbcb959ae09e472fa9e6bb2c9e84169",`),
				1,
			),
			key: "base_commit",
		},
		{
			name: "nested change name",
			data: bytes.Replace(data,
				[]byte(`{"name":`),
				[]byte(`{"name":"duplicate.yaml","name":`),
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
	source := filepath.Join("retirements", "2026-08-01-rfc205.json")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
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
			data: bytes.Replace(data, []byte(`"rfc": "RFC-205"`),
				[]byte(`"RFC": "RFC-205"`), 1),
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
		{name: "fc_0000000001_q0_p0.yaml", want: true},
		{name: "nested/file.yaml"},
		{name: `nested\file.yaml`},
		{name: `C:\file.yaml`},
		{name: "../file.yaml"},
		{name: "CON.yaml"},
		{name: "lpt9.anything.yaml"},
		{name: "trailing.yaml "},
		{name: "control\x1f.yaml"},
		{name: "less<than.yaml"},
		{name: "greater>than.yaml"},
		{name: `quote"name.yaml`},
		{name: "pipe|name.yaml"},
		{name: "question?.yaml"},
		{name: "star*.yaml"},
		{name: "café.yaml"},
		{name: "upper.YAML"},
		{name: "Upper.yaml"},
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
	path := filepath.Join(dir, "replacement.yaml")
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stable.yaml"), []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beforeDir, "replacement.yaml"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beforeDir, "stable.yaml"), []byte("stable"), 0o644); err != nil {
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
			Name: "replacement.yaml", Disposition: factorycorpus.DispositionReplaced,
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
	if err := os.WriteFile(filepath.Join(dir, "stable.yaml"), []byte("unlisted edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := factorycorpus.ValidateRetirementTransition(ledger, before, after, dir); err == nil {
		t.Fatal("unlisted corpus edit was accepted")
	}
	if err := os.WriteFile(filepath.Join(beforeDir, "replacement.yaml"), []byte("invented old bytes"), 0o644); err != nil {
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
