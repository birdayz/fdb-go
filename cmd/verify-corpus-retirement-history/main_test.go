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
		{name: "fc_0001.yaml", want: true},
		{name: "nested/file.yaml"},
		{name: `..\escape.yaml`},
		{name: `C:\escape.yaml`},
		{name: "../escape.yaml"},
		{name: "not-yaml.json"},
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
	ledgerPath := filepath.Join(repo, filepath.FromSlash(ledgerRepoPath), "2026-08-01-rfc205.json")
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
	wantRepoPath := ledgerRepoPath + "/2026-08-01-rfc205.json"
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
	repoPath := ledgerRepoPath + "/2026-08-01-rfc205.json"
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
	oldName := "fc_0000000001_q0_p0.yaml"
	oldBefore := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=before")
	writeHistoryFile(t, filepath.Join(corpusDir, oldName), oldBefore)
	writeHistoryFile(t, filepath.Join(beforeDir, oldName), oldBefore)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "stale ledger base")
	baseCommit := gitForTest(t, repo, "rev-parse", "HEAD")

	targetName := "fc_0000000002_q0_p0.yaml"
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
	beforeName := "fc_0000000001_q0_p0.yaml"
	beforeBytes := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=before")
	writeHistoryFile(t, filepath.Join(corpusDir, beforeName), beforeBytes)
	writeHistoryFile(t, filepath.Join(beforeDir, beforeName), beforeBytes)
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted base")
	trustedCommit := gitForTest(t, repo, "rev-parse", "HEAD")

	addedName := "fc_0000000002_q0_p0.yaml"
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
	name := "fc_0000000001_q0_p0.yaml"
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

	growthName := "fc_0000000002_q0_p0.yaml"
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
	swapName := "fc_0000000003_q0_p0.yaml"
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
	addedSymlink := filepath.Join(corpusDir, "fc_0000000004_q0_p0.yaml")
	if err := os.Symlink(corpusSymlinkTarget, addedSymlink); err != nil {
		t.Fatal(err)
	}
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "proposed corpus file") || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("added corpus symlink error = %v, want non-regular-file rejection", err)
	}
	if err := os.Remove(addedSymlink); err != nil {
		t.Fatal(err)
	}
	weirdName := filepath.Join(corpusDir, `C:\x.yaml`)
	writeHistoryFile(t, weirdName,
		historyCorpusScenario(t, "fc_0000000004_q0_p0", "shape=nonportable-name"))
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "portable flat .yaml name") {
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
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "regular 0644 file") {
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
	if err := runAtRepo(repo, trustedGrowth); err == nil || !strings.Contains(err.Error(), "regular 0644 file") {
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
	if err := os.Symlink(target, filepath.Join(corpusDir, "fc_0000000001_q0_p0.yaml")); err != nil {
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
	name := "fc_0000000001_q0_p0.yaml"
	data := historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=$Format:%H$")
	writeHistoryFile(t, filepath.Join(corpusDir, name), data)
	attributes := fmt.Sprintf("%s/*.yaml export-ignore export-subst\n", corpusRepoPath)
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
	writeHistoryFile(t, filepath.Join(corpusDir, "fc_0000000001_q0_p0.yaml"),
		historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=base"))
	if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(ledgerRepoPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted flat corpus")
	trusted := gitForTest(t, repo, "rev-parse", "HEAD")

	nestedDir := filepath.Join(corpusDir, "nested")
	nestedPath := filepath.Join(nestedDir, "fc_0000000002_q0_p0.yaml")
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
	name := "fc_0000000001_q0_p0.yaml"
	data := append(historyCorpusScenario(t, "fc_0000000001_q0_p0", "shape=base"), []byte("\n# $Id$\n")...)
	writeHistoryFile(t, filepath.Join(corpusDir, name), data)
	if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(ledgerRepoPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, repo, "add", ".")
	gitForTest(t, repo, "commit", "--quiet", "-m", "trusted raw corpus")
	trusted := gitForTest(t, repo, "rev-parse", "HEAD")

	attributes := fmt.Sprintf("%s/*.yaml ident\n", corpusRepoPath)
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
	name := "fc_0000000001_q0_p0.yaml"
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
			repoPath: ledgerRepoPath + "/2026-08-01-rfc205.json",
			date:     "2026-08-01",
		},
		{
			name:      "filename date mismatch",
			repoPath:  ledgerRepoPath + "/2026-08-01-rfc205.json",
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
			repoPath:  ledgerRepoPath + "/2026-06-01-rfc205.json",
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

func newHistoryTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitForTest(t, repo, "init", "--quiet")
	gitForTest(t, repo, "config", "user.name", "Corpus Gate Test")
	gitForTest(t, repo, "config", "user.email", "corpus-gate@example.invalid")
	return repo
}

func historyCorpusScenario(t *testing.T, name, feature string) []byte {
	t.Helper()
	shape := "0123456789abcdef"
	dedup := factorycorpus.DedupKeyOf(feature, shape)
	data := []byte(fmt.Sprintf(`# %s
#
# format-version: 1
# generator: history-gate-test/1
# seed: 1
# query-index: 0
# projection: 0
# date: 2026-08-01
# blessing: metamorphic
# oracles: test
# feature-vector: %s
# plan-shape: %s
# dedup-key: %s

name: %q
schema_template: |-
  CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))
setup:
  - |-
    INSERT INTO t VALUES (1)
tests:
  - query: |-
      SELECT id FROM t
    unordered: true
    rows:
      - [1]
`, name, feature, shape, dedup, name))
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
