package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every token-shaped value here is assembled at run time, so this file never
// holds one literally: it is scanned by the hook that runs this scanner.
var (
	privateKeyHeader = "-----BEGIN " + "RSA PRIVATE KEY-----"
	opensshHeader    = "-----BEGIN " + "OPENSSH PRIVATE KEY-----"
	githubToken      = "gh" + "p_" + strings.Repeat("aB3", 12)
	githubPAT        = "github" + "_pat_" + strings.Repeat("Ab1_", 6)
	awsKey           = "AK" + "IA" + strings.Repeat("Q7", 8)
	slackToken       = "xo" + "xb-" + "1234567890-abcdef"
	skKey            = "sk-" + "ant-" + strings.Repeat("a1B2", 9)
	opaqueValue      = strings.Repeat("x9Y8", 10)
	publicIP         = fmt.Sprintf("%d.%d.%d.%d", 93, 184, 216, 34)
)

func lines(path string, text ...string) change {
	c := change{Path: path}
	for i, t := range text {
		c.Lines = append(c.Lines, addedLine{Line: i + 1, Text: t})
	}
	return c
}

func rulesOf(fs []finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func ident(p string) string { return p }

// TestNameRules drives every file-name rule with a path it must refuse and
// checks the look-alikes it must not.
func TestNameRules(t *testing.T) {
	t.Parallel()
	refused := map[string]string{
		"infra/terraform.tfstate":        "OpenTofu/Terraform state or plan file",
		"infra/terraform.tfstate.backup": "OpenTofu/Terraform state or plan file",
		"infra/plan.tfplan":              "OpenTofu/Terraform state or plan file",
		"infra/prod.tfvars":              "OpenTofu/Terraform variables file",
		"infra/prod.tfvars.json":         "OpenTofu/Terraform variables file",
		"infra/.terraform/providers/x":   "OpenTofu/Terraform working directory",
		"keys/id_rsa":                    "SSH private key file",
		"id_rsa_old":                     "SSH private key file",
		"id_ed25519":                     "SSH private key file",
		"certs/server.pem":               "key or certificate store",
		"certs/server.key":               "key or certificate store",
		"certs/client.p12":               "key or certificate store",
		".env":                           "credentials file",
		"deploy/.env.production":         "credentials file",
		".netrc":                         "credentials file",
		"service-account-ci.json":        "credentials file",
		"hetzner_api_key":                "API key or token file",
		"github_token":                   "API key or token file",
	}
	for p, want := range refused {
		got := rulesOf(scan([]change{{Path: p}}, nil, ident))
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: rules %v, want exactly [%s]", p, got, want)
		}
	}
	for _, p := range []string{
		"infra/.terraform.lock.hcl", "infra/main.tf", "keys/id_rsa.pub", "id_ed25519.pub",
		"pkg/relational/continuation_token.go", "docs/tokens.md", "env.go", "keymap.go",
	} {
		if got := scan([]change{{Path: p}}, nil, ident); len(got) != 0 {
			t.Errorf("%s refused as %v; it is not a secret file", p, rulesOf(got))
		}
	}
}

// TestContentRules drives every content rule with a line it must refuse.
func TestContentRules(t *testing.T) {
	t.Parallel()
	cases := []struct{ line, rule string }{
		{privateKeyHeader, "private key block"},
		{opensshHeader, "private key block"},
		{"found " + githubToken, "GitHub token"},
		{githubPAT, "GitHub token"},
		{"id " + awsKey + " end", "AWS access key id"},
		{slackToken, "Slack token"},
		{"key " + skKey, "API secret key (sk-)"},
		{`HCLOUD_TOKEN="` + opaqueValue + `"`, "credential assigned an opaque literal"},
		{"api_key = " + opaqueValue, "credential assigned an opaque literal"},
		{"password: " + opaqueValue, "credential assigned an opaque literal"},
		{"ssh root@" + publicIP, "public IPv4 address"},
		{"listen on " + publicIP + ":4500", "public IPv4 address"},
	}
	for _, c := range cases {
		got := rulesOf(scan([]change{lines("f.go", c.line)}, nil, ident))
		if len(got) != 1 || got[0] != c.rule {
			t.Errorf("%q: rules %v, want exactly [%s]", c.line, got, c.rule)
		}
	}
}

