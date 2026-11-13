package cmd

import (
	"fmt"
	"strings"
)

// quoteShellArg keeps generated command suggestions safe to paste into a shell,
// including the dollar signs in proto-derived index names.
func quoteShellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// validateOutputFormat enforces that --output matches one of the allowed
// values. An empty string is accepted (commands treat it as their default,
// which matches the cobra flag default — usually "text"). The error
// message lists allowed values so users can fix the typo without
// reaching for --help.
//
// Consolidates the "switch outputFmt { case …: default: return fmt.Errorf(…) }"
// block that every command with -o/--output used to repeat.
func validateOutputFormat(v string, allowed ...string) error {
	if v == "" {
		return nil
	}
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("invalid --output %q: want %s",
		v, strings.Join(allowed, " or "))
}
