package client

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func TestBuildOpenDatabaseCoordRequest_CarriesCoordinatorsAndKnownID(t *testing.T) {
	t.Parallel()
	cf, err := ParseClusterString("desc:abc@10.1.2.3:4500,[2001:db8::7]:4501,coord.example:4502")
	if err != nil {
		t.Fatal(err)
	}
	known := transport.UID{First: 0x1122334455667788, Second: 0x99aabbccddeeff00}
	body, err := buildOpenDatabaseCoordRequest(cf, false, known, transport.UID{First: 7, Second: 8})
	if err != nil {
		t.Fatal(err)
	}
	r, err := wire.NewReader(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(r.ReadBytes(types.OpenDatabaseCoordRequestSlotClusterKey)); got != "desc:abc" {
		t.Errorf("cluster key = %q", got)
	}
	first, second := r.ReadUIDPair(types.OpenDatabaseCoordRequestSlotKnownClientInfoID)
	if (transport.UID{First: first, Second: second}) != known {
		t.Errorf("knownClientInfoID = %x/%x, want %x/%x", first, second, known.First, known.Second)
	}
	coords := types.ParseNetworkAddressVectorFromReader(r, types.OpenDatabaseCoordRequestSlotCoordinators)
	if len(coords) != 2 {
		t.Fatalf("coordinators = %+v, want the two IP addresses", coords)
	}
	if c := coords[0]; c.Ip.AddrTag != 1 || c.Ip.AddrAlt0 != 0x0a010203 || c.Port != 4500 || c.Flags != 0 {
		t.Errorf("coordinators[0] = %+v, want public IPv4 10.1.2.3:4500", c)
	}
	if c := coords[1]; c.Ip.AddrTag != 2 || !net.IP(c.Ip.AddrAlt1).Equal(net.ParseIP("2001:db8::7")) || c.Port != 4501 {
		t.Errorf("coordinators[1] = %+v, want IPv6 [2001:db8::7]:4501", c)
	}
	if n, err := r.ReadVectorCount(types.OpenDatabaseCoordRequestSlotHostnames); err != nil || n != 1 {
		t.Fatalf("hostnames count = %d, %v; want 1", n, err)
	}
	h, err := r.ReadVectorElementReader(types.OpenDatabaseCoordRequestSlotHostnames, 0)
	if err != nil {
		t.Fatal(err)
	}
	if host, svc := string(h.ReadBytes(0)), string(h.ReadBytes(1)); host != "coord.example" || svc != "4502" {
		t.Errorf("hostname = %q:%q, want coord.example:4502", host, svc)
	}
}

// The long poll echoes this id back, so a zero id would make it return at once.
func TestParseCoordinatorResponse_ReadsClientInfoID(t *testing.T) {
	t.Parallel()
	want := transport.UID{First: 0x0102030405060708, Second: 0x1112131415161718}
	info, err := parseCoordinatorResponse((&types.ClientDBInfo{Id: wire.UIDFromParts(want.First, want.Second)}).MarshalFDB())
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != want {
		t.Fatalf("ID = %x/%x, want %x/%x", info.ID.First, info.ID.Second, want.First, want.Second)
	}
}

// The TLS flag follows the transport the client dials: WithTLSConfig dials TLS
// for a plain cluster string, and the coordinators must be advertised as TLS.
func TestCoordinatorAddresses_TLSFollowsTransport(t *testing.T) {
	t.Parallel()
	cf, err := ParseClusterString("desc:abc@10.1.2.3:4500,coord.example:4502")
	if err != nil {
		t.Fatal(err)
	}
	for _, useTLS := range []bool{false, true} {
		coords, hostnames, err := coordinatorAddresses(cf, useTLS)
		if err != nil {
			t.Fatal(err)
		}
		want := uint16(0)
		if useTLS {
			want = types.FlagTLS
		}
		if len(coords) != 1 || coords[0].Flags != want {
			t.Errorf("tls=%t: coords = %+v, want flags %d", useTLS, coords, want)
		}
		if len(hostnames) != 1 || hostnames[0].IsTLS != useTLS {
			t.Errorf("tls=%t: hostnames = %+v", useTLS, hostnames)
		}
	}
}

// A coordinator that cannot be encoded fails the request rather than being
// dropped, and an empty list is never sent.
func TestCoordinatorAddresses_NeverDropsOrSendsEmpty(t *testing.T) {
	t.Parallel()
	for _, cf := range []*ClusterFile{
		{Description: "d", ID: "i"},
		{Description: "d", ID: "i", Coordinators: []string{"10.1.2.3:4500", "10.1.2.3"}},
		{Description: "d", ID: "i", Coordinators: []string{"10.1.2.3:70000"}},
	} {
		if coords, hostnames, err := coordinatorAddresses(cf, false); err == nil {
			t.Errorf("%v encoded as %+v %+v, want an error", cf.Coordinators, coords, hostnames)
		}
	}
}

type pollCall struct {
	addr  string
	known transport.UID
}

// scriptedPoll answers polls from a script and cancels the database when it runs out.
type scriptedPoll struct {
	t      *testing.T
	cancel context.CancelFunc
	script []func(addr string) (*DBInfo, error)
	calls  []pollCall
	pauses []int // len(calls) at each reconnection pause
}

func (p *scriptedPoll) pause() { p.pauses = append(p.pauses, len(p.calls)) }

func (p *scriptedPoll) poll(_ context.Context, _ *ClusterFile, addr string, known transport.UID) (*DBInfo, error) {
	p.calls = append(p.calls, pollCall{addr, known})
	if len(p.script) == 0 {
		p.cancel()
		return nil, context.Canceled
	}
	step := p.script[0]
	p.script = p.script[1:]
	return step(addr)
}

func newPollTestDB(t *testing.T, coordinators ...string) *database {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &database{
		ctx:            ctx,
		cancel:         cancel,
		proxiesChanged: make(chan struct{}),
		logger:         slog.New(slog.DiscardHandler),
		connRecord:     newConnRecord(&ClusterFile{Description: "d", ID: "i", Coordinators: coordinators}, "", nil),
	}
}

func proxyInfo(id uint64, proxy string) *DBInfo {
	p := []ProxyInfo{{Address: proxy, Token: transport.UID{First: id}}}
	return &DBInfo{ID: transport.UID{First: id}, GRVProxies: p, CommitProxies: p}
}

// A reply re-arms the long poll on the same coordinator with the new id, even
// when the proxies did not change (MonitorLeader.actor.cpp:876-878, :966).
func TestMonitorProxies_LongPollsSameCoordinatorWithLatestID(t *testing.T) {
	t.Parallel()
	db := newPollTestDB(t, "10.0.0.1:4500", "10.0.0.2:4500", "10.0.0.3:4500")
	p := &scriptedPoll{t: t, cancel: db.cancel, script: []func(string) (*DBInfo, error){
		func(string) (*DBInfo, error) { return proxyInfo(1, "10.0.0.9:4500"), nil },
		func(string) (*DBInfo, error) { return proxyInfo(2, "10.0.0.9:4500"), nil },
		func(string) (*DBInfo, error) { return nil, errLongPollExpired },
		func(string) (*DBInfo, error) { return proxyInfo(3, "10.0.0.8:4500"), nil },
	}}
	known := transport.UID{First: 99}
	db.monitorProxiesOneGeneration(p.poll, p.pause, "", &known)

	wantKnown := []uint64{99, 1, 2, 2, 3}
	if len(p.calls) != len(wantKnown) {
		t.Fatalf("polls = %+v, want %d", p.calls, len(wantKnown))
	}
	for i, c := range p.calls {
		if c.addr != p.calls[0].addr {
			t.Errorf("poll %d went to %s, want the answering coordinator %s", i, c.addr, p.calls[0].addr)
		}
		if c.known.First != wantKnown[i] {
			t.Errorf("poll %d known = %d, want %d", i, c.known.First, wantKnown[i])
		}
	}
	if got := db.dbInfo.Load(); got == nil || got.GRVProxies[0].Address != "10.0.0.8:4500" {
		t.Errorf("installed proxies = %+v, want the latest reply's", got)
	}
	if len(p.pauses) != 0 {
		t.Errorf("paused after polls %v; a reply or an expired poll never pauses", p.pauses)
	}
}

// A failure moves to the next coordinator; only a sweep in which every
// coordinator failed pauses COORDINATOR_RECONNECTION_DELAY (:974-979).
func TestMonitorProxies_FailureRotatesAndPausesAfterFullSweep(t *testing.T) {
	t.Parallel()
	coords := []string{"10.0.0.1:4500", "10.0.0.2:4500", "10.0.0.3:4500"}
	db := newPollTestDB(t, coords...)
	fail := func(string) (*DBInfo, error) { return nil, errors.New("connection reset") }
	p := &scriptedPoll{t: t, cancel: db.cancel, script: []func(string) (*DBInfo, error){
		fail, fail, fail, fail,
		func(string) (*DBInfo, error) { return proxyInfo(1, "10.0.0.9:4500"), nil },
	}}
	var known transport.UID
	db.monitorProxiesOneGeneration(p.poll, p.pause, "", &known)

	if len(p.calls) != 6 {
		t.Fatalf("polls = %+v, want 6", p.calls)
	}
	sweep := []string{p.calls[0].addr, p.calls[1].addr, p.calls[2].addr}
	slices.Sort(sweep)
	if !slices.Equal(sweep, coords) {
		t.Errorf("first sweep = %v, want every coordinator once", sweep)
	}
	if p.calls[3].addr != p.calls[0].addr {
		t.Errorf("after the sweep the poll went to %s, want %s again", p.calls[3].addr, p.calls[0].addr)
	}
	if !slices.Equal(p.pauses, []int{3}) {
		t.Errorf("paused after polls %v, want once, after the third", p.pauses)
	}
	if p.calls[5].addr != p.calls[4].addr || p.calls[5].known.First != 1 {
		t.Errorf("after success poll = %+v, want same coordinator with known 1", p.calls[5])
	}
}

// A followed forward ends the generation so the monitor restarts on the new set.
func TestMonitorProxies_ForwardEndsGeneration(t *testing.T) {
	t.Parallel()
	db := newPollTestDB(t, "10.0.0.1:4500")
	p := &scriptedPoll{t: t, cancel: db.cancel, script: []func(string) (*DBInfo, error){
		func(string) (*DBInfo, error) { return &DBInfo{Forward: "d:j@10.0.0.5:4500"}, nil },
	}}
	var known transport.UID
	db.monitorProxiesOneGeneration(p.poll, p.pause, "", &known)

	if len(p.calls) != 1 {
		t.Fatalf("polls = %+v, want the generation to end after the forward", p.calls)
	}
	if got := db.connRecord.get().String(); got != "d:j@10.0.0.5:4500" {
		t.Errorf("active connection string = %q, want the forward", got)
	}
	if !db.clusterSwitchPending.Load() {
		t.Error("forward must mark the coordinator set adopted")
	}
}

// A reply without an id is a failure: echoing a zero id back would make every
// poll return at once.
func TestMonitorProxies_ZeroIDReplyIsAFailure(t *testing.T) {
	t.Parallel()
	db := newPollTestDB(t, "10.0.0.1:4500")
	p := &scriptedPoll{t: t, cancel: db.cancel, script: []func(string) (*DBInfo, error){
		func(string) (*DBInfo, error) { return proxyInfo(0, "10.0.0.9:4500"), nil },
	}}
	known := transport.UID{First: 5}
	db.monitorProxiesOneGeneration(p.poll, p.pause, "", &known)

	if got := db.dbInfo.Load(); got != nil {
		t.Errorf("installed %+v from a reply without an id", got)
	}
	// The script's end fails the second poll too, hence a second pause.
	if !slices.Equal(p.pauses, []int{1, 2}) || known.First != 5 {
		t.Errorf("pauses %v, known %d; want a failed sweep and the old id", p.pauses, known.First)
	}
}

// A port past 65535 is rejected, never truncated or dropped from the request.
func TestParseClusterString_RejectsPortOutOfRange(t *testing.T) {
	t.Parallel()
	if _, err := ParseClusterString("d:i@10.1.2.3:65536"); err == nil {
		t.Error("port 65536 accepted")
	}
	cf, err := ParseClusterString("d:i@10.1.2.3:65535")
	if err != nil {
		t.Fatal(err)
	}
	if coords, _, err := coordinatorAddresses(cf, false); err != nil || len(coords) != 1 || coords[0].Port != 65535 {
		t.Errorf("coordinators = %+v, want port 65535", coords)
	}
}

// The first poll goes to the coordinator bootstrap heard from.
func TestMonitorProxies_FirstPollGoesToBootstrapCoordinator(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"10.0.0.1:4500", "10.0.0.2:4500", "10.0.0.3:4500"} {
		db := newPollTestDB(t, "10.0.0.1:4500", "10.0.0.2:4500", "10.0.0.3:4500")
		p := &scriptedPoll{t: t, cancel: db.cancel}
		var known transport.UID
		db.monitorProxiesOneGeneration(p.poll, p.pause, first, &known)
		if p.calls[0].addr != first {
			t.Errorf("first poll went to %s, want %s", p.calls[0].addr, first)
		}
	}
}
