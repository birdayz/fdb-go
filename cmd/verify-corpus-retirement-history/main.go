// Command verify-corpus-retirement-history anchors both sides of every
// factory-corpus retirement ledger: BEFORE at its declared base commit and
// AFTER at the ledger's unique first-add commit. Once present on the trusted
// target branch, ledger bytes are immutable and undeletable.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

const (
	corpusRepoPath = "pkg/relational/conformance/factorycorpus/testdata"
	ledgerRepoPath = "pkg/relational/conformance/factorycorpus/retirements"
	// corpusFileExt is the committed corpus's file extension. The corpus is
	// genuine yamsql grouped one file per feature family (RFC-201 §5.7), not
	// one file per scenario.
	corpusFileExt = factorycorpus.FileExt
)

// requireCommittableFileMode rejects anything Git could not have produced as a
// regular non-executable blob: a directory, a symlink, a device, or a file
// carrying an execute bit.
//
// It deliberately does NOT demand the exact permission word 0644. Git tracks
// only the execute bit (100644 vs 100755); the read/write bits of a checkout
// come from the checking-out process's umask, so a runner with umask 002
// materializes every tracked file as 0664. Asserting 0644 would make the gate
// pass or fail on the environment rather than on the tree. The Git-side mode is
// asserted where it is authoritative — against `git ls-tree` output.
func requireCommittableFileMode(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file (mode %s)", info.Mode())
	}
	if info.Mode().Perm()&0o111 != 0 {
		return fmt.Errorf("carries an execute bit (mode %s)", info.Mode())
	}
	return nil
}

func main() {
	trustedRef := flag.String(
		"trusted-ref",
		"",
		"Git commit/ref whose history is trusted (normally the pre-PR target branch)",
	)
	flag.Parse()
	if flag.NArg() != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "verify corpus retirement history: unexpected arguments: %v\n", flag.Args())
		os.Exit(2)
	}
	if err := run(*trustedRef); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "verify corpus retirement history:", err)
		os.Exit(1)
	}
}

func run(trustedRef string) error {
	if strings.TrimSpace(trustedRef) == "" {
		return fmt.Errorf("-trusted-ref is required; use the pre-PR target branch, not the proposed HEAD")
	}
	repoRootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("locate Git repository: %w", err)
	}
	repoRoot := strings.TrimSpace(string(repoRootBytes))
	return runAtRepo(repoRoot, trustedRef)
}