// TestContentRulesLeaveOrdinaryTextAlone pins the look-alikes this
// repository is full of: versions that parse as addresses, reserved and
// documentation addresses, credential words bound to words, hashes under a
// non-credential name, and a placeholder key.
func TestContentRulesLeaveOrdinaryTextAlone(t *testing.T) {
	t.Parallel()
	for _, l := range []string{
		"Java fdb-record-layer-core 4.14.2.0 and FDB 7.3.77",
		"upgrade from 4.12.11.0 to 4.14.2.0",
		"bind 127.0.0.1:4500 and 10.1.2.3:4500, docs use 203.0.113.9 and 198.51.100.7",
		"a version 1.2.3.4.5 is not an address",
		"coordinators 1.2.3.4:4500,5.6.7.8:4500 and resolver 8.8.8.8:53 are made up",
		`"reply_token": "0a000000000000100500000000000020",`,
		"token = nextToken(scanner)",
		"password: required",
		`sha256 = "` + strings.Repeat("ab12", 16) + `"`,
		"secret_name_that_is_long_but_all_letters = abcdefghijklmnopqrstuvwxyzabcdefgh",
		`appPrivateKey: "-----BEGIN RSA ...`,
		"the ghp_ prefix names a GitHub token",
	} {
		if got := scan([]change{lines("f.go", l)}, nil, ident); len(got) != 0 {
			t.Errorf("%q refused as %v", l, rulesOf(got))
		}
	}
}

// TestAllowMarker: the marker exempts its own line from the pattern rules,
// not its neighbours, and never exempts a literal secret or a file name.
func TestAllowMarker(t *testing.T) {
	t.Parallel()
	c := lines("certs/test.pem",
		privateKeyHeader+" // secretscan:allow (test fixture)",
		privateKeyHeader,
		"x "+opaqueValue+" // secretscan:allow",
	)
	got := scan([]change{c}, []secret{{Source: "~/hetzner_api_key", Value: opaqueValue}}, ident)
	want := []string{"key or certificate store", "private key block", "the contents of ~/hetzner_api_key"}
	if fmt.Sprint(rulesOf(got)) != fmt.Sprint(want) {
		t.Fatalf("rules %v, want %v", rulesOf(got), want)
	}
	if got[1].Line != 2 || got[2].Line != 3 {
		t.Fatalf("findings on lines %d and %d, want 2 and 3", got[1].Line, got[2].Line)
	}
}

// TestFindingsNeverCarryTheMatch: a finding is printed, in CI into a public
// log, so it must name where and what rule, and never echo the matched text.
func TestFindingsNeverCarryTheMatch(t *testing.T) {
	t.Parallel()
	values := []string{privateKeyHeader, githubToken, githubPAT, awsKey, slackToken, skKey, opaqueValue, publicIP}
	var c change
	c.Path = "f.go"
	for i, v := range values {
		c.Lines = append(c.Lines, addedLine{Line: i + 1, Text: "password = " + v})
	}
	fs := scan([]change{c}, []secret{{Source: "$HCLOUD_TOKEN", Value: opaqueValue}}, ident)
	if len(fs) < len(values) {
		t.Fatalf("%d findings for %d secret lines", len(fs), len(values))
	}
	for _, f := range fs {
		for _, v := range values {
			if strings.Contains(f.String(), v) {
				t.Fatalf("finding %q carries the matched text", f.String())
			}
		}
	}
}

