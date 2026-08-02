package infra

import "testing"

// renderTemplate is a MODEL of Terraform's templatefile(), and a size guard built on a
// model that has drifted reports a comfortable number about a payload that does not fit.
// These cases pin the semantics the model claims, so the drift shows up here rather than
// at `tofu apply`.
//
// Cross-checked against the real thing while these were written: `tofu` rendering
// infra/cloud-init.yaml with the values main.tf passes produced 31060 bytes, and this
// model produced 31101 for the same file with a 40-char registration token and a
// one-char-longer runner name -- exactly the +41 that difference accounts for. The
// checked-in cross-check is not that number (it would churn on every edit to the
// template) but the semantics below, which are what made the numbers agree.
func TestRenderTemplateSemantics(t *testing.T) {
	t.Parallel()

	vars := map[string]string{
		"name":     "runner",
		"on":       "true",
		"off":      "false",
		"key":      "PEM",
		"emptyval": "",
	}

	cases := []struct {
		desc, in, want string
	}{
		{"plain interpolation", "a-${name}-b", "a-runner-b"},
		{"empty value interpolates to nothing", "[${emptyval}]", "[]"},
		// The escape that carries a shell expansion through to the box unchanged. The
		// bootcmd Docker decision depends on it: $${CI_ROOT:-} must reach the box as a
		// shell parameter expansion, not be resolved by Terraform.
		{"escaped marker survives as a literal", "$${CI_ROOT:-}/mnt", "${CI_ROOT:-}/mnt"},
		{"escaped directive marker", "%%{ if x }", "%{ if x }"},
		{"base64encode", "${base64encode(key)}", "UEVN"},
		{"if taken", "x%{ if on }Y%{ endif }z", "xYz"},
		{"if not taken", "x%{ if off }Y%{ endif }z", "xz"},
		{"else branch", "%{ if off }A%{ else }B%{ endif }", "B"},
		{"else branch not taken", "%{ if on }A%{ else }B%{ endif }", "A"},
		{
			"nested if inside a false branch stays suppressed",
			"%{ if off }a%{ if on }b%{ endif }c%{ endif }d", "d",
		},
		{"interpolation inside a taken branch", "%{ if on }${name}%{ endif }", "runner"},
		{"interpolation inside a skipped branch emits nothing", "%{ if off }${name}%{ endif }", ""},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			got, err := renderTemplate(tc.in, vars)
			if err != nil {
				t.Fatalf("renderTemplate(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("renderTemplate(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The model must REFUSE what it does not understand. Guessing is how a guard reports a
// byte count for a payload that is not the one Terraform will build.
func TestRenderTemplateRefusesTheUnknown(t *testing.T) {
	t.Parallel()

	cases := []struct{ desc, in string }{
		{"unknown variable", "${nope}"},
		{"unknown variable inside base64encode", "${base64encode(nope)}"},
		{"unknown variable in a condition", "%{ if nope }x%{ endif }"},
		{"unsupported directive", "%{ for x in y }a%{ endfor }"},
		{"unterminated interpolation", "${name"},
		{"unbalanced if", "%{ if on }x"},
		{"endif without if", "x%{ endif }"},
		{"else without if", "x%{ else }y"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			if got, err := renderTemplate(tc.in, map[string]string{"name": "n", "on": "true"}); err == nil {
				t.Errorf("renderTemplate(%q) = %q, want an error", tc.in, got)
			}
		})
	}
}
