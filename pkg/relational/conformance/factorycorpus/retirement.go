package factorycorpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// RetirementLedger is the explicit exception to the corpus growth ratchet.
// A behavior-changing planner patch can make an old physical plan point cease
// to exist; silently lowering the census would hide that loss, while retaining
// a stale reproduction recipe would lie. The ledger binds one reviewed reason
// to the exact before/after census and every file transition.
type RetirementLedger struct {
	FormatVersion      int                `json:"format_version"`
	RFC                string             `json:"rfc"`
	Date               string             `json:"date"`
	Reason             string             `json:"reason"`
	BaseCommit         string             `json:"base_commit"`
	BeforeCensusSHA256 string             `json:"before_census_sha256"`
	AfterCensusSHA256  string             `json:"after_census_sha256"`
	BeforeTreeSHA256   string             `json:"before_tree_sha256"`
	AfterTreeSHA256    string             `json:"after_tree_sha256"`
	Changes            []RetirementChange `json:"changes"`
}

// ValidateRetirementLedgerBefore checks the historical half of a retirement
// against corpus files materialized from ledger.BaseCommit. The CI history
// command supplies this directory from raw `git ls-tree`/`git cat-file` blobs,
// then validates the AFTER side at the unique commit that first added the
// ledger (and against raw proposed HEAD while that ledger is new to the trusted
// branch). Both endpoints are therefore anchored to repository history without
// checkout or archive attributes rewriting evidence, and without making an old
// ledger compare against a later, legitimately grown corpus.
func ValidateRetirementLedgerBefore(
	ledger RetirementLedger,
	before Census,
	corpusDir string,
) error {
	if err := validateRetirementLedgerMetadata(ledger); err != nil {
		return err
	}
	beforeDigest, err := CensusSHA256(before)
	if err != nil {
		return fmt.Errorf("fingerprint before census: %w", err)
	}
	if beforeDigest != ledger.BeforeCensusSHA256 {
		return fmt.Errorf("before census SHA-256 %s, ledger authorizes %s", beforeDigest, ledger.BeforeCensusSHA256)
	}
	beforeTree, err := loadCorpusTree(corpusDir)
	if err != nil {
		return fmt.Errorf("fingerprint before corpus tree: %w", err)
	}
	beforeTreeDigest := corpusTreeSHA256(beforeTree)
	if beforeTreeDigest != ledger.BeforeTreeSHA256 {
		return fmt.Errorf("before corpus tree SHA-256 %s at base commit %s, ledger authorizes %s",
			beforeTreeDigest, ledger.BaseCommit, ledger.BeforeTreeSHA256)
	}
	for _, change := range ledger.Changes {
		digest, exists := beforeTree[change.Name]
		switch change.Disposition {
		case DispositionRetired, DispositionReplaced:
			if !exists {
				return fmt.Errorf("%s file %s is absent at base commit %s",
					change.Disposition, change.Name, ledger.BaseCommit)
			}
			if digest != change.OldSHA256 {
				return fmt.Errorf("%s file %s SHA-256 %s at base commit %s, ledger requires %s",
					change.Disposition, change.Name, digest, ledger.BaseCommit, change.OldSHA256)
			}
		case DispositionAdded:
			if exists {
				return fmt.Errorf("added file %s already exists at base commit %s", change.Name, ledger.BaseCommit)
			}
		}
	}
	return nil
}

// RetirementChange identifies one exact corpus file before and after the
// transaction. OldSHA256 is absent only for an addition; NewSHA256 is absent
// only for a retirement.
type RetirementChange struct {
	Name        string `json:"name"`
	Disposition string `json:"disposition"`
	OldSHA256   string `json:"old_sha256,omitempty"`
	NewSHA256   string `json:"new_sha256,omitempty"`
}

const (
	DispositionRetired  = "retired"
	DispositionReplaced = "replaced"
	DispositionAdded    = "added"
)

