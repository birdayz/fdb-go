package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func qrTestStore() *tenantStore {
	path, schema := relTenantPaths("qa_run_0001", 3)
	return &tenantStore{databasePath: path, schema: schema}
}

func qrSortKey(start int64, trace, span int) spanSortKey {
	id := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			panic(err)
		}
		return b
	}
	return spanSortKey{start, id(qaID(trace, 16)), id(qaID(span, 8))}
}

func TestRelContinuationRoundTrip(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{7}, continuationKeyBytes)
	s := qrTestStore()
	digest := queryDigest(continuationKind, []Attribute{{"span", "gen_ai.request.model", "string", "gpt-4o"}}, timeWindow{})
	last := qrSortKey(42, 1, 2)
	token := signRelContinuation(secret, continuationKind, s.databasePath, s.schema, digest, last.pack())
	c, err := parseRelContinuation(token, continuationKind)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.authenticate(secret, s, digest); err != nil {
		t.Fatal(err)
	}
	got, err := parseSpanSortKey(c.last)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, last) {
		t.Fatalf("resume key: got %#v want %#v", got, last)
	}
	if c.databasePath != "/OTEL/R_QA_RUN_0001" || c.schema != "T3" || len(token) > maxContinuationBytes {
		t.Fatalf("token binds %q %q in %d bytes", c.databasePath, c.schema, len(token))
	}
}

// The v1 token-security matrix on v2 tokens: the MAC and the absent key are
// corruption, another store or query is config, as is any token shape
// other than a v2 token of the query's kind; an authenticated but malformed
// sort key is corruption.
func TestRelContinuationRejections(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{7}, continuationKeyBytes)
	s := qrTestStore()
	digest := queryDigest(continuationKind, []Attribute{{"span", "k", "string", "v"}}, timeWindow{})
	last := qrSortKey(42, 1, 2).pack()
	sign := func(kind, path, schema string, dig, key []byte) []byte {
		return signRelContinuation(secret, kind, path, schema, dig, key)
	}
	token := sign(continuationKind, s.databasePath, s.schema, digest, last)
	check := func(tok, key, dig []byte) error {
		c, err := parseRelContinuation(tok, continuationKind)
		if err != nil {
			return err
		}
		if err := c.authenticate(key, s, dig); err != nil {
			return err
		}
		_, err = parseSpanSortKey(c.last)
		return err
	}
	if err := check(token, secret, digest); err != nil {
		t.Fatalf("valid token: %v", err)
	}

	// Every single-bit flip anywhere in the token must be rejected.
	for i := range token {
		for bit := range 8 {
			mutated := append([]byte(nil), token...)
			mutated[i] ^= 1 << bit
			err := check(mutated, secret, digest)
			if err == nil {
				t.Fatalf("bit flip at byte %d bit %d accepted", i, bit)
			}
			requireConfigOrCorruption(t, fmt.Sprintf("flip %d/%d", i, bit), err)
		}
	}
	macFlip := append([]byte(nil), token...)
	macFlip[len(macFlip)-1] ^= 0x80
	requireTyped[*CorruptionError](t, "MAC flip", check(macFlip, secret, digest))
	requireTyped[*CorruptionError](t, "wrong secret", check(token, bytes.Repeat([]byte{8}, continuationKeyBytes), digest))
	requireTyped[*CorruptionError](t, "absent secret", check(token, nil, digest))

	other := queryDigest(continuationKind, []Attribute{{"span", "k", "string", "w"}}, timeWindow{})
	requireTyped[*ConfigError](t, "wrong query", check(token, secret, other))
	otherPath, _ := relTenantPaths("qa_run_other", 3)
	requireTyped[*ConfigError](t, "wrong database", check(sign(continuationKind, otherPath, s.schema, digest, last), secret, digest))
	_, otherSchema := relTenantPaths("qa_run_0001", 4)
	requireTyped[*ConfigError](t, "wrong schema", check(sign(continuationKind, s.databasePath, otherSchema, digest, last), secret, digest))

	// Authenticated but semantically malformed sort keys are corruption.
	trace, span := qaBytes(t, qaID(1, 16)), qaBytes(t, qaID(2, 8))
	for name, key := range map[string][]byte{
		"not a tuple":      {0xff, 0x01},
		"v1 time key":      postingSuffix(Span{Trace: qaID(1, 16), ID: qaID(2, 8), Start: 42}, "time").Pack(),
		"hex identities":   tuple.Tuple{int64(5), qaID(1, 16), qaID(2, 8)}.Pack(),
		"negative start":   tuple.Tuple{int64(-5), trace, span}.Pack(),
		"short trace":      tuple.Tuple{int64(5), trace[:15], span}.Pack(),
		"zero trace":       tuple.Tuple{int64(5), make([]byte, 16), span}.Pack(),
		"zero span":        tuple.Tuple{int64(5), trace, make([]byte, 8)}.Pack(),
		"extra key fields": tuple.Tuple{int64(5), trace, span, int64(0)}.Pack(),
		"missing span":     tuple.Tuple{int64(5), trace}.Pack(),
	} {
		requireTyped[*CorruptionError](t, name, check(sign(continuationKind, s.databasePath, s.schema, digest, key), secret, digest))
	}

	// Structurally unsupported tokens are configuration errors before any read.
	payload := tuple.Tuple{int64(3), continuationKind, s.databasePath, s.schema, digest, last}.Pack()
	badVersion := append(payload, continuationMACIn(relContinuationDomain, secret, payload)...)
	payload = tuple.Tuple{relContinuationVersion, continuationKind, s.databasePath}.Pack()
	short := append(payload, continuationMACIn(relContinuationDomain, secret, payload)...)
	payload = tuple.Tuple{relContinuationVersion, continuationKind, s.databasePath, int64(3), digest, last}.Pack()
	badSchema := append(payload, continuationMACIn(relContinuationDomain, secret, payload)...)
	for name, tok := range map[string][]byte{
		"empty MAC only": token[len(token)-32:],
		"one byte":       {1},
		"oversized":      bytes.Repeat([]byte{1}, maxContinuationBytes+1),
		"garbage":        bytes.Repeat([]byte{0xfe}, 64),
		"v1 token":       signContinuation(secret, "qa_run_0001", 3, "time", digest, last),
		"bad version":    badVersion,
		"other kind":     sign(messageContinuationKind, s.databasePath, s.schema, digest, last),
		"few fields":     short,
		"typed fields":   badSchema,
	} {
		_, err := parseRelContinuation(tok, continuationKind)
		requireTyped[*ConfigError](t, name, err)
	}
	// A v2 token never verifies under the v1 domain, nor a v1 token under v2.
	if c, err := parseContinuation(signContinuation(secret, "qa_run_0001", 3, "time", digest, last)); err != nil || checkContinuationMAC(relContinuationDomain, "store", secret, c.payload, c.mac) == nil {
		t.Fatalf("a v1 MAC verified in the v2 domain: %v", err)
	}
}

