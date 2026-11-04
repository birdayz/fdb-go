// Portions derived from FoundationDB Record Layer (TextTokenizer.java,
// DefaultTextTokenizer.java, TextTokenizerFactory.java,
// TextTokenizerRegistry.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"strings"
	"sync"

	"github.com/rivo/uniseg"

	"fdb.dev/internal/unicode15"
	"fdb.dev/internal/unicode15/text/unicode/norm"
)

// TokenizerMode controls whether tokenization is for indexing or querying.
// Some tokenizers may behave differently in each mode (e.g., n-gram tokenizers).
// Matches Java's TextTokenizer.TokenizerMode.
type TokenizerMode int

const (
	// TokenizerModeIndex tokenizes for indexing documents.
	TokenizerModeIndex TokenizerMode = iota
	// TokenizerModeQuery tokenizes for query strings.
	TokenizerModeQuery
)

// GlobalMinVersion is the absolute minimum tokenizer version. All tokenizers
// should begin at this version. Matches Java's TextTokenizer.GLOBAL_MIN_VERSION.
const GlobalMinVersion = 0

// TextTokenizer tokenizes text fields for full-text indexes.
// Matches Java's com.apple.foundationdb.record.provider.common.text.TextTokenizer.
type TextTokenizer interface {
	// Name returns the tokenizer's name. Matches Java's getName().
	Name() string

	// Tokenize returns an iterator of tokens from the input text.
	// Returns an error if the version is out of bounds.
	// Matches Java's tokenize(String, int, TokenizerMode).
	Tokenize(text string, version int, mode TokenizerMode) (TokenIterator, error)

	// TokenizeToMap returns a map from token to position list (0-indexed).
	// Returns an error if the version is out of bounds.
	// Matches Java's tokenizeToMap(String, int, TokenizerMode).
	TokenizeToMap(text string, version int, mode TokenizerMode) (map[string][]int, error)

	// TokenizeToList returns all tokens as a list.
	// Returns an error if the version is out of bounds.
	// Matches Java's tokenizeToList(String, int, TokenizerMode).
	TokenizeToList(text string, version int, mode TokenizerMode) ([]string, error)

	// MaxVersion returns the maximum supported tokenizer version.
	MaxVersion() int

	// MinVersion returns the minimum supported tokenizer version.
	MinVersion() int
}

// TokenIterator iterates over tokens produced by a TextTokenizer.
type TokenIterator interface {
	// HasNext returns true if there are more tokens.
	HasNext() bool
	// Next returns the next token. Panics if no more tokens.
	Next() string
}

// ValidateTokenizerVersion checks that the given version is within the
// tokenizer's supported range. Returns an error if out of bounds.
// Matches Java's TextTokenizer.validateVersion().
func ValidateTokenizerVersion(t TextTokenizer, version int) error {
	if version < t.MinVersion() || version > t.MaxVersion() {
		// Java's text (TextTokenizer.java:210); the tokenizer, version and
		// bounds are its log info, not part of its message.
		return &MetaDataError{Message: "unknown tokenizer version"}
	}
	return nil
}

// defaultTokenizeToMap implements the default tokenizeToMap logic matching
// Java's TextTokenizer.tokenizeToMap() default method.
func defaultTokenizeToMap(t TextTokenizer, text string, version int, mode TokenizerMode) (map[string][]int, error) {
	iter, err := t.Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]int)
	offset := 0
	for iter.HasNext() {
		token := iter.Next()
		if token != "" {
			result[token] = append(result[token], offset)
		}
		offset++
	}
	return result, nil
}

// defaultTokenizeToList implements the default tokenizeToList logic matching
// Java's TextTokenizer.tokenizeToList() default method.
func defaultTokenizeToList(t TextTokenizer, text string, version int, mode TokenizerMode) ([]string, error) {
	iter, err := t.Tokenize(text, version, mode)
	if err != nil {
		return nil, err
	}
	var result []string
	for iter.HasNext() {
		result = append(result, iter.Next())
	}
	return result, nil
}

// DefaultTextTokenizer is the default tokenizer for full-text indexes.
// It splits text using Unicode word boundary rules, normalizes to NFKD,
// case-folds to lowercase, and strips diacritical marks.
// Matches Java's com.apple.foundationdb.record.provider.common.text.DefaultTextTokenizer.
type DefaultTextTokenizer struct{}

