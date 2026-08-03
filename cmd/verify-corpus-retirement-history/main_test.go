package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

func TestVerifyBaseCommitRequiresTrustedCommitHistory(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitForTest(t, repo, "init", "--quiet")
	gitForTest(t, repo, "config", "user.name", "Corpus Gate Test")
	gitForTest(t, repo, "config", "user.email", "corpus-gate@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "corpus.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", "corpus.txt")
	gitForTest(t, repo, "commit", "--quiet", "-m", "base")
	baseCommit := gitForTest(t, repo, "rev-parse", "HEAD")
	baseTree := gitForTest(t, repo, "rev-parse", "HEAD^{tree}")
	gitForTest(t, repo, "commit", "--quiet", "--allow-empty", "-m", "trusted descendant")
	trustedCommit := gitForTest(t, repo, "rev-parse", "HEAD")

	if err := verifyBaseCommit(repo, baseCommit, trustedCommit); err != nil {
		t.Fatalf("trusted ancestor rejected: %v", err)
	}
	if err := verifyBaseCommit(repo, baseTree, trustedCommit); err == nil || !strings.Contains(err.Error(), "want commit") {
		t.Fatalf("tree object error = %v, want non-commit rejection", err)
	}

	unrelatedCommit := gitForTest(t, repo, "commit-tree", baseTree, "-m", "synthetic unrelated history")
	if err := verifyBaseCommit(repo, unrelatedCommit, trustedCommit); err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("unrelated commit error = %v, want ancestry rejection", err)
	}
}

func TestResolveCommitRejectsUnknownTrustedRef(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitForTest(t, repo, "init", "--quiet")
	if _, err := resolveCommit(repo, "refs/heads/does-not-exist"); err == nil {
		t.Fatal("unknown trusted ref was accepted")
	}
}