// LoadRetirementLedger reads and validates the ledger's static structure.
func LoadRetirementLedger(path string) (RetirementLedger, error) {
	var ledger RetirementLedger
	data, err := os.ReadFile(path)
	if err != nil {
		return ledger, fmt.Errorf("read retirement ledger %s: %w", path, err)
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return ledger, fmt.Errorf("parse retirement ledger %s: %w", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil {
		return ledger, fmt.Errorf("parse retirement ledger %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return ledger, fmt.Errorf("parse retirement ledger %s: trailing content: %w", path, err)
	}
	if err := validateRetirementLedgerMetadata(ledger); err != nil {
		return ledger, fmt.Errorf("retirement ledger %s: %w", path, err)
	}
	return ledger, nil
}

// rejectDuplicateJSONKeys performs a recursive token pass before decoding into
// the typed ledger. encoding/json otherwise accepts duplicate object keys with
// last-value-wins semantics, case-insensitively matches struct fields, and
// replaces invalid UTF-8. A governance authorization must have one unambiguous
// interpretation of the base commit, endpoint digest, filename, and
// disposition it names.
func rejectDuplicateJSONKeys(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("ledger is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	return scanJSONValueForDuplicateKeys(decoder, "$")
}

var retirementLedgerJSONKeys = map[string]struct{}{
	"format_version":       {},
	"rfc":                  {},
	"date":                 {},
	"reason":               {},
	"base_commit":          {},
	"before_census_sha256": {},
	"after_census_sha256":  {},
	"before_tree_sha256":   {},
	"after_tree_sha256":    {},
	"changes":              {},
}

var retirementChangeJSONKeys = map[string]struct{}{
	"name":        {},
	"disposition": {},
	"old_sha256":  {},
	"new_sha256":  {},
}

func canonicalRetirementKeysAt(jsonPath string) map[string]struct{} {
	if jsonPath == "$" {
		return retirementLedgerJSONKeys
	}
	if strings.HasPrefix(jsonPath, "$.changes[") && strings.HasSuffix(jsonPath, "]") &&
		!strings.Contains(strings.TrimPrefix(jsonPath, "$.changes["), "].") {
		return retirementChangeJSONKeys
	}
	return nil
}

func scanJSONValueForDuplicateKeys(decoder *json.Decoder, jsonPath string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		canonicalKeys := canonicalRetirementKeysAt(jsonPath)
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is %T, want string", jsonPath, keyToken)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q at %s", key, jsonPath)
			}
			if canonicalKeys != nil {
				if _, canonical := canonicalKeys[key]; !canonical {
					return fmt.Errorf("non-canonical JSON object key %q at %s", key, jsonPath)
				}
			}
			seen[key] = struct{}{}
			if err := scanJSONValueForDuplicateKeys(decoder, jsonPath+"."+key); err != nil {
				return err
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil {
			return closeErr
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("object at %s closed by %v", jsonPath, closing)
		}
		return nil
	case '[':
		index := 0
		for decoder.More() {
			if err := scanJSONValueForDuplicateKeys(decoder, fmt.Sprintf("%s[%d]", jsonPath, index)); err != nil {
				return err
			}
			index++
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil {
			return closeErr
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("array at %s closed by %v", jsonPath, closing)
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delim, jsonPath)
	}
}

// CorpusTreeSHA256 fingerprints every flat YAML file in corpusDir, including
// its basename and exact bytes. CensusSHA256 deliberately measures logical
// coverage; the tree fingerprint independently prevents an unlisted setup,
// query, row, or expectation edit from hitchhiking on a retirement.
func CorpusTreeSHA256(corpusDir string) (string, error) {
	tree, err := loadCorpusTree(corpusDir)
	if err != nil {
		return "", err
	}
	return corpusTreeSHA256(tree), nil
}

// CensusSHA256 fingerprints the canonical census bytes used by the committed
// baseline. A ledger therefore authorizes exactly one measured transition,
// not any future shrink that happens to reuse its path.
func CensusSHA256(c Census) (string, error) {
	data, err := RenderCensus(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateRetirementTransition authorizes one non-additive corpus transition
// only when both census endpoints and every resulting file match the reviewed
// ledger. The census can remain flat for a content replacement.
func ValidateRetirementTransition(ledger RetirementLedger, before, after Census, corpusDir string) error {
	beforeDigest, err := CensusSHA256(before)
	if err != nil {
		return fmt.Errorf("fingerprint before census: %w", err)
	}
	if beforeDigest != ledger.BeforeCensusSHA256 {
		return fmt.Errorf("before census SHA-256 %s, ledger authorizes %s", beforeDigest, ledger.BeforeCensusSHA256)
	}
	return ValidateRetirementLedgerAfter(ledger, after, corpusDir)
}

// ValidateRetirementLedgerAfter checks the committed half of a transaction.
// It is used by CI after the old files no longer exist in the checkout.
func ValidateRetirementLedgerAfter(ledger RetirementLedger, after Census, corpusDir string) error {
	if err := validateRetirementLedgerMetadata(ledger); err != nil {
		return err
	}
	afterDigest, err := CensusSHA256(after)
	if err != nil {
		return fmt.Errorf("fingerprint after census: %w", err)
	}
	if afterDigest != ledger.AfterCensusSHA256 {
		return fmt.Errorf("after census SHA-256 %s, ledger authorizes %s", afterDigest, ledger.AfterCensusSHA256)
	}
	afterTree, err := loadCorpusTree(corpusDir)
	if err != nil {
		return fmt.Errorf("fingerprint after corpus tree: %w", err)
	}
	afterTreeDigest := corpusTreeSHA256(afterTree)
	if afterTreeDigest != ledger.AfterTreeSHA256 {
		return fmt.Errorf("after corpus tree SHA-256 %s, ledger authorizes %s", afterTreeDigest, ledger.AfterTreeSHA256)
	}
	beforeTree := make(map[string]string, len(afterTree)+len(ledger.Changes))
	for name, digest := range afterTree {
		beforeTree[name] = digest
	}
	for _, change := range ledger.Changes {
		switch change.Disposition {
		case DispositionRetired:
			if _, exists := afterTree[change.Name]; exists {
				return fmt.Errorf("retired file %s still exists", change.Name)
			}
			beforeTree[change.Name] = change.OldSHA256
		case DispositionReplaced, DispositionAdded:
			digest, exists := afterTree[change.Name]
			if !exists {
				return fmt.Errorf("%s file %s is absent", change.Disposition, change.Name)
			}
			if digest != change.NewSHA256 {
				return fmt.Errorf("%s file %s SHA-256 %s, ledger requires %s", change.Disposition, change.Name, digest, change.NewSHA256)
			}
			if change.Disposition == DispositionReplaced {
				beforeTree[change.Name] = change.OldSHA256
			} else {
				delete(beforeTree, change.Name)
			}
		}
	}
	beforeTreeDigest := corpusTreeSHA256(beforeTree)
	if beforeTreeDigest != ledger.BeforeTreeSHA256 {
		return fmt.Errorf("reconstructed before corpus tree SHA-256 %s, ledger authorizes %s; old hashes or change list are incomplete", beforeTreeDigest, ledger.BeforeTreeSHA256)
	}
	return nil
}

func validateRetirementLedgerMetadata(ledger RetirementLedger) error {
	if ledger.FormatVersion != 2 {
		return fmt.Errorf("format_version %d, want 2", ledger.FormatVersion)
	}
	if strings.TrimSpace(ledger.RFC) == "" || ledger.Date == "" || strings.TrimSpace(ledger.Reason) == "" {
		return fmt.Errorf("rfc, date, and reason are required")
	}
	if parsed, err := time.Parse("2006-01-02", ledger.Date); err != nil || parsed.Format("2006-01-02") != ledger.Date {
		return fmt.Errorf("date %q is not YYYY-MM-DD", ledger.Date)
	}
	if !validGitCommit(ledger.BaseCommit) {
		return fmt.Errorf("base_commit must be a lowercase full Git object ID")
	}
	if !validSHA256(ledger.BeforeCensusSHA256) || !validSHA256(ledger.AfterCensusSHA256) ||
		!validSHA256(ledger.BeforeTreeSHA256) || !validSHA256(ledger.AfterTreeSHA256) {
		return fmt.Errorf("census and corpus-tree endpoints must be lowercase SHA-256 digests")
	}
	if len(ledger.Changes) == 0 {
		return fmt.Errorf("changes is empty")
	}
	seen := make(map[string]struct{}, len(ledger.Changes))
	for i, change := range ledger.Changes {
		if !IsPortableCorpusFilename(change.Name) {
			return fmt.Errorf("changes[%d] name %q is not a portable flat .yaml corpus name", i, change.Name)
		}
		if _, duplicate := seen[change.Name]; duplicate {
			return fmt.Errorf("changes[%d] duplicates %s", i, change.Name)
		}
		seen[change.Name] = struct{}{}
		switch change.Disposition {
		case DispositionRetired:
			if !validSHA256(change.OldSHA256) || change.NewSHA256 != "" {
				return fmt.Errorf("changes[%d] retired entry requires only old_sha256", i)
			}
		case DispositionReplaced:
			if !validSHA256(change.OldSHA256) || !validSHA256(change.NewSHA256) || change.OldSHA256 == change.NewSHA256 {
				return fmt.Errorf("changes[%d] replaced entry requires distinct old/new SHA-256 values", i)
			}
		case DispositionAdded:
			if change.OldSHA256 != "" || !validSHA256(change.NewSHA256) {
				return fmt.Errorf("changes[%d] added entry requires only new_sha256", i)
			}
		default:
			return fmt.Errorf("changes[%d] disposition %q is invalid", i, change.Disposition)
		}
	}
	return nil
}

// IsPortableCorpusFilename reports whether name is one lowercase-ASCII flat
// YAML basename on both POSIX and Windows. The restricted alphabet prevents
// forbidden characters, Unicode normalization ambiguity, and case-folding
// collisions; Windows device names remain forbidden separately.
func IsPortableCorpusFilename(name string) bool {
	if name == "" || !utf8.ValidString(name) || strings.TrimSpace(name) != name ||
		path.Base(name) != name || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\:`) || filepath.IsAbs(name) ||
		filepath.VolumeName(name) != "" || path.Ext(name) != ".yaml" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	stem := strings.TrimSuffix(name, ".yaml")
	deviceStem := strings.ToUpper(strings.SplitN(stem, ".", 2)[0])
	switch deviceStem {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return false
	}
	return true
}

func validGitCommit(value string) bool {
	if len(value) != 40 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func loadCorpusTree(corpusDir string) (map[string]string, error) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		return nil, fmt.Errorf("read corpus directory %s: %w", corpusDir, err)
	}
	tree := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		digest, err := fileSHA256(filepath.Join(corpusDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("hash corpus file %s: %w", entry.Name(), err)
		}
		tree[entry.Name()] = digest
	}
	return tree, nil
}

func corpusTreeSHA256(tree map[string]string) string {
	names := make([]string, 0, len(tree))
	for name := range tree {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		// NUL cannot occur in a filename, and every digest has a fixed width.
		// This makes the stream unambiguous without depending on JSON map order.
		_, _ = fmt.Fprintf(hash, "%s\x00%s\n", name, tree[name])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
