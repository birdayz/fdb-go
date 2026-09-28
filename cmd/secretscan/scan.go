package main

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// allowMarker exempts one line from the pattern rules. It never exempts a
// literal match: the contents of a secret file on this machine are refused
// wherever they appear, marker or not.
const allowMarker = "secretscan:allow"

// finding is one refusal. It never carries the matched text: a finding printed
// into a public CI log would publish the very secret it caught.
type finding struct {
	Where string // "path", or "commit:path" in -commits mode
	Line  int    // 0 for a file-name finding
	Rule  string
}

func (f finding) String() string {
	if f.Line == 0 {
		return fmt.Sprintf("%s: %s", f.Where, f.Rule)
	}
	return fmt.Sprintf("%s:%d: %s", f.Where, f.Line, f.Rule)
}

type addedLine struct {
	Line int
	Text string
}

// change is one file of a diff: its path and the lines it adds.
type change struct {
	Path  string
	Lines []addedLine
}

// secret is a string that must not appear in committed content, and where it
// was found. The value is only ever compared, never printed.
type secret struct {
	Source string
	Value  string
}

// nameRules refuse a path by what the file is, whatever it holds: a state
// file or a key store is a leak even when its contents match no pattern.
var nameRules = []struct {
	rule  string
	match func(p, base string) bool
}{
	{"OpenTofu/Terraform state or plan file", func(_, b string) bool {
		return strings.HasSuffix(b, ".tfstate") || strings.Contains(b, ".tfstate.") || strings.HasSuffix(b, ".tfplan")
	}},
	{"OpenTofu/Terraform variables file", func(_, b string) bool {
		return strings.HasSuffix(b, ".tfvars") || strings.HasSuffix(b, ".tfvars.json")
	}},
	{"OpenTofu/Terraform working directory", func(p, _ string) bool {
		for _, c := range strings.Split(p, "/") {
			if c == ".terraform" {
				return true
			}
		}
		return false
	}},
	{"SSH private key file", func(_, b string) bool {
		for _, k := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
			if strings.HasPrefix(b, k) && !strings.HasSuffix(b, ".pub") {
				return true
			}
		}
		return false
	}},
	{"key or certificate store", func(_, b string) bool {
		for _, s := range []string{".pem", ".key", ".p12", ".pfx", ".jks", ".keystore"} {
			if strings.HasSuffix(b, s) {
				return true
			}
		}
		return false
	}},
	{"credentials file", func(_, b string) bool {
		switch b {
		case ".env", ".netrc", ".pgpass", ".htpasswd", "credentials.json", "kubeconfig":
			return true
		}
		return strings.HasPrefix(b, ".env.") || (strings.HasPrefix(b, "service-account") && strings.HasSuffix(b, ".json"))
	}},
	{"API key or token file", func(_, b string) bool {
		return strings.Contains(b, "api_key") || strings.Contains(b, "api-key") || strings.Contains(b, "apikey") ||
			strings.HasSuffix(b, "_token") || strings.HasSuffix(b, ".token")
	}},
}

