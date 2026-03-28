package recordlayer

import (
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
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
		return &MetaDataError{
			Message: fmt.Sprintf(
				"unknown tokenizer version: tokenizer=%s version=%d min=%d max=%d",
				t.Name(), version, t.MinVersion(), t.MaxVersion(),
			),
		}
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
//  1. Segment text into words using Unicode word boundary rules
//     (matching Java's BreakIterator.getWordInstance(Locale.ROOT))
//  2. NFKD normalize each segment
//  3. Filter segments that don't contain any letter or digit
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

// breakIteratorWrapper wraps word segmentation to produce tokens matching
// Java's BreakIterator.getWordInstance(Locale.ROOT) behavior.
type breakIteratorWrapper struct {
	segments []string
	pos      int
	next     *string
}

func newBreakIteratorWrapper(text string) *breakIteratorWrapper {
	return &breakIteratorWrapper{
		segments: wordSegments(text),
		pos:      0,
	}
}

func (b *breakIteratorWrapper) HasNext() bool {
	if b.next != nil {
		return true
	}
	for b.pos < len(b.segments) {
		segment := b.segments[b.pos]
		b.pos++

		// NFKD normalize the segment.
		normalized := norm.NFKD.String(segment)

		// Check if it contains at least one letter or digit.
		if !hasLetterOrDigit(normalized) {
			continue
		}

		// Lowercase then strip combining marks (Unicode category M).
		token := stripMarks(strings.ToLower(normalized))
		b.next = &token
		return true
	}
	return false
}

func (b *breakIteratorWrapper) Next() string {
	if b.HasNext() {
		token := *b.next
		b.next = nil
		return token
	}
	panic("no more tokens")
}

// hasLetterOrDigit returns true if s contains at least one Unicode letter or digit.
func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// stripMarks removes all Unicode combining marks (category M: Mn, Mc, Me)
// from the string. Matches Java's Pattern.compile("\\p{M}+").
func stripMarks(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.In(r, unicode.Mn, unicode.Mc, unicode.Me) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wordSegments splits text into word segments using UAX #29 Unicode Text Segmentation,
// matching Java's BreakIterator.getWordInstance(Locale.ROOT).
//
// Uses github.com/rivo/uniseg which implements the full UAX #29 word boundary algorithm,
// including proper handling of:
//   - MidLetter/MidNumLet (apostrophe, period between letters/digits)
//   - ExtendNumLet (underscore joining words)
//   - Extend (combining marks don't break words)
//   - CJK ideographs (each is its own word segment)
func wordSegments(text string) []string {
	if len(text) == 0 {
		return nil
	}
	var segments []string
	remaining := text
	state := -1
	for len(remaining) > 0 {
		var word string
		word, remaining, state = uniseg.FirstWordInString(remaining, state)
		if word != "" {
			segments = append(segments, word)
		}
	}
	return segments
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
		return nil, &MetaDataError{
			Message: fmt.Sprintf("unrecognized text tokenizer: %s", name),
		}
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

// getTextTokenizerForIndex returns the tokenizer and version for an index,
// reading from the index's options. Returns the default tokenizer if no
// tokenizer name is specified.
func getTextTokenizerForIndex(idx *Index) (TextTokenizer, int, error) {
	registry := GlobalTextTokenizerRegistry()

	name := idx.Options[IndexOptionTextTokenizerName]
	tokenizer, err := registry.GetTokenizer(name)
	if err != nil {
		return nil, 0, err
	}

	version := GlobalMinVersion
	if vs, ok := idx.Options[IndexOptionTextTokenizerVersion]; ok {
		v, err := parseTokenizerVersion(vs)
		if err != nil {
			return nil, 0, err
		}
		version = v
	}

	if err := ValidateTokenizerVersion(tokenizer, version); err != nil {
		return nil, 0, err
	}

	return tokenizer, version, nil
}

func parseTokenizerVersion(s string) (int, error) {
	var v int
	_, err := fmt.Sscanf(s, "%d", &v)
	if err != nil {
		return 0, &MetaDataError{
			Message: fmt.Sprintf("invalid tokenizer version: %s", s),
		}
	}
	return v, nil
}