func runAtRepo(repoRoot, trustedRef string) error {
	if _, err := realDirectoryPath(repoRoot, ledgerRepoPath); err != nil {
		return fmt.Errorf("retirement-ledger directory: %w", err)
	}
	if err := requireRealDirectoryPath(repoRoot, corpusRepoPath); err != nil {
		return fmt.Errorf("factory-corpus directory: %w", err)
	}
	trustedCommit, err := resolveCommit(repoRoot, trustedRef)
	if err != nil {
		return fmt.Errorf("resolve trusted ref %q: %w", trustedRef, err)
	}
	headCommit, err := resolveCommit(repoRoot, "HEAD")
	if err != nil {
		return fmt.Errorf("resolve proposed HEAD: %w", err)
	}
	// The trusted commit is the base AS THIS CHANGE SEES IT, not the live tip
	// of the base branch.
	//
	// Using the tip is a race, and it fires on innocent changes: CI evaluates a
	// pull request against a merge commit built at trigger time, and the base
	// branch keeps moving afterwards. Minutes later the tip is no longer an
	// ancestor of that merge commit, and the gate rejects a change that did
	// nothing wrong. Observed exactly that way — the base gained three commits
	// between the merge ref being built and this step running.
	//
	// The merge base is the right anchor and loses nothing. Ledgers that exist
	// on the base at the fork point are still immutable and undeletable here;
	// ledgers added to the base AFTER the fork point are not in this change's
	// history at all, so this change cannot have altered them.
	trustedCommit, err = mergeBaseCommit(repoRoot, trustedCommit, headCommit)
	if err != nil {
		return fmt.Errorf("locate the base this change forked from: %w", err)
	}
	if ancestryErr := verifyBaseCommit(repoRoot, trustedCommit, headCommit); ancestryErr != nil {
		return fmt.Errorf("trusted commit must be an ancestor of proposed HEAD: %w", ancestryErr)
	}
	if err := validateCurrentCorpusEntries(repoRoot); err != nil {
		return fmt.Errorf("factory-corpus files: %w", err)
	}
	if err := validateCurrentCorpusMatchesCommit(repoRoot, headCommit); err != nil {
		return fmt.Errorf("factory-corpus checkout does not exactly match raw proposed HEAD %s: %w", headCommit, err)
	}
	headLedgerPaths, err := ledgerPathsAtCommit(repoRoot, headCommit)
	if err != nil {
		return fmt.Errorf("validate proposed-HEAD retirement-ledger Git modes: %w", err)
	}
	ledgerPaths, err := currentLedgerPaths(repoRoot)
	if err != nil {
		return fmt.Errorf("retirement-ledger files: %w", err)
	}
	trustedLedgerPaths, err := ledgerPathsAtCommit(repoRoot, trustedCommit)
	if err != nil {
		return fmt.Errorf("list trusted retirement ledgers: %w", err)
	}
	currentByRepoPath := make(map[string]string, len(ledgerPaths))
	trustedByRepoPath := make(map[string]struct{}, len(trustedLedgerPaths))
	for _, trustedPath := range trustedLedgerPaths {
		trustedByRepoPath[trustedPath] = struct{}{}
		touches, touchErr := ledgerTouchesInRange(repoRoot, trustedCommit, headCommit, trustedPath)
		if touchErr != nil {
			return fmt.Errorf("inspect trusted retirement-ledger history for %s: %w", trustedPath, touchErr)
		}
		if len(touches) != 0 {
			return fmt.Errorf(
				"trusted retirement ledger %s was changed in proposed history at %s; trusted ledgers are immutable and undeletable in every commit, even if later restored",
				trustedPath, strings.Join(touches, ", "),
			)
		}
	}
	for _, ledgerPath := range ledgerPaths {
		repoPath, relErr := filepath.Rel(repoRoot, ledgerPath)
		if relErr != nil {
			return fmt.Errorf("relativize retirement ledger %s: %w", ledgerPath, relErr)
		}
		currentByRepoPath[filepath.ToSlash(repoPath)] = ledgerPath
	}
	for _, headPath := range headLedgerPaths {
		ledgerPath, exists := currentByRepoPath[headPath]
		if !exists {
			return fmt.Errorf("proposed-HEAD retirement ledger %s was deleted from the checkout", headPath)
		}
		currentBytes, readErr := os.ReadFile(ledgerPath)
		if readErr != nil {
			return fmt.Errorf("read retirement ledger %s: %w", ledgerPath, readErr)
		}
		headBytes, exists, readHeadErr := fileAtCommit(repoRoot, headCommit, headPath)
		if readHeadErr != nil {
			return fmt.Errorf("read proposed-HEAD retirement ledger %s: %w", headPath, readHeadErr)
		}
		if !exists {
			return fmt.Errorf("proposed-HEAD retirement ledger %s disappeared while reading commit %s", headPath, headCommit)
		}
		if !bytes.Equal(currentBytes, headBytes) {
			return fmt.Errorf("retirement ledger %s was modified or transformed: checkout bytes differ from raw proposed HEAD; commit exact ledger bytes without checkout filters", headPath)
		}
	}
	if len(currentByRepoPath) != len(headLedgerPaths) {
		return fmt.Errorf("retirement-ledger checkout has %d JSON files, raw proposed HEAD has %d; commit or remove untracked ledger files", len(currentByRepoPath), len(headLedgerPaths))
	}
	for _, trustedPath := range trustedLedgerPaths {
		if _, exists := currentByRepoPath[trustedPath]; !exists {
			return fmt.Errorf("trusted retirement ledger %s was deleted; ledgers are immutable history", trustedPath)
		}
	}
	newLedgerCount := 0
	for _, ledgerPath := range ledgerPaths {
		repoPath, relErr := filepath.Rel(repoRoot, ledgerPath)
		if relErr != nil {
			return fmt.Errorf("relativize retirement ledger %s: %w", ledgerPath, relErr)
		}
		repoPath = filepath.ToSlash(repoPath)
		ledgerInfo, statErr := os.Lstat(ledgerPath)
		if statErr != nil {
			return fmt.Errorf("stat retirement ledger %s: %w", ledgerPath, statErr)
		}
		if modeErr := requireCommittableFileMode(ledgerInfo); modeErr != nil {
			return fmt.Errorf("retirement ledger %s: %w", ledgerPath, modeErr)
		}
		currentLedgerBytes, readErr := os.ReadFile(ledgerPath)
		if readErr != nil {
			return fmt.Errorf("read retirement ledger %s: %w", ledgerPath, readErr)
		}
		_, existedAtTrustedRef := trustedByRepoPath[repoPath]
		if existedAtTrustedRef {
			trustedLedgerBytes, exists, readTrustedErr := fileAtCommit(repoRoot, trustedCommit, repoPath)
			if readTrustedErr != nil {
				return fmt.Errorf("read known trusted retirement ledger %s: %w", repoPath, readTrustedErr)
			}
			if !exists {
				return fmt.Errorf("known trusted retirement ledger %s disappeared while reading commit %s", repoPath, trustedCommit)
			}
			if !bytes.Equal(currentLedgerBytes, trustedLedgerBytes) {
				return fmt.Errorf("trusted retirement ledger %s was modified; ledgers are immutable history", repoPath)
			}
		}
		ledger, loadErr := factorycorpus.LoadRetirementLedger(ledgerPath)
		if loadErr != nil {
			return loadErr
		}
		searchCommit := headCommit
		if !existedAtTrustedRef {
			newLedgerCount++
			if newLedgerCount > 1 {
				return fmt.Errorf("more than one retirement ledger is new relative to trusted commit %s; combine one corpus transition into one ledger", trustedCommit)
			}
			if orderErr := validateNewLedgerOrdering(repoPath, ledger.Date, trustedLedgerPaths); orderErr != nil {
				return orderErr
			}
		}
		if verifyErr := verifyBaseCommit(repoRoot, ledger.BaseCommit, trustedCommit); verifyErr != nil {
			return fmt.Errorf("%s: %w", filepath.Base(ledgerPath), verifyErr)
		}
		firstAddCommit, firstAddErr := ledgerFirstAddCommit(repoRoot, searchCommit, repoPath)
		if firstAddErr != nil {
			return fmt.Errorf("%s: %w", filepath.Base(ledgerPath), firstAddErr)
		}
		if verifyErr := verifyBaseCommit(repoRoot, ledger.BaseCommit, firstAddCommit); verifyErr != nil {
			return fmt.Errorf("%s first-add commit %s: %w", filepath.Base(ledgerPath), firstAddCommit, verifyErr)
		}
		if beforeErr := validateLedgerBeforeCommit(repoRoot, ledger, ledger.BaseCommit); beforeErr != nil {
			return fmt.Errorf("%s declared BEFORE snapshot: %w", filepath.Base(ledgerPath), beforeErr)
		}
		if !existedAtTrustedRef {
			// A stale ancestor is not an acceptable BEFORE endpoint for a new
			// retirement. Otherwise files added between that ancestor and the
			// independently fetched target branch could disappear from both the
			// declared BEFORE and proposed AFTER without appearing in Changes.
			if beforeErr := validateLedgerBeforeCommit(repoRoot, ledger, trustedCommit); beforeErr != nil {
				return fmt.Errorf("%s BEFORE does not match trusted target %s: %w", filepath.Base(ledgerPath), trustedCommit, beforeErr)
			}
		}
		if afterErr := validateLedgerAfterCommit(repoRoot, ledger, firstAddCommit); afterErr != nil {
			return fmt.Errorf("%s first-add AFTER snapshot: %w", filepath.Base(ledgerPath), afterErr)
		}
		if !existedAtTrustedRef {
			if afterErr := validateLedgerAfterCommit(repoRoot, ledger, headCommit); afterErr != nil {
				return fmt.Errorf("%s current proposed-HEAD AFTER snapshot: %w", filepath.Base(ledgerPath), afterErr)
			}
		}
		currentSuffix := ""
		if !existedAtTrustedRef {
			currentSuffix = " and current proposed HEAD"
		}
		fmt.Printf(
			"verified %s: BEFORE at %s, AFTER at first-add %s%s\n",
			filepath.Base(ledgerPath), ledger.BaseCommit, firstAddCommit, currentSuffix,
		)
	}
	nonAdditiveChanges, err := trustedCorpusChanges(repoRoot, trustedCommit)
	if err != nil {
		return fmt.Errorf("compare trusted and proposed corpus trees: %w", err)
	}
	switch {
	case len(nonAdditiveChanges) > 0 && newLedgerCount == 0:
		return fmt.Errorf(
			"trusted corpus files changed without one new retirement ledger:\n  %s",
			strings.Join(nonAdditiveChanges, "\n  "),
		)
	case len(nonAdditiveChanges) == 0 && newLedgerCount > 0:
		return fmt.Errorf("a new retirement ledger was supplied, but every trusted corpus file remains byte-identical")
	}
	return nil
}

