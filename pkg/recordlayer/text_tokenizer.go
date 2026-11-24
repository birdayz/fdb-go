// Portions derived from FoundationDB Record Layer (TextTokenizer.java,
// DefaultTextTokenizer.java, TextTokenizerFactory.java,
// TextTokenizerRegistry.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"sync"

	"fdb.dev/internal/recordlayer/textcmp"
)

// TokenizerMode controls whether tokenization is for indexing or querying.
// Some tokenizers may behave differently in each mode (e.g., n-gram tokenizers).
// Matches Java's TextTokenizer.TokenizerMode.
type TokenizerMode = textcmp.TokenizerMode

const (
	// TokenizerModeIndex tokenizes for indexing documents.
	TokenizerModeIndex = textcmp.TokenizerModeIndex
	// TokenizerModeQuery tokenizes for query strings.
	TokenizerModeQuery = textcmp.TokenizerModeQuery
)

// GlobalMinVersion is the absolute minimum tokenizer version. All tokenizers
// should begin at this version. Matches Java's TextTokenizer.GLOBAL_MIN_VERSION.
const GlobalMinVersion = textcmp.GlobalMinVersion

// TextTokenizer tokenizes text fields for full-text indexes.
// Matches Java's com.apple.foundationdb.record.provider.common.text.TextTokenizer.
type TextTokenizer = textcmp.TextTokenizer

// TokenIterator iterates over tokens produced by a TextTokenizer.
type TokenIterator = textcmp.TokenIterator

// wrapTextCmpMetaDataError converts textcmp errors to public recordlayer types
// while keeping underlying metadata causes in the unwrap chain.
func wrapTextCmpMetaDataError(err error) error {
	return TranslateTextCmpError(err)
}

// ValidateTokenizerVersion checks that the given version is within the
// tokenizer's supported range. Returns an error if out of bounds.
// Matches Java's TextTokenizer.validateVersion().
func ValidateTokenizerVersion(t TextTokenizer, version int) error {
	return wrapTextCmpMetaDataError(textcmp.ValidateTokenizerVersion(t, version))
}

// DefaultTextTokenizer is the default tokenizer for full-text indexes.
// Matches Java's com.apple.foundationdb.record.provider.common.text.DefaultTextTokenizer.
type DefaultTextTokenizer struct{}

// DefaultTextTokenizerName is the name of the default tokenizer.
const DefaultTextTokenizerName = textcmp.DefaultTextTokenizerName

var defaultTextTokenizerInstance = &DefaultTextTokenizer{}

// DefaultTextTokenizerInstance returns the singleton default tokenizer.
func DefaultTextTokenizerInstance() *DefaultTextTokenizer {
	return defaultTextTokenizerInstance
}

// Name returns "default".
func (t *DefaultTextTokenizer) Name() string {
	return textcmp.DefaultTextTokenizerInstance().Name()
}

// MinVersion returns 0 (GLOBAL_MIN_VERSION).
func (t *DefaultTextTokenizer) MinVersion() int {
	return textcmp.DefaultTextTokenizerInstance().MinVersion()
}

// MaxVersion returns the same as MinVersion (only one version exists).
func (t *DefaultTextTokenizer) MaxVersion() int {
	return textcmp.DefaultTextTokenizerInstance().MaxVersion()
}

// Tokenize returns an iterator over tokens from the input text.
func (t *DefaultTextTokenizer) Tokenize(text string, version int, mode TokenizerMode) (TokenIterator, error) {
	iter, err := textcmp.DefaultTextTokenizerInstance().Tokenize(text, version, mode)
	if err != nil {
		return nil, wrapTextCmpMetaDataError(err)
	}
	return iter, nil
}

// TokenizeToMap returns a map from token to list of positions.
func (t *DefaultTextTokenizer) TokenizeToMap(text string, version int, mode TokenizerMode) (map[string][]int, error) {
	m, err := textcmp.DefaultTextTokenizerInstance().TokenizeToMap(text, version, mode)
	if err != nil {
		return nil, wrapTextCmpMetaDataError(err)
	}
	return m, nil
}

