package cmd

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
)

// Store-level writes (RFC-174 §3.3): lock / unlock / truncate. Truncate
// is the most destructive command in the CLI and is double-gated: --yes
// is always required, and an interactive terminal additionally asks for
// the store address to be typed back.

// armFullStoreLockBypass pre-reads the store header and, when the store
// is FULL_STORE locked, arms the target with the stored reason so the
// subsequent open succeeds — Java's recovery path: open with
// setBypassFullStoreLockReason(<stored reason>), then set/clear the
// state. Only `store lock`/`store unlock` use this (managing the lock IS
// their job); everything else, truncate included, keeps refusing on a
// fully-locked store until it is unlocked.
func armFullStoreLockBypass(ctx context.Context, target *storeTarget, ss subspace.Subspace) error {
	db, err := openDatabase(target.clusterFile())
	if err != nil {
		return err
	}
	rec := recordlayer.NewFDBDatabase(db)
	info, err := readStoreInfo(ctx, rec, ss)
	if err != nil {
		return err
	}
	if ls := info.GetStoreLockState(); ls.GetLockState() == gen.DataStoreInfo_StoreLockState_FULL_STORE {
		reason := ls.GetReason()
		target.bypassFullStoreLock = &reason
	}
	return nil
}

func newStoreLockCmd() *cobra.Command {
	var (
		addr   storeAddressFlags
		yes    bool
		reason string
	)
	c := &cobra.Command{
		Use:   "lock <forbid-record-update|full-store> [--reason text]",
		Short: "Set the store's lock state (write)",
		Example: `  frl store lock forbid-record-update --reason "migration window" --yes
  frl store lock full-store --yes`,
		Long: "Writes the StoreLockState into the store header. " +
			"`forbid-record-update` rejects record writes while allowing " +
			"store maintenance; `full-store` locks the whole store. " +
			"`frl store info` shows the state; `frl store unlock` clears it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var state gen.DataStoreInfo_StoreLockState_State
			switch args[0] {
			case "forbid-record-update":
				state = gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			case "full-store":
				state = gen.DataStoreInfo_StoreLockState_FULL_STORE
			default:
				return fmt.Errorf("unknown lock state %q — want forbid-record-update or full-store", args[0])
			}
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}
			if err := confirmWrite(cmd, yes, fmt.Sprintf("lock store %s (%s)", target.describe(), args[0])); err != nil {
				return err
			}
			// A store that is ALREADY full-store locked must stay
			// manageable (change the reason, tighten forbid→full).
			if err := armFullStoreLockBypass(cmd.Context(), target, ss); err != nil {
				return err
			}
			if _, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (struct{}, error) {
					return struct{}{}, store.SetStoreLockState(state, reason)
				}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "locked %s (%s)\n", target.describe(), args[0])
			return nil
		},
	}
	addr.register(c, true)
	c.Flags().StringVar(&reason, "reason", "", "human-readable reason shown by store info")
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return c
}

func newStoreUnlockCmd() *cobra.Command {
	var (
		addr storeAddressFlags
		yes  bool
	)
	c := &cobra.Command{
		Use:     "unlock",
		Short:   "Clear the store's lock state (write)",
		Example: `  frl store unlock --yes`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}
			if err := confirmWrite(cmd, yes, fmt.Sprintf("unlock store %s", target.describe())); err != nil {
				return err
			}
			// Without this, a full-store lock would be permanent: Open()
			// rejects the locked store, so the unlock could never run
			// (codex P1).
			if err := armFullStoreLockBypass(cmd.Context(), target, ss); err != nil {
				return err
			}
			if _, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (struct{}, error) {
					return struct{}{}, store.ClearStoreLockState()
				}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unlocked %s\n", target.describe())
			return nil
		},
	}
	addr.register(c, true)
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation")
	return c
}

func newStoreTruncateCmd() *cobra.Command {
	var (
		addr storeAddressFlags
		yes  bool
	)
	c := &cobra.Command{
		Use:   "truncate",
		Short: "Delete EVERY record in the store (write, destructive)",
		Example: `  frl store truncate --yes            # non-interactive (scripts)
  frl store truncate --yes            # a terminal additionally asks you to type the address back`,
		Long: "Deletes all records, index entries, and version stamps in the " +
			"store (the header survives). The most destructive command in " +
			"frl — double-gated: --yes is ALWAYS required, and when run on a " +
			"terminal you must additionally type the store address back.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			ss, err := target.subspace()
			if err != nil {
				return err
			}
			if err := guardNotCatalog(ss); err != nil {
				return err
			}
			// Gate 1: --yes, unconditionally — even on a terminal.
			if !yes {
				return fmt.Errorf("refusing to truncate %s without --yes", target.describe())
			}
			// Gate 2: on a terminal, type the address back. Scripts
			// (non-TTY stdin) rely on gate 1 alone. Read the whole line —
			// Fscanln stops at whitespace, which would make an address
			// containing a space (a quoted keyspace-tuple element)
			// impossible to confirm.
			if isTerminalReader(cmd.InOrStdin()) {
				fmt.Fprintf(cmd.ErrOrStderr(), "This DELETES EVERY RECORD in %s. Type the store address to confirm: ", target.describe())
				typed, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && typed == "" {
					return fmt.Errorf("read confirmation: %w", err)
				}
				if strings.TrimSpace(typed) != target.describe() {
					return fmt.Errorf("aborted — %q does not match %q", strings.TrimSpace(typed), target.describe())
				}
			}
			if _, err := withStore(cmd.Context(), target,
				func(store *recordlayer.FDBRecordStore) (struct{}, error) {
					return struct{}{}, store.DeleteAllRecords()
				}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "truncated %s\n", target.describe())
			return nil
		},
	}
	addr.register(c, true)
	c.Flags().BoolVar(&yes, "yes", false, "required — this command never runs without it")
	return c
}