func requireRealDirectoryPath(repoRoot, repoPath string) error {
	exists, err := realDirectoryPath(repoRoot, repoPath)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("directory %s does not exist", repoPath)
	}
	return nil
}

// realDirectoryPath reports whether repoPath resolves to a real directory
// without traversing a symlink at any component. A directory that is simply
// absent is not an error: Git cannot track an empty directory, so the
// retirement-ledger directory disappears from a checkout the moment its last
// ledger is retired from history — which is the state of a repository that has
// never had to retire a corpus scenario.
func realDirectoryPath(repoRoot, repoPath string) (bool, error) {
	clean := path.Clean(repoPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false, fmt.Errorf("invalid repository path %q", repoPath)
	}
	current := repoRoot
	for _, component := range strings.Split(clean, "/") {
		current = filepath.Join(current, filepath.FromSlash(component))
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("stat path component %s: %w", current, err)
		}
		if !info.IsDir() {
			return false, fmt.Errorf("path component %s is not a real directory (mode %s)", current, info.Mode())
		}
	}
	return true, nil
}

func validateCurrentCorpusEntries(repoRoot string) error {
	currentDir := filepath.Join(repoRoot, corpusRepoPath)
	entries, err := os.ReadDir(currentDir)
	if err != nil {
		return fmt.Errorf("read proposed corpus: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("proposed corpus must be flat; nested entry %q is not allowed", entry.Name())
		}
		if filepath.Ext(entry.Name()) != corpusFileExt {
			return fmt.Errorf("proposed corpus entry %q is not a %s family file", entry.Name(), corpusFileExt)
		}
		if !safeArchivedCorpusName(entry.Name()) {
			return fmt.Errorf("proposed corpus filename %q is not a portable flat %s name", entry.Name(), corpusFileExt)
		}
		info, statErr := os.Lstat(filepath.Join(currentDir, entry.Name()))
		if statErr != nil {
			return fmt.Errorf("stat proposed corpus file %s: %w", entry.Name(), statErr)
		}
		if modeErr := requireCommittableFileMode(info); modeErr != nil {
			return fmt.Errorf("proposed corpus file %s: %w", entry.Name(), modeErr)
		}
	}
	if len(entries) == 0 {
		return fmt.Errorf("proposed corpus is empty")
	}
	return nil
}

