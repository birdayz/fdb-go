package docscheck

import (
	"net/netip"
	"strings"
	"testing"
)

// The four-part version matcher and a dotted-quad IPv4 address have the same shape, and
// the pin itself ("4.12.11.0") is a numerically valid address — so nothing about the FORM
// of a token separates the two. These tests drive the discriminator from explicit in-test
// state rather than from whatever the living docs happen to contain today: the corpus is
// currently free of addresses, so a corpus-only reading exercises the address arm zero
// times and would report green with the filter entirely broken.

// TestVersionCitationsSkipsAddressesAndKeepsVersions drives BOTH directions of the
// filter. The address rows must be skipped (no false failure) and the version rows must
// survive (no weakening) — a filter that satisfies only one of those is the bug in either
// its original or its over-corrected form.
func TestVersionCitationsSkipsAddressesAndKeepsVersions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want []string // the version citations that must survive the filter
	}{
		// --- addresses: must NOT be read as versions ---
		{"loopback bare", "run it against 127.0.0.1 locally", nil},
		{"loopback with port", "the cluster file points at 127.0.0.1:4500", nil},
		{"private 10/8", "the pool box is 10.0.2.15", nil},
		{"private 10/8 with port", "ssh to 10.0.2.15:22 and check", nil},
		{"private 192.168/16", "a laptop on 192.168.1.1", nil},
		{"private 172.16/12", "the bridge is 172.16.0.5", nil},
		{"link-local", "no DHCP, so 169.254.10.3", nil},
		{"unspecified", "bind to 0.0.0.0 to listen everywhere", nil},
		{"broadcast", "the 255.255.255.255 broadcast", nil},
		{"multicast", "joins 224.0.0.1", nil},
		{"cgnat", "behind CGNAT at 100.64.3.7", nil},
		{"rfc5737 test-net-1", "for example 192.0.2.10", nil},
		{"rfc5737 test-net-2", "for example 198.51.100.7", nil},
		{"rfc5737 test-net-3", "for example 203.0.113.9", nil},
		{"benchmarking", "iperf against 198.18.0.1", nil},
		{"public address with port", "reach the mirror at 8.8.8.8:53", nil},
		{"several addresses in one line", "from 10.0.0.1 via 192.168.0.1 to 127.0.0.1", nil},

		// --- versions: must STILL be judged ---
		{"current pin", "we target fdb-record-layer-core 4.12.11.0 today", []string{"4.12.11.0"}},
		{"stale version", "we target fdb-record-layer-core 4.2.6.0 today", []string{"4.2.6.0"}},
		{"older stale version", "ported from 3.4.5.6", []string{"3.4.5.6"}},
		{"version in a release URL", "see /releases/tag/4.2.6.0 for notes", []string{"4.2.6.0"}},
		{"version in a markdown link", "[4.2.6.0](https://example.invalid/x)", []string{"4.2.6.0"}},
		{"version after a maven coordinate", "org.foundationdb:fdb-record-layer-core:4.2.6.0", []string{"4.2.6.0"}},
		{"version range keeps both sides", "bumped 4.2.6.0/4.12.11.0", []string{"4.2.6.0", "4.12.11.0"}},
		{"bare public address stays a citation", "the number 8.8.8.8 alone", []string{"8.8.8.8"}},

		// --- mixed: the shape that made this a false positive in a real doc ---
		{
			"address and stale version together",
			"Connect to 10.0.2.15:4500 (or 127.0.0.1); we still pin 4.2.6.0.",
			[]string{"4.2.6.0"},
		},
		{
			"address and current pin together",
			"Connect to 10.0.2.15:4500 (or 127.0.0.1); we pin 4.12.11.0.",
			[]string{"4.12.11.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := versionCitations(tc.body)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("versionCitations(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestLivingDocsJavaTargetGateFiresOnAStaleVersionAndNotOnAnAddress drives the GATE, not
// just the matcher: it runs the same comparison TestLivingDocsCiteCurrentJavaTarget runs,
// over synthetic doc bodies, so both arms of the decision are exercised regardless of what
// the real docs contain.
func TestLivingDocsJavaTargetGateFiresOnAStaleVersionAndNotOnAnAddress(t *testing.T) {
	t.Parallel()
	want := javaTarget(t, repoRoot(t))

	// offenders replays the gate's body: every surviving citation must equal the pin.
	offenders := func(body string) []string {
		var bad []string
		for _, v := range versionCitations(body) {
			if v != want {
				bad = append(bad, v)
			}
		}
		return bad
	}

	if got := offenders("Operators connect to 10.0.2.15:4500, or 127.0.0.1 for a local run."); got != nil {
		t.Errorf("a doc citing only IPv4 addresses was reported stale: %v. An address is not a version; do not fix this by allowlisting the address, fix the discriminator", got)
	}
	if got := offenders("We target fdb-record-layer-core " + want + "."); got != nil {
		t.Errorf("a doc citing the current pin %q was reported stale: %v", want, got)
	}
	stale := "4.2.6.0"
	if stale == want {
		t.Fatalf("the fixture version %q became the real pin; pick a different stale fixture or this arm proves nothing", stale)
	}
	if got := offenders("We target fdb-record-layer-core " + stale + "."); len(got) != 1 || got[0] != stale {
		t.Errorf("a doc citing the stale version %q was NOT reported stale (got %v); the address filter has been widened until it swallows real drift", stale, got)
	}
	// A stale version standing next to an address must still be caught: the filter has
	// to be per-token, not "this doc mentions an address, skip it".
	if got := offenders("Host 127.0.0.1 runs " + stale + "."); len(got) != 1 || got[0] != stale {
		t.Errorf("a stale version %q beside an address was NOT reported (got %v); the filter must classify each token, never whole lines or docs", stale, got)
	}
}

// TestJavaTargetPinIsNotItselfAddressShaped is the shelf-life guard on the exclusion
// above. The reserved-block rule assumes no record-layer version lands in one; the day
// that stops being true — a 10.x major is the realistic one, since 10.0.0.0/8 is private
// space — the pin itself would be filtered out and the whole gate would go quietly
// vacuous. This makes that day loud instead.
func TestJavaTargetPinIsNotItselfAddressShaped(t *testing.T) {
	t.Parallel()
	want := javaTarget(t, repoRoot(t))

	if got := versionCitations(want); len(got) != 1 || got[0] != want {
		t.Fatalf("the current pin %q is filtered out as an IPv4 address (versionCitations = %v), so TestLivingDocsCiteCurrentJavaTarget now compares nothing and passes vacuously. The reserved-block exclusion in dottedQuadIsAddress must be narrowed before a pin in that range ships", want, got)
	}
	if a, err := netip.ParseAddr(want); err == nil {
		for _, p := range reservedIPv4Blocks {
			if p.Contains(a) {
				t.Fatalf("the pin %q falls in reserved block %s; the address exclusion and this gate are now in conflict", want, p)
			}
		}
	}
}

// TestReservedIPv4BlocksAreAllIPv4 keeps the block table honest: a mistyped or IPv6 prefix
// would silently never match, retiring one arm of the filter without any test noticing.
func TestReservedIPv4BlocksAreAllIPv4(t *testing.T) {
	t.Parallel()
	if len(reservedIPv4Blocks) == 0 {
		t.Fatal("reservedIPv4Blocks is empty; every dotted quad would now be read as a version and any address in a living doc becomes a false failure")
	}
	for _, p := range reservedIPv4Blocks {
		if !p.Addr().Is4() {
			t.Errorf("reserved block %s is not IPv4; it can never match a dotted quad", p)
		}
		if p.Addr() != p.Masked().Addr() {
			t.Errorf("reserved block %s is not in canonical masked form (%s); Contains still works, but the table reads as a lie", p, p.Masked())
		}
	}
}