// TokenizeToList returns all tokens as a list.
func (t *DefaultTextTokenizer) TokenizeToList(text string, version int, mode TokenizerMode) ([]string, error) {
	list, err := textcmp.DefaultTextTokenizerInstance().TokenizeToList(text, version, mode)
	if err != nil {
		return nil, wrapTextCmpMetaDataError(err)
	}
	return list, nil
}

// TextTokenizerFactory creates instances of a TextTokenizer.
// Matches Java's TextTokenizerFactory.
type TextTokenizerFactory = textcmp.TextTokenizerFactory

// DefaultTextTokenizerFactory creates DefaultTextTokenizer instances.
type DefaultTextTokenizerFactory struct{}

func (f *DefaultTextTokenizerFactory) Name() string { return DefaultTextTokenizerName }
func (f *DefaultTextTokenizerFactory) GetTokenizer() TextTokenizer {
	return defaultTextTokenizerInstance
}

// TextTokenizerRegistry manages TextTokenizer instances by name.
// Matches Java's TextTokenizerRegistry / TextTokenizerRegistryImpl.
type TextTokenizerRegistry struct {
	inner *textcmp.TextTokenizerRegistry
}

var (
	globalTokenizerRegistry     *TextTokenizerRegistry
	globalTokenizerRegistryOnce sync.Once
)

// GlobalTextTokenizerRegistry returns the singleton tokenizer registry,
// pre-loaded with the default tokenizer.
func GlobalTextTokenizerRegistry() *TextTokenizerRegistry {
	globalTokenizerRegistryOnce.Do(func() {
		globalTokenizerRegistry = &TextTokenizerRegistry{inner: textcmp.GlobalTextTokenizerRegistry()}
	})
	return globalTokenizerRegistry
}

func newTextTokenizerRegistry() *TextTokenizerRegistry {
	return &TextTokenizerRegistry{inner: textcmp.NewTextTokenizerRegistry()}
}

// GetTokenizer returns the tokenizer with the given name.
// If name is empty, returns the default tokenizer.
// Returns an error if no tokenizer with that name exists.
func (r *TextTokenizerRegistry) GetTokenizer(name string) (TextTokenizer, error) {
	tok, err := r.inner.GetTokenizer(name)
	if err != nil {
		return nil, wrapTextCmpMetaDataError(err)
	}
	if tok == textcmp.DefaultTextTokenizerInstance() {
		return defaultTextTokenizerInstance, nil
	}
	return tok, nil
}

// Register adds a tokenizer factory to the registry.
// Returns an error if a different factory with the same name is already registered.
func (r *TextTokenizerRegistry) Register(factory TextTokenizerFactory) error {
	return r.inner.Register(factory)
}

// Reset clears and reinitializes the registry with only the default tokenizer.
func (r *TextTokenizerRegistry) Reset() {
	r.inner.Reset()
}

// GetTextTokenizer returns the tokenizer with the given name from the global
// registry. If name is empty, returns the default tokenizer.
func GetTextTokenizer(name string) (TextTokenizer, error) {
	return GlobalTextTokenizerRegistry().GetTokenizer(name)
}

func defaultTokenizeToMap(t TextTokenizer, text string, version int, mode TokenizerMode) (map[string][]int, error) {
	m, err := textcmp.DefaultTokenizeToMap(t, text, version, mode)
	if err != nil {
		return nil, TranslateTextCmpError(err)
	}
	return m, nil
}

func defaultTokenizeToList(t TextTokenizer, text string, version int, mode TokenizerMode) ([]string, error) {
	list, err := textcmp.DefaultTokenizeToList(t, text, version, mode)
	if err != nil {
		return nil, TranslateTextCmpError(err)
	}
	return list, nil
}
