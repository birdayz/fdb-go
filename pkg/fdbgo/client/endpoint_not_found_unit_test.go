package client

import (
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

func TestFailureMonitorEndpointSet(t *testing.T) {
	t.Parallel()
	var nilFM *failureMonitor
	if nilFM.endpointFailed("a:1", transport.UID{First: 1}) {
		t.Fatal("nil monitor reports a failed endpoint")
	}
	fm := newFailureMonitor()
	tok := transport.UID{First: 5, Second: 6}
	fm.markEndpointNotFound("a:1", tok)
	if !fm.endpointFailed("a:1", tok) {
		t.Fatal("marked endpoint not reported failed")
	}
	if fm.endpointFailed("b:1", tok) || fm.endpointFailed("a:1", transport.UID{First: 5}) {
		t.Fatal("failure leaked to another address or token")
	}
	if fm.isFailed("a:1") {
		t.Fatal("an endpoint failure must not fail the address")
	}

	// Past the cap the whole set is cleared before the insert, as in C++.
	for i := 0; len(fm.epFailed) <= maxFailedEndpoints; i++ {
		fm.epFailed[endpointKey{"c:1", transport.UID{First: uint64(i) + 100}}] = struct{}{}
	}
	fm.markEndpointNotFound("d:1", tok)
	if len(fm.epFailed) != 1 || fm.endpointFailed("a:1", tok) || !fm.endpointFailed("d:1", tok) {
		t.Fatalf("cap overflow: len=%d", len(fm.epFailed))
	}
}

func TestLiveServers(t *testing.T) { // not parallel: AllocsPerRun
	servers := []ServerInfo{
		{Address: "a:1", Token: transport.UID{First: 0x10, Second: 0x20}},
		{Address: "b:1", Token: transport.UID{First: 0x30, Second: 0x40}},
		{Address: "c:1", Token: transport.UID{First: 0x50, Second: 0x60}},
	}
	fm := newFailureMonitor()
	if allocs := testing.AllocsPerRun(100, func() { fm.liveServers(servers, EndpointGetValue) }); allocs != 0 {
		t.Fatalf("empty set must be allocation-free, got %v allocs", allocs)
	}
	// The set is keyed by the stream's adjusted token: a dead getKey stream
	// does not hide the replica from getValue.
	fm.markEndpointNotFound("a:1", getAdjustedEndpoint(servers[0].Token, EndpointGetKey))
	if live, ok := fm.liveServers(servers, EndpointGetValue); !ok || len(live) != 3 {
		t.Fatalf("getValue: live=%v ok=%v", live, ok)
	}
	live, ok := fm.liveServers(servers, EndpointGetKey)
	if !ok || len(live) != 2 || live[0].Address != "b:1" || live[1].Address != "c:1" {
		t.Fatalf("getKey: live=%v ok=%v", live, ok)
	}
	fm.markEndpointNotFound("b:1", getAdjustedEndpoint(servers[1].Token, EndpointGetKey))
	fm.markEndpointNotFound("c:1", getAdjustedEndpoint(servers[2].Token, EndpointGetKey))
	if live, ok := fm.liveServers(servers, EndpointGetKey); ok || len(live) != 0 {
		t.Fatalf("all dead: live=%v ok=%v", live, ok)
	}
	if len(servers) != 3 || servers[0].Address != "a:1" {
		t.Fatal("liveServers mutated its input")
	}
}

// An ENDPOINT_NOT_FOUND reply is maybe-delivered for the load balancer, but it
// must neither close the pooled connection nor fail the address.
func TestEndpointNotFoundClassification(t *testing.T) {
	t.Parallel()
	err := &transport.EndpointNotFoundError{Token: transport.UID{First: 9}}
	if !isEndpointNotFound(err) || !isMaybeDelivered(err) {
		t.Fatal("not-found must classify as maybe-delivered")
	}
	if dispositionForReplyError(err, false) != replyTryNextAlternative ||
		dispositionForReplyError(err, true) != replyConvertToMaybeDelivered {
		t.Fatal("not-found disposition differs from request_maybe_delivered")
	}
	db := &database{failMon: newFailureMonitor(), connPool: map[string]*transport.Conn{}}
	db.handleReadConnError("a:1", err)
	if db.failMon.isFailed("a:1") {
		t.Fatal("not-found marked the address failed")
	}
	db.handleReadConnError("a:1", transport.ErrConnClosed)
	if !db.failMon.isFailed("a:1") {
		t.Fatal("a real teardown must still mark the address failed")
	}
}

func TestNoteBrokenPromise(t *testing.T) {
	t.Parallel()
	db := &database{failMon: newFailureMonitor()}
	tok := transport.UID{First: 7}
	db.noteBrokenPromise(&wire.FDBError{Code: ErrRequestMaybeDelivered}, "a:1", tok)
	db.noteBrokenPromise(nil, "a:1", tok)
	if db.failMon.endpointFailed("a:1", tok) {
		t.Fatal("only broken_promise marks the endpoint")
	}
	db.noteBrokenPromise(&wire.FDBError{Code: ErrBrokenPromise}, "a:1", tok)
	if !db.failMon.endpointFailed("a:1", tok) {
		t.Fatal("in-band broken_promise must mark the endpoint (genericactors.actor.h:397-402)")
	}
	servers := []ServerInfo{{Address: "b:1", Token: transport.UID{First: 0x11}}}
	db.noteReadBrokenPromise(&wire.FDBError{Code: ErrBrokenPromise}, servers, "b:1", EndpointGetKeyValues)
	if !db.failMon.endpointFailed("b:1", getAdjustedEndpoint(servers[0].Token, EndpointGetKeyValues)) {
		t.Fatal("read broken_promise must mark the replica's stream endpoint")
	}
}

func TestGetCommitProxySkipsFailedEndpoint(t *testing.T) {
	t.Parallel()
	db := &database{failMon: newFailureMonitor()}
	proxies := []ProxyInfo{
		{Address: "p:1", Token: transport.UID{First: 1}},
		{Address: "p:2", Token: transport.UID{First: 2}},
	}
	db.dbInfo.Store(&DBInfo{CommitProxies: proxies})
	db.failMon.markEndpointNotFound("p:1", proxies[0].Token)
	for i := 0; i < 4; i++ {
		p, _, err := db.getCommitProxy()
		if err != nil || p.Address != "p:2" {
			t.Fatalf("pick %d: %v %v", i, p, err)
		}
	}
	db.failMon.markEndpointNotFound("p:2", proxies[1].Token)
	if _, _, err := db.getCommitProxy(); err == nil {
		t.Fatal("all commit proxies failed: want the no-proxy path")
	}
}