func TestSafeArchivedCorpusNameIsCrossPlatformFlat(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "single__cmp__none.yamsql", want: true},
		{name: "nested/file.yamsql"},
		{name: `..\escape.yaml`},
		{name: `C:\escape.yamsql`},
		{name: "../escape.yamsql"},
		{name: "not-yamsql.json"},
		{name: ""},
	} {
		if got := safeArchivedCorpusName(test.name); got != test.want {
			t.Errorf("safeArchivedCorpusName(%q) = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestLedgerHistoryDiscoveryPinsFirstAddition(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitForTest(t, repo, "init", "--quiet")
	gitForTest(t, repo, "config", "user.name", "Corpus Gate Test")
	gitForTest(t, repo, "config", "user.email", "corpus-gate@example.invalid")
	ledgerPath := filepath.Join(repo, filepath.FromSlash(ledgerRepoPath), "2026-08-01-rfc208.json")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	firstBytes := []byte("first immutable ledger\n")
	if err := os.WriteFile(ledgerPath, firstBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "add ledger with transition")
	firstCommit := gitForTest(t, repo, "rev-parse", "HEAD")

	paths, err := ledgerPathsAtCommit(repo, firstCommit)
	if err != nil {
		t.Fatal(err)
	}
	wantRepoPath := ledgerRepoPath + "/2026-08-01-rfc208.json"
	if len(paths) != 1 || paths[0] != wantRepoPath {
		t.Fatalf("ledger paths = %v, want [%s]", paths, wantRepoPath)
	}
	archived, exists, err := fileAtCommit(repo, firstCommit, wantRepoPath)
	if err != nil || !exists || !bytes.Equal(archived, firstBytes) {
		t.Fatalf("fileAtCommit = (%q,%v,%v), want first bytes", archived, exists, err)
	}
	if _, exists, err := fileAtCommit(repo, firstCommit, ledgerRepoPath+"/missing.json"); err != nil || exists {
		t.Fatalf("missing fileAtCommit = (exists=%v, err=%v), want (false,nil)", exists, err)
	}

	if err := os.WriteFile(ledgerPath, []byte("later mutation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "mutate ledger")
	latestCommit := gitForTest(t, repo, "rev-parse", "HEAD")
	firstAdd, err := ledgerFirstAddCommit(repo, latestCommit, wantRepoPath)
	if err != nil || firstAdd != firstCommit {
		t.Fatalf("first add = (%s,%v), want (%s,nil)", firstAdd, err, firstCommit)
	}
	archived, exists, err = fileAtCommit(repo, firstAdd, wantRepoPath)
	if err != nil || !exists || !bytes.Equal(archived, firstBytes) {
		t.Fatalf("first-add bytes = (%q,%v,%v), want immutable first snapshot", archived, exists, err)
	}

	gitForTest(t, repo, "rm", "--quiet", wantRepoPath)
	gitForTest(t, repo, "commit", "--quiet", "-m", "delete ledger")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, firstBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "re-add ledger")
	if _, err := ledgerFirstAddCommit(repo, "HEAD", wantRepoPath); err == nil || !strings.Contains(err.Error(), "found 2 additions") {
		t.Fatalf("re-added ledger error = %v, want exactly-once rejection", err)
	}
}

func TestLedgerFirstAddRejectsIndependentMergeAdditions(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	gitForTest(t, repo, "init", "--quiet")
	gitForTest(t, repo, "config", "user.name", "Corpus Gate Test")
	gitForTest(t, repo, "config", "user.email", "corpus-gate@example.invalid")
	gitForTest(t, repo, "commit", "--quiet", "--allow-empty", "-m", "root")
	root := gitForTest(t, repo, "rev-parse", "HEAD")
	repoPath := ledgerRepoPath + "/2026-08-01-rfc208.json"
	absPath := filepath.Join(repo, filepath.FromSlash(repoPath))

	gitForTest(t, repo, "switch", "--quiet", "-c", "branch-a", root)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absPath, []byte("same ledger\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "add ledger on a")

	gitForTest(t, repo, "switch", "--quiet", "-c", "branch-b", root)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absPath, []byte("same ledger\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "add ledger on b")
	gitForTest(t, repo, "merge", "--quiet", "--no-edit", "branch-a")

	if _, err := ledgerFirstAddCommit(repo, "HEAD", repoPath); err == nil || !strings.Contains(err.Error(), "found 2 additions") {
		t.Fatalf("independent merged additions error = %v, want unique-first-add rejection", err)
	}
}

func TestTrustedLedgerHistoryDetectsTemporaryMutationAndRename(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		attack func(t *testing.T, repo, ledgerPath, original string)
	}{
		{
			name: "modify then restore",
			attack: func(t *testing.T, repo, ledgerPath, original string) {
				writeHistoryFile(t, ledgerPath, []byte("temporary mutation\n"))
				gitForTest(t, repo, "add", ".")
				gitForTest(t, repo, "commit", "--quiet", "-m", "temporarily mutate trusted ledger")
				writeHistoryFile(t, ledgerPath, []byte(original))
				gitForTest(t, repo, "add", ".")
				gitForTest(t, repo, "commit", "--quiet", "-m", "restore trusted ledger bytes")
			},
		},
		{
			name: "rename away then back",
			attack: func(t *testing.T, repo, ledgerPath, _ string) {
				repoPath := filepath.ToSlash(strings.TrimPrefix(ledgerPath, repo+string(filepath.Separator)))
				temporary := ledgerRepoPath + "/temporary-name.json"
				gitForTest(t, repo, "mv", repoPath, temporary)
				gitForTest(t, repo, "commit", "--quiet", "-m", "rename trusted ledger away")
				gitForTest(t, repo, "mv", temporary, repoPath)
				gitForTest(t, repo, "commit", "--quiet", "-m", "rename trusted ledger back")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := newHistoryTestRepo(t)
			ledgerPath := filepath.Join(repo, filepath.FromSlash(ledgerRepoPath), "2026-08-01-rfc-test.json")
			const original = "trusted immutable ledger\n"
			writeHistoryFile(t, ledgerPath, []byte(original))
			gitForTest(t, repo, "add", ".")
			gitForTest(t, repo, "commit", "--quiet", "-m", "trusted ledger")
			trusted := gitForTest(t, repo, "rev-parse", "HEAD")
			test.attack(t, repo, ledgerPath, original)
			head := gitForTest(t, repo, "rev-parse", "HEAD")
			touches, err := ledgerTouchesInRange(repo, trusted, head, ledgerRepoPath+"/2026-08-01-rfc-test.json")
			if err != nil {
				t.Fatal(err)
			}
			if len(touches) < 2 {
				t.Fatalf("temporary trusted-ledger attack touches = %v, want both attack and restoration commits", touches)
			}
		})
	}
}

func TestRunRejectsNewLedgerBasedBeforeTrustedCorpusGrowth(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	beforeDir := t.TempDir()
	oldName := historyCorpusFileName(t, "fc_0000000001_q0_p0")
	oldBefore := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=before")
	writeHistoryFile(t, filepath.Join(corpusDir, oldName), oldBefore)
	writeHistoryFile(t, filepath.Join(beforeDir, oldName), oldBefore)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "stale ledger base")
	baseCommit := gitForTest(t, repo, "rev-parse", "HEAD")

	targetName := historyCorpusFileName(t, "fc_0000000002_q0_p0")
	writeHistoryFile(t, filepath.Join(corpusDir, targetName),
		historyCorpusScenario(t, "fc_0000000002_q0_p0", "shape=target-addition"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted target grows corpus")
	trustedCommit := gitForTest(t, repo, "rev-parse", "HEAD")

	if err := os.Remove(filepath.Join(corpusDir, targetName)); err != nil {
		t.Fatal(err)
	}
	oldAfter := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=after")
	writeHistoryFile(t, filepath.Join(corpusDir, oldName), oldAfter)
	ledger := historyTestLedger(t, baseCommit, beforeDir, corpusDir, factorycorpus.RetirementChange{
		Name: oldName, Disposition: factorycorpus.DispositionReplaced,
		OldSHA256: historyDigest(oldBefore), NewSHA256: historyDigest(oldAfter),
	})
	writeHistoryLedger(t, repo, ledger)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "propose stale-base retirement")

	err := runAtRepo(repo, trustedCommit)
	if err == nil || !strings.Contains(err.Error(), "BEFORE does not match trusted target") {
		t.Fatalf("stale-base retirement error = %v, want trusted-corpus BEFORE rejection", err)
	}
}

func TestRunRejectsAdditionOnlyRetirementLedger(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	beforeDir := t.TempDir()
	beforeName := historyCorpusFileName(t, "fc_0000000001_q0_p0")
	beforeBytes := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=before")
	writeHistoryFile(t, filepath.Join(corpusDir, beforeName), beforeBytes)
	writeHistoryFile(t, filepath.Join(beforeDir, beforeName), beforeBytes)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted base")
	trustedCommit := gitForTest(t, repo, "rev-parse", "HEAD")

	addedName := historyCorpusFileName(t, "fc_0000000002_q0_p0")
	addedBytes := historyCorpusScenario(t, "fc_0000000002_q0_p0", "shape=pure-addition")
	writeHistoryFile(t, filepath.Join(corpusDir, addedName), addedBytes)
	ledger := historyTestLedger(t, trustedCommit, beforeDir, corpusDir, factorycorpus.RetirementChange{
		Name: addedName, Disposition: factorycorpus.DispositionAdded,
		NewSHA256: historyDigest(addedBytes),
	})
	writeHistoryLedger(t, repo, ledger)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "unnecessary addition-only ledger")

	err := runAtRepo(repo, trustedCommit)
	if err == nil || !strings.Contains(err.Error(), "every trusted corpus file remains byte-identical") {
		t.Fatalf("addition-only ledger error = %v, want unused-ledger rejection", err)
	}
}

func TestRunPinsNewAndHistoricalLedgerLifecycle(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	beforeDir := t.TempDir()
	name := historyCorpusFileName(t, "fc_0000000001_q0_p0")
	beforeBytes := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=before")
	writeHistoryFile(t, filepath.Join(corpusDir, name), beforeBytes)
	writeHistoryFile(t, filepath.Join(beforeDir, name), beforeBytes)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted base")
	trustedBase := gitForTest(t, repo, "rev-parse", "HEAD")

	afterBytes := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=after")
	writeHistoryFile(t, filepath.Join(corpusDir, name), afterBytes)
	ledger := historyTestLedger(t, trustedBase, beforeDir, corpusDir, factorycorpus.RetirementChange{
		Name: name, Disposition: factorycorpus.DispositionReplaced,
		OldSHA256: historyDigest(beforeBytes), NewSHA256: historyDigest(afterBytes),
	})
	ledgerPath, ledgerBytes := writeHistoryLedger(t, repo, ledger)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "ledger and exact after")
	retirementCommit := gitForTest(t, repo, "rev-parse", "HEAD")
	if err := runAtRepo(repo, trustedBase); err != nil {
		t.Fatalf("valid new ledger: %v", err)
	}

	writeHistoryFile(t, filepath.Join(corpusDir, name),
		historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=tampered-after"))
	if err := runAtRepo(repo, trustedBase); err == nil || !strings.Contains(err.Error(), "checkout does not exactly match raw proposed HEAD") {
		t.Fatalf("malformed current AFTER error = %v, want raw-HEAD/checkout rejection", err)
	}
	writeHistoryFile(t, filepath.Join(corpusDir, name), afterBytes)

	growthName := historyCorpusFileName(t, "fc_0000000002_q0_p0")
	writeHistoryFile(t, filepath.Join(corpusDir, growthName),
		historyCorpusScenario(t, "fc_0000000002_q0_p0", "shape=later-growth"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "later legitimate corpus growth")
	trustedGrowth := gitForTest(t, repo, "rev-parse", "HEAD")
	if err := runAtRepo(repo, retirementCommit); err != nil {
		t.Fatalf("add-only growth relative to pre-growth trusted ref: %v", err)
	}
	if err := runAtRepo(repo, trustedGrowth); err != nil {
		t.Fatalf("historical ledger after later growth: %v", err)
	}

	writeHistoryFile(t, filepath.Join(corpusDir, name),
		historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=unledgered-replacement"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "attempt unledgered replacement")
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "changed without one new retirement ledger") {
		t.Fatalf("unledgered replacement error = %v, want trusted-tree rejection", err)
	}
	writeHistoryFile(t, filepath.Join(corpusDir, name), afterBytes)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "restore trusted corpus bytes")

	if err := os.Remove(filepath.Join(corpusDir, name)); err != nil {
		t.Fatal(err)
	}
	swapName := historyCorpusFileName(t, "fc_0000000003_q0_p0")
	writeHistoryFile(t, filepath.Join(corpusDir, swapName),
		historyCorpusScenario(t, "fc_0000000003_q0_p0", "shape=balanced-swap"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "attempt balanced corpus swap")
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "deleted or renamed") {
		t.Fatalf("balanced delete+add error = %v, want lost trusted-file rejection", err)
	}
	if err := os.Remove(filepath.Join(corpusDir, swapName)); err != nil {
		t.Fatal(err)
	}
	writeHistoryFile(t, filepath.Join(corpusDir, name), afterBytes)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "restore balanced corpus swap")
	corpusSymlinkTarget := filepath.Join(repo, "same-corpus-bytes.yaml.target")
	writeHistoryFile(t, corpusSymlinkTarget, afterBytes)
	if err := os.Remove(filepath.Join(corpusDir, name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(corpusSymlinkTarget, filepath.Join(corpusDir, name)); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "proposed corpus file") || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("replacement corpus symlink error = %v, want non-regular-file rejection", err)
	}
	if err := os.Remove(filepath.Join(corpusDir, name)); err != nil {
		t.Fatal(err)
	}
	writeHistoryFile(t, filepath.Join(corpusDir, name), afterBytes)
	addedSymlink := filepath.Join(corpusDir, historyCorpusFileName(t, "fc_0000000004_q0_p0"))
	if err := os.Symlink(corpusSymlinkTarget, addedSymlink); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "proposed corpus file") || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("added corpus symlink error = %v, want non-regular-file rejection", err)
	}
	if err := os.Remove(addedSymlink); err != nil {
		t.Fatal(err)
	}
	weirdName := filepath.Join(corpusDir, `C:\x.yamsql`)
	writeHistoryFile(t, weirdName,
		historyCorpusScenario(t, "fc_0000000004_q0_p0", "shape=nonportable-name"))
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "portable flat .yamsql name") {
		t.Fatalf("nonportable corpus filename error = %v, want path-policy rejection", err)
	}
	if err := os.Remove(weirdName); err != nil {
		t.Fatal(err)
	}
	corpusBackup := filepath.Join(repo, "real-corpus-directory")
	if err := os.Rename(corpusDir, corpusBackup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(corpusBackup, corpusDir); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "factory-corpus directory") || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("corpus directory symlink error = %v, want real-directory rejection", err)
	}
	if err := os.Remove(corpusDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(corpusBackup, corpusDir); err != nil {
		t.Fatal(err)
	}
	ledgerDir := filepath.Dir(ledgerPath)
	ledgerDirBackup := filepath.Join(repo, "real-ledger-directory")
	if err := os.Rename(ledgerDir, ledgerDirBackup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ledgerDirBackup, ledgerDir); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "retirement-ledger directory") || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("ledger directory symlink error = %v, want real-directory rejection", err)
	}
	if err := os.Remove(ledgerDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ledgerDirBackup, ledgerDir); err != nil {
		t.Fatal(err)
	}

	writeHistoryFile(t, ledgerPath, append(append([]byte(nil), ledgerBytes...), '\n'))
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "was modified") {
		t.Fatalf("modified historical ledger error = %v, want immutability rejection", err)
	}
	writeHistoryFile(t, ledgerPath, ledgerBytes)
	if err := os.Chmod(ledgerPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "carries an execute bit") {
		t.Fatalf("executable ledger error = %v, want mode rejection", err)
	}
	if err := os.Chmod(ledgerPath, 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkTarget := filepath.Join(repo, "valid-ledger-target.json")
	writeHistoryFile(t, symlinkTarget, ledgerBytes)
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(symlinkTarget, ledgerPath); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink ledger error = %v, want non-regular-file rejection", err)
	}
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	writeHistoryFile(t, ledgerPath, ledgerBytes)
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "was deleted") {
		t.Fatalf("deleted historical ledger error = %v, want deletion rejection", err)
	}
	ledgerRepoRelative := filepath.ToSlash(strings.TrimPrefix(ledgerPath, repo+string(filepath.Separator)))
	gitForTest(t, repo, "add", "-u", ledgerRepoRelative)
	gitForTest(t, repo, "commit", "--quiet", "-m", "delete trusted ledger")
	writeHistoryFile(t, ledgerPath, ledgerBytes)
	gitForTest(t, repo, "add", ledgerRepoRelative)
	gitForTest(t, repo, "commit", "--quiet", "-m", "re-add identical trusted ledger")
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "changed in proposed history") {
		t.Fatalf("committed ledger delete+readd error = %v, want trusted-history immutability rejection", err)
	}
}