// DefaultTextTokenizerName is the name of the default tokenizer.
const DefaultTextTokenizerName = "default"

var defaultTextTokenizerInstance = &DefaultTextTokenizer{}

// DefaultTextTokenizerInstance returns the singleton default tokenizer.
func DefaultTextTokenizerInstance() *DefaultTextTokenizer {
	return defaultTextTokenizerInstance
}

// Name returns "default".
func (t *DefaultTextTokenizer) Name() string {
	return DefaultTextTokenizerName
}

// MinVersion returns 0 (GLOBAL_MIN_VERSION).
func (t *DefaultTextTokenizer) MinVersion() int {
	return GlobalMinVersion
}

// MaxVersion returns the same as MinVersion (only one version exists).
func (t *DefaultTextTokenizer) MaxVersion() int {
	return t.MinVersion()
}

// Tokenize returns an iterator over tokens from the input text.
// Returns an error if the version is out of bounds.
// The tokenization process:
//  1. Segment text into words using UAX #29 word boundaries (Java's
//     BreakIterator.getWordInstance(Locale.ROOT) differs; docs/compatibility.md)
//  2. NFKD normalize each segment
//  3. Filter segments without a letter or digit in the Basic Multilingual Plane
//  4. Lowercase and strip combining marks (\p{M})
func (t *DefaultTextTokenizer) Tokenize(text string, version int, mode TokenizerMode) (TokenIterator, error) {
	if err := ValidateTokenizerVersion(t, version); err != nil {
		return nil, err
	}
	return newBreakIteratorWrapper(text), nil
}

// TokenizeToMap returns a map from token to list of positions.
func (t *DefaultTextTokenizer) TokenizeToMap(text string, version int, mode TokenizerMode) (map[string][]int, error) {
	return defaultTokenizeToMap(t, text, version, mode)
}

// TokenizeToList returns all tokens as a list.
func (t *DefaultTextTokenizer) TokenizeToList(text string, version int, mode TokenizerMode) ([]string, error) {
	return defaultTokenizeToList(t, text, version, mode)
}

// breakIteratorWrapper turns word segments into tokens as Java's
// DefaultTextTokenizer.BreakIteratorWrapper does. Like Java's BreakIterator,
// it finds the next word boundary only when the next token is asked for.
type breakIteratorWrapper struct {
	remaining string // the text after the last segment taken
	state     int    // uniseg's word-boundary state across segments
	next      string
	hasNext   bool
}

func newBreakIteratorWrapper(text string) *breakIteratorWrapper {
	return &breakIteratorWrapper{remaining: text, state: -1}
}

func (b *breakIteratorWrapper) HasNext() bool {
	if b.hasNext {
		return true
	}
	for len(b.remaining) > 0 {
		var segment string
		segment, b.remaining, b.state = uniseg.FirstWordInString(b.remaining, b.state)
		if segment == "" {
			continue
		}

		// NFKD normalize the segment.
		normalized := norm.NFKD.String(segment)

		// Check if it contains at least one letter or digit.
		if !hasLetterOrDigit(normalized) {
			continue
		}

		// Lowercase then strip combining marks (Unicode category M).
		b.next, b.hasNext = stripMarks(lowerWord(normalized)), true
		return true
	}
	return false
}

func (b *breakIteratorWrapper) Next() string {
	if b.HasNext() {
		token := b.next
		b.next, b.hasNext = "", false
		return token
	}
	panic("no more tokens")
}

// hasLetterOrDigit reports whether s has a letter or digit in the Basic
// Multilingual Plane: Java tests UTF-16 chars, and a surrogate is neither.
func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if r <= 0xFFFF && (unicode15.IsLetter(r) || unicode15.IsDigit(r)) {
			return true
		}
	}
	return false
}

