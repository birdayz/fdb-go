package fdb_test

// The Apple binding hands keys to libfdb_c synchronously, so a caller may reuse
// its buffer as soon as Get/GetKey/Watch returns. Holding the GRV reply parks
// the asynchronous read after the call returns, so the reuse is ordered before
// the read without relying on scheduling.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/transport"
)

// ErrorOr<GetReadVersionReply>: ComposedIdentifier<T, 2> | 15709388 (CommitProxyInterface.h:264).
const heldGRVReplyFileID = 2<<24 | 15709388

type grvHoldDialer struct {
	mu     sync.Mutex
	armed  bool
	held   []grvHeldFrame
	heldCh chan struct{}
}

type grvHeldFrame struct {
	conn  *grvHoldConn
	token transport.UID
	body  []byte
}

type grvHoldConn struct {
	net.Conn
	d     *grvHoldDialer
	pr    *io.PipeReader
	pw    *io.PipeWriter
	fwdMu sync.Mutex
}

func (d *grvHoldDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	c := &grvHoldConn{Conn: raw, d: d, pr: pr, pw: pw}
	go c.readLoop()
	return c, nil
}

func (d *grvHoldDialer) arm() {
	d.mu.Lock()
	d.armed = true
	d.mu.Unlock()
}

// release stops withholding and delivers every withheld reply in order.
func (d *grvHoldDialer) release() {
	d.mu.Lock()
	held := d.held
	d.held, d.armed = nil, false
	d.mu.Unlock()
	for _, f := range held {
		_ = f.conn.forward(f.token, f.body) // a closed connection drops it
	}
}

func (d *grvHoldDialer) withhold(c *grvHoldConn, token transport.UID, body []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.armed {
		return false
	}
	d.held = append(d.held, grvHeldFrame{conn: c, token: token, body: bytes.Clone(body)})
	select {
	case d.heldCh <- struct{}{}:
	default:
	}
	return true
}

func (c *grvHoldConn) Read(b []byte) (int, error) { return c.pr.Read(b) }

func (c *grvHoldConn) Close() error {
	c.pr.Close() // unblocks a release racing teardown
	return c.Conn.Close()
}

func (c *grvHoldConn) forward(token transport.UID, body []byte) error {
	c.fwdMu.Lock()
	defer c.fwdMu.Unlock()
	return transport.WriteFrame(c.pw, token, body, false)
}

func (c *grvHoldConn) readLoop() {
	var hello [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(c.Conn, hello[:]); err != nil {
		c.pw.CloseWithError(err)
		return
	}
	c.fwdMu.Lock()
	_, err := c.pw.Write(hello[:])
	c.fwdMu.Unlock()
	if err != nil {
		return
	}
	for {
		token, body, err := transport.ReadFrame(c.Conn, false)
		if err != nil {
			c.pw.CloseWithError(err)
			return
		}
		if len(body) >= 8 && binary.LittleEndian.Uint32(body[4:8]) == heldGRVReplyFileID && c.d.withhold(c, token, body) {
			continue
		}
		if c.forward(token, body) != nil {
			return
		}
	}
}

func openGRVHoldDB(t *testing.T) (fdb.Database, *grvHoldDialer) {
	t.Helper()
	if sharedClusterFile == nil {
		t.Fatal("shared FDB container not initialized — TestMain must run first")
	}
	d := &grvHoldDialer{heldCh: make(chan struct{}, 16)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := fdb.OpenDatabaseFromConfig(ctx, sharedClusterFile, fdb.WithDialFunc(d.dial))
	if err != nil {
		t.Fatalf("OpenDatabaseFromConfig: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Options().SetTransactionTimeout(10000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.release) // runs before Close
	// Learn the proxies and open the connections before anything is withheld.
	if _, err := db.ReadTransact(func(rt fdb.ReadTransaction) (any, error) {
		return rt.GetReadVersion().Get()
	}); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	return db, d
}

func scribbleKeyBuf(b []byte) {
	for i := range b {
		b[i] = 0xEE
	}
}

func awaitResult[T any](t *testing.T, what string, get func() (T, error)) (T, error) {
	t.Helper()
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := get()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not complete", what)
		var zero T
		return zero, nil
	}
}

func TestFacadeAsyncReadKeyOwnedAfterReturn(t *testing.T) {
	t.Parallel()
	db, gate := openGRVHoldDB(t)
	key := []byte(t.Name() + "/a")
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(key), []byte("original"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct {
		name string
		read func(tr fdb.Transaction, k fdb.Key) func() ([]byte, error)
		want []byte
	}{
		{"Snapshot.Get", func(tr fdb.Transaction, k fdb.Key) func() ([]byte, error) {
			return tr.Snapshot().Get(k).Get
		}, []byte("original")},
		{"GetKey", func(tr fdb.Transaction, k fdb.Key) func() ([]byte, error) {
			f := tr.GetKey(fdb.FirstGreaterOrEqual(k))
			return func() ([]byte, error) { got, err := f.Get(); return got, err }
		}, key},
		{"Snapshot.GetKey", func(tr fdb.Transaction, k fdb.Key) func() ([]byte, error) {
			f := tr.Snapshot().GetKey(fdb.FirstGreaterOrEqual(k))
			return func() ([]byte, error) { got, err := f.Get(); return got, err }
		}, key},
	}
	// Sequential: one gate, one parked read at a time.
	for _, c := range cases {
		tr, err := db.CreateTransaction() // fresh: the read needs a GRV
		if err != nil {
			t.Fatalf("%s: CreateTransaction: %v", c.name, err)
		}
		defer tr.Cancel()
		buf := bytes.Clone(key)
		gate.arm()
		get := c.read(tr, fdb.Key(buf))
		scribbleKeyBuf(buf) // the caller reuses its buffer once the call returns
		select {
		case <-gate.heldCh:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: GRV reply was never withheld", c.name)
		}
		gate.release()
		got, err := awaitResult(t, c.name, get)
		tr.Cancel()
		if err != nil || !bytes.Equal(got, c.want) {
			t.Errorf("ASYNC_READ_KEY_ALIASED: %s = %q, %v; want %q (the key before the caller reused its buffer)", c.name, got, err, c.want)
		}
	}
}

// Watch reads its value synchronously but polls after Commit: reusing the key
// buffer between Watch and Commit must not move the watch.
func TestFacadeWatchKeyOwnedAfterReturn(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	key := []byte(t.Name() + "/watched")
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Clear(fdb.Key(key)) // absent, so a watch on the reused buffer also sees "absent" and never fires
		return nil, nil
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	defer tr.Cancel()
	if err := tr.Options().SetTimeout(10000); err != nil {
		t.Fatal(err)
	}
	buf := bytes.Clone(key)
	w := tr.Watch(fdb.Key(buf))
	defer w.Cancel()
	scribbleKeyBuf(buf)
	if err := tr.Commit().Get(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(fdb.Key(key), []byte("changed"))
		return nil, nil
	}); err != nil {
		t.Fatalf("change: %v", err)
	}
	if _, err := awaitResult(t, "WATCH_KEY_ALIASED: watch on the original key", func() (struct{}, error) {
		return struct{}{}, w.Get()
	}); err != nil {
		t.Fatalf("watch: %v", err)
	}
}
