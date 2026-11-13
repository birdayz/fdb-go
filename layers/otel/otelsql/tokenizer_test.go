package otelsql

import (
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// The registered otel_message_v1 tokenizer is the default one, except that a
// token over MaxIndexedTokenBytes is "" and keeps its position.
func TestMessageTokenizer(t *testing.T) {
	t.Parallel()
	if err := messageTokenizerRegistration; err != nil {
		t.Fatal(err)
	}
	tok, err := recordlayer.GetTextTokenizer(MessageTokenizerName)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tok.(MessageTokenizer); !ok {
		t.Fatalf("registered tokenizer %T", tok)
	}
	if tok.MinVersion() != 0 || tok.MaxVersion() != 0 {
		t.Fatalf("versions %d..%d; SQL text access needs version 0 only", tok.MinVersion(), tok.MaxVersion())
	}
	text := "Crème BRÛLÉE, crème! Ångström " + strings.Repeat("a", MaxIndexedTokenBytes+1) + " ok " + strings.Repeat("b", MaxIndexedTokenBytes)
	got, err := tok.TokenizeToMap(text, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int{"creme": {0, 2}, "brulee": {1}, "angstrom": {3}, "ok": {5}, strings.Repeat("b", MaxIndexedTokenBytes): {6}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens %v", got)
	}
	def, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToList(text, 0, recordlayer.TokenizerModeIndex)
	if err != nil {
		t.Fatal(err)
	}
	list, err := tok.TokenizeToList(text, 0, recordlayer.TokenizerModeIndex)
	if err != nil || len(list) != len(def) || list[4] != "" || def[4] == "" {
		t.Fatalf("list %d vs default %d: %v", len(list), len(def), err)
	}
	for i := range def {
		if i != 4 && list[i] != def[i] {
			t.Fatalf("token %d = %q, default %q", i, list[i], def[i])
		}
	}
	if err := recordlayer.GlobalTextTokenizerRegistry().Register(MessageTokenizer{}); err != nil {
		t.Fatalf("re-registering the same tokenizer: %v", err)
	}
}