func validateCurrentCorpusMatchesCommit(repoRoot, commit string) error {
	committedDir, cleanup, err := materializeCorpusAtCommit(repoRoot, commit)
	if err != nil {
		return err
	}
	defer cleanup()
	currentDir := filepath.Join(repoRoot, corpusRepoPath)
	currentTree, err := loadExactCorpusBytes(currentDir)
	if err != nil {
		return fmt.Errorf("read checkout corpus: %w", err)
	}
	committedTree, err := loadExactCorpusBytes(committedDir)
	if err != nil {
		return fmt.Errorf("read raw committed corpus: %w", err)
	}
	if len(currentTree) != len(committedTree) {
		return fmt.Errorf("checkout has %d YAML files, raw commit has %d", len(currentTree), len(committedTree))
	}
	for name, committedBytes := range committedTree {
		currentBytes, exists := currentTree[name]
		if !exists {
			return fmt.Errorf("raw committed corpus file %s is absent from checkout", name)
		}
		if !bytes.Equal(currentBytes, committedBytes) {
			return fmt.Errorf("corpus file %s checkout bytes differ from raw commit; commit exact bytes without checkout filters", name)
		}
	}
	return nil
}

func loadExactCorpusBytes(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != corpusFileExt {
			return nil, fmt.Errorf("unexpected corpus entry %q", entry.Name())
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return nil, readErr
		}
		files[entry.Name()] = data
	}
	return files, nil
}

