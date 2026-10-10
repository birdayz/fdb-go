package cmd

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const fdbFixturePortCount = 65536 - 1024

var errFDBFixturePortsExhausted = errors.New("no available non-ephemeral FDB fixture port")

type fdbFixturePortLease struct {
	port int
	udp  *net.UDPConn
	tcp  net.Listener
}

func (p *fdbFixturePortLease) close() {
	if p.tcp != nil {
		_ = p.tcp.Close()
	}
	_ = p.udp.Close()
}

func (p *fdbFixturePortLease) handoffTCP() error {
	err := p.tcp.Close()
	p.tcp = nil
	return err
}

func fdbFixtureEphemeralRange(text string) (int, int, error) {
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("invalid Linux ephemeral port range %q", text)
	}
	first, firstErr := strconv.Atoi(fields[0])
	last, lastErr := strconv.Atoi(fields[1])
	if firstErr != nil || lastErr != nil || first < 1 || first > last || last > 65535 {
		return 0, 0, fmt.Errorf("invalid Linux ephemeral port range %q", text)
	}
	return first, last, nil
}

func fdbFixturePortCandidates(start int) []int {
	ports := make([]int, fdbFixturePortCount)
	for i := range ports {
		ports[i] = 1024 + (start+i)%fdbFixturePortCount
	}
	return ports
}

func fdbFixturePortAllowed(port, ephemeralFirst, ephemeralLast int) bool {
	return port >= 1024 && port <= 65535 && (port < ephemeralFirst || port > ephemeralLast)
}

// UDP leases coordinate these fixtures across processes sharing the host network.
// They do not reserve TCP against unrelated explicit binders; excluding the kernel's
// ephemeral range prevents outbound TCP connections from taking the handoff port.
func acquireFDBFixturePort(candidates []int, ephemeralFirst, ephemeralLast int) (*fdbFixturePortLease, error) {
	for _, port := range candidates {
		if !fdbFixturePortAllowed(port, ephemeralFirst, ephemeralLast) {
			continue
		}
		udp, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				continue
			}
			return nil, fmt.Errorf("lease UDP port %d: %w", port, err)
		}
		tcp, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			_ = udp.Close()
			if errors.Is(err, syscall.EADDRINUSE) {
				continue
			}
			return nil, fmt.Errorf("probe TCP port %d: %w", port, err)
		}
		return &fdbFixturePortLease{port: port, udp: udp, tcp: tcp}, nil
	}
	return nil, errFDBFixturePortsExhausted
}

func fdbFixtureContainerName(dir string) string {
	digest := sha256.Sum256([]byte(dir))
	return fmt.Sprintf("frl-e2e-%x", digest[:12])
}

func TestFDBFixturePortRange(t *testing.T) {
	t.Parallel()
	first, last, err := fdbFixtureEphemeralRange("32768\t60999\n")
	if err != nil || first != 32768 || last != 60999 {
		t.Fatalf("range=(%d,%d,%v), want (32768,60999,nil)", first, last, err)
	}
	for _, text := range []string{"", "32768", "32768 60999 extra", "bad 60999", "0 65535", "60999 32768", "1024 65536"} {
		if _, _, err := fdbFixtureEphemeralRange(text); err == nil {
			t.Errorf("invalid range %q accepted", text)
		}
	}
	for _, tc := range []struct {
		port int
		want bool
	}{
		{1023, false},
		{1024, true},
		{32767, true},
		{32768, false},
		{47501, false},
		{60999, false},
		{61000, true},
		{65535, true},
		{65536, false},
	} {
		if got := fdbFixturePortAllowed(tc.port, first, last); got != tc.want {
			t.Errorf("port %d allowed=%v, want %v", tc.port, got, tc.want)
		}
	}
	ports := fdbFixturePortCandidates(fdbFixturePortCount - 1)
	seen := make(map[int]bool)
	for _, port := range ports {
		if port < 1024 || port > 65535 || seen[port] {
			t.Fatalf("invalid or duplicate candidate %d", port)
		}
		seen[port] = true
	}
	if len(seen) != fdbFixturePortCount || ports[0] != 65535 || ports[1] != 1024 {
		t.Fatalf("candidate rotation incomplete: count=%d, first=%v", len(seen), ports[:2])
	}
}

func fixturePortForTest(t *testing.T, candidates []int) *fdbFixturePortLease {
	t.Helper()
	lease, err := acquireFDBFixturePort(candidates, 32768, 60999)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.close)
	return lease
}

func TestFDBFixturePortLeaseSurvivesTCPHandoff(t *testing.T) {
	t.Parallel()
	candidates := fdbFixturePortCandidates(0)
	first := fixturePortForTest(t, candidates)
	if err := first.handoffTCP(); err != nil {
		t.Fatal(err)
	}
	second := fixturePortForTest(t, candidates)
	if first.port == second.port {
		t.Fatalf("simultaneous fixtures leased the same port %d after TCP handoff", first.port)
	}
	for _, ports := range [][]int{nil, {first.port, second.port}} {
		if _, err := acquireFDBFixturePort(ports, 32768, 60999); !errors.Is(err, errFDBFixturePortsExhausted) {
			t.Fatalf("claimed/empty candidate set: %v, want exhaustion", err)
		}
	}
	if _, err := acquireFDBFixturePort([]int{47501}, 32768, 60999); !errors.Is(err, errFDBFixturePortsExhausted) {
		t.Fatalf("ephemeral-only candidate set: %v, want exhaustion", err)
	}
}

func TestFDBFixturePortSkipsOccupiedTCP(t *testing.T) {
	t.Parallel()
	candidates := fdbFixturePortCandidates(0)
	occupied := fixturePortForTest(t, candidates)
	// Leave only TCP occupied; the next fixture must select a different candidate.
	if err := occupied.udp.Close(); err != nil {
		t.Fatal(err)
	}
	next := fixturePortForTest(t, append([]int{occupied.port}, candidates...))
	if next.port == occupied.port {
		t.Fatalf("selected occupied TCP port %d", occupied.port)
	}
}

func TestFDBFixtureContainerNamesDoNotUsePID(t *testing.T) {
	t.Parallel()
	first := fdbFixtureContainerName(t.TempDir())
	second := fdbFixtureContainerName(t.TempDir())
	if first == second {
		t.Fatalf("two fixture directories in one process got the same container name %q", first)
	}
	for _, name := range []string{first, second} {
		if !strings.HasPrefix(name, "frl-e2e-") || len(name) != len("frl-e2e-")+24 {
			t.Fatalf("unexpected container name %q", name)
		}
	}
}
