package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func qrTestStore() *tenantStore {
	path, schema := relTenantPaths("qa_run_0001", 3)
	return &tenantStore{databasePath: path, schema: schema}
}

// signV1Continuation is a time-order same_span token of the removed custom
// store (v1: run and tenant bound, domain otel/continuation/v1), which v2
// parsing refuses.
func signV1Continuation(secret []byte, run string, tenant int, digest, last []byte) []byte {
	payload := tuple.Tuple{int64(1), continuationKind, run, int64(tenant), "time", digest, last}.Pack()
	return append(payload, continuationMACIn("otel/continuation/v1\x00", secret, payload)...)
}

// v1TimeKey is the removed custom store's time-order sort key (its
// postingSuffix): negated start, the complemented hex trace, the hex span.
func v1TimeKey(trace, span string, start int64) []byte {
	rev := []byte(trace)
	for i := range rev {
		rev[i] = ^rev[i]
	}
	return tuple.Tuple{-start, rev, span}.Pack()
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
		"v1 time key":      v1TimeKey(qaID(1, 16), qaID(2, 8), 42),
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
		"v1 token":       signV1Continuation(secret, "qa_run_0001", 3, digest, last),
		"bad version":    badVersion,
		"other kind":     sign(messageContinuationKind, s.databasePath, s.schema, digest, last),
		"few fields":     short,
		"typed fields":   badSchema,
	} {
		_, err := parseRelContinuation(tok, continuationKind)
		requireTyped[*ConfigError](t, name, err)
	}
	// A v2 token never verifies under the v1 domain, nor a v1 token under v2.
	if _, payload, mac, err := splitContinuation(signV1Continuation(secret, "qa_run_0001", 3, digest, last)); err != nil || checkContinuationMAC(relContinuationDomain, "store", secret, payload, mac) == nil {
		t.Fatalf("a v1 MAC verified in the v2 domain: %v", err)
	}
}