func currentLedgerPaths(repoRoot string) ([]string, error) {
	ledgerDir := filepath.Join(repoRoot, ledgerRepoPath)
	entries, err := os.ReadDir(ledgerDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read retirement-ledger directory: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || filepath.Base(entry.Name()) != entry.Name() {
			return nil, fmt.Errorf("unexpected retirement-ledger entry %q; the directory must contain flat .json files only", entry.Name())
		}
		ledgerPath := filepath.Join(ledgerDir, entry.Name())
		info, statErr := os.Lstat(ledgerPath)
		if statErr != nil {
			return nil, fmt.Errorf("stat retirement ledger %s: %w", ledgerPath, statErr)
		}
		if modeErr := requireCommittableFileMode(info); modeErr != nil {
			return nil, fmt.Errorf("retirement ledger %s: %w", ledgerPath, modeErr)
		}
		paths = append(paths, ledgerPath)
	}
	sort.Strings(paths)
	return paths, nil
}

// trustedCorpusChanges lists every non-additive proposed corpus edit. A
// committed scenario is a frozen reproduction point: dropping one, renaming it,
// or rewriting its bytes requires a retirement ledger even when aggregate
// census counts remain flat (the balanced delete+add bypass). New scenarios are
// allowed without a ledger because they can only raise the coverage floor.
//
// The unit compared is the SCENARIO, not the file. Since RFC-201 §5.7 the
// corpus is grouped one file per feature family, and a batch appends its new
// scenarios into the families they belong to — so a family file's bytes change
// on a purely additive batch. A file-level byte comparison would demand a
// retirement ledger for every routine append, which is both wrong and the
// fastest way to teach everyone to write a meaningless ledger.
func trustedCorpusChanges(repoRoot, trustedCommit string) ([]string, error) {
	trustedDir, cleanup, err := materializeCorpusAtCommit(repoRoot, trustedCommit)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	trusted, err := corpusScenarioDigests(trustedDir)
	if err != nil {
		return nil, fmt.Errorf("read trusted corpus: %w", err)
	}
	current, err := corpusScenarioDigests(filepath.Join(repoRoot, corpusRepoPath))
	if err != nil {
		return nil, fmt.Errorf("read proposed corpus: %w", err)
	}
	var changes []string
	for name, trustedDigest := range trusted {
		currentDigest, exists := current[name]
		switch {
		case !exists:
			changes = append(changes, name+" (deleted or renamed)")
		case currentDigest != trustedDigest:
			changes = append(changes, name+" (scenario rewritten)")
		}
	}
	sort.Strings(changes)
	return changes, nil
}

