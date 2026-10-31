package otel

import (
	"context"
	"encoding/json"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

func assertOtelCommittedPostings(t *testing.T, ctx context.Context, b *FDBBackend, sources []Span) {
	t.Helper()
	identity := func(tenant int64, trace, span string, attribute Attribute) string {
		wire, err := json.Marshal([]any{tenant, trace, span, attribute})
		if err != nil {
			t.Fatal(err)
		}
		return string(wire)
	}
	want := map[string]int64{}
	count := 0
	for _, source := range sources {
		for _, attribute := range source.Attributes {
			id := identity(int64(source.Tenant), source.Trace, source.ID, attribute)
			if _, duplicate := want[id]; duplicate {
				t.Fatalf("duplicate expected source membership %s", id)
			}
			want[id] = source.Start
			count++
		}
	}
	postings := otelSnapshot(t, ctx, b, otelSecondary(b))
	if len(postings) != count {
		t.Fatalf("committed posting population %d want %d", len(postings), count)
	}
	for key, value := range postings {
		covered, err := decodeCoveringValue([]byte(value), b.cfg.PostingValueFormat)
		if err != nil {
			t.Fatal(err)
		}
		prefix, err := otelSecondary(b).Unpack(fdb.Key(key))
		if err != nil || len(prefix) < 2 {
			t.Fatalf("malformed posting prefix %+v: %v", prefix, err)
		}
		tenant, ok := prefix[1].(int64)
		if !ok || tenant < 0 || tenant >= int64(b.cfg.Tenants) {
			t.Fatalf("invalid posting tenant %+v", prefix)
		}
		term := termPrefix(otelSecondary(b), int(tenant), covered.Attribute, b.cfg.HashBytes)
		suffix, err := term.Unpack(fdb.Key(key))
		if err != nil {
			t.Fatal(err)
		}
		var trace, span string
		if b.cfg.Order == "trace" {
			if len(suffix) != 3 {
				t.Fatalf("trace posting suffix %+v", suffix)
			}
			var traceOK, spanOK bool
			trace, traceOK = suffix[0].(string)
			span, spanOK = suffix[1].(string)
			if !traceOK || !spanOK {
				t.Fatalf("trace posting identity %+v", suffix)
			}
		} else {
			if len(suffix) != 4 {
				t.Fatalf("time posting suffix %+v", suffix)
			}
			timestamp, timeOK := suffix[0].(int64)
			encoded, traceOK := suffix[1].([]byte)
			var spanOK bool
			span, spanOK = suffix[2].(string)
			if !timeOK || timestamp != -covered.Start || !traceOK || !spanOK {
				t.Fatalf("time posting identity %+v", suffix)
			}
			decoded := make([]byte, len(encoded))
			for i, octet := range encoded {
				decoded[i] = ^octet
			}
			trace = string(decoded)
		}
		id := identity(tenant, trace, span, covered.Attribute)
		if timestamp, exists := want[id]; !exists || timestamp != covered.Start {
			t.Fatalf("unexpected or duplicate committed posting %s: %+v", id, covered)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("missing %d committed source-attribute memberships", len(want))
	}
}