// contentRules refuse an added line by a credential's shape.
var contentRules = []struct {
	rule string
	re   *regexp.Regexp
	// value, when set, must accept the regexp's first submatch too: the
	// shape alone would refuse ordinary identifiers.
	value func(string) bool
}{
	{rule: "private key block", re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{rule: "GitHub token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{rule: "AWS access key id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{rule: "Slack token", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{rule: "API secret key (sk-)", re: regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{32,}`)},
	{
		rule: "credential assigned an opaque literal",
		re: regexp.MustCompile(`(?i)(?:token|secret|passw(?:or)?d|api[_-]?key|access[_-]?key|private[_-]?key)[a-z0-9_]*["']?\s*[:=]\s*["` +
			"`" + `']?([A-Za-z0-9+/=_-]{32,})`),
		value: credentialValue,
	},
}

// credentialValue: key material, and not a hex digest or wire token. Hex
// values bound to a credential-sounding name are everywhere in this
// repository (an FDB reply_token, a sha256 pin); a hex secret of this machine
// is still refused byte for byte by the literal check.
func credentialValue(s string) bool {
	return opaque(s) && strings.Trim(s, "0123456789abcdefABCDEF") != ""
}

// opaque reports whether s looks like generated key material rather than a
// word: letters AND digits, both.
func opaque(s string) bool {
	return strings.ContainsAny(s, "0123456789") && strings.IndexFunc(s, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
	}) >= 0
}

var dottedQuad = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)

// reservedIPv4 are the blocks an address in documentation or a test is drawn
// from; none of them names a machine on the internet.
var reservedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// publicIPv4 reports whether the dotted quad at line[lo:hi] names a public
// host. A four-part version (4.14.2.0) is a valid address too, so validity
// cannot decide it: an address is taken to be public when it sits in no
// reserved block AND has two octets of 32 or more, which no version this
// repository cites has and every real host of its fleet does. A port is no
// tell: the client's tests are full of made-up coordinators ("1.2.3.4:4500").
// The dotted quad must also stand alone: a digit or ".<digit>" on either side
// makes it part of a longer number.
func publicIPv4(line string, lo, hi int) bool {
	if lo > 0 && (isDigit(line[lo-1]) || line[lo-1] == '.') {
		return false
	}
	if hi < len(line) && isDigit(line[hi]) {
		return false
	}
	if hi+1 < len(line) && line[hi] == '.' && isDigit(line[hi+1]) {
		return false
	}
	a, err := netip.ParseAddr(line[lo:hi])
	if err != nil || !a.Is4() {
		return false
	}
	for _, p := range reservedIPv4 {
		if p.Contains(a) {
			return false
		}
	}
	big := 0
	for _, o := range a.As4() {
		if o >= 32 {
			big++
		}
	}
	return big >= 2
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// scan applies every rule to changes and returns the findings. where maps a
// path to how a finding names it.
func scan(changes []change, secrets []secret, where func(string) string) []finding {
	var out []finding
	for _, c := range changes {
		w := where(c.Path)
		base := strings.ToLower(path.Base(c.Path))
		for _, r := range nameRules {
			if r.match(c.Path, base) {
				out = append(out, finding{Where: w, Rule: r.rule})
			}
		}
		for _, l := range c.Lines {
			for _, s := range secrets {
				if strings.Contains(l.Text, s.Value) {
					out = append(out, finding{Where: w, Line: l.Line, Rule: "the contents of " + s.Source})
				}
			}
			if strings.Contains(l.Text, allowMarker) {
				continue
			}
			for _, r := range contentRules {
				for _, m := range r.re.FindAllStringSubmatch(l.Text, -1) {
					if r.value == nil || (len(m) > 1 && r.value(m[1])) {
						out = append(out, finding{Where: w, Line: l.Line, Rule: r.rule})
						break
					}
				}
			}
			for _, ix := range dottedQuad.FindAllStringIndex(l.Text, -1) {
				if publicIPv4(l.Text, ix[0], ix[1]) {
					out = append(out, finding{Where: w, Line: l.Line, Rule: "public IPv4 address"})
					break
				}
			}
		}
	}
	return out
}

// parseDiff reads `git diff -U0 --src-prefix=a/ --dst-prefix=b/` output. A
// file is recorded at its `diff --git` header, so a binary or mode-only change
// is still named; `+++` names it again for a text change. File headers only
// occur before the first hunk, so an added line that itself begins "++ " (and
// so reads "+++ ") is content, not a header.
func parseDiff(r io.Reader) ([]change, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	var out []change
	inHeader := false
	next := 0
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "diff --git ") {
			out = append(out, change{Path: pathFromGitHeader(l)})
			inHeader = true
			continue
		}
		if len(out) == 0 {
			continue
		}
		cur := &out[len(out)-1]
		switch {
		case inHeader && strings.HasPrefix(l, "+++ "):
			if p := unquoteGitPath(strings.TrimPrefix(l, "+++ ")); p != "/dev/null" {
				cur.Path = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(l, "@@ "):
			inHeader = false
			n, err := hunkNewStart(l)
			if err != nil {
				return nil, err
			}
			next = n
		case inHeader:
		case strings.HasPrefix(l, "+"):
			cur.Lines = append(cur.Lines, addedLine{Line: next, Text: l[1:]})
			next++
		case strings.HasPrefix(l, " "):
			next++
		}
	}
	return out, sc.Err()
}

// parseCombined reads `git diff-tree -c -U0` output for one merge. A hunk
// header opens with one '@' per parent plus one; a content line carries one
// column per parent, and it is the merge's own addition when every column is
// '+'. Lines with no '-' column exist in the result and advance its numbering.
func parseCombined(r io.Reader) ([]change, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	var out []change
	inHeader := false
	cols, next := 0, 0
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "diff --cc ") || strings.HasPrefix(l, "diff --combined ") {
			p := l[strings.Index(l, " ")+1:]
			p = unquoteGitPath(p[strings.Index(p, " ")+1:])
			out = append(out, change{Path: p})
			inHeader = true
			continue
		}
		if len(out) == 0 {
			continue
		}
		cur := &out[len(out)-1]
		switch {
		case inHeader && strings.HasPrefix(l, "+++ "):
			if p := unquoteGitPath(strings.TrimPrefix(l, "+++ ")); p != "/dev/null" {
				cur.Path = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(l, "@@@"):
			inHeader = false
			ats := len(l) - len(strings.TrimLeft(l, "@"))
			cols = ats - 1
			i := strings.Index(l, " +")
			if i < 0 {
				return nil, fmt.Errorf("malformed combined hunk header %q", l)
			}
			s := l[i+2:]
			if j := strings.IndexAny(s, ", "); j >= 0 {
				s = s[:j]
			}
			n, err := strconv.Atoi(s)
			if err != nil {
				return nil, fmt.Errorf("malformed combined hunk header %q: %v", l, err)
			}
			next = n
		case inHeader || cols == 0 || len(l) < cols:
		default:
			prefix := l[:cols]
			if strings.Contains(prefix, "-") {
				continue
			}
			if strings.Trim(prefix, "+") == "" {
				cur.Lines = append(cur.Lines, addedLine{Line: next, Text: l[cols:]})
			}
			next++
		}
	}
	return out, sc.Err()
}

// hunkNewStart reads c from "@@ -a,b +c,d @@".
func hunkNewStart(l string) (int, error) {
	i := strings.Index(l, " +")
	if i < 0 {
		return 0, fmt.Errorf("malformed hunk header %q", l)
	}
	s := l[i+2:]
	if j := strings.IndexAny(s, ", "); j >= 0 {
		s = s[:j]
	}
	return strconv.Atoi(s)
}

func pathFromGitHeader(l string) string {
	rest := strings.TrimPrefix(l, "diff --git ")
	if i := strings.LastIndex(rest, ` "b/`); i >= 0 {
		return strings.TrimPrefix(unquoteGitPath(rest[i+1:]), "b/")
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return rest
}

// unquoteGitPath undoes git's C-style quoting of a path with special bytes,
// and drops the tab git appends after a ---/+++ path that holds a space.
func unquoteGitPath(p string) string {
	p = strings.TrimSuffix(p, "\t")
	if len(p) >= 2 && p[0] == '"' {
		if u, err := strconv.Unquote(p); err == nil {
			return u
		}
	}
	return p
}

// secretRun is a run of key-material characters long enough to be a token or
// a line of a key body.
var secretRun = regexp.MustCompile(`[A-Za-z0-9+/=_.-]{32,}`)

// localSecrets collects the secrets present on the machine running the scan:
// the files that hold credentials here, and the environment variables that
// carry them. Their values are refused byte for byte, which catches a token
// no pattern knows the shape of (a Hetzner Cloud token is 64 bare
// alphanumerics).
func localSecrets(getenv func(string) string) []secret {
	var files []string
	if home := getenv("HOME"); home != "" {
		files = append(files,
			filepath.Join(home, "hetzner_api_key"),
			filepath.Join(home, ".config", "hcloud", "cli.toml"),
			filepath.Join(home, ".config", "gh", "hosts.yml"),
			filepath.Join(home, ".aws", "credentials"),
			filepath.Join(home, ".docker", "config.json"),
			filepath.Join(home, ".netrc"),
		)
		if keys, err := filepath.Glob(filepath.Join(home, ".ssh", "id_*")); err == nil {
			for _, k := range keys {
				if !strings.HasSuffix(k, ".pub") {
					files = append(files, k)
				}
			}
		}
	}
	for _, f := range strings.Split(getenv("SECRETSCAN_FILES"), ":") {
		if f != "" {
			files = append(files, f)
		}
	}
	var out []secret
	seen := map[string]bool{}
	add := func(src, v string) {
		if !seen[v] {
			seen[v] = true
			out = append(out, secret{Source: src, Value: v})
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		src := f
		if home := getenv("HOME"); home != "" && strings.HasPrefix(f, home+"/") {
			src = "~/" + strings.TrimPrefix(f, home+"/")
		}
		for _, v := range secretRun.FindAllString(string(b), -1) {
			if opaque(v) {
				add(src, v)
			}
		}
	}
	for _, name := range []string{
		"HCLOUD_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "MINIO_PASSWORD",
		"AWS_SECRET_ACCESS_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
	} {
		if v := strings.TrimSpace(getenv(name)); len(v) >= 12 {
			add("$"+name, v)
		}
	}
	return out
}
