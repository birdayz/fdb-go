package client

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestSendRecorderLockOrder drives every recorder entry point concurrently
// with writes through recording connections, PING replies included, with
// recording switched on and off. A path that takes a connection's mu while
// holding the recorder's deadlocks against a write reporting a frame; the
// deadline turns that into a failure.
func TestSendRecorderLockOrder(t *testing.T) {
	t.Parallel()
	rec := &sendRecorder{}
	dial := rec.wrap(func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			buf := make([]byte, 4096)
			for {
				if _, err := b.Read(buf); err != nil {
					return
				}
			}
		}()
		return a, nil
	})
	grv := recorderFrame(transport.UID{First: 9}, (&types.GetReadVersionRequest{}).MarshalFDB())
	ping := recorderFrame(transport.UID{First: 3}, pingReplyBody)
	var conns []net.Conn
	for i := 0; i < 4; i++ {
		c, err := dial(context.Background(), "tcp", "x")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write(make([]byte, transport.ConnectPacketSize)); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}

	stopAt := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stopAt) {
				if _, err := c.Write(grv); err != nil {
					t.Error(err)
					return
				}
				if _, err := c.Write(ping); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stopAt) {
			rec.start()
			parser, remove := rec.parser()
			parser.observe(grv)
			_ = rec.isStorageReply(transport.UID{First: 1})
			got := rec.stop()
			remove()
			for _, p := range got.problems {
				if !strings.HasPrefix(p, "partial frame") { // a write may be mid-frame
					t.Errorf("recorder problem: %s", p)
				}
			}
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("recorder deadlocked: a path takes a connection's mu while holding the recorder's")
	}
}

// TestSendRecorderRejectsMalformedField: a GetKeyValuesRequest with an
// out-of-bounds field offset passes wire.NewReader but fails the generated
// decoder, and one with a trailing byte decodes but does not re-encode. The
// recorder rejects both, while the same request well formed is recorded.
func TestSendRecorderRejectsMalformedField(t *testing.T) {
	t.Parallel()
	good := (&types.GetKeyValuesRequest{
		Begin:   types.KeySelectorRef{Key: []byte("a"), Offset: 1},
		End:     types.KeySelectorRef{Key: []byte("b"), Offset: 1},
		Version: 1, Limit: 1, LimitBytes: 1,
	}).MarshalFDB()
	rec := &sendRecorder{}
	rec.start()
	parser, remove := rec.parser()
	defer remove()
	parser.observe(recorderFrame(transport.UID{First: 7}, good))
	parser.observe(recorderFrame(transport.UID{First: 7}, malformedGetKeyValues()))
	parser.observe(recorderFrame(transport.UID{First: 7}, nonCanonicalGetKeyValues()))
	got := rec.stop()
	if len(got.requests) != 1 || !strings.HasPrefix(got.requests[0], "GetKeyValuesRequest") {
		t.Fatalf("requests %q, want the well-formed GetKeyValuesRequest", got.requests)
	}
	if len(got.problems) != 2 || !strings.Contains(got.problems[0], "decode GetKeyValuesRequest.Begin") ||
		!strings.Contains(got.problems[1], "does not re-encode") {
		t.Fatalf("problems %q, want a decode failure and a re-encoding failure", got.problems)
	}
}
