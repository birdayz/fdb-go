package client

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestEndpointNotFound_RealServer pins the 7.3.77 ENDPOINT_NOT_FOUND packet
// against a real storage server: a request to a stream token the server does
// not have fails at once (not at the RPC timeout), the endpoint is recorded,
// the connection survives, and later reads skip the dead endpoint.
func TestEndpointNotFound_RealServer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	key := []byte(t.Name())
	want := []byte("v")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, want)
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rv, _, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	loc, err := db.db.locCache.locate(db.db, ctx, key, NoTenantID, types.SpanContext{}, false)
	if err != nil || len(loc.Servers) == 0 {
		t.Fatalf("locate: %v", err)
	}
	replica := loc.Servers[0]
	conn, err := db.db.getOrDial(ctx, replica.Address)
	if err != nil {
		t.Fatal(err)
	}
	// TOKEN_STREAM_FLAG (low bit of First) set: the server only answers
	// stream tokens with ENDPOINT_NOT_FOUND (FlowTransport.actor.cpp:1215-1237).
	bogus := transport.UID{First: transport.NewUID().First | 1, Second: transport.NewUID().Second}

	token, reply, handle := conn.PrepareReplyTo(bogus)
	defer handle.Release()
	body, pooled := buildGetValueRequest(key, rv, false, NoTenantID, types.SpanContext{}, readTagSample{}, token, bogus)
	start := time.Now()
	if err := conn.SendFrame(bogus, body); err != nil {
		t.Fatalf("send: %v", err)
	}
	getValueBufPool.Put(pooled)
	select {
	case resp := <-reply:
		var enf *transport.EndpointNotFoundError
		if !errors.As(resp.Err, &enf) || enf.Token != bogus {
			t.Fatalf("want EndpointNotFoundError{%v}, got %+v", bogus, resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no ENDPOINT_NOT_FOUND within 3s: packet not decoded")
	}
	t.Logf("ENDPOINT_NOT_FOUND after %v", time.Since(start))
	if !db.db.failMon.endpointFailed(replica.Address, bogus) {
		t.Fatal("the database did not record the dead endpoint")
	}
	if conn.IsClosed() || db.db.failMon.isFailed(replica.Address) {
		t.Fatal("ENDPOINT_NOT_FOUND must not close the connection or fail the address")
	}

	// A read whose replica list holds the dead endpoint skips it.
	dead := ServerInfo{Address: replica.Address, Token: bogus}
	if live, ok := db.db.failMon.liveServers([]ServerInfo{dead, replica}, EndpointGetValue); !ok || len(live) != 1 || live[0] != replica {
		t.Fatalf("liveServers: %v %v", live, ok)
	}
	tx := db.CreateTransaction()
	tx.readVersion = rv
	got, err := tx.sendGetValue(ctx, key, []ServerInfo{dead, replica}, readTagSample{})
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read past dead endpoint: %q %v", got, err)
	}
}