// lowerWord lowercases a word as Java's toLowerCase(Locale.ROOT) does: a
// capital sigma with a cased letter anywhere before it in the word and none after
// it becomes final ς. Java rechecks word boundaries inside the normalized word;
// the rare words where that differs are pinned in the conformance suite.
func lowerWord(s string) string {
	if !strings.ContainsRune(s, 'Σ') {
		return unicode15.ToLowerString(s)
	}
	runes := []rune(s)
	first, last := -1, -1
	for i, r := range runes {
		if isCased(r) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range runes {
		if r == 'Σ' && first < i && i == last {
			r = 'ς'
		} else {
			r = unicode15.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isCased is Unicode's Cased property; Java's set is narrower (U+10780 differs).
func isCased(r rune) bool {
	return unicode15.IsUpper(r) || unicode15.IsLower(r) || unicode15.IsTitle(r) ||
		unicode15.In(r, unicode15.Other_Lowercase, unicode15.Other_Uppercase)
}

// stripMarks removes all Unicode combining marks (category M: Mn, Mc, Me)
// from the string. Matches Java's Pattern.compile("\\p{M}+").
func stripMarks(s string) string {
	if !hasMark(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode15.In(r, unicode15.Mn, unicode15.Mc, unicode15.Me) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hasMark reports whether s has a combining mark. ASCII has none.
func hasMark(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			for _, r := range s[i:] {
				if unicode15.In(r, unicode15.Mn, unicode15.Mc, unicode15.Me) {
					return true
				}
			}
			return false
		}
	}
	return false
}

// TextTokenizerFactory creates instances of a TextTokenizer.
// Matches Java's TextTokenizerFactory.
type TextTokenizerFactory interface {
	// Name returns the tokenizer name.
	Name() string
	// GetTokenizer returns a tokenizer instance.
	GetTokenizer() TextTokenizer
}

// DefaultTextTokenizerFactory creates DefaultTextTokenizer instances.
type DefaultTextTokenizerFactory struct{}

func (f *DefaultTextTokenizerFactory) Name() string { return DefaultTextTokenizerName }
func (f *DefaultTextTokenizerFactory) GetTokenizer() TextTokenizer {
	return defaultTextTokenizerInstance
}

// TextTokenizerRegistry manages TextTokenizer instances by name.
// Matches Java's TextTokenizerRegistry / TextTokenizerRegistryImpl.
type TextTokenizerRegistry struct {
	mu       sync.RWMutex
	registry map[string]TextTokenizerFactory
}

var (
	globalTokenizerRegistry     *TextTokenizerRegistry
	globalTokenizerRegistryOnce sync.Once
)

// GlobalTextTokenizerRegistry returns the singleton tokenizer registry,
// pre-loaded with the default tokenizer.
func GlobalTextTokenizerRegistry() *TextTokenizerRegistry {
	globalTokenizerRegistryOnce.Do(func() {
		globalTokenizerRegistry = newTextTokenizerRegistry()
	})
	return globalTokenizerRegistry
}

func newTextTokenizerRegistry() *TextTokenizerRegistry {
	r := &TextTokenizerRegistry{
		registry: make(map[string]TextTokenizerFactory),
	}
	// Register the default tokenizer.
	r.registry[DefaultTextTokenizerName] = &DefaultTextTokenizerFactory{}
	return r
}

// GetTokenizer returns the tokenizer with the given name.
// If name is empty, returns the default tokenizer.
// Returns an error if no tokenizer with that name exists.
func (r *TextTokenizerRegistry) GetTokenizer(name string) (TextTokenizer, error) {
	if name == "" {
		return defaultTextTokenizerInstance, nil
	}
	r.mu.RLock()
	factory, ok := r.registry[name]
	r.mu.RUnlock()
	if !ok {
		// Java's text (TextTokenizerRegistryImpl.java:100); the name is its
		// log info.
		return nil, &MetaDataError{Message: "unrecognized text tokenizer"}
	}
	return factory.GetTokenizer(), nil
}

// Register adds a tokenizer factory to the registry.
// Returns an error if a different factory with the same name is already registered.
func (r *TextTokenizerRegistry) Register(factory TextTokenizerFactory) error {
	name := factory.Name()
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.registry[name]
	if ok && existing != factory {
		return fmt.Errorf("attempted to register duplicate tokenizer: %s", name)
	}
	r.registry[name] = factory
	return nil
}

// Reset clears and reinitializes the registry with only the default tokenizer.
func (r *TextTokenizerRegistry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registry = make(map[string]TextTokenizerFactory)
	r.registry[DefaultTextTokenizerName] = &DefaultTextTokenizerFactory{}
}

// GetTextTokenizer returns the tokenizer with the given name from the global
// registry. If name is empty, returns the default tokenizer.
func GetTextTokenizer(name string) (TextTokenizer, error) {
	registry := GlobalTextTokenizerRegistry()
	return registry.GetTokenizer(name)
}
