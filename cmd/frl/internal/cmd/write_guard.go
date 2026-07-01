package cmd

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

// Shared guardrails for the write wave (RFC-174 §3.3). Every mutating
// command goes through confirmWrite (interactive confirm or --yes) and
// guardNotCatalog (`__SYS/CATALOG` is never a write target — mutating
// the relational layer's own bookkeeping from frl would corrupt the
// cluster for real relational clients).

// confirmWrite gates a mutation: --yes skips the prompt; otherwise an
// interactive terminal gets a y/N prompt. Non-interactive stdin without
// --yes is a hard error — a script must opt in explicitly, and a CLI
// must never hang waiting for a TTY that isn't there.
func confirmWrite(cmd *cobra.Command, yes bool, action string) error {
	if yes {
		return nil
	}
	if !isTerminalReader(cmd.InOrStdin()) {
		return fmt.Errorf("refusing to %s without --yes (stdin is not a terminal, cannot prompt)", action)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "About to %s. Type y to confirm: ", action)
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
		return fmt.Errorf("aborted — %s not confirmed", action)
	}
	return nil
}

// isTerminalReader mirrors isTerminalWriter for stdin.
func isTerminalReader(r any) bool {
	type fdHolder interface{ Fd() uintptr }
	if f, ok := r.(fdHolder); ok {
		return term.IsTerminal(f.Fd())
	}
	return false
}

// guardNotCatalog rejects writes whose subspace overlaps the relational
// catalog at ("__SYS", "__SYS", "CATALOG") — in either direction (a
// target inside the catalog, or one that contains it, like a truncate
// of ("__SYS",)).
func guardNotCatalog(ss subspace.Subspace) error {
	catalogBytes := relationalKeyspace().CatalogSubspace().Bytes()
	target := ss.Bytes()
	if bytes.HasPrefix(target, catalogBytes) || bytes.HasPrefix(catalogBytes, target) {
		return fmt.Errorf("refusing to write: target keyspace overlaps `__SYS/CATALOG` — the relational catalog is never a write target for frl (evolve schemas through SQL DDL)")
	}
	return nil
}
