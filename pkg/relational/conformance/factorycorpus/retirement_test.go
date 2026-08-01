package factorycorpus_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

func TestRFC205RetirementLedgerMatchesCommittedCorpus(t *testing.T) {
	t.Parallel()
	ledger, err := factorycorpus.LoadRetirementLedger(filepath.Join("retirements", "2026-08-01-rfc205.json"))
	if err != nil {
		t.Fatalf("LoadRetirementLedger: %v", err)
	}
	files, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if err := factorycorpus.ValidateRetirementLedgerAfter(
		ledger,
		factorycorpus.ComputeCensus(files),
		factorycorpus.TestdataDir,
	); err != nil {
		t.Fatalf("retirement ledger no longer describes the committed corpus: %v", err)
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
		FormatVersion:      1,
		RFC:                "RFC-test",
		Date:               "2026-08-01",
		Reason:             "a measured test retirement",
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
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
