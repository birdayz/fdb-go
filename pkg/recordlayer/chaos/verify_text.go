package chaos

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// verifyTextIndexes checks that every TEXT index contains exactly the entries
// predicted by the model records.
//
// For each TEXT index:
//   - For each model record, evaluate the index expression to get the text field,
//     tokenize it, and build the expected set of (token, PK) pairs
//   - Scan ALL actual entries from the store via ScanIndexByType(BY_TEXT_TOKEN, TupleRangeAll)
//   - Compare sets: missing entries, orphan entries
func verifyTextIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		if idx.Type != recordlayer.IndexTypeText {
			continue
		}

		violations = append(violations, verifyOneTextIndex(ctx, store, model, md, idx)...)
	}

	return violations
}

func verifyOneTextIndex(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	model *StoreModel,
	md *recordlayer.RecordMetaData,
	idx *recordlayer.Index,
) []Violation {
	var violations []Violation

	tokenizer, err := recordlayer.GetTextTokenizer("")
	if err != nil {
		return []Violation{{Invariant: fmt.Sprintf("failed to get text tokenizer: %v", err)}}
	}

	// Build expected set of (token, packedPK) from model records.
	// Use a map from "token\x00packedPK" → true for set membership.
	type textEntry struct {
		token string
		pk    tuple.Tuple
	}
	expected := make(map[string]textEntry)

	for _, rec := range model.Records {
		if !model.indexAppliesToType(idx, rec.TypeName) {
			continue
		}

		// Extract the text field value by evaluating the index expression.
		textValue := extractTextField(idx, rec.Message)
		if textValue == "" {
			continue
		}

		positionMap, err := tokenizer.TokenizeToMap(textValue, 0, recordlayer.TokenizerModeIndex)
		if err != nil {
			violations = append(violations, Violation{
				Invariant:  "text_tokenize_error",
				PrimaryKey: rec.PrimaryKey,
				Expected:   fmt.Sprintf("index %q tokenizable", idx.Name),
				Actual:     err.Error(),
			})
			continue
		}

		for token := range positionMap {
			key := token + "\x00" + string(rec.PrimaryKey.Pack())
			expected[key] = textEntry{token: token, pk: rec.PrimaryKey}
		}
	}

	// Scan actual entries from the store.
	actual := make(map[string]textEntry)
	cursor := store.ScanIndexByType(idx, recordlayer.IndexScanByTextToken,
		recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())

	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "text_scan_error",
				Expected:  fmt.Sprintf("index %q scannable", idx.Name),
				Actual:    err.Error(),
			})
			break
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()

		// entry.Key = [token, pk_elements...]
		if len(entry.Key) < 1 {
			violations = append(violations, Violation{
				Invariant: "text_entry_malformed",
				Expected:  "at least 1 key element",
				Actual:    fmt.Sprintf("%d elements", len(entry.Key)),
			})
			continue
		}

		token, ok := entry.Key[0].(string)
		if !ok {
			violations = append(violations, Violation{
				Invariant: "text_entry_token_type",
				Expected:  "string token",
				Actual:    fmt.Sprintf("%T", entry.Key[0]),
			})
			continue
		}

		pk := entry.PrimaryKey()
		key := token + "\x00" + string(pk.Pack())
		actual[key] = textEntry{token: token, pk: pk}
	}
	_ = cursor.Close()

	// Diff: missing entries (in model but not in store).
	for key, exp := range expected {
		if _, ok := actual[key]; !ok {
			violations = append(violations, Violation{
				Invariant:  "text_entry_missing",
				PrimaryKey: exp.pk,
				Expected:   fmt.Sprintf("index %q token %q pk %v", idx.Name, exp.token, exp.pk),
				Actual:     "not in store",
			})
		}
	}

	// Diff: orphan entries (in store but not in model).
	for key, act := range actual {
		if _, ok := expected[key]; !ok {
			violations = append(violations, Violation{
				Invariant:  "text_entry_orphan",
				PrimaryKey: act.pk,
				Expected:   fmt.Sprintf("index %q: no entry for token %q pk %v", idx.Name, act.token, act.pk),
				Actual:     "exists in store but not in model",
			})
		}
	}

	// Count cross-check.
	if len(expected) != len(actual) {
		violations = append(violations, Violation{
			Invariant: "text_entry_count",
			Expected:  fmt.Sprintf("index %q: %d entries", idx.Name, len(expected)),
			Actual:    fmt.Sprintf("%d entries", len(actual)),
		})
	}

	return violations
}

// extractTextField extracts the text field value from a proto message using
// the index's root expression. Evaluates the expression and picks the element
// at the text field position. Returns "" if the field is unset or empty.
func extractTextField(idx *recordlayer.Index, msg proto.Message) string {
	tuples, err := idx.RootExpression.Evaluate(nil, msg)
	if err != nil || len(tuples) == 0 || len(tuples[0]) == 0 {
		return ""
	}

	textPos := textFieldPosition(idx.RootExpression)
	if textPos >= len(tuples[0]) {
		return ""
	}
	if s, ok := tuples[0][textPos].(string); ok {
		return s
	}
	return ""
}

// textFieldPosition returns the position of the text field in the evaluated tuple.
// Matches the index maintainer's textFieldPosition().
func textFieldPosition(expr recordlayer.KeyExpression) int {
	if g, ok := expr.(*recordlayer.GroupingKeyExpression); ok {
		return g.GetGroupingCount()
	}
	return 0
}
