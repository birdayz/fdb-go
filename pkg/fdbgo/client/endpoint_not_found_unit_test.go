package client

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

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
	full := make(map[endpointKey]struct{}, maxFailedEndpoints+1)
	for i := 0; len(full) <= maxFailedEndpoints; i++ {
		full[endpointKey{"c:1", transport.UID{First: uint64(i) + 100}}] = struct{}{}
	}
	fm.epFailed.Store(&full)
	fm.markEndpointNotFound("d:1", tok)
	if n := len(*fm.epFailed.Load()); n != 1 || fm.endpointFailed("a:1", tok) || !fm.endpointFailed("d:1", tok) {
		t.Fatalf("cap overflow: len=%d", n)
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

// A primary that fails before the hedge timer must not consume the secondary:
// the secondary is sent and wins, and the primary is handed back for accounting.
func TestHedge_FailedPrimaryTriesSecondary(t *testing.T) {
	t.Parallel()
	enf := &transport.EndpointNotFoundError{Token: transport.UID{First: 3}}
	pCh := make(chan transport.Response, 1)
	pCh <- transport.Response{Err: enf}
	sCh := make(chan transport.Response, 1)
	sCh <- transport.Response{Body: []byte("secondary")}
	primary := func() inFlightRPC {
		return inFlightRPC{replyCh: pCh, replyHandle: &transport.ReplyHandle{}, addr: "p", delta: 1, start: time.Now()}
	}
	secondary := func() inFlightRPC {
		return inFlightRPC{replyCh: sCh, replyHandle: &transport.ReplyHandle{}, addr: "s", delta: 2, start: time.Now()}
	}
	res := sendFrameWithHedge(context.Background(), time.Hour, primary, secondary, 5*time.Second)
	if res.err != nil || string(res.body) != "secondary" || res.addr != "s" {
		t.Fatalf("want the secondary's reply, got %+v", res)
	}
	if len(res.others) != 1 || res.others[0].addr != "p" || res.others[0].delta != 1 || !errors.Is(res.others[0].err, transport.ErrEndpointNotFound) {
		t.Fatalf("failed primary must be returned for accounting: %+v", res.others)
	}

	// endOther reports the failure: a teardown fails the address, not-found does not.
	db := &database{failMon: newFailureMonitor(), queueModel: newQueueModel(), connPool: map[string]*transport.Conn{}}
	db.queueModel.startRequest("p")
	db.queueModel.startRequest("q")
	db.endOther(res.others[0])
	db.endOther(rpcAccount{addr: "q", delta: 1, start: time.Now(), err: transport.ErrConnClosed})
	if db.failMon.isFailed("p") || !db.failMon.isFailed("q") {
		t.Fatalf("endOther: p failed=%v q failed=%v", db.failMon.isFailed("p"), db.failMon.isFailed("q"))
	}
}

// The delay follows LoadBalance.actor.cpp:28-53: 50ms floor, growth with the
// storm's age once per skip window, reset after a quiet period.
func TestAllAlternativesFailedDelay(t *testing.T) {
	t.Parallel()
	fm := newFailureMonitor()
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	t0 := 1000.0
	steps := []struct {
		at, wantMS float64
	}{
		{t0, 50},         // first failure: floor, starts the skip window
		{t0 + 0.5, 100},  // elapsed 0.5s * 0.2
		{t0 + 0.9, 180},  // elapsed 0.9s * 0.2
		{t0 + 1.5, 50},   // skip window lapsed: floor once more
		{t0 + 2.0, 400},  // elapsed 2s * 0.2
		{t0 + 4.5, 50},   // skip
		{t0 + 5.0, 1000}, // capped at ALTERNATIVES_FAILURE_MAX_DELAY
		{t0 + 30.0, 50},  // quiet > ALTERNATIVES_FAILURE_RESET_TIME: storm restarts
		{t0 + 30.5, 100},
	}
	for i, st := range steps {
		if got := ms(fm.allAlternativesFailedDelay(st.at)); math.Abs(got-st.wantMS) > 0.01 {
			t.Fatalf("step %d at +%.1fs: %.3fms, want %.0fms", i, st.at-t0, got, st.wantMS)
		}
	}
	// A long storm grows past 1s via the slow ratio, capped at 30s.
	slow := newFailureMonitor()
	var last time.Duration
	for at := t0; at <= t0+1000; at += 0.5 {
		last = slow.allAlternativesFailedDelay(at)
	}
	if math.Abs(ms(last)-30000) > 0.01 {
		t.Fatalf("long storm: %.0fms, want 30000", ms(last))
	}
	var nilFM *failureMonitor
	if math.Abs(ms(nilFM.allAlternativesFailedDelay(t0))-50) > 0.01 {
		t.Fatal("nil monitor must use the floor")
	}
}

func TestAllAlternativesFailedWaitsAndHonorsCancel(t *testing.T) {
	t.Parallel()
	db := &database{failMon: newFailureMonitor()}
	start := time.Now()
	err := db.allAlternativesFailed(context.Background())
	if !isAllAlternativesFailed(err) {
		t.Fatalf("want all_alternatives_failed, got %v", err)
	}
	if waited := time.Since(start); waited < 45*time.Millisecond {
		t.Fatalf("returned after %v, want >= the jittered 50ms floor", waited)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.allAlternativesFailed(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
}