type qrFixedKeys struct{ winner []byte }

func (k qrFixedKeys) load(*recordlayer.FDBRecordStore) ([]byte, error) { return k.winner, nil }
func (k qrFixedKeys) publish(context.Context, *tenantStore, []byte) ([]byte, error) {
	return k.winner, nil
}

// The published key, not the caller's candidate, signs: a concurrent winner
// wins. A malformed stored key is corruption.
func TestEnsureContinuationKey(t *testing.T) {
	t.Parallel()
	winner := bytes.Repeat([]byte{9}, continuationKeyBytes)
	r := &SpanReader{store: qrTestStore(), keys: qrFixedKeys{winner}}
	got, err := r.ensureContinuationKey(t.Context())
	if err != nil || !bytes.Equal(got, winner) {
		t.Fatalf("ensure = %x, %v; want the winner", got, err)
	}
	r.keys = qrFixedKeys{winner[:5]}
	_, err = r.ensureContinuationKey(t.Context())
	requireTyped[*CorruptionError](t, "short published key", err)
	_, err = r.loadContinuationKey(nil)
	requireTyped[*CorruptionError](t, "short loaded key", err)

	mem := &memoryContinuationKeys{}
	r.keys = mem
	first, err := r.ensureContinuationKey(t.Context())
	if err != nil || len(first) != continuationKeyBytes {
		t.Fatalf("first ensure = %x, %v", first, err)
	}
	second, err := r.ensureContinuationKey(t.Context())
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("second ensure replaced the key: %x then %x, %v", first, second, err)
	}
	if loaded, err := r.loadContinuationKey(nil); err != nil || !bytes.Equal(loaded, first) {
		t.Fatalf("load = %x, %v", loaded, err)
	}

	// Until STORE_META lands no key can be published, and none is loaded.
	r.keys = storeMetaContinuationKeys{}
	if k, err := r.loadContinuationKey(nil); err != nil || k != nil {
		t.Fatalf("STORE_META stub load = %x, %v", k, err)
	}
	_, err = r.ensureContinuationKey(t.Context())
	requireTyped[*OperationError](t, "STORE_META stub publish", err)
}