func TestMaterializeCorpusRejectsSymlinkScenario(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	target := filepath.Join(repo, "scenario-target")
	writeHistoryFile(t, target, historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=symlink"))
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(corpusDir, historyCorpusFileName(t, "fc_0000000001_q0_p0"))); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "symlink corpus scenario")
	if _, cleanup, err := materializeCorpusAtCommit(repo, "HEAD"); err == nil || !strings.Contains(err.Error(), "not a regular blob") {
		cleanup()
		t.Fatalf("materialize symlink error = %v, want non-regular-blob rejection", err)
	}
}

func TestMaterializeCorpusReadsRawBlobsDespiteExportAttributes(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	name := historyCorpusFileName(t, "fc_0000000001_q0_p0")
	data := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=$Format:%H$")
	writeHistoryFile(t, filepath.Join(corpusDir, name), data)
	attributes := fmt.Sprintf("%s/*.yamsql export-ignore export-subst\n", corpusRepoPath)
	writeHistoryFile(t, filepath.Join(repo, ".gitattributes"), []byte(attributes))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "attributes must not rewrite corpus evidence")

	materialized, cleanup, err := materializeCorpusAtCommit(repo, "HEAD")
	if err != nil {
		t.Fatalf("materialize raw corpus blobs: %v", err)
	}
	defer cleanup()
	got, err := os.ReadFile(filepath.Join(materialized, name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("historical materialization honored export-ignore/export-subst instead of reading raw blob bytes")
	}
}

