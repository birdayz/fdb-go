package otelsql

import "fdb.dev/pkg/recordlayer"

// MessageTokenizerName names MessageTokenizer, registered by any program that
// imports otelsql. No store names it any more: Java cannot open a store whose
// metadata names a tokenizer it lacks, so MESSAGES_TEXT uses the default
// tokenizer over IndexableText. The registration remains for the Java probe
// that pins that refusal (conformance/otel_template_overlay_java_probe_test.go);
// package otel's message segmentation uses MessageTokenizer's capped tokens.
const MessageTokenizerName = "otel_message_v1"

// MaxIndexedTokenBytes is the longest token a message TEXT index holds: a
// token is part of an index key, which FDB caps at 10 KB.
const MaxIndexedTokenBytes = 1024

// MessageTokenizer is the default tokenizer, except that a token over
// MaxIndexedTokenBytes is the empty stop word and keeps its position.
type MessageTokenizer struct{}

func (MessageTokenizer) Name() string                            { return MessageTokenizerName }
func (MessageTokenizer) GetTokenizer() recordlayer.TextTokenizer { return MessageTokenizer{} }
func (MessageTokenizer) MinVersion() int {
	return recordlayer.DefaultTextTokenizerInstance().MinVersion()
}

func (MessageTokenizer) MaxVersion() int {
	return recordlayer.DefaultTextTokenizerInstance().MaxVersion()
}

func (MessageTokenizer) Tokenize(text string, version int, mode recordlayer.TokenizerMode) (recordlayer.TokenIterator, error) {
	it, err := recordlayer.DefaultTextTokenizerInstance().Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	return cappedTokens{it}, nil
}

// TokenizeToMap and TokenizeToList follow TextTokenizer's default methods
// (text_tokenizer.go defaultTokenizeToMap/defaultTokenizeToList).
func (t MessageTokenizer) TokenizeToMap(text string, version int, mode recordlayer.TokenizerMode) (map[string][]int, error) {
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

func (t MessageTokenizer) TokenizeToList(text string, version int, mode recordlayer.TokenizerMode) ([]string, error) {
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
	if tok := c.TokenIterator.Next(); len(tok) <= MaxIndexedTokenBytes {
		return tok
	}
	return ""
}

var messageTokenizerRegistration = recordlayer.GlobalTextTokenizerRegistry().Register(MessageTokenizer{})

// MessageTokenizerRegistrationError is the error registering MessageTokenizer
// returned: non-nil only when another tokenizer already has its name.
func MessageTokenizerRegistrationError() error { return messageTokenizerRegistration }
