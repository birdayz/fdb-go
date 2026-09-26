package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo makes a repository whose diff configuration is as hostile to the
// parse as a user's could be: no a/ b/ prefixes, quoted paths, an external
// diff. The scan must override all of it.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", "."},
		{"config", "user.email", "scan@example.com"},
		{"config", "user.name", "scan"},
		{"config", "diff.noprefix", "true"},
		{"config", "diff.mnemonicPrefix", "true"},
		{"config", "core.quotePath", "true"},
		{"config", "diff.external", "false"},
		{"config", "commit.gpgsign", "false"},
	} {
		gitT(t, dir, args...)
	}
	return dir
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func put(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runScan runs the command in-process with an empty home, so no secret of the
// machine running the test takes part.
func runScan(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	home := t.TempDir()
	var out, errOut bytes.Buffer
	code := run(append([]string{"-repo", dir}, args...), &out, &errOut, func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	})
	return code, out.String(), errOut.String()
}

func TestStaged(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	put(t, dir, "clean.go", "package p\n\nconst version = \"4.14.2.0\"\n")
	put(t, dir, "dir with space/ok.txt", "fine\n")
	gitT(t, dir, "add", ".")
	code, out, errOut := runScan(t, dir, "-staged")
	if code != 0 || out != "" {
		t.Fatalf("clean staged content: exit %d, findings %q, stderr %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "2 files, 4 added lines") {
		t.Fatalf("the summary must state the population scanned; got %q", errOut)
	}
	gitT(t, dir, "commit", "-qm", "base")

	put(t, dir, "clean.go", "package p\n\nconst version = \"4.14.2.0\"\nvar hcloudToken = \""+opaqueValue+"\"\n")
	put(t, dir, "infra/terraform.tfstate", "{}\n")
	put(t, dir, "unstaged.txt", privateKeyHeader+"\n")
	gitT(t, dir, "add", "clean.go", "infra/terraform.tfstate")
	code, out, _ = runScan(t, dir, "-staged")
	want := "clean.go:4: credential assigned an opaque literal\n" +
		"infra/terraform.tfstate: OpenTofu/Terraform state or plan file\n"
	if code != 1 || out != want {
		t.Fatalf("exit %d, findings:\n%s\nwant exit 1 and:\n%s(the unstaged file is not the commit's)", code, out, want)
	}
}

// TestCommitsScansEveryCommit: a secret added and deleted again inside the
// range is still in the published history, so the scan reads each commit,
// the root commit included, and names the commit.
func TestCommitsScansEveryCommit(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	put(t, dir, "a.txt", "root "+githubToken+"\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "root")
	root := strings.TrimSpace(gitT(t, dir, "rev-parse", "HEAD"))
	put(t, dir, "a.txt", "clean\n")
	gitT(t, dir, "commit", "-qam", "scrub")
	put(t, dir, "b.txt", "ssh root@"+publicIP+"\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "leak")
	leak := strings.TrimSpace(gitT(t, dir, "rev-parse", "HEAD"))
	put(t, dir, "b.txt", "gone\n")
	gitT(t, dir, "commit", "-qam", "scrub again")

	code, out, errOut := runScan(t, dir, "-commits", "HEAD")
	want := root[:12] + ":a.txt:1: GitHub token\n" + leak[:12] + ":b.txt:1: public IPv4 address\n"
	if code != 1 || out != want {
		t.Fatalf("exit %d, findings:\n%s\nwant:\n%s\nstderr: %s", code, out, want, errOut)
	}
	if !strings.Contains(errOut, "4 commits") {
		t.Fatalf("summary does not state the commits read: %q", errOut)
	}
	if code, out, _ := runScan(t, dir, "-commits", "HEAD~1..HEAD"); code != 0 || out != "" {
		t.Fatalf("the last commit alone is clean: exit %d, %q", code, out)
	}
}

// TestCommitsChargesAMergeOnlyWithItsOwnLines: a branch that merges master in
// is not charged with master's content, but a secret written into the merge's
// own conflict resolution is caught.
func TestCommitsChargesAMergeOnlyWithItsOwnLines(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	put(t, dir, "f.txt", "base\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "base")
	gitT(t, dir, "checkout", "-q", "-b", "feature")
	put(t, dir, "f.txt", "feature\n")
	gitT(t, dir, "commit", "-qam", "feature")
	gitT(t, dir, "checkout", "-q", "main")
	put(t, dir, "f.txt", "main\n")
	put(t, dir, "upstream.txt", "an address upstream published: "+publicIP+" // secretscan:allow\nand one it did not mark: "+publicIP+"\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "main moves")
	gitT(t, dir, "checkout", "-q", "feature")

	cmd := exec.Command("git", "merge", "-q", "main")
	cmd.Dir = dir
	_ = cmd.Run() // conflicts on f.txt, by construction
	put(t, dir, "f.txt", "resolved with "+awsKey+"\n")
	gitT(t, dir, "add", "f.txt")
	gitT(t, dir, "commit", "-qm", "merge main")
	merge := strings.TrimSpace(gitT(t, dir, "rev-parse", "HEAD"))

	code, out, errOut := runScan(t, dir, "-commits", "main..feature")
	want := merge[:12] + ":f.txt:1: AWS access key id\n"
	if code != 1 || out != want {
		t.Fatalf("exit %d, findings:\n%s\nwant only the merge's own line:\n%s\nstderr: %s", code, out, want, errOut)
	}
}

func TestRefusesToReportNothingAsClean(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	put(t, dir, "a.txt", "x\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "one")
	if code, _, errOut := runScan(t, dir, "-commits", "HEAD..HEAD"); code != 2 || !strings.Contains(errOut, "holds no commits") {
		t.Fatalf("an empty range: exit %d, %q; want 2, refused", code, errOut)
	}
	if code, _, errOut := runScan(t, dir, "-commits", "no-such-ref"); code != 2 {
		t.Fatalf("a bad range: exit %d, %q; want 2", code, errOut)
	}
	if code, _, _ := runScan(t, dir); code != 2 {
		t.Fatalf("no mode: exit %d, want 2", code)
	}
	if code, _, _ := runScan(t, dir, "-staged", "-tree"); code != 2 {
		t.Fatalf("two modes: exit %d, want 2", code)
	}
}

func TestTree(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	put(t, dir, "ok.md", "Java 4.14.2.0\n")
	put(t, dir, "bin.dat", "\x00"+privateKeyHeader)
	put(t, dir, "doc.md", "line\n"+slackToken+"\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "c")
	code, out, _ := runScan(t, dir, "-tree")
	if code != 1 || out != "doc.md:2: Slack token\n" {
		t.Fatalf("exit %d, findings %q; want the one text finding (a binary file is named, not read)", code, out)
	}
}

// TestCommitsReadsAMergeThatKeptOneSide: a merge whose file differs from
// BOTH parents while every hunk of it equals one of them (a clean hunk from
// main, a conflict resolved to the branch's side) introduces no line of its
// own. git's dense combined diff (--cc) omits that file from the patch while
// --name-only still lists it, which the coverage check refuses; the merge must
// scan clean, not fail. The shape is 9c410f65d's (claude.yml).
func TestCommitsReadsAMergeThatKeptOneSide(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	body := func(a, b string) string {
		return a + "\n1\n2\n3\n4\n5\n6\n7\n8\n9\n" + b + "\n"
	}
	put(t, dir, "f.txt", body("top", "bottom"))
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "base")
	gitT(t, dir, "checkout", "-q", "-b", "feature")
	put(t, dir, "f.txt", body("top", "bottom from feature"))
	gitT(t, dir, "commit", "-qam", "feature")
	gitT(t, dir, "checkout", "-q", "main")
	put(t, dir, "f.txt", body("top from main", "bottom from main"))
	gitT(t, dir, "commit", "-qam", "main")
	gitT(t, dir, "checkout", "-q", "feature")
	cmd := exec.Command("git", "merge", "-q", "main")
	cmd.Dir = dir
	_ = cmd.Run() // bottom conflicts; top merges cleanly from main
	put(t, dir, "f.txt", body("top from main", "bottom from feature"))
	gitT(t, dir, "add", "f.txt")
	gitT(t, dir, "commit", "-qm", "merge main, keep our bottom")
	if names := gitT(t, dir, "diff-tree", "-r", "--cc", "--name-only", "--no-commit-id", "HEAD"); names != "f.txt\n" {
		t.Fatalf("the fixture lost its shape: --cc --name-only lists %q, want f.txt", names)
	}
	if patch := gitT(t, dir, "diff-tree", "-r", "--cc", "--no-commit-id", "HEAD"); patch != "" {
		t.Fatalf("the fixture lost its shape: the dense patch is %q, want empty", patch)
	}
	if code, out, errOut := runScan(t, dir, "-commits", "main..feature"); code != 0 || out != "" {
		t.Fatalf("exit %d, findings %q, stderr %s; want a clean scan", code, out, errOut)
	}
}