func TestRunRejectsNestedProposedHeadCorpus(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	writeHistoryFile(t, filepath.Join(corpusDir, historyCorpusFileName(t, "fc_0000000001_q0_p0")),
		historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=base"))
	if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(ledgerRepoPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted flat corpus")
	trusted := gitForTest(t, repo, "rev-parse", "HEAD")

	nestedDir := filepath.Join(corpusDir, "nested")
	nestedPath := filepath.Join(nestedDir, historyCorpusFileName(t, "fc_0000000002_q0_p0"))
	writeHistoryFile(t, nestedPath,
		historyCorpusScenario(t, "fc_0000000002_q0_p0", "shape=nested"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "nested corpus entry")
	if err := os.Remove(nestedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(nestedDir); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trusted); err == nil || !strings.Contains(err.Error(), "unexpected corpus tree path") {
		t.Fatalf("nested proposed-HEAD corpus error = %v, want raw-tree path rejection", err)
	}
}

func TestRunRejectsCheckoutTransformedCorpusBytes(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	name := historyCorpusFileName(t, "fc_0000000001_q0_p0")
	data := append(historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=base"), []byte("\n# $Id$\n")...)
	writeHistoryFile(t, filepath.Join(corpusDir, name), data)
	if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(ledgerRepoPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted raw corpus")
	trusted := gitForTest(t, repo, "rev-parse", "HEAD")

	attributes := fmt.Sprintf("%s/*.yamsql ident\n", corpusRepoPath)
	writeHistoryFile(t, filepath.Join(repo, ".gitattributes"), []byte(attributes))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "attempt checkout corpus transform")
	repoPath := corpusRepoPath + "/" + name
	if err := os.Remove(filepath.Join(corpusDir, name)); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "checkout", "--", repoPath)
	working, err := os.ReadFile(filepath.Join(corpusDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(working, data) {
		t.Fatal("git ident fixture did not transform checkout bytes")
	}
	if err := runAtRepo(repo, trusted); err == nil || !strings.Contains(err.Error(), "checkout bytes differ from raw commit") {
		t.Fatalf("checkout-transformed corpus error = %v, want raw-blob mismatch rejection", err)
	}
}

func TestRunRejectsCheckoutTransformedNewLedgerBytes(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	beforeDir := t.TempDir()
	name := historyCorpusFileName(t, "fc_0000000001_q0_p0")
	beforeBytes := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=before")
	writeHistoryFile(t, filepath.Join(corpusDir, name), beforeBytes)
	writeHistoryFile(t, filepath.Join(beforeDir, name), beforeBytes)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted corpus")
	trusted := gitForTest(t, repo, "rev-parse", "HEAD")

	afterBytes := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=after")
	writeHistoryFile(t, filepath.Join(corpusDir, name), afterBytes)
	ledger := historyTestLedger(t, trusted, beforeDir, corpusDir, factorycorpus.RetirementChange{
		Name: name, Disposition: factorycorpus.DispositionReplaced,
		OldSHA256: historyDigest(beforeBytes), NewSHA256: historyDigest(afterBytes),
	})
	ledger.Reason += " $Id$"
	ledgerPath, rawLedger := writeHistoryLedger(t, repo, ledger)
	attributes := fmt.Sprintf("%s/*.json ident\n", ledgerRepoPath)
	writeHistoryFile(t, filepath.Join(repo, ".gitattributes"), []byte(attributes))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "attempt checkout ledger transform")
	ledgerRepoRelative := filepath.ToSlash(strings.TrimPrefix(ledgerPath, repo+string(filepath.Separator)))
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "checkout", "--", ledgerRepoRelative)
	working, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(working, rawLedger) {
		t.Fatal("git ident fixture did not transform ledger checkout bytes")
	}
	if err := runAtRepo(repo, trusted); err == nil || !strings.Contains(err.Error(), "checkout bytes differ from raw proposed HEAD") {
		t.Fatalf("checkout-transformed ledger error = %v, want raw-blob mismatch rejection", err)
	}
}

func TestValidateNewLedgerOrdering(t *testing.T) {
	t.Parallel()
	trusted := []string{
		ledgerRepoPath + "/2026-07-01-rfc200.json",
		ledgerRepoPath + "/2026-07-15-rfc201.json",
	}
	for _, test := range []struct {
		name, repoPath, date string
		wantError            string
	}{
		{
			name:     "append",
			repoPath: ledgerRepoPath + "/2026-08-01-rfc208.json",
			date:     "2026-08-01",
		},
		{
			name:      "filename date mismatch",
			repoPath:  ledgerRepoPath + "/2026-08-01-rfc208.json",
			date:      "2026-08-02",
			wantError: "must use <date>-<lowercase-slug>.json",
		},
		{
			name:      "noncanonical slug",
			repoPath:  ledgerRepoPath + "/2026-08-01-RFC_205.json",
			date:      "2026-08-01",
			wantError: "non-canonical lowercase slug",
		},
		{
			name:      "does not append",
			repoPath:  ledgerRepoPath + "/2026-06-01-rfc208.json",
			date:      "2026-06-01",
			wantError: "must sort after trusted ledger",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateNewLedgerOrdering(test.repoPath, test.date, trusted)
			if test.wantError == "" && err != nil {
				t.Fatalf("valid ordering: %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("ordering error = %v, want %q", err, test.wantError)
			}
		})
	}
}

// TestRunAcceptsGroupWritableCheckoutModes pins the gate to what Git actually
// tracks. Git records only the execute bit; the read/write bits a checkout
// lands with come from the checking-out process's umask, so a CI runner with
// umask 002 materializes every tracked corpus file and ledger as 0664 and a
// developer with umask 022 gets 0644 — from byte-identical trees. A gate that
// demanded the literal permission word 0644 therefore passed or failed on the
// machine rather than on the change, and did fail, on a corpus file this branch
// never touched.
//
// Both endpoints of that environment range are asserted here, together with the
// two properties that ARE tree properties and must still be rejected.
func TestRunAcceptsGroupWritableCheckoutModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []os.FileMode{0o644, 0o664, 0o600, 0o666} {
		repo := newHistoryTestRepo(t)
		corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
		corpusName := historyCorpusFileName(t, "fc_0000000001_q0_p0")
		corpusPath := filepath.Join(corpusDir, corpusName)
		writeHistoryFile(t, corpusPath, historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=base"))
		gitForTest(t, repo, "add", ".")
		gitForTest(t, repo, "commit", "--quiet", "-m", "trusted corpus")
		trusted := gitForTest(t, repo, "rev-parse", "HEAD")
		if err := os.Chmod(corpusPath, mode); err != nil {
			t.Fatal(err)
		}
		if err := runAtRepo(repo, trusted); err != nil {
			t.Fatalf("corpus file checked out with mode %v was rejected: %v", mode, err)
		}
		if err := os.Chmod(corpusPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := runAtRepo(repo, trusted); err == nil || !strings.Contains(err.Error(), "carries an execute bit") {
			t.Fatalf("executable corpus file error = %v, want execute-bit rejection", err)
		}
		if err := os.Chmod(corpusPath, 0o644); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(repo, "corpus-target"+factorycorpus.FileExt)
		writeHistoryFile(t, target, historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=base"))
		if err := os.Remove(corpusPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, corpusPath); err != nil {
			t.Fatal(err)
		}
		if err := runAtRepo(repo, trusted); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("symlinked corpus file error = %v, want non-regular-file rejection", err)
		}
	}
}

// TestRunAcceptsAnAbsentRetirementLedgerDirectory pins that a repository which
// has never had to retire a corpus point is not in violation. Git cannot track
// an empty directory, so `retirements/` simply does not exist until the first
// ledger lands; requiring one would force the first person who needs the gate
// to invent a ledger for a retirement that never happened.
func TestRunAcceptsAnAbsentRetirementLedgerDirectory(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	writeHistoryFile(t, filepath.Join(corpusDir, historyCorpusFileName(t, "fc_0000000001_q0_p0")),
		historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=base"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "corpus without any retirement ledger")
	trusted := gitForTest(t, repo, "rev-parse", "HEAD")
	if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(ledgerRepoPath))); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly has a retirement-ledger directory: %v", err)
	}
	if err := runAtRepo(repo, trusted); err != nil {
		t.Fatalf("ledger-free repository was rejected: %v", err)
	}
}

// TestAdditiveFamilyAppendNeedsNoLedger pins the granularity of the gate.
// Since RFC-201 §5.7 the corpus is grouped one file per feature family, so a
// routine batch APPENDS its scenarios into existing family files and those
// files' bytes change. Comparing files would demand a retirement ledger for
// every batch — a governance instrument that fires on everything authorizes
// nothing. The unit compared is the scenario.
func TestAdditiveFamilyAppendNeedsNoLedger(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	name := historyCorpusFileName(t, "fc_0000000001_q0_p0")
	first := historyCorpusScenarios(t, []string{"fc_0000000001_q0_p0"}, "shape=base")
	writeHistoryFile(t, filepath.Join(corpusDir, name), first)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted corpus")
	trusted := gitForTest(t, repo, "rev-parse", "HEAD")

	grown := historyCorpusScenarios(t, []string{"fc_0000000001_q0_p0", "fc_0000000001_q1_p0"}, "shape=base")
	if bytes.Equal(first, grown) {
		t.Fatal("appending a scenario did not change the family file's bytes")
	}
	writeHistoryFile(t, filepath.Join(corpusDir, name), grown)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "append a scenario into an existing family")
	if err := runAtRepo(repo, trusted); err != nil {
		t.Fatalf("additive append into an existing family file was rejected: %v", err)
	}

	rewritten := historyCorpusScenarios(t, []string{"fc_0000000001_q0_p0", "fc_0000000001_q1_p0"}, "shape=re-blessed")
	writeHistoryFile(t, filepath.Join(corpusDir, name), rewritten)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "rewrite a committed scenario without a ledger")
	if err := runAtRepo(repo, trusted); err == nil ||
		!strings.Contains(err.Error(), "changed without one new retirement ledger") ||
		!strings.Contains(err.Error(), "fc_0000000001_q0_p0 (scenario rewritten)") {
		t.Fatalf("unledgered scenario rewrite error = %v, want a scenario-level rejection", err)
	}
}

