package client

import (
	"encoding/binary"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// TestIPAddressString_IPv4 verifies IPv4 address rendering from big-endian uint32.
func TestIPAddressString_IPv4(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		addr uint32
		want string
	}{
		{"loopback", 0x7f000001, "127.0.0.1"},
		{"zero", 0, "0.0.0.0"},
		{"broadcast", 0xffffffff, "255.255.255.255"},
		{"typical", 0xc0a80101, "192.168.1.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ip := &types.IPAddress{AddrTag: 1, AddrAlt0: tt.addr}
			got := ipAddressString(ip)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIPAddressString_IPv6 verifies IPv6 address rendering from 16-byte slice.
// FDB stores IPv6 as a raw 16-byte slice in AddrAlt1 with AddrTag=2.
func TestIPAddressString_IPv6(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		addr []byte
		want string
	}{
		{"loopback", []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, "::1"},
		{"all_zeros", make([]byte, 16), "::"},
		{"link_local", []byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, "fe80::1"},
		// Short slice (< 16 bytes) should return "::0" instead of panicking.
		{"short_slice", []byte{1, 2, 3}, "::0"},
		// nil slice returns "::0".
		{"nil_slice", nil, "::0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ip := &types.IPAddress{AddrTag: 2, AddrAlt1: tt.addr}
			got := ipAddressString(ip)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIPAddressString_UnknownTag verifies unknown AddrTag returns "0.0.0.0".
func TestIPAddressString_UnknownTag(t *testing.T) {
	t.Parallel()
	ip := &types.IPAddress{AddrTag: 99}
	got := ipAddressString(ip)
	if got != "0.0.0.0" {
		t.Fatalf("got %q, want %q", got, "0.0.0.0")
	}
}

// TestNetworkAddressString verifies "ip:port" formatting.
func TestNetworkAddressString(t *testing.T) {
	t.Parallel()
	na := &types.NetworkAddress{
		Ip:   types.IPAddress{AddrTag: 1, AddrAlt0: 0x0a000164}, // 10.0.1.100
		Port: 4500,
	}
	got := networkAddressString(na)
	if got != "10.0.1.100:4500" {
		t.Fatalf("got %q, want %q", got, "10.0.1.100:4500")
	}
}

// TestEndpointToken verifies UID extraction from the 16-byte token.
func TestEndpointToken(t *testing.T) {
	t.Parallel()
	var ep types.Endpoint
	binary.LittleEndian.PutUint64(ep.Token[:8], 0xDEADBEEFCAFEBABE)
	binary.LittleEndian.PutUint64(ep.Token[8:], 0x1234567890ABCDEF)
	uid := endpointToken(&ep)
	if uid.First != 0xDEADBEEFCAFEBABE || uid.Second != 0x1234567890ABCDEF {
		t.Fatalf("got (%x, %x), want (DEADBEEFCAFEBABE, 1234567890ABCDEF)", uid.First, uid.Second)
	}
}

// TestEndpointValid verifies zero-token detection.
func TestEndpointValid(t *testing.T) {
	t.Parallel()
	var ep types.Endpoint
	if endpointValid(&ep) {
		t.Fatal("zero token should be invalid")
	}
	binary.LittleEndian.PutUint64(ep.Token[:8], 1)
	if !endpointValid(&ep) {
		t.Fatal("non-zero token should be valid")
	}
}

// TestEndpointAddress verifies that endpointAddress delegates to
// the primary address, not secondary.
func TestEndpointAddress(t *testing.T) {
	t.Parallel()
	ep := types.Endpoint{
		Addresses: types.NetworkAddressList{
			Address: types.NetworkAddress{
				Ip:   types.IPAddress{AddrTag: 1, AddrAlt0: 0xac100264}, // 172.16.2.100
				Port: 4689,
			},
		},
	}
	got := endpointAddress(&ep)
	if got != "172.16.2.100:4689" {
		t.Fatalf("got %q, want %q", got, "172.16.2.100:4689")
	}
}

// TestGetAdjustedEndpoint verifies token arithmetic for endpoint offsets.
// C++ uses endpoint.token.first() + (index << 32) for First, and
// (token.second() & upper32) | (lower32 + index) for Second.
func TestGetAdjustedEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		base       transport.UID
		index      int
		wantFirst  uint64
		wantSecond uint64
	}{
		{
			"zero_offset",
			transport.UID{First: 0xAABB, Second: 0xFF00000000000005},
			0,
			0xAABB,
			0xFF00000000000005,
		},
		{
			"offset_3",
			transport.UID{First: 0x100, Second: 0xFF00000000000005},
			3,
			0x100 + (3 << 32),
			0xFF00000000000008, // lower 32 bits: 5+3=8
		},
		{
			"lower32_wrap",
			transport.UID{First: 0, Second: 0x00000000FFFFFFFE},
			3,
			3 << 32,
			0x0000000000000001, // 0xFFFFFFFE + 3 wraps to 1
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := getAdjustedEndpoint(tt.base, tt.index)
			if got.First != tt.wantFirst {
				t.Fatalf("First: got %#x, want %#x", got.First, tt.wantFirst)
			}
			if got.Second != tt.wantSecond {
				t.Fatalf("Second: got %#x, want %#x", got.Second, tt.wantSecond)
			}
		})
	}
}
