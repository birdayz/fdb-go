package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestTranslateCorrelationRecognizesValidatedQuantifiedObjectValue(t *testing.T) {
	t.Parallel()

	from := values.NamedCorrelationIdentifier("from")
	to := values.NamedCorrelationIdentifier("to")
	qov := mustQOV(t, from, values.NullableLong)
	replacement := mustQOV(t, to, values.NullableLong)
	input := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "matched", Value: qov},
		values.RecordConstructorField{Name: "untouched", Value: values.LiteralValue(int64(7))},
	)

	translated := TranslateCorrelation(input, from, replacement)
	if translated == input {
		t.Fatal("TranslateCorrelation returned the original value after an exact QOV match")
	}
	children := translated.Children()
	if len(children) != 2 || children[0] != replacement || children[1] != input.Children()[1] {
		t.Fatalf("translated children = %#v, want replacement plus pointer-stable untouched child", children)
	}
	if got := TranslateCorrelation(input, values.NamedCorrelationIdentifier("absent"), replacement); got != input {
		t.Fatal("TranslateCorrelation must preserve the root pointer on a miss")
	}
}