// TestRunToleratesTheBaseBranchMovingAfterTheMergeCommit pins the gate against
// a RACE, not against a malformed change.
//
// CI evaluates a pull request on a merge commit that GitHub builds at trigger
// time. The base branch keeps moving afterwards, so by the time this gate runs
// the base TIP is frequently no longer an ancestor of that merge commit. A gate
// anchored to the tip therefore rejects changes that did nothing wrong, at
// random, depending on how busy the base branch is — which is worse than no
// gate, because a check that fails for unrelated reasons trains everyone to
// re-run it without reading it.
//
// The anchor is the merge base: the base AS THIS CHANGE SEES IT. Nothing is
// lost. Ledgers present at the fork point are still immutable here; ledgers
// added to the base afterwards are not in this change's history, so this change
// cannot have touched them.
func TestRunToleratesTheBaseBranchMovingAfterTheMergeCommit(t *testing.T) {
	t.Parallel()
	repo := newHistoryTestRepo(t)
	corpusDir := filepath.Join(repo, filepath.FromSlash(corpusRepoPath))
	writeHistoryFile(t, filepath.Join(corpusDir, historyCorpusFileName(t, "fc_0000000001_q0_p0")),
		historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=base"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "base")
	baseBranch := gitForTest(t, repo, "rev-parse", "--abbrev-ref", "HEAD")
	forkPoint := gitForTest(t, repo, "rev-parse", "HEAD")

	// The change: a purely additive corpus growth on its own branch.
	gitForTest(t, repo, "checkout", "--quiet", "-b", "change")
	writeHistoryFile(t, filepath.Join(corpusDir, historyCorpusFileName(t, "fc_0000000002_q0_p0")),
		historyCorpusScenario(t, "fc_0000000002_q0_p0", "shape=added"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "add a scenario")

	// The merge commit CI actually evaluates, built against the fork point.
	gitForTest(t, repo, "checkout", "--quiet", forkPoint)
	gitForTest(t, repo, "merge", "--quiet", "--no-ff", "-m", "Merge change into base", "change")
	mergeCommit := gitForTest(t, repo, "rev-parse", "HEAD")

	// The base branch moves on, exactly as a busy trunk does.
	gitForTest(t, repo, "checkout", "--quiet", baseBranch)
	writeHistoryFile(t, filepath.Join(repo, "unrelated.txt"), []byte("later base work\n"))
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "unrelated base movement")
	movedTip := gitForTest(t, repo, "rev-parse", "HEAD")
	gitForTest(t, repo, "checkout", "--quiet", mergeCommit)

	if err := verifyBaseCommit(repo, movedTip, mergeCommit); err == nil {
		t.Fatal("fixture is not exercising the race: the moved tip is still an ancestor of the merge commit")
	}
	if err := runAtRepo(repo, baseBranch); err != nil {
		t.Fatalf("gate rejected an innocent change because the base branch moved: %v", err)
	}
}

func newHistoryTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitForTest(t, repo, "init", "--quiet")
	gitForTest(t, repo, "config", "user.name", "Corpus Gate Test")
	gitForTest(t, repo, "config", "user.email", "corpus-gate@example.invalid")
	return repo
}

