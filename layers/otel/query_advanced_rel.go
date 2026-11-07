package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Advanced span queries on a tenant store (rfcs/relational-exporter-migration.md
// §1.4, §3.2): same-span conjunctions paged by v2 continuation tokens, the
// trace existential join and the trace graph. Each is one transaction of
// budgeted direct reads of the standard OTEL_V1 span indexes.

// ---------------------------------------------------------------------------
// Continuation tokens v2
// ---------------------------------------------------------------------------

const (
	relContinuationVersion = int64(2)
	relContinuationDomain  = "otel/continuation/v2\x00"
)

// relContinuation is a parsed, not yet authenticated, v2 token:
// tuple(2, kind, database_path, schema, digest, last) and its MAC.
type relContinuation struct {
	databasePath, schema string
	digest, last         []byte
	payload, mac         []byte
}

// signRelContinuation signs a token of one query family of a tenant store;
// the kind is authenticated, so families never accept each other's tokens.
func signRelContinuation(secret []byte, kind, databasePath, schema string, digest, last []byte) []byte {
	payload := tuple.Tuple{relContinuationVersion, kind, databasePath, schema, digest, last}.Pack()
	return append(payload, continuationMACIn(relContinuationDomain, secret, payload)...)
}

// parseRelContinuation parses a v2 token of kind. Any other version (v1
// tokens included), another kind or another shape is config.
func parseRelContinuation(token []byte, kind string) (*relContinuation, error) {
	const op = "continuation"
	fields, payload, mac, err := splitContinuation(token)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fail("config", op, "malformed continuation token fields")
	}
	if version, ok := fields[0].(int64); !ok || version != relContinuationVersion {
		return nil, fail("config", op, "unsupported continuation token")
	}
	if len(fields) != 6 {
		return nil, fail("config", op, "malformed continuation token fields")
	}
	c := &relContinuation{payload: payload, mac: mac}
	got, kok := fields[1].(string)
	var pok, sok, dok, lok bool
	c.databasePath, pok = fields[2].(string)
	c.schema, sok = fields[3].(string)
	c.digest, dok = fields[4].([]byte)
	c.last, lok = fields[5].([]byte)
	if !kok || !pok || !sok || !dok || !lok || got != kind {
		return nil, fail("config", op, "unsupported continuation token")
	}
	return c, nil
}

// authenticate checks the MAC under the store's key, then that the token
// belongs to this store and query.
func (c *relContinuation) authenticate(secret []byte, s *tenantStore, digest []byte) error {
	const op = "continuation"
	if err := checkContinuationMAC(relContinuationDomain, "store", secret, c.payload, c.mac); err != nil {
		return err
	}
	switch {
	case c.databasePath != s.databasePath || c.schema != s.schema:
		return fail("config", op, "continuation belongs to another store")
	case !bytes.Equal(c.digest, digest):
		return fail("config", op, "continuation belongs to another query")
	}
	return nil
}

// spanSortKey is a same-span page's last emitted span, the v2 sort key
// (start_ns, trace_id, span_id).
type spanSortKey struct {
	start       int64
	trace, span []byte
}

func (k spanSortKey) pack() []byte { return tuple.Tuple{k.start, k.trace, k.span}.Pack() }

// parseSpanSortKey validates an authenticated token's sort key.
func parseSpanSortKey(raw []byte) (spanSortKey, error) {
	bad := fail("corruption", "continuation", "malformed continuation sort key")
	t, err := tuple.Unpack(raw)
	if err != nil || len(t) != 3 {
		return spanSortKey{}, bad
	}
	start, sok := t[0].(int64)
	trace, tok := t[1].([]byte)
	span, pok := t[2].([]byte)
	if !sok || !tok || !pok || start < 0 || len(trace) != 16 || zeroID(trace) || len(span) != 8 || zeroID(span) {
		return spanSortKey{}, bad
	}
	return spanSortKey{start, trace, span}, nil
}

// continuationKeys holds a tenant store's continuation signing key: 32
// bytes, published once and never replaced.
type continuationKeys interface {
	// load reads the published key in the page's transaction; nil when none is.
	load(store *recordlayer.FDBRecordStore) ([]byte, error)
	// publish stores candidate unless a key exists, in its own transaction,
	// and returns the key that won.
	publish(ctx context.Context, s *tenantStore, candidate []byte) ([]byte, error)
}

// memoryContinuationKeys keeps the key in memory: for tests, which share one
// value among the readers of one store as STORE_META would.
type memoryContinuationKeys struct {
	mu  sync.Mutex
	key []byte
}

func (m *memoryContinuationKeys) load(*recordlayer.FDBRecordStore) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.key, nil
}

func (m *memoryContinuationKeys) publish(_ context.Context, _ *tenantStore, candidate []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key == nil {
		m.key = candidate
	}
	return m.key, nil
}

// storeMetaContinuationKeys is the store's STORE_META("continuation_hmac") row.
//
// TODO(otel phase 2D): wire once the OTEL_V1 template has STORE_META. load is
// a point read of the row in the page's transaction; publish runs s.run,
// reads the row and saves candidate only when it is absent, returning the
// stored key, so a concurrent winner wins. Until then no key can be
// published: a full page fails to sign its continuation.
type storeMetaContinuationKeys struct{}

func (storeMetaContinuationKeys) load(*recordlayer.FDBRecordStore) ([]byte, error) {
	return nil, nil
}

func (storeMetaContinuationKeys) publish(context.Context, *tenantStore, []byte) ([]byte, error) {
	return nil, fail("operation", "continuation key", "the template has no STORE_META row for the continuation signing key yet")
}

// loadContinuationKey reads the store's published key, if any.
func (r *SpanReader) loadContinuationKey(store *recordlayer.FDBRecordStore) ([]byte, error) {
	key, err := r.keys.load(store)
	if err != nil {
		return nil, err
	}
	if len(key) != 0 && len(key) != continuationKeyBytes {
		return nil, fail("corruption", "continuation", "malformed continuation signing key")
	}
	return key, nil
}

// ensureContinuationKey publishes the store's signing key on first use; the
// candidate is drawn outside the retry loop and loses to a concurrent winner.
func (r *SpanReader) ensureContinuationKey(ctx context.Context) ([]byte, error) {
	fresh := make([]byte, continuationKeyBytes)
	if _, err := rand.Read(fresh); err != nil {
		return nil, wrap("operation", "continuation key", err)
	}
	key, err := r.keys.publish(ctx, r.store, fresh)
	if err != nil {
		return nil, err
	}
	if len(key) != continuationKeyBytes {
		return nil, fail("corruption", "continuation", "malformed continuation signing key")
	}
	return key, nil
}
