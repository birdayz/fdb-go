package client

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestClusterFileString_GoldenToString pins ClusterFile.String() byte-for-byte
// against C++ ClusterConnectionString::toString (MonitorLeader.actor.cpp:438-453):
// `description:id@`, IP coordinators first in order, then hostname coordinators in
// order, each with ":tls" when UseTLS. This is cross-tool-shared on-disk state, so
// the bytes MUST match what a C++/Java client would write. Hand-derived goldens
// (not a Go self-round-trip) prove cross-tool readability (RFC-111 §4, test 2).
func TestClusterFileString_GoldenToString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cf   ClusterFile
		want string
	}{
		{"single-ipv4", ClusterFile{Description: "desc", ID: "id", Coordinators: []string{"1.2.3.4:4500"}}, "desc:id@1.2.3.4:4500"},
		{"single-ipv4-tls", ClusterFile{Description: "desc", ID: "id", Coordinators: []string{"1.2.3.4:4500"}, UseTLS: true}, "desc:id@1.2.3.4:4500:tls"},
		{"ipv6", ClusterFile{Description: "d", ID: "i", Coordinators: []string{"[::1]:4500"}}, "d:i@[::1]:4500"},
		{"hostname", ClusterFile{Description: "d", ID: "i", Coordinators: []string{"host.example.com:4500"}}, "d:i@host.example.com:4500"},
		{"hostname-tls", ClusterFile{Description: "d", ID: "i", Coordinators: []string{"h1:4500"}, UseTLS: true}, "d:i@h1:4500:tls"},
		{"multi-ipv4", ClusterFile{Description: "p", ID: "q", Coordinators: []string{"1.1.1.1:1", "2.2.2.2:2"}}, "p:q@1.1.1.1:1,2.2.2.2:2"},
		// Mixed: C++ emits IPs first (in order), then hostnames (in order) —
		// regardless of the original interleaving.
		{"mixed-reorder", ClusterFile{Description: "x", ID: "y", Coordinators: []string{"h1:4500", "1.2.3.4:4500", "h2:4500", "5.6.7.8:4500"}}, "x:y@1.2.3.4:4500,5.6.7.8:4500,h1:4500,h2:4500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cf.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseClusterString_AcceptanceSet pins the tightened token validation
// against C++'s `isHostname || NetworkAddress::parse` (RFC-111 §8, test 8). Group
// (a) is rejected by C++ too; group (b) is a deliberate Go-stricter SAFE tightening
// (C++ accepts+truncates these via sscanf, Go rejects — unreachable on real
// toString-normalized inputs).
func TestParseClusterString_AcceptanceSet(t *testing.T) {
	t.Parallel()
	accept := []string{
		"d:i@1.2.3.4:4500",
		"d:i@[::1]:4500",
		"d:i@host.example.com:4500",
		"d:i@1.2.3.4:4500:tls",
		"d:i@h1:4500,h2:4501",
		"d:i@1.2.3.4:4500,host.example.com:4501",
	}
	for _, s := range accept {
		if _, err := ParseClusterString(s); err != nil {
			t.Errorf("ParseClusterString(%q) = error %v, want accept", s, err)
		}
	}
	rejectMatchesCpp := []string{
		"d:i@foo:abc",        // non-numeric port
		"d:i@:1234",          // empty host
		"d:i@host name:4500", // space in host
		"d:i@1.2.3.4.5:4500", // 5 octets — C++ sscanf leaves count!=len
	}
	rejectGoStricter := []string{
		"d:i@999.999.999.999:4500", // C++ accepts+truncates; net.ParseIP rejects
		"d:i@256.1.1.1:4500",       // C++ accepts+truncates; net.ParseIP rejects
	}
	for _, s := range append(rejectMatchesCpp, rejectGoStricter...) {
		if _, err := ParseClusterString(s); err == nil {
			t.Errorf("ParseClusterString(%q) = accept, want reject", s)
		}
	}
}

// TestConnRecord_SetAndPersist_Format pins that setAndPersist rewrites the cluster
// file to the exact C++ ClusterConnectionFile::persist byte layout (the two `#`
// header lines + cs.toString() + trailing newline) and that ParseClusterFile reads
// the new coordinators back (RFC-111 test 3).
func TestConnRecord_SetAndPersist_Format(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "fdb.cluster")
	old := &ClusterFile{Description: "old", ID: "a", Coordinators: []string{"1.1.1.1:4500"}}
	if err := os.WriteFile(path, []byte(old.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newConnRecord(old, path, discardLogger())

	next := &ClusterFile{Description: "new", ID: "b", Coordinators: []string{"2.2.2.2:4500", "3.3.3.3:4500"}}
	r.setAndPersist(next)

	if got := r.get().String(); got != next.String() {
		t.Fatalf("in-memory not swapped: %q", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# DO NOT EDIT!\n# This file is auto-generated, it is not to be edited by hand\nnew:b@2.2.2.2:4500,3.3.3.3:4500\n"
	if string(raw) != want {
		t.Fatalf("persisted bytes = %q\nwant %q", raw, want)
	}
	back, err := ParseClusterFile(path)
	if err != nil {
		t.Fatalf("ParseClusterFile rejected persisted file: %v", err)
	}
	if back.String() != next.String() {
		t.Fatalf("round-trip mismatch: %q vs %q", back.String(), next.String())
	}
}

// TestConnRecord_SetAndPersist_BestEffort proves a persist failure (read-only dir)
// is swallowed and the in-memory swap STILL takes effect — never fail the
// connection on a file-write error (RFC-111 test 3, matching C++
// ClusterConnectionFile::persist catch-and-continue). Revert-proof: if setAndPersist
// returned/propagated the error and skipped the swap, the in-memory check fails.
func TestConnRecord_SetAndPersist_BestEffort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "fdb.cluster")
	old := &ClusterFile{Description: "old", ID: "a", Coordinators: []string{"1.1.1.1:4500"}}
	if err := os.WriteFile(path, []byte(old.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the directory read-only so the temp-write inside atomicReplace fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	r := newConnRecord(old, path, discardLogger())
	next := &ClusterFile{Description: "new", ID: "b", Coordinators: []string{"2.2.2.2:4500"}}
	r.setAndPersist(next) // must not panic / must swallow the write error

	if got := r.get().String(); got != next.String() {
		t.Fatalf("in-memory swap did not take effect despite persist failure: %q", got)
	}
}

// TestConnRecord_AdoptStoredIfChanged covers Path B (file re-read): an external
// rewrite is adopted, an identical file is a no-op, and an empty/garbage file is
// not adopted (RFC-111 test 4).
func TestConnRecord_AdoptStoredIfChanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "fdb.cluster")
	a := &ClusterFile{Description: "d", ID: "a", Coordinators: []string{"1.1.1.1:4500"}}
	if err := os.WriteFile(path, []byte(a.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newConnRecord(a, path, discardLogger())

	// identical file → no change.
	if r.adoptStoredIfChanged() {
		t.Fatal("adoptStoredIfChanged reported a change for an identical file")
	}

	// external rewrite → adopted.
	b := &ClusterFile{Description: "d", ID: "z", Coordinators: []string{"9.9.9.9:4500"}}
	if err := os.WriteFile(path, []byte(b.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !r.adoptStoredIfChanged() {
		t.Fatal("adoptStoredIfChanged did not adopt an externally rewritten file")
	}
	if got := r.get().String(); got != b.String() {
		t.Fatalf("adopted %q, want %q", got, b.String())
	}

	// garbage file → not adopted (swallowed).
	if err := os.WriteFile(path, []byte("not a cluster string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r.adoptStoredIfChanged() {
		t.Fatal("adoptStoredIfChanged adopted a garbage file")
	}
	if got := r.get().String(); got != b.String() {
		t.Fatalf("state changed after garbage read: %q", got)
	}
}

// TestConnRecord_MemoryOnly proves a no-path record never persists and never
// adopts from disk (ClusterConnectionMemoryRecord semantics).
func TestConnRecord_MemoryOnly(t *testing.T) {
	t.Parallel()
	a := &ClusterFile{Description: "d", ID: "a", Coordinators: []string{"1.1.1.1:4500"}}
	r := newConnRecord(a, "", discardLogger())
	b := &ClusterFile{Description: "d", ID: "b", Coordinators: []string{"2.2.2.2:4500"}}
	r.setAndPersist(b) // in-memory only, no file
	if got := r.get().String(); got != b.String() {
		t.Fatalf("memory swap failed: %q", got)
	}
	if r.adoptStoredIfChanged() {
		t.Fatal("memory-only record must never adopt from disk")
	}
}

func newFollowTestDB(t *testing.T, start *ClusterFile, path string) *database {
	t.Helper()
	return &database{
		logger:     discardLogger(),
		connRecord: newConnRecord(start, path, discardLogger()),
	}
}

// TestFollowForward_Adopts proves a distinct, valid forward is adopted + persisted
// and increments the hop counter (RFC-111 Path A).
func TestFollowForward_Adopts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "fdb.cluster")
	start := &ClusterFile{Description: "old", ID: "a", Coordinators: []string{"1.1.1.1:4500"}}
	if err := os.WriteFile(path, []byte(start.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := newFollowTestDB(t, start, path)

	if !db.followForward(start, "new:b@2.2.2.2:4500") {
		t.Fatal("followForward refused a valid distinct forward")
	}
	if got := db.connRecord.get().String(); got != "new:b@2.2.2.2:4500" {
		t.Fatalf("not adopted: %q", got)
	}
	if db.forwardHops != 1 {
		t.Fatalf("forwardHops = %d, want 1", db.forwardHops)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "# DO NOT EDIT!\n# This file is auto-generated, it is not to be edited by hand\nnew:b@2.2.2.2:4500\n" {
		t.Fatalf("forward not persisted: %q", raw)
	}
}

// TestFollowForward_Rejects proves empty, unparseable, and self forwards are NOT
// followed and leave the file untouched (RFC-111 §5, test 5; port of C++
// getNumberOfCoordinators()>0 guard + the Go-only self-forward fast-path).
func TestFollowForward_Rejects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "fdb.cluster")
	start := &ClusterFile{Description: "old", ID: "a", Coordinators: []string{"1.1.1.1:4500"}}
	startBytes := []byte(start.String() + "\n")
	if err := os.WriteFile(path, startBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	db := newFollowTestDB(t, start, path)

	for _, fwd := range []string{
		"",                  // empty
		"garbage",           // unparseable (no @)
		"x:y@",              // zero coordinators
		"x:y@bad host:4500", // unparseable coordinator
		start.String(),      // self-forward
	} {
		if db.followForward(start, fwd) {
			t.Errorf("followForward(%q) adopted, want reject", fwd)
		}
	}
	if got := db.connRecord.get().String(); got != start.String() {
		t.Fatalf("state changed on a rejected forward: %q", got)
	}
	if db.forwardHops != 0 {
		t.Fatalf("forwardHops = %d after only-rejected forwards, want 0", db.forwardHops)
	}
	if raw, _ := os.ReadFile(path); string(raw) != string(startBytes) {
		t.Fatalf("file mutated on a rejected forward: %q", raw)
	}
}

// TestFollowForward_HopBound proves a pathological forward chain is bounded: past
// maxForwardHops the client stops following and backs off (RFC-111 §5, test 7 —
// the Go-only divergence from C++'s unbounded follow).
func TestFollowForward_HopBound(t *testing.T) {
	t.Parallel()
	start := &ClusterFile{Description: "old", ID: "a", Coordinators: []string{"1.1.1.1:4500"}}
	db := newFollowTestDB(t, start, "") // memory-only

	// At the bound, a valid distinct forward is refused.
	db.forwardHops = maxForwardHops
	if db.followForward(start, "new:b@2.2.2.2:4500") {
		t.Fatal("followForward kept following past maxForwardHops — unbounded spin")
	}

	// Below the bound it is followed and the counter increments.
	db.forwardHops = maxForwardHops - 1
	if !db.followForward(start, "new:b@2.2.2.2:4500") {
		t.Fatal("followForward refused below the bound")
	}
	if db.forwardHops != maxForwardHops {
		t.Fatalf("forwardHops = %d, want %d", db.forwardHops, maxForwardHops)
	}
}
