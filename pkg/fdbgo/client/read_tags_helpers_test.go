package client

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// readTagDraws scripts sampleReadTags' random source and counts its draws. At
// rate 0.5 a drawHit samples and a drawMiss does not; draws past the script miss.
type readTagDraws struct {
	mu   sync.Mutex
	vals []float64
	n    int
}

const (
	drawHit  = 0.0
	drawMiss = 1.0
)

func (d *readTagDraws) draw() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := drawMiss
	if d.n < len(d.vals) {
		v = d.vals[d.n]
	}
	d.n++
	return v
}

// script restarts the count with the given draws.
func (d *readTagDraws) script(vals ...float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.vals, d.n = vals, 0
}

func (d *readTagDraws) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

// taggedRequest is a recorded read request's kind and read tags.
type taggedRequest struct {
	kind string
	has  bool
	tags string
}

func (r taggedRequest) String() string {
	return fmt.Sprintf("%s HasTags=%v Tags=%q", r.kind, r.has, r.tags)
}

var (
	recordedHasTags = regexp.MustCompile(`[ \[]HasTags=(true|false)`)
	recordedTags    = regexp.MustCompile(`[ \[]Tags=("(?:[^"\\]|\\.)*")`)
	readTagKinds    = map[string]bool{"GetValueRequest": true, "GetKeyRequest": true, "GetKeyValuesRequest": true, "GetMappedKeyValuesRequest": true, "WatchValueRequest": true}
)

// taggedRequests returns the recorded read requests of the five kinds that carry read tags.
func taggedRequests(t *testing.T, rec recording) []taggedRequest {
	t.Helper()
	// The database's coordinator long poll may fall into a recording window;
	// it is background traffic, not a read.
	coordPoll := fmt.Sprintf("unexpected request kind: file identifier %d ", types.OpenDatabaseCoordRequestFileID)
	var problems []string
	for _, p := range rec.problems {
		if !strings.HasPrefix(p, coordPoll) {
			problems = append(problems, p)
		}
	}
	if len(problems) > 0 {
		t.Fatalf("recorder problems:\n%s", strings.Join(problems, "\n"))
	}
	var out []taggedRequest
	for _, r := range rec.requests {
		kind, _, _ := strings.Cut(r, " ")
		if !readTagKinds[kind] {
			continue
		}
		has, tags := recordedHasTags.FindStringSubmatch(r), recordedTags.FindStringSubmatch(r)
		if has == nil || tags == nil {
			t.Fatalf("no tag fields in recorded request:\n%s", r)
		}
		unquoted, err := strconv.Unquote(tags[1])
		if err != nil {
			t.Fatalf("recorded Tags %s: %v", tags[1], err)
		}
		out = append(out, taggedRequest{kind: kind, has: has[1] == "true", tags: unquoted})
	}
	return out
}

func openReadTagDB(t *testing.T, ctx context.Context, cf *ClusterFile, dial transport.DialFunc, rate float64, draw func() float64) (*Database, *sendRecorder) {
	t.Helper()
	rec := &sendRecorder{}
	db, err := OpenDatabaseFromConfig(ctx, cf, WithDialFunc(rec.wrap(dial)), WithAPIVersion(730), withReadTagSampling(rate, draw))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, rec
}

// waitForRecorded waits until rec has recorded a request of kind.
func waitForRecorded(t *testing.T, ctx context.Context, rec *sendRecorder, kind string) {
	t.Helper()
	for {
		rec.mu.Lock()
		for _, r := range rec.rec.requests {
			if strings.HasPrefix(r, kind+" ") {
				rec.mu.Unlock()
				return
			}
		}
		rec.mu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatalf("no %s recorded", kind)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// setKey writes value at key in its own transaction.
func setKey(t *testing.T, ctx context.Context, db *Database, key, value []byte) {
	t.Helper()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, value)
		return nil, nil
	}); err != nil {
		t.Fatalf("set %q: %v", key, err)
	}
}