// TestLocalSecrets loads the secret files and variables of a fake home and
// refuses their values byte for byte, including a token with no known shape.
func TestLocalSecrets(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	hcloud := strings.Repeat("Qz7k", 16) // 64 bare alphanumerics, the shape of a Hetzner token
	keyLine := strings.Repeat("b3BlbnNzaC1rZXktdjE", 3)
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("hetzner_api_key", hcloud+"\n")
	write(".ssh/id_rsa_old", opensshHeader+"\n"+keyLine+"\n")
	write(".ssh/id_rsa_old.pub", "ssh-rsa "+strings.Repeat("PUBLICkey9", 5)+" me@host\n")
	extra := filepath.Join(home, "elsewhere.txt")
	write("elsewhere.txt", "minio "+strings.Repeat("m1N", 12)+"\n")
	env := map[string]string{
		"HOME":             home,
		"SECRETSCAN_FILES": extra,
		"GH_TOKEN":         "gho_" + strings.Repeat("T0k", 8),
		"MINIO_PASSWORD":   "short",
	}
	ss := localSecrets(func(k string) string { return env[k] })
	bySource := map[string]string{}
	for _, s := range ss {
		bySource[s.Source] = s.Value
	}
	for src, want := range map[string]string{
		"~/hetzner_api_key": hcloud,
		"~/.ssh/id_rsa_old": keyLine,
		"~/elsewhere.txt":   strings.Repeat("m1N", 12),
		"$GH_TOKEN":         env["GH_TOKEN"],
	} {
		if bySource[src] != want {
			t.Errorf("secret from %s = %q, want %q", src, bySource[src], want)
		}
	}
	if _, ok := bySource["~/.ssh/id_rsa_old.pub"]; ok {
		t.Errorf("a public key was loaded as a secret")
	}
	if _, ok := bySource["$MINIO_PASSWORD"]; ok {
		t.Errorf("a 5-byte variable was loaded; a value that short matches ordinary text")
	}
	got := scan([]change{lines("notes.md", "the token is "+hcloud)}, ss, ident)
	if len(got) != 1 || got[0].Rule != "the contents of ~/hetzner_api_key" {
		t.Fatalf("a Hetzner token in a line: %v, want exactly the literal finding", rulesOf(got))
	}
}

// TestParseDiff pins the parse: line numbers from the hunk header, an added
// line that reads "+++ " inside a hunk kept as content, a binary file named,
// a quoted path unquoted, removed lines ignored.
func TestParseDiff(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --git a/a.txt b/a.txt",
		"index 1..2 100644",
		"--- a/a.txt",
		"+++ b/a.txt",
		"@@ -3,0 +4,2 @@ ctx",
		"+first",
		"+++ not a header",
		"@@ -9 +11 @@",
		"-removed",
		"+eleventh",
		"diff --git a/img.bin b/img.bin",
		"new file mode 100644",
		"Binary files /dev/null and b/img.bin differ",
		`diff --git "a/sp\"ace.txt" "b/sp\"ace.txt"`,
		"new file mode 100644",
		"--- /dev/null",
		`+++ "b/sp\"ace.txt"`,
		"@@ -0,0 +1 @@",
		"+q",
		"diff --git a/d s/x.txt b/d s/x.txt",
		"--- /dev/null",
		"+++ b/d s/x.txt\t",
		"@@ -0,0 +1 @@",
		"+y",
		"",
	}, "\n")
	cs, err := parseDiff(strings.NewReader(diff))
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(cs)
	want := `[{a.txt [{4 first} {5 ++ not a header} {11 eleventh}]} {img.bin []} {sp"ace.txt [{1 q}]} {d s/x.txt [{1 y}]}]`
	if got != want {
		t.Fatalf("parse:\n got %s\nwant %s", got, want)
	}
}

// TestParseCombined pins the merge parse: only lines new against every parent
// are the merge's own.
func TestParseCombined(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --cc f.txt",
		"index 1,2..3",
		"--- a/f.txt",
		"+++ b/f.txt",
		"@@@ -1,1 -1,1 +1,3 @@@",
		"- ours",
		" -theirs",
		"++resolved",
		" +from theirs",
		"+ from ours",
		"",
	}, "\n")
	cs, err := parseCombined(strings.NewReader(diff))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(cs), "[{f.txt [{1 resolved}]}]"; got != want {
		t.Fatalf("parse:\n got %s\nwant %s", got, want)
	}
}

func TestCheckCovered(t *testing.T) {
	t.Parallel()
	cs := []change{{Path: "a"}, {Path: "b c"}}
	if err := checkCovered(cs, "a\x00b c\x00"); err != nil {
		t.Fatalf("every listed file parsed: %v", err)
	}
	if err := checkCovered(cs, "a\x00missing\x00"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("a listed file the parse did not record: %v, want a refusal naming it", err)
	}
}