// historyFamilyVectors are four feature vectors that land in four DISTINCT
// families, hence four distinct committed family files. The gate compares
// SCENARIOS parsed out of family files, so a fixture has to be a real one: it
// goes through the writer and comes back through the loader, which is also
// what keeps these tests honest if the committed format moves again.
var historyFamilyVectors = map[string]string{
	"fc_0000000001_q0_p0": "shape=single;idx=A;proj=star;where=cmp.gt;order=none",
	"fc_0000000002_q0_p0": "shape=single;idx=A;proj=star;where=in.in;order=none",
	"fc_0000000003_q0_p0": "shape=single;idx=A;proj=star;where=between.ge;order=none",
	"fc_0000000004_q0_p0": "shape=single;idx=A;proj=star;where=colcol.le;order=none",
	// A second point in the SAME family as fc_0000000001_q0_p0: a family file
	// holds many scenarios, and an append into one is the routine batch shape.
	"fc_0000000001_q1_p0": "shape=single;idx=A;proj=star;where=cmp.gt;order=none",
}

func historyFeatureVector(t *testing.T, key string) string {
	t.Helper()
	fv, ok := historyFamilyVectors[key]
	if !ok {
		t.Fatalf("no fixture feature vector for %q", key)
	}
	return fv
}

// historyCorpusFileName is the family file a fixture scenario belongs in. The
// loader re-derives it and rejects a scenario sitting in the wrong file, so the
// name cannot be chosen freely.
func historyCorpusFileName(t *testing.T, key string) string {
	t.Helper()
	return factorycorpus.FamilyFileName(factorycorpus.FamilyOf(historyFeatureVector(t, key)))
}