// D1: double and bool are typed predicates now, so the formerly rejected bool
// type is valid; bytes and JSON stay unqueryable, and typed values must be
// canonical.
func TestRelSameSpanPlanValidation(t *testing.T) {
	t.Parallel()
	r := &SpanReader{cfg: Config{K: 100}}
	ok := Attribute{"span", "k", "string", "v"}
	many := make([]Attribute, maxQueryPredicates+1)
	for i := range many {
		many[i] = Attribute{"span", fmt.Sprint(i), "string", "v"}
	}
	for name, q := range map[string]SameSpanQuery{
		"no predicates":       {},
		"too many":            {Predicates: many},
		"unknown scope":       {Predicates: []Attribute{{"event", "k", "string", "v"}}},
		"bytes type":          {Predicates: []Attribute{{"span", "k", "bytes", "AQI="}}},
		"json type":           {Predicates: []Attribute{{"span", "k", "json", "[1]"}}},
		"noncanonical int":    {Predicates: []Attribute{{"span", "k", "int", "007"}}},
		"int overflow":        {Predicates: []Attribute{{"span", "k", "int", "9223372036854775808"}}},
		"noncanonical double": {Predicates: []Attribute{{"span", "k", "double", "0.50"}}},
		"double word":         {Predicates: []Attribute{{"span", "k", "double", "half"}}},
		"lowercase inf":       {Predicates: []Attribute{{"span", "k", "double", "inf"}}},
		"noncanonical bool":   {Predicates: []Attribute{{"span", "k", "bool", "TRUE"}}},
		"oversized key":       {Predicates: []Attribute{{"span", string(make([]byte, maxIndexedKeyBytes+1)), "string", "v"}}},
		"oversized value":     {Predicates: []Attribute{{"span", "k", "string", string(make([]byte, maxIndexedValueBytes+1))}}},
		"negative limit":      {Predicates: []Attribute{ok}, Limit: -1},
		"inverted interval":   {Predicates: []Attribute{ok}, TimeWindow: &TimeRange{Start: 5, End: 4}},
	} {
		_, err := r.newSameSpanPlan("same-span query", q)
		requireTyped[*ConfigError](t, name, err)
	}
	long := strings.Repeat("y", maxIndexedKeyBytes+1)
	p, err := r.newSameSpanPlan("same-span query", SameSpanQuery{Predicates: []Attribute{
		{"resource", "k", "int", "-12"},
		{"span", "b", "bool", "true"},
		{"span", "d", "double", "0.5"},
		{"span", "n", "double", "NaN"},
		{"span", "z", "double", "-0"},
		{"scope", "l", "string", long},
		ok, ok,
	}})
	if err != nil {
		t.Fatalf("canonical typed predicates rejected: %v", err)
	}
	if len(p.attrs) != 7 || p.limit != 100 || p.attrs[0].q.index != spansIntAttrIndex || p.attrs[1].q.index != spansBoolAttrIndex ||
		p.attrs[2].q.index != spansDoubleAttrIndex || p.attrs[5].q.index != spansLongStrIndex || p.attrs[5].q.long == nil || p.attrs[6].q.index != spansStrAttrIndex {
		t.Fatalf("plan: %+v", p)
	}
	if v, ok := p.attrs[4].prefix[2].(float64); !ok || v != 0 || !math.Signbit(v) {
		t.Fatalf("-0 predicate value %#v", p.attrs[4].prefix[2])
	}
	// With messages decomposed, equality on GenAI content attributes is refused.
	r.cfg.Messages = messagesDeltas
	_, err = r.newSameSpanPlan("same-span query", SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.input.messages", "string", "x"}}})
	requireTyped[*ConfigError](t, "decomposed message attribute", err)
}

// The driver range holds exactly the entries in the window strictly below
// the resumed sort key, in index-key order.
func TestSpanAttrRangeBounds(t *testing.T) {
	t.Parallel()
	space := subspace.Sub("qr_index")
	// Spare capacity: the range's bounds must not share the prefix's array.
	prefix := append(make(tuple.Tuple, 0, 8), "span", "k", "v")
	entry := func(k spanSortKey) []byte {
		return space.Pack(append(append(tuple.Tuple{}, prefix...), k.start, k.trace, k.span, int64(7)))
	}
	keys := []spanSortKey{
		qrSortKey(0, 1, 1), qrSortKey(9, 1, 1), qrSortKey(10, 1, 1), qrSortKey(10, 1, 2), qrSortKey(10, 2, 1),
		qrSortKey(15, 3, 3), qrSortKey(19, 9, 9), qrSortKey(20, 1, 1), qrSortKey(math.MaxInt64, 5, 5),
	}
	outside := space.Pack(tuple.Tuple{"span", "k", "w", int64(10), keys[2].trace, keys[2].span})
	for _, c := range []struct {
		name   string
		window *TimeRange
		resume *spanSortKey
		want   func(spanSortKey) bool
	}{
		{"all", nil, nil, func(spanSortKey) bool { return true }},
		{"window", &TimeRange{10, 20}, nil, func(k spanSortKey) bool { return k.start >= 10 && k.start < 20 }},
		{"negative start clamps", &TimeRange{-5, 10}, nil, func(k spanSortKey) bool { return k.start < 10 }},
		{"resume", nil, &keys[3], func(k spanSortKey) bool { return k.less(keys[3]) }},
		{"resume in window", &TimeRange{10, 20}, &keys[3], func(k spanSortKey) bool { return k.start >= 10 && k.less(keys[3]) }},
		{"resume at window top", &TimeRange{10, 20}, &keys[6], func(k spanSortKey) bool { return k.start >= 10 && k.less(keys[6]) }},
		{"resume above window", &TimeRange{10, 20}, &keys[7], func(k spanSortKey) bool { return k.start >= 10 && k.start < 20 }},
	} {
		w, err := newTimeWindow(c.window)
		if err != nil {
			t.Fatal(err)
		}
		rng, ok := spanAttrRange(prefix, w, c.resume)
		if !ok {
			t.Fatalf("%s: empty range", c.name)
		}
		kr := rng.ToFDBRange(space)
		in := func(b []byte) bool {
			return bytes.Compare(b, kr.Begin.FDBKey()) >= 0 && bytes.Compare(b, kr.End.FDBKey()) < 0
		}
		for _, k := range keys {
			if in(entry(k)) != c.want(k) {
				t.Fatalf("%s: entry %d/%x/%x in range = %v", c.name, k.start, k.trace, k.span, !c.want(k))
			}
		}
		if in(outside) {
			t.Fatalf("%s: another value's entry is in range", c.name)
		}
	}
	// A resumed key below the window leaves nothing.
	w, _ := newTimeWindow(&TimeRange{10, 20})
	if _, ok := spanAttrRange(prefix, w, &keys[1]); ok {
		t.Fatal("a resume key below the window must end the scan")
	}
}

// less is index-key order of the (start, trace, span) suffix.
func (k spanSortKey) less(o spanSortKey) bool {
	if k.start != o.start {
		return k.start < o.start
	}
	if c := bytes.Compare(k.trace, o.trace); c != 0 {
		return c < 0
	}
	return bytes.Compare(k.span, o.span) < 0
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
}

type qrFakeWitnesses[W any] struct {
	traces    []string
	witnesses []W
	err       error
}

func (f qrFakeWitnesses[W]) scanWitnesses(_ context.Context, _ *recordlayer.FDBRecordStore, _ timeWindow, _ *queryWork, visit func(string, W)) error {
	for i, tr := range f.traces {
		visit(tr, f.witnesses[i])
	}
	return f.err
}

// The join plumbing for every clause kind: witnesses of later kinds (logs, as
// stream 2B will scan them) join on trace with span witnesses, prune traces
// earlier clauses excluded, count toward Latest and are ordered newest first.
func TestTraceJoinWitnessScanners(t *testing.T) {
	t.Parallel()
	T1, T2, T3 := qaID(1, 16), qaID(2, 16), qaID(3, 16)
	sm := func(tr string, span int, start int64) SpanMatch { return SpanMatch{tr, qaID(span, 8), start} }
	spans := []witnessScanner[SpanMatch]{qrFakeWitnesses[SpanMatch]{
		traces:    []string{T1, T2, T1, T3},
		witnesses: []SpanMatch{sm(T1, 1, 100), sm(T2, 2, 300), sm(T1, 3, 100), sm(T3, 4, 50)},
	}}
	logs := []witnessScanner[LogMatch]{qrFakeWitnesses[LogMatch]{
		traces:    []string{T1, T2, T2, qaID(9, 16)},
		witnesses: []LogMatch{{"e1", T1, "", 500}, {"e3", T2, "", 10}, {"e2", T2, "", 10}, {"e9", qaID(9, 16), "", 999}},
	}}
	j := &traceJoin{spans: 1, logs: 1}
	if ok, err := joinClauses(t.Context(), nil, spans, timeWindow{}, nil, j, func(tw *relWitnesses) [][]SpanMatch { return tw.spans }); !ok || err != nil {
		t.Fatalf("span clause: %v %v", ok, err)
	}
	if ok, err := joinClauses(t.Context(), nil, logs, timeWindow{}, nil, j, func(tw *relWitnesses) [][]LogMatch { return tw.logs }); !ok || err != nil {
		t.Fatalf("log clause: %v %v", ok, err)
	}
	want := []TraceExistentialHit{
		{T1, 500, [][]SpanMatch{{sm(T1, 1, 100), sm(T1, 3, 100)}}, [][]LogMatch{{{"e1", T1, "", 500}}}, nil},
		{T2, 300, [][]SpanMatch{{sm(T2, 2, 300)}}, [][]LogMatch{{{"e2", T2, "", 10}, {"e3", T2, "", 10}}}, nil},
	}
	if got := j.hits(10); !reflect.DeepEqual(got, want) {
		t.Fatalf("hits\n got %+v\nwant %+v", got, want)
	}
	if got := j.hits(1); len(got) != 1 || got[0].Trace != T1 {
		t.Fatalf("limit 1: %+v", got)
	}
	// A clause no remaining trace satisfies ends the join; a scanner error fails it.
	none := []witnessScanner[LogMatch]{qrFakeWitnesses[LogMatch]{traces: []string{T3}, witnesses: []LogMatch{{"e", T3, "", 1}}}}
	if ok, err := joinClauses(t.Context(), nil, none, timeWindow{}, nil, j, func(tw *relWitnesses) [][]LogMatch { return tw.logs }); ok || err != nil {
		t.Fatalf("disjoint clause: %v %v", ok, err)
	}
	boom := errors.New("boom")
	failing := []witnessScanner[SpanMatch]{qrFakeWitnesses[SpanMatch]{err: boom}}
	if _, err := joinClauses(t.Context(), nil, failing, timeWindow{}, nil, &traceJoin{spans: 1}, func(tw *relWitnesses) [][]SpanMatch { return tw.spans }); !errors.Is(err, boom) {
		t.Fatalf("scanner error: %v", err)
	}
}
