// Package infra is test-only: it holds the guards for the CI fleet's provisioning inputs.
// Nothing here ships to a box — the boxes run cloud-init.yaml and the two shell scripts,
// and these tests are what keep those honest between provisions.
package infra

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// renderTemplate applies the subset of Terraform's templatefile() semantics that
// cloud-init.yaml actually uses: ${identifier}, ${base64encode(identifier)},
// %{ if identifier } ... %{ endif }, and the $${ / %%{ escapes for markers that must
// reach the box literally.
//
// It is a model of templatefile, not a reimplementation of it, so it refuses anything it
// does not understand rather than guessing: an unknown variable or an unsupported
// directive is an error. That is what keeps the model honest as the template evolves — a
// new construct fails the guard loudly instead of silently rendering to a wrong size.
func renderTemplate(tmpl string, vars map[string]string) (string, error) {
	var out strings.Builder
	// emit[len-1] reports whether the innermost enclosing %{if} is taken; output is
	// discarded while any enclosing branch is false, exactly as templatefile does.
	emit := []bool{true}
	live := func() bool { return emit[len(emit)-1] }

	for i := 0; i < len(tmpl); {
		switch {
		case strings.HasPrefix(tmpl[i:], "$${"):
			if live() {
				out.WriteString("${")
			}
			i += 3
		case strings.HasPrefix(tmpl[i:], "%%{"):
			if live() {
				out.WriteString("%{")
			}
			i += 3
		case strings.HasPrefix(tmpl[i:], "${"), strings.HasPrefix(tmpl[i:], "%{"):
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated %q at offset %d", tmpl[i:i+2], i)
			}
			expr := strings.TrimSpace(tmpl[i+2 : i+end])
			directive := tmpl[i] == '%'
			i += end + 1
			if directive {
				if err := applyDirective(expr, vars, &emit); err != nil {
					return "", err
				}
				continue
			}
			v, err := evalInterpolation(expr, vars)
			if err != nil {
				return "", err
			}
			if live() {
				out.WriteString(v)
			}
		default:
			if live() {
				out.WriteByte(tmpl[i])
			}
			i++
		}
	}
	if len(emit) != 1 {
		return "", fmt.Errorf("unbalanced %%{if}: %d branches left open", len(emit)-1)
	}
	return out.String(), nil
}

func evalInterpolation(expr string, vars map[string]string) (string, error) {
	if name, ok := strings.CutPrefix(expr, "base64encode("); ok {
		name, ok = strings.CutSuffix(name, ")")
		if !ok {
			return "", fmt.Errorf("malformed base64encode call: %q", expr)
		}
		v, err := lookup(name, vars)
		if err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString([]byte(v)), nil
	}
	return lookup(expr, vars)
}

func lookup(name string, vars map[string]string) (string, error) {
	v, ok := vars[name]
	if !ok {
		return "", fmt.Errorf("template references %q, which the size guard does not know; "+
			"add it (and the value main.tf passes) to templateVars", name)
	}
	return v, nil
}

// applyDirective handles `if <var>` / `else` / `endif`. Truthiness follows the one use in
// the template: a bool rendered by Terraform as the string "true".
func applyDirective(expr string, vars map[string]string, emit *[]bool) error {
	switch {
	case strings.HasPrefix(expr, "if "):
		v, err := lookup(strings.TrimSpace(strings.TrimPrefix(expr, "if ")), vars)
		if err != nil {
			return err
		}
		*emit = append(*emit, (*emit)[len(*emit)-1] && v == "true")
	case expr == "else":
		if len(*emit) < 2 {
			return fmt.Errorf("%%{else} outside a %%{if}")
		}
		(*emit)[len(*emit)-1] = (*emit)[len(*emit)-2] && !(*emit)[len(*emit)-1]
	case expr == "endif":
		if len(*emit) < 2 {
			return fmt.Errorf("%%{endif} outside a %%{if}")
		}
		*emit = (*emit)[:len(*emit)-1]
	default:
		return fmt.Errorf("unsupported template directive %%{%s}; teach the size guard about "+
			"it before using it in cloud-init.yaml", expr)
	}
	return nil
}
