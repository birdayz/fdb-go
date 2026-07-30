package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// A streaming aggregate's PROVIDED group-key ordering and an ORDER BY's
// REQUESTED key must agree on the column's IDENTITY, not merely on its
// spelling.
//
// The two values are built by different producers in different packages and
// never meet as one node: the provided key comes from
// RecordQueryStreamingAggregationPlan.HintOrdering (via AggregateKeyColumnName),
// the requested key from canonicalizeAggregateOutputValue (via
// GroupByOutputColumnNames). Both spell the aggregate's output column, and for a
// long time the spelling was the WHOLE of the agreement — RichOrdering addresses
// its ordering set by the rendered string, so the two sides matched because two
// independent producers happened to route through one rendering authority.
//
// An agreement by convention between two producers is not an identity. What
// makes it one is the ORDINAL DOMAIN: both sides number their ordinal against
// the aggregate's output row, both can name that row
// (GroupByOutputColumnNames is the single authority for it), and once both
// STATE it the pair is comparable by ordinal — RFC-197's triple — with the name
// out of the decision entirely.
//
// This test asserts the identity, in both directions:
//
//   - each side must be ABLE to state one (OrderingIdentityOf must not decline);
//     a producer that mints an ordinal without its domain yields a key no
//     consumer may compare on ordinal, which is why the domain-free bake reads
//     as an identity and is not one;
//   - the two must state the SAME one. Equal ordinals in DIFFERENT domains is
//     the conflation OrdinalDomain exists to refuse, so the domains must match,
//     which means both sides must derive them from the same authority.
//
// Mutate either producer back to a domain-free bake and the corresponding half
// goes red: the ordinal survives, the identity does not.
func TestAggregateOutputOrderingKeysAgreeOnIdentityNotSpelling(t *testing.T) {
	t.Parallel()

	// Two group keys sharing nothing but their source, plus an aggregate — so
	// the output row is WIDER than the group-key prefix. The width matters: a
	// domain derived from the group keys alone and one derived from the whole
	// output row are different tokens, and only the output row is the layout
	// both ordinals actually index.
	inputType := values.NewRecordType("", false, []values.Field{
		{Name: "K", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "K2", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 2},
	})
	qov := values.NewQuantifiedObjectValueOfType(
		values.NamedCorrelationIdentifier("T"), inputType)
	keyA, err := values.NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(0): %v", err)
	}
	keyB, err := values.NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(1): %v", err)
	}
	groupKeys := []values.Value{keyA, keyB}
	aggs := []expressions.AggregateSpec{{Function: expressions.AggCount}}

	nativeNames := expressions.GroupByOutputColumnNames(groupKeys, aggs)
	if len(nativeNames) != len(groupKeys)+len(aggs) {
		t.Fatalf("test setup: GroupByOutputColumnNames = %v, want %d entries",
			nativeNames, len(groupKeys)+len(aggs))
	}

	provided := plans.NewRecordQueryStreamingAggregationPlan(nil, groupKeys, aggs).
		HintOrdering()
	if !provided.IsKnown || len(provided.Keys) != len(groupKeys) {
		t.Fatalf("test setup: HintOrdering = %#v, want %d known keys",
			provided, len(groupKeys))
	}

	for i := range groupKeys {
		providedIdent, providedOK := values.OrderingIdentityOf(provided.Keys[i])
		if !providedOK {
			t.Fatalf("group key %d: the streaming aggregate's PROVIDED ordering key "+
				"%q cannot state a column identity.\n\n"+
				"HintOrdering knows the layout it numbered against — its own output "+
				"row, GroupByOutputColumnNames — so an ordinal minted here without "+
				"that domain is an ordinal no consumer may compare. The ordering "+
				"property is then back to matching this key by its rendered name, "+
				"which is an agreement between two producers rather than an identity.",
				i, values.ExplainValue(provided.Keys[i]))
		}

		// The requested side exactly as the translator hands it over: a
		// post-aggregate reference at the output slot, canonicalized against the
		// producer's native names. The pre-canonical value deliberately carries a
		// WRONG display name and NO domain — that is the translator's input, and
		// canonicalization is what is supposed to resolve it.
		preCanonical := values.NewFieldValueWithResolvedOrdinal(
			"SOME_ALIAS", i, values.NullableLong)
		canonical, valid := canonicalizeAggregateOutputValue(preCanonical, nativeNames)
		if !valid {
			t.Fatalf("group key %d: canonicalizeAggregateOutputValue rejected the "+
				"post-aggregate reference at output slot %d", i, i)
		}
		requestedIdent, requestedOK := values.OrderingIdentityOf(canonical)
		if !requestedOK {
			t.Fatalf("group key %d: the REQUESTED ordering key %q cannot state a "+
				"column identity.\n\n"+
				"canonicalizeAggregateOutputValue holds the very layout the ordinal "+
				"indexes — it is handed nativeNames to resolve the slot against — so "+
				"an ordinal minted here without that domain leaves the requested side "+
				"addressable only by name. This is the half RFC-197 measured as the "+
				"requested-side authority; a provided side that carries its identity "+
				"and a requested side that does not cannot meet on one.",
				i, values.ExplainValue(canonical))
		}

		if providedIdent != requestedIdent {
			t.Fatalf("group key %d: provided identity %+v != requested identity %+v "+
				"(provided %q, requested %q).\n\n"+
				"Both sides number a slot of the SAME row — the aggregate's output "+
				"row — so they must state the same domain and the same ordinal. Two "+
				"equal ordinals in different domains is precisely the conflation "+
				"OrdinalDomain exists to refuse, and it would show up here as a lost "+
				"sort elision rather than as an error: the group-key order stops "+
				"satisfying an ORDER BY over the same key and a second sort appears "+
				"above the aggregate.",
				i, providedIdent, requestedIdent,
				values.ExplainValue(provided.Keys[i]), values.ExplainValue(canonical))
		}
	}

	// The domain both sides agree on must be the OUTPUT ROW's, not the group-key
	// prefix's. Stated separately because the two coincide whenever an aggregate
	// has no non-key output columns, and every corpus shape that matters has one.
	wantDomain := values.OrdinalDomainOfColumnNames(nativeNames)
	if !wantDomain.IsKnown() {
		t.Fatalf("test setup: %v has no layout token", nativeNames)
	}
	prefixDomain := values.OrdinalDomainOfColumnNames(nativeNames[:len(groupKeys)])
	if prefixDomain == wantDomain {
		t.Fatalf("test setup: the group-key prefix %v and the full output row %v "+
			"hash to the same domain token, so this test cannot tell them apart",
			nativeNames[:len(groupKeys)], nativeNames)
	}
	firstIdent, _ := values.OrderingIdentityOf(provided.Keys[0])
	if firstIdent.Domain != wantDomain {
		t.Fatalf("the provided ordering key states domain %v, want the aggregate's "+
			"OUTPUT ROW %v (%v).\n\n"+
			"A group key's ordinal is its slot in the output row, and the output row "+
			"is wider than the group-key prefix whenever there is an aggregate. "+
			"Numbering against the prefix would make the token disagree with the "+
			"requested side, which resolves against the full native name list.",
			firstIdent.Domain, wantDomain, nativeNames)
	}
}
