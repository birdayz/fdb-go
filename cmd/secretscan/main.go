// Command secretscan refuses content that must never reach this repository,
// which is public: credentials and tokens, private keys, OpenTofu state and
// variables, public host addresses, and the literal contents of the secret
// files on the machine running it (a Hetzner Cloud token has no shape a
// pattern could know; its bytes do).
//
//	secretscan -staged           what `git commit` would record (the pre-commit hook)
//	secretscan -commits A..B     every commit in the range, one by one (CI): a
//	                             secret added and then deleted is still public
//	secretscan -tree             every tracked file (an audit)
//
// A finding names the file, line and rule, never the matched text: a finding
// printed into a public CI log would publish what it caught. One line may opt
// out of the pattern rules with "secretscan:allow"; nothing opts out of the
// literal check or of the file-name rules.
//
// Exit status: 0 clean, 1 findings, 2 the scan could not run. The summary line
// states how many files and lines were read, so a clean result is checkable
// against the population it covers.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("secretscan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "the repository to scan")
	staged := fs.Bool("staged", false, "scan what the index adds over HEAD")
	commits := fs.String("commits", "", "scan every commit of this rev-list range")
	tree := fs.Bool("tree", false, "scan every tracked file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	modes := 0
	for _, on := range []bool{*staged, *commits != "", *tree} {
		if on {
			modes++
		}
	}
	if modes != 1 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "secretscan: exactly one of -staged, -commits RANGE, -tree")
		return 2
	}

	secrets := localSecrets(getenv)
	var findings []finding
	files, lines := 0, 0
	tally := func(cs []change) {
		files += len(cs)
		for _, c := range cs {
			lines += len(c.Lines)
		}
	}
	switch {
	case *staged:
		cs, err := diffChanges(*repo, "--cached")
		if err != nil {
			fmt.Fprintf(stderr, "secretscan: %v\n", err)
			return 2
		}
		tally(cs)
		findings = scan(cs, secrets, func(p string) string { return p })
	case *commits != "":
		out, err := git(*repo, "rev-list", "--reverse", *commits)
		if err != nil {
			fmt.Fprintf(stderr, "secretscan: %v\n", err)
			return 2
		}
		revs := strings.Fields(out)
		if len(revs) == 0 {
			fmt.Fprintf(stderr, "secretscan: the range %q holds no commits; nothing was scanned\n", *commits)
			return 2
		}
		for _, rev := range revs {
			cs, err := commitChanges(*repo, rev)
			if err != nil {
				fmt.Fprintf(stderr, "secretscan: commit %s: %v\n", rev, err)
				return 2
			}
			tally(cs)
			short := rev
			if len(short) > 12 {
				short = short[:12]
			}
			findings = append(findings, scan(cs, secrets, func(p string) string { return short + ":" + p })...)
		}
		fmt.Fprintf(stderr, "secretscan: %d commits\n", len(revs))
	case *tree:
		cs, err := treeChanges(*repo)
		if err != nil {
			fmt.Fprintf(stderr, "secretscan: %v\n", err)
			return 2
		}
		tally(cs)
		findings = scan(cs, secrets, func(p string) string { return p })
	}

	// Git's order: commits oldest first, paths sorted within each.
	for _, f := range findings {
		fmt.Fprintln(stdout, f)
	}
	fmt.Fprintf(stderr, "secretscan: %d files, %d added lines, %d local secrets compared: %d findings\n",
		files, lines, len(secrets), len(findings))
	if len(findings) > 0 {
		fmt.Fprintln(stderr, "secretscan: this repository is public; remove these before committing. "+
			"A line that is a documented false positive may carry \""+allowMarker+"\".")
		return 1
	}
	return 0
}

// diffArgs force the output parseDiff reads, whatever the user's config says:
// fixed a/ b/ prefixes, unquoted non-ASCII paths, no external diff or
// textconv, no rename pairing (a renamed file's whole content is re-read).
var diffArgs = []string{
	"-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "--no-textconv",
	"--no-renames", "--src-prefix=a/", "--dst-prefix=b/", "-U0", "--diff-filter=d",
}

// diffChanges parses `git diff <spec>` and checks the parse against git's own
// list of the files it covers: a file git lists that the parse did not record
// would be a file nobody scanned, and a clean result over it would be a lie.
func diffChanges(repo string, spec ...string) ([]change, error) {
	out, err := git(repo, append(append([]string{}, diffArgs...), spec...)...)
	if err != nil {
		return nil, err
	}
	cs, err := parseDiff(strings.NewReader(out))
	if err != nil {
		return nil, err
	}
	names, err := git(repo, append([]string{
		"-c", "core.quotePath=false", "diff", "--no-renames",
		"--name-only", "-z", "--diff-filter=d",
	}, spec...)...)
	if err != nil {
		return nil, err
	}
	return cs, checkCovered(cs, names)
}

func commitChanges(repo, rev string) ([]change, error) {
	parents, err := git(repo, "rev-list", "--parents", "-n", "1", rev)
	if err != nil {
		return nil, err
	}
	if len(strings.Fields(parents)) == 1 {
		// A root commit: diff against the empty tree.
		empty, err := git(repo, "hash-object", "-t", "tree", "/dev/null")
		if err != nil {
			return nil, err
		}
		return diffChanges(repo, strings.TrimSpace(empty), rev)
	}
	if len(strings.Fields(parents)) > 2 {
		return mergeChanges(repo, rev)
	}
	return diffChanges(repo, rev+"^1", rev)
}

// mergeChanges reads what a merge itself introduces: git's combined diff,
// whose lines new against EVERY parent are the merge's own (a conflict
// resolution). Diffing against the first parent instead would re-read the
// whole merged-in branch as this commit's work: a branch merging master would
// be charged with master's content. The combined form is -c, not the dense
// --cc: --cc drops from the PATCH a file whose every hunk equals one parent
// while --name-only still lists it (9c410f65d, claude.yml).
func mergeChanges(repo, rev string) ([]change, error) {
	out, err := git(repo, "-c", "core.quotePath=false", "diff-tree", "-r", "-c", "--no-color", "--no-ext-diff",
		"--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "-U0", "--no-commit-id", rev)
	if err != nil {
		return nil, err
	}
	cs, err := parseCombined(strings.NewReader(out))
	if err != nil {
		return nil, err
	}
	names, err := git(repo, "-c", "core.quotePath=false", "diff-tree", "-r", "-c", "--name-only", "-z",
		"--no-commit-id", rev)
	if err != nil {
		return nil, err
	}
	return cs, checkCovered(cs, names)
}

func checkCovered(cs []change, nulNames string) error {
	got := map[string]bool{}
	for _, c := range cs {
		got[c.Path] = true
	}
	var missing []string
	for _, n := range strings.Split(nulNames, "\x00") {
		if n != "" && !got[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the diff parse did not record %d of the files git lists (first: %q); "+
			"refusing to report them clean", len(missing), missing[0])
	}
	return nil
}

// treeChanges reads every tracked file's working-tree content as added lines.
// Binary files (a NUL in the first 8000 bytes) are named but not read.
func treeChanges(repo string) ([]change, error) {
	out, err := git(repo, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var cs []change
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		c := change{Path: p}
		b, err := os.ReadFile(repo + "/" + p)
		if err == nil && !bytes.Contains(b[:min(len(b), 8000)], []byte{0}) {
			for i, l := range strings.Split(string(b), "\n") {
				c.Lines = append(c.Lines, addedLine{Line: i + 1, Text: l})
			}
		}
		cs = append(cs, c)
	}
	return cs, nil
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