// corpusScenarioDigests fingerprints every committed scenario by its canonical
// marshalled form. Reading through the loader and re-marshalling — rather than
// hashing the raw family-file bytes — makes the fingerprint independent of
// which family file a scenario sits in and of where in that file it sits, so
// moving a scenario between families is not mistaken for rewriting it.
func corpusScenarioDigests(dir string) (map[string]string, error) {
	scenarios, err := factorycorpus.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	digests := make(map[string]string, len(scenarios))
	for _, scenario := range scenarios {
		canonical, marshalErr := factorycorpus.MarshalFamily([]*factorycorpus.Scenario{scenario})
		if marshalErr != nil {
			return nil, fmt.Errorf("re-marshal scenario %s: %w", scenario.Header.Name, marshalErr)
		}
		if _, duplicate := digests[scenario.Header.Name]; duplicate {
			return nil, fmt.Errorf("scenario name %q appears twice in the corpus", scenario.Header.Name)
		}
		sum := sha256.Sum256(canonical)
		digests[scenario.Header.Name] = hex.EncodeToString(sum[:])
	}
	return digests, nil
}

type gitTreeEntry struct {
	mode, objectType, objectID, name string
}

func gitTreeEntries(repoRoot, commit, rootPath string) ([]gitTreeEntry, error) {
	output, err := exec.Command(
		"git", "-C", repoRoot, "ls-tree", "-r", "-z", commit, "--", rootPath,
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var entries []gitTreeEntry
	for _, rawEntry := range bytes.Split(output, []byte{0}) {
		if len(rawEntry) == 0 {
			continue
		}
		tab := bytes.IndexByte(rawEntry, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("malformed git ls-tree record %q", rawEntry)
		}
		metadata := strings.Fields(string(rawEntry[:tab]))
		if len(metadata) != 3 {
			return nil, fmt.Errorf("malformed git ls-tree metadata %q", rawEntry[:tab])
		}
		entries = append(entries, gitTreeEntry{
			mode: metadata[0], objectType: metadata[1], objectID: metadata[2],
			name: string(rawEntry[tab+1:]),
		})
	}
	return entries, nil
}

func ledgerPathsAtCommit(repoRoot, commit string) ([]string, error) {
	entries, err := gitTreeEntries(repoRoot, commit, ledgerRepoPath)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		clean := path.Clean(entry.name)
		if path.Dir(clean) != ledgerRepoPath || path.Ext(clean) != ".json" {
			return nil, fmt.Errorf("unexpected retirement-ledger tree path %q; the directory must contain flat .json files only", entry.name)
		}
		if entry.mode != "100644" || entry.objectType != "blob" {
			return nil, fmt.Errorf("retirement ledger %s has non-regular Git mode/type %s/%s", clean, entry.mode, entry.objectType)
		}
		paths = append(paths, clean)
	}
	sort.Strings(paths)
	return paths, nil
}

func fileAtCommit(repoRoot, commit, repoPath string) ([]byte, bool, error) {
	output, err := exec.Command(
		"git", "-C", repoRoot, "cat-file", "blob", commit+":"+repoPath,
	).Output()
	if err == nil {
		return output, true, nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil, false, nil
	}
	return nil, false, err
}

func ledgerFirstAddCommit(repoRoot, searchCommit, repoPath string) (string, error) {
	output, err := exec.Command(
		"git", "-C", repoRoot, "log", "--full-history", "--diff-filter=A", "--format=%H", "--reverse",
		searchCommit, "--", repoPath,
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("find first-add commit: %w: %s", err, strings.TrimSpace(string(output)))
	}
	commits := strings.Fields(string(output))
	if len(commits) != 1 {
		return "", fmt.Errorf("ledger must be added exactly once in history through %s; found %d additions", searchCommit, len(commits))
	}
	return commits[0], nil
}

