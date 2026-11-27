package values

import "testing"

func mustQOV(t testing.TB, correlation CorrelationIdentifier, flowed ...Type) *quantifiedObjectValue {
	t.Helper()
	typ := Type(&RecordType{Fields: []Field{}})
	if len(flowed) > 1 {
		t.Fatalf("mustQOV accepts at most one flowed type, got %d", len(flowed))
	}
	if len(flowed) == 1 {
		typ = flowed[0]
	}
	qov, err := NewQuantifiedObjectValue(correlation, typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%q, %v): %v", correlation, typ, err)
	}
	concrete, ok := qov.(*quantifiedObjectValue)
	if !ok {
		t.Fatalf("NewQuantifiedObjectValue returned %T, want values-owned QOV", qov)
	}
	return concrete
}

func mustExistsValue(
	t testing.TB,
	correlation CorrelationIdentifier,
	flowed ...Type,
) *ExistsValue {
	t.Helper()
	typ := Type(&RecordType{Fields: []Field{}})
	if len(flowed) > 1 {
		t.Fatalf("mustExistsValue accepts at most one flowed type, got %d", len(flowed))
	}
	if len(flowed) == 1 {
		typ = flowed[0]
	}
	value, err := NewExistsValue(correlation, typ)
	if err != nil {
		t.Fatalf("NewExistsValue(%q, %v): %v", correlation, typ, err)
	}
	return value
}

func mustAliasMap(t testing.TB, pairs ...AliasPair) AliasMap {
	t.Helper()
	aliases, err := NewAliasMap(pairs)
	if err != nil {
		t.Fatalf("NewAliasMap(%v): %v", pairs, err)
	}
	return aliases
}

func mustPullUpValue(
	t testing.TB,
	value Value,
	result Value,
	alias CorrelationIdentifier,
) Value {
	t.Helper()
	pulled, err := PullUpValue(value, result, alias)
	if err != nil {
		t.Fatalf("PullUpValue: %v", err)
	}
	return pulled
}

func mustPullUpValues(
	t testing.TB,
	values []Value,
	result Value,
	alias CorrelationIdentifier,
) map[Value]Value {
	t.Helper()
	pulled, err := PullUpValues(values, result, alias)
	if err != nil {
		t.Fatalf("PullUpValues: %v", err)
	}
	return pulled
}

func mustPullUpThroughPassthrough(
	t testing.TB,
	value Value,
	result Value,
	alias CorrelationIdentifier,
) Value {
	t.Helper()
	pulled, err := pullUpThroughPassthrough(value, result, alias)
	if err != nil {
		t.Fatalf("pullUpThroughPassthrough: %v", err)
	}
	return pulled
}

// mustRebaseValue is the test spelling of a rebase that is EXPECTED to succeed.
// RebaseValue — the error-less wrapper that returned nil on failure — is gone
// (see rebase.go); a test that wants the failure calls RebaseValueChecked and
// asserts on the error, and one that does not wants this.
func mustRebaseValue(t testing.TB, v Value, aliases AliasMap) Value {
	t.Helper()
	rebased, err := RebaseValueChecked(v, aliases)
	if err != nil {
		t.Fatalf("RebaseValueChecked: %v", err)
	}
	return rebased
}
