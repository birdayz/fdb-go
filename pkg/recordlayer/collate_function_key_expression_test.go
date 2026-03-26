package recordlayer

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestCollateEvaluatorBasic(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// Simple string collation
	results, err := eval(nil, nil, [][]any{{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0]) != 1 {
		t.Fatalf("expected 1 result with 1 element, got %v", results)
	}
	key, ok := results[0][0].([]byte)
	if !ok {
		t.Fatalf("expected []byte, got %T", results[0][0])
	}
	if len(key) == 0 {
		t.Fatal("collation key should not be empty")
	}
}

func TestCollateEvaluatorNullInput(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	results, err := eval(nil, nil, [][]any{{nil}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0]) != 1 {
		t.Fatalf("expected 1 result with 1 element, got %v", results)
	}
	if results[0][0] != nil {
		t.Fatalf("expected nil for null input, got %v", results[0][0])
	}
}

func TestCollateEvaluatorCaseInsensitive(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// Default strength (PRIMARY) should be case-insensitive
	r1, err := eval(nil, nil, [][]any{{"Hello"}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := eval(nil, nil, [][]any{{"hello"}})
	if err != nil {
		t.Fatal(err)
	}

	key1 := r1[0][0].([]byte)
	key2 := r2[0][0].([]byte)
	if !bytes.Equal(key1, key2) {
		t.Errorf("PRIMARY strength: 'Hello' and 'hello' should produce identical keys\ngot: %x\n     %x", key1, key2)
	}
}

func TestCollateEvaluatorTertiaryStrengthCaseSensitive(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// TERTIARY strength should be case-sensitive
	r1, err := eval(nil, nil, [][]any{{"Hello", "", int64(CollateStrengthTertiary)}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := eval(nil, nil, [][]any{{"hello", "", int64(CollateStrengthTertiary)}})
	if err != nil {
		t.Fatal(err)
	}

	key1 := r1[0][0].([]byte)
	key2 := r2[0][0].([]byte)
	if bytes.Equal(key1, key2) {
		t.Error("TERTIARY strength: 'Hello' and 'hello' should produce different keys")
	}
}

func TestCollateEvaluatorOrdering(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// "apple" < "banana" should hold after collation
	r1, err := eval(nil, nil, [][]any{{"apple"}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := eval(nil, nil, [][]any{{"banana"}})
	if err != nil {
		t.Fatal(err)
	}

	key1 := r1[0][0].([]byte)
	key2 := r2[0][0].([]byte)
	if bytes.Compare(key1, key2) >= 0 {
		t.Errorf("expected 'apple' < 'banana' in collation order, got key1=%x key2=%x", key1, key2)
	}
}

func TestCollateEvaluatorWithLocale(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// Test with explicit locale
	results, err := eval(nil, nil, [][]any{{"hello", "en_US"}})
	if err != nil {
		t.Fatal(err)
	}
	key := results[0][0].([]byte)
	if len(key) == 0 {
		t.Fatal("collation key should not be empty for locale en_US")
	}
}

func TestCollateEvaluatorWithLocaleAndStrength(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	results, err := eval(nil, nil, [][]any{{"hello", "fr_CA", int64(CollateStrengthSecondary)}})
	if err != nil {
		t.Fatal(err)
	}
	key := results[0][0].([]byte)
	if len(key) == 0 {
		t.Fatal("collation key should not be empty for fr_CA/SECONDARY")
	}
}

func TestCollateEvaluatorMultipleArguments(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// Multiple input tuples
	results, err := eval(nil, nil, [][]any{{"alpha"}, {"beta"}, {"gamma"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		key := r[0].([]byte)
		if len(key) == 0 {
			t.Errorf("result %d: collation key should not be empty", i)
		}
	}
}

func TestCollateEvaluatorErrorOnNonString(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	_, err := eval(nil, nil, [][]any{{int64(42)}})
	if err == nil {
		t.Fatal("expected error for non-string argument")
	}
}

func TestCollateEvaluatorErrorOnNoArguments(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	_, err := eval(nil, nil, [][]any{{}})
	if err == nil {
		t.Fatal("expected error for empty arguments")
	}
}

func TestCollateFunctionRegistered(t *testing.T) {
	t.Parallel()

	// Verify both function names are registered
	for _, name := range []string{CollateFuncJRE, CollateFuncICU} {
		globalFunctionRegistryMu.RLock()
		_, ok := globalFunctionRegistry[name]
		globalFunctionRegistryMu.RUnlock()
		if !ok {
			t.Errorf("function %q not registered", name)
		}
	}
}

func TestCollateFunctionProtoRoundTrip(t *testing.T) {
	t.Parallel()

	// Create a collate function expression
	expr := FunctionExpr(CollateFuncJRE, Field("name"))
	p := expr.ToKeyExpression()
	if p.Function == nil {
		t.Fatal("expected Function in proto")
	}
	if p.Function.GetName() != CollateFuncJRE {
		t.Errorf("expected name %q, got %q", CollateFuncJRE, p.Function.GetName())
	}

	// Deserialize
	expr2, err := KeyExpressionFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	fn, ok := expr2.(*FunctionKeyExpression)
	if !ok {
		t.Fatalf("expected *FunctionKeyExpression, got %T", expr2)
	}
	if fn.Name() != CollateFuncJRE {
		t.Errorf("expected name %q after round-trip, got %q", CollateFuncJRE, fn.Name())
	}
}

func TestCollateEvaluatorDeterministic(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// Same input should produce same output
	var keys [][]byte
	for i := 0; i < 10; i++ {
		r, err := eval(nil, nil, [][]any{{"deterministic", "en_US", int64(1)}})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, r[0][0].([]byte))
	}
	for i := 1; i < len(keys); i++ {
		if !bytes.Equal(keys[0], keys[i]) {
			t.Errorf("iteration %d: key differs from iteration 0", i)
		}
	}
}

func TestCollateEvaluatorDiacriticsPrimary(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// PRIMARY strength should ignore diacritics: "o" == "ö"
	r1, err := eval(nil, nil, [][]any{{"o"}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := eval(nil, nil, [][]any{{"\u00f6"}}) // ö
	if err != nil {
		t.Fatal(err)
	}

	key1 := r1[0][0].([]byte)
	key2 := r2[0][0].([]byte)
	if !bytes.Equal(key1, key2) {
		t.Errorf("PRIMARY strength: 'o' and 'ö' should produce identical keys\ngot: %x\n     %x", key1, key2)
	}
}

func TestCollateEvaluatorDiacriticsSecondary(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// SECONDARY strength should distinguish diacritics: "o" != "ö"
	r1, err := eval(nil, nil, [][]any{{"o", "", int64(CollateStrengthSecondary)}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := eval(nil, nil, [][]any{{"\u00f6", "", int64(CollateStrengthSecondary)}})
	if err != nil {
		t.Fatal(err)
	}

	key1 := r1[0][0].([]byte)
	key2 := r2[0][0].([]byte)
	if bytes.Equal(key1, key2) {
		t.Error("SECONDARY strength: 'o' and 'ö' should produce different keys")
	}
}

func TestCollateEvaluatorUnicodeNormalization(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// Composed é (U+00E9) vs decomposed e + combining acute (U+0065 U+0301)
	// Should produce identical collation keys at any strength
	composed := "\u00e9"
	decomposed := "e\u0301"

	for _, strength := range []int64{0, 1, 2} {
		r1, err := eval(nil, nil, [][]any{{composed, "", strength}})
		if err != nil {
			t.Fatal(err)
		}
		r2, err := eval(nil, nil, [][]any{{decomposed, "", strength}})
		if err != nil {
			t.Fatal(err)
		}

		key1 := r1[0][0].([]byte)
		key2 := r2[0][0].([]byte)
		if !bytes.Equal(key1, key2) {
			t.Errorf("strength %d: composed and decomposed é should produce identical keys\ngot: %x\n     %x", strength, key1, key2)
		}
	}
}

func TestCollateFunctionViaFunctionExpr(t *testing.T) {
	t.Parallel()

	// Test via FunctionExpr → Evaluate chain (with mock message)
	expr := FunctionExpr(CollateFuncJRE, LiteralExpr("test_string"))
	results, err := expr.Evaluate(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0]) != 1 {
		t.Fatalf("expected 1 result tuple with 1 element, got %v", results)
	}
	key, ok := results[0][0].([]byte)
	if !ok {
		t.Fatalf("expected []byte, got %T", results[0][0])
	}
	if len(key) == 0 {
		t.Fatal("collation key should not be empty")
	}
}

// LiteralExpr creates a simple literal key expression for testing.
func LiteralExpr(value any) KeyExpression {
	return Literal(value)
}

func TestGetCollatorPoolCaching(t *testing.T) {
	t.Parallel()

	p1 := getCollatorPool("en_US", 0)
	p2 := getCollatorPool("en_US", 0)
	if p1 != p2 {
		t.Error("expected same Pool for same (locale, strength)")
	}

	p3 := getCollatorPool("en_US", 1)
	if p1 == p3 {
		t.Error("expected different Pool for different strength")
	}

	p4 := getCollatorPool("fr_CA", 0)
	if p1 == p4 {
		t.Error("expected different Pool for different locale")
	}
}

// Verify we didn't break anything by importing the collation evaluator.
func TestCollateFunctionConstants(t *testing.T) {
	t.Parallel()

	if CollateFuncJRE != "collate_jre" {
		t.Errorf("unexpected CollateFuncJRE: %s", CollateFuncJRE)
	}
	if CollateFuncICU != "collate_icu" {
		t.Errorf("unexpected CollateFuncICU: %s", CollateFuncICU)
	}
}

func TestCollateEvaluatorEmptyString(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()
	results, err := eval(nil, nil, [][]any{{""}})
	if err != nil {
		t.Fatal(err)
	}
	// Empty string should produce a key (possibly short but valid)
	key := results[0][0].([]byte)
	_ = key // Just verify no panic

	// Non-nil result for empty string (it IS a valid string)
	if results[0][0] == nil {
		t.Error("empty string should not produce nil key")
	}
}

func TestCollateEvaluatorNilLocale(t *testing.T) {
	t.Parallel()

	eval := makeCollateEvaluator()

	// nil locale should use root locale
	results, err := eval(nil, nil, [][]any{{"hello", nil}})
	if err != nil {
		t.Fatal(err)
	}
	key := results[0][0].([]byte)
	if len(key) == 0 {
		t.Fatal("collation key should not be empty for nil locale")
	}

	// Should match default (no locale argument)
	r2, err := eval(nil, nil, [][]any{{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	key2 := r2[0][0].([]byte)
	if !bytes.Equal(key, key2) {
		t.Error("nil locale should produce same key as default (no locale)")
	}
}

// Verify the unused proto import doesn't cause issues
var _ = proto.Int64