func ledgerTouchesInRange(repoRoot, trustedCommit, headCommit, repoPath string) ([]string, error) {
	output, err := exec.Command(
		"git", "-C", repoRoot, "log", "--full-history", "--format=%H",
		trustedCommit+".."+headCommit, "--", repoPath,
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("find path-touching commits: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.Fields(string(output)), nil
}

func validateNewLedgerOrdering(repoPath, ledgerDate string, trustedPaths []string) error {
	base := path.Base(repoPath)
	prefix := ledgerDate + "-"
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".json") {
		return fmt.Errorf("new retirement ledger %s must use <date>-<lowercase-slug>.json for date %s", repoPath, ledgerDate)
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".json")
	if slug == "" || strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return fmt.Errorf("new retirement ledger %s has a non-canonical lowercase slug", repoPath)
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("new retirement ledger %s has a non-canonical lowercase slug", repoPath)
		}
	}
	if len(trustedPaths) > 0 && repoPath <= trustedPaths[len(trustedPaths)-1] {
		return fmt.Errorf("new retirement ledger %s must sort after trusted ledger %s", repoPath, trustedPaths[len(trustedPaths)-1])
	}
	return nil
}

func validateLedgerAfterCommit(
	repoRoot string,
	ledger factorycorpus.RetirementLedger,
	commit string,
) error {
	corpusDir, cleanup, err := materializeCorpusAtCommit(repoRoot, commit)
	if err != nil {
		return err
	}
	defer cleanup()
	files, err := factorycorpus.LoadDir(corpusDir)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	return factorycorpus.ValidateRetirementLedgerAfter(
		ledger, factorycorpus.ComputeCensus(files), corpusDir,
	)
}

func validateLedgerBeforeCommit(
	repoRoot string,
	ledger factorycorpus.RetirementLedger,
	commit string,
) error {
	corpusDir, cleanup, err := materializeCorpusAtCommit(repoRoot, commit)
	if err != nil {
		return err
	}
	defer cleanup()
	files, err := factorycorpus.LoadDir(corpusDir)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	return factorycorpus.ValidateRetirementLedgerBefore(
		ledger, factorycorpus.ComputeCensus(files), corpusDir,
	)
}