// historyCorpusScenario renders one scenario as a whole family file. label
// varies the committed PLAN SHAPE, which is how a fixture expresses "the same
// corpus point, re-blessed" — the bytes differ, the family file does not.
func historyCorpusScenario(t *testing.T, key, label string) []byte {
	t.Helper()
	return historyCorpusScenarios(t, []string{key}, label)
}

// historyCorpusScenarios renders several scenarios as ONE family file, which is
// how the committed corpus is actually shaped. Every key must belong to the
// same family; the loader re-derives the family per scenario and rejects a file
// that mixes them.
func historyCorpusScenarios(t *testing.T, keys []string, label string) []byte {
	t.Helper()
	entries := make([]*factorycorpus.Scenario, 0, len(keys))
	for i, key := range keys {
		fv := historyFeatureVector(t, key)
		sum := sha256.Sum256([]byte(label + "|" + key))
		shape := hex.EncodeToString(sum[:8])
		entries = append(entries, &factorycorpus.Scenario{
			Header: factorycorpus.Header{
				Name:          key,
				Generator:     "history-gate-test/1",
				Seed:          1,
				QueryIndex:    i,
				Projection:    0,
				Date:          "2026-08-01",
				Blessing:      factorycorpus.BlessingMetamorphic,
				Oracles:       []string{"test"},
				FeatureVector: fv,
				PlanShape:     shape,
				DedupKey:      factorycorpus.DedupKeyOf(fv, shape),
			},
			Doc: &yamsql.Scenario{
				Name:           key,
				SchemaTemplate: "CREATE TABLE t (id BIGINT, PRIMARY KEY (id))",
				Setup:          []string{"INSERT INTO t VALUES (1)"},
				Tests: []yamsql.Test{{
					Query:     "SELECT id FROM t",
					Unordered: true,
					Columns:   []string{"ID"},
					Rows:      [][]any{{int64(1)}},
				}},
			},
		})
	}
	data, err := factorycorpus.MarshalFamily(entries)
	if err != nil {
		t.Fatalf("marshal fixture family file: %v", err)
	}
	return data
}

