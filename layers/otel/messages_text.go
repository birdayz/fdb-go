package otel

// The message tokenizer; text extraction and segmentation are in messages_codec.go.

import (
	"fdb.dev/pkg/recordlayer"
)

// ---------------------------------------------------------------------------
// Tokenization
// ---------------------------------------------------------------------------

// messageTokenizer skips overlong tokens as empty stop words, preserving their positions.
type messageTokenizer struct{}

func (messageTokenizer) Name() string                            { return messageTokenizerName }
func (messageTokenizer) GetTokenizer() recordlayer.TextTokenizer { return messageTokenizer{} }
func (messageTokenizer) MinVersion() int {
	return recordlayer.DefaultTextTokenizerInstance().MinVersion()
}

func (messageTokenizer) MaxVersion() int {
	return recordlayer.DefaultTextTokenizerInstance().MaxVersion()
}

func (messageTokenizer) Tokenize(text string, version int, mode recordlayer.TokenizerMode) (recordlayer.TokenIterator, error) {
	it, err := recordlayer.DefaultTextTokenizerInstance().Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	return cappedTokens{it}, nil
}

// TokenizeToMap and TokenizeToList follow TextTokenizer's default methods
// (text_tokenizer.go defaultTokenizeToMap/defaultTokenizeToList).
func (t messageTokenizer) TokenizeToMap(text string, version int, mode recordlayer.TokenizerMode) (map[string][]int, error) {
	it, err := t.Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for offset := 0; it.HasNext(); offset++ {
		if tok := it.Next(); tok != "" {
			out[tok] = append(out[tok], offset)
		}
	}
	return out, nil
}

func (t messageTokenizer) TokenizeToList(text string, version int, mode recordlayer.TokenizerMode) ([]string, error) {
	it, err := t.Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	var out []string
	for it.HasNext() {
		out = append(out, it.Next())
	}
	return out, nil
}

type cappedTokens struct{ recordlayer.TokenIterator }

func (c cappedTokens) Next() string {
	if tok := c.TokenIterator.Next(); len(tok) <= maxIndexedTokenBytes {
		return tok
	}
	return ""
}

var messageTokenizerRegistrationError = recordlayer.GlobalTextTokenizerRegistry().Register(messageTokenizer{})