func resolveCommit(repoRoot, ref string) (string, error) {
	output, err := exec.Command(
		"git", "-C", repoRoot, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// mergeBaseCommit returns the best common ancestor of the base branch and the
// proposed HEAD. When HEAD already descends from the base tip this is the tip
// itself, so the ordinary case is unchanged.
func mergeBaseCommit(repoRoot, trustedCommit, headCommit string) (string, error) {
	output, err := exec.Command(
		"git", "-C", repoRoot, "merge-base", trustedCommit, headCommit,
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git merge-base: %w: %s", err, strings.TrimSpace(string(output)))
	}
	base := strings.TrimSpace(string(output))
	if base == "" {
		return "", fmt.Errorf("no common ancestor of %s and %s", trustedCommit, headCommit)
	}
	return base, nil
}

func verifyBaseCommit(repoRoot, baseCommit, trustedCommit string) error {
	objectType, err := exec.Command(
		"git", "-C", repoRoot, "cat-file", "-t", baseCommit,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("read base object %s: %w: %s", baseCommit, err, strings.TrimSpace(string(objectType)))
	}
	if got := strings.TrimSpace(string(objectType)); got != "commit" {
		return fmt.Errorf("base object %s is a %s, want commit", baseCommit, got)
	}
	command := exec.Command(
		"git", "-C", repoRoot, "merge-base", "--is-ancestor", baseCommit, trustedCommit,
	)
	if output, ancestorErr := command.CombinedOutput(); ancestorErr != nil {
		if exitErr, ok := ancestorErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return fmt.Errorf("base commit %s is not an ancestor of trusted commit %s", baseCommit, trustedCommit)
		}
		return fmt.Errorf("check base commit ancestry: %w: %s", ancestorErr, strings.TrimSpace(string(output)))
	}
	return nil
}

func materializeCorpusAtCommit(repoRoot, commit string) (string, func(), error) {
	entries, err := gitTreeEntries(repoRoot, commit, corpusRepoPath)
	if err != nil {
		return "", func() {}, err
	}
	type corpusEntry struct {
		gitTreeEntry
		relative string
	}
	corpusEntries := make([]corpusEntry, 0, len(entries))
	prefix := corpusRepoPath + "/"
	for _, entry := range entries {
		cleanName := path.Clean(entry.name)
		if !strings.HasPrefix(cleanName, prefix) {
			continue
		}
		relative := strings.TrimPrefix(cleanName, prefix)
		if path.Ext(relative) != corpusFileExt || !safeArchivedCorpusName(relative) {
			return "", func() {}, fmt.Errorf("unexpected corpus tree path %q", entry.name)
		}
		if entry.mode != "100644" || entry.objectType != "blob" {
			return "", func() {}, fmt.Errorf(
				"corpus tree entry %q is not a regular blob with Git mode 100644 (mode/type %s/%s)",
				entry.name, entry.mode, entry.objectType,
			)
		}
		corpusEntries = append(corpusEntries, corpusEntry{gitTreeEntry: entry, relative: relative})
	}
	if len(corpusEntries) == 0 {
		return "", func() {}, fmt.Errorf("commit %s contains no corpus files", commit)
	}

	// Read raw Git blobs in one batch. Unlike `git archive` or a checkout,
	// cat-file does not honor export-ignore, export-subst, filters, or working-
	// tree encodings from .gitattributes; the ledger fingerprints the committed
	// bytes themselves, so attributes cannot omit or transform evidence.
	var request bytes.Buffer
	for _, entry := range corpusEntries {
		_, _ = fmt.Fprintln(&request, entry.objectID)
	}
	command := exec.Command("git", "-C", repoRoot, "cat-file", "--batch")
	command.Stdin = &request
	var stderr bytes.Buffer
	command.Stderr = &stderr
	batchOutput, err := command.Output()
	if err != nil {
		return "", func() {}, fmt.Errorf("read corpus blobs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	tempDir, err := os.MkdirTemp("", "factory-corpus-before-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temporary corpus directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	reader := bufio.NewReader(bytes.NewReader(batchOutput))
	for _, entry := range corpusEntries {
		header, readErr := reader.ReadString('\n')
		if readErr != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("read cat-file header for %s: %w", entry.relative, readErr)
		}
		fields := strings.Fields(strings.TrimSuffix(header, "\n"))
		if len(fields) != 3 || fields[0] != entry.objectID || fields[1] != "blob" {
			cleanup()
			return "", func() {}, fmt.Errorf("unexpected cat-file header %q for %s", strings.TrimSpace(header), entry.relative)
		}
		size, parseErr := strconv.ParseInt(fields[2], 10, 64)
		if parseErr != nil || size < 0 || size > int64(len(batchOutput)) {
			cleanup()
			return "", func() {}, fmt.Errorf("invalid cat-file size %q for %s", fields[2], entry.relative)
		}
		data := make([]byte, int(size))
		if _, readErr := io.ReadFull(reader, data); readErr != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("read blob bytes for %s: %w", entry.relative, readErr)
		}
		if delimiter, readErr := reader.ReadByte(); readErr != nil || delimiter != '\n' {
			cleanup()
			return "", func() {}, fmt.Errorf("read blob delimiter for %s: got %q, err=%v", entry.relative, delimiter, readErr)
		}
		target := filepath.Join(tempDir, entry.relative)
		file, createErr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("create historical corpus file %s: %w", entry.relative, createErr)
		}
		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			cleanup()
			if writeErr != nil {
				return "", func() {}, fmt.Errorf("write historical corpus file %s: %w", entry.relative, writeErr)
			}
			return "", func() {}, fmt.Errorf("close historical corpus file %s: %w", entry.relative, closeErr)
		}
	}
	if trailing, readErr := io.ReadAll(reader); readErr != nil || len(trailing) != 0 {
		cleanup()
		return "", func() {}, fmt.Errorf("unexpected trailing cat-file output: %d bytes, err=%v", len(trailing), readErr)
	}
	return tempDir, cleanup, nil
}

func safeArchivedCorpusName(name string) bool {
	return factorycorpus.IsPortableCorpusFilename(name)
}