func historyTestLedger(
	t *testing.T,
	baseCommit, beforeDir, afterDir string,
	change factorycorpus.RetirementChange,
) factorycorpus.RetirementLedger {
	t.Helper()
	beforeFiles, err := factorycorpus.LoadDir(beforeDir)
	if err != nil {
		t.Fatalf("load before corpus: %v", err)
	}
	afterFiles, err := factorycorpus.LoadDir(afterDir)
	if err != nil {
		t.Fatalf("load after corpus: %v", err)
	}
	beforeCensus, err := factorycorpus.CensusSHA256(factorycorpus.ComputeCensus(beforeFiles))
	if err != nil {
		t.Fatal(err)
	}
	afterCensus, err := factorycorpus.CensusSHA256(factorycorpus.ComputeCensus(afterFiles))
	if err != nil {
		t.Fatal(err)
	}
	beforeTree, err := factorycorpus.CorpusTreeSHA256(beforeDir)
	if err != nil {
		t.Fatal(err)
	}
	afterTree, err := factorycorpus.CorpusTreeSHA256(afterDir)
	if err != nil {
		t.Fatal(err)
	}
	return factorycorpus.RetirementLedger{
		FormatVersion:      2,
		RFC:                "RFC-test",
		Date:               "2026-08-01",
		Reason:             "history gate adversarial transition",
		BaseCommit:         baseCommit,
		BeforeCensusSHA256: beforeCensus,
		AfterCensusSHA256:  afterCensus,
		BeforeTreeSHA256:   beforeTree,
		AfterTreeSHA256:    afterTree,
		Changes:            []factorycorpus.RetirementChange{change},
	}
}

func writeHistoryLedger(t *testing.T, repo string, ledger factorycorpus.RetirementLedger) (string, []byte) {
	t.Helper()
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	ledgerPath := filepath.Join(repo, filepath.FromSlash(ledgerRepoPath), "2026-08-01-rfc-test.json")
	writeHistoryFile(t, ledgerPath, data)
	return ledgerPath, data
}

func writeHistoryFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func historyDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func gitForTest(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
