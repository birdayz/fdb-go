package types

import (
	"bytes"
	"encoding/hex"
	"net"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// libfdbcVTableOrder is the vtable packing order of the libfdb_c 7.3.77 release
// binary. C++ orders vtables by pointer (flat_buffers.h:770), so it differs
// from the schema extractor's order without changing what a decoder reads.
var libfdbcVTableOrder = wire.NewMessageTemplate(OpenDatabaseCoordRequestFileID, OpenDatabaseCoordRequestVTable,
	OpenDatabaseCoordRequestMaxAlign, []wire.VTable{
		{6, 8, 4},
		{8, 9, 8, 4},
		clientVersionRefVTable, hostnameVTable, NetworkAddressVTable,
		ReplyPromiseVTable, OpenDatabaseCoordRequestVTable,
	})

func ipv4(a string, port, flags uint16) NetworkAddress {
	v4 := net.ParseIP(a).To4()
	return NetworkAddress{Ip: IPAddress{AddrTag: 1, AddrAlt0: uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])}, Port: port, Flags: flags}
}

// TestCoordOpenDatabaseRequest_MatchesLibfdbc pins the encoding to requests
// libfdb_c 7.3.77 sent to a listening socket for the named connection strings.
func TestCoordOpenDatabaseRequest_MatchesLibfdbc(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		connectionString string
		coordinators     []NetworkAddress
		hostnames        []Hostname
		hex              string
	}{
		{
			"desc:abcdef@127.0.0.1:57739",
			[]NetworkAddress{ipv4("127.0.0.1", 57739, 0)},
			nil,
			"54000000c8460300000006000800040008000900080004000a001000040008000c000a000d00040008000c000c000d00" +
				"040008000a000c0006001400040016003100140018001c0004002000240028002c0030004a000000040000001e000000" +
				"00000000000000000000000000000000e40000007c0000006c000000580000002c00000010000000cc00000000000000" +
				"000000005c0000006ca6f2c2fc500720190000001d68b9e6000000000100000004000000880000000c0000008be10000" +
				"00000000b400000008000000010000000100007f0b000000646573633a616263646566000700000064656661756c7400" +
				"0100000004000000e00000004c0000001c000000040000000f0000006664623030623037333030303030300028000000" +
				"3365613434636531643930303361643039356534303830333965316637353563333139633464666206000000372e332e" +
				"3737000000000000",
		},
		{
			"desc:abcdef@127.0.0.1:44715,10.1.2.3:4501,coord.example.invalid:4502",
			[]NetworkAddress{ipv4("127.0.0.1", 44715, 0), ipv4("10.1.2.3", 4501, 0)},
			[]Hostname{{Host: []byte("coord.example.invalid"), Service: []byte("4502")}},
			"54000000c8460300000006000800040008000900080004000a001000040008000c000a000d00040008000c000c000d00" +
				"040008000a000c0006001400040016003100140018001c0004002000240028002c0030004a000000040000001e000000" +
				"000000000000000000000000000000003c010000d4000000c4000000b000000060000000480000000800000000000000" +
				"010000000400000076000000140000000800000000000000040000003435303215000000636f6f72642e6578616d706c" +
				"652e696e76616c69640000009400000098bb44f8ac94ea901a0000005467f239020000002800000004000000c0000000" +
				"0c0000009511000000000000ec00000008000000010000000302010ae00000000c000000abae0000000000000c010000" +
				"08000000010000000100007f0b000000646573633a616263646566000700000064656661756c74000100000004000000" +
				"380100004c0000001c000000040000000f00000066646230306230373330303030303000280000003365613434636531" +
				"643930303361643039356534303830333965316637353563333139633464666206000000372e332e3737000000000000",
		},
		{
			"desc:abcdef@127.0.0.1:57105,[2001:db8::7]:4500,10.1.2.3:4501:tls",
			[]NetworkAddress{
				ipv4("127.0.0.1", 57105, 0),
				{Ip: IPAddress{AddrTag: 2, AddrAlt1: net.ParseIP("2001:db8::7")}, Port: 4500},
				ipv4("10.1.2.3", 4501, FlagTLS),
			},
			nil,
			"54000000c8460300000006000800040008000900080004000a001000040008000c000a000d00040008000c000c000d00" +
				"040008000a000c0006001400040016003100140018001c0004002000240028002c0030004a000000040000001e000000" +
				"000000000000000000000000000000003c010000d4000000c4000000b00000002c000000100000002401000000000000" +
				"000000005c000000009a9195b6b931d0190000006571472d00000000030000005c000000280000000400000090000000" +
				"0c0000009511020000000000bc00000008000000010000000302010ab00000000c0000009411000000000000dc000000" +
				"08000000020000001000000020010db8000000000000000000000007e00000000c00000011df0000000000000c010000" +
				"08000000010000000100007f0b000000646573633a616263646566000700000064656661756c74000100000004000000" +
				"380100004c0000001c000000040000000f00000066646230306230373330303030303000280000003365613434636531" +
				"643930303361643039356534303830333965316637353563333139633464666206000000372e332e3737000000000000",
		},
	} {
		t.Run(tc.connectionString, func(t *testing.T) {
			t.Parallel()
			want, err := hex.DecodeString(tc.hex)
			if err != nil {
				t.Fatal(err)
			}
			// The reply token is random per request; take it from the capture.
			var captured OpenDatabaseCoordRequest
			if err := captured.UnmarshalFDB(want); err != nil {
				t.Fatal(err)
			}
			req := CoordOpenDatabaseRequest{
				SupportedVersions: []ClientVersionRef{{
					ClientVersion:   []byte("7.3.77"),
					SourceVersion:   []byte("3ea44ce1d9003ad095e408039e1f755c319c4dfb"),
					ProtocolVersion: []byte("fdb00b073000000"),
				}},
				TraceLogGroup: []byte("default"),
				ClusterKey:    []byte("desc:abcdef"),
				Coordinators:  tc.coordinators,
				Reply:         captured.Reply,
				Hostnames:     tc.hostnames,
			}
			if got := req.marshalWith(libfdbcVTableOrder); !bytes.Equal(got, want) {
				t.Fatalf("encoding differs from libfdb_c\n got %x\nwant %x", got, want)
			}
			// The default template differs only in vtable order, so it decodes alike.
			r, err := wire.NewReader(req.MarshalFDB())
			if err != nil {
				t.Fatal(err)
			}
			coords, err := ParseNetworkAddressVectorFromReader(r, OpenDatabaseCoordRequestSlotCoordinators)
			if err != nil {
				t.Fatal(err)
			}
			if len(coords) != len(tc.coordinators) {
				t.Fatalf("decoded %d coordinators, want %d", len(coords), len(tc.coordinators))
			}
			for i, c := range coords {
				w := tc.coordinators[i]
				if c.Ip.AddrTag != w.Ip.AddrTag || c.Ip.AddrAlt0 != w.Ip.AddrAlt0 || !bytes.Equal(c.Ip.AddrAlt1, w.Ip.AddrAlt1) ||
					c.Port != w.Port || c.Flags != w.Flags {
					t.Errorf("coordinator %d decoded as %+v, want %+v", i, c, w)
				}
			}
			if n, _ := r.ReadVectorCount(OpenDatabaseCoordRequestSlotHostnames); n != len(tc.hostnames) {
				t.Errorf("decoded %d hostnames, want %d", n, len(tc.hostnames))
			}
		})
	}
}
