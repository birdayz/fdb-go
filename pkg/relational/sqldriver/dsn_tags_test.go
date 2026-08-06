package sqldriver

// The transaction_tags DSN parameter: what a multi-tenant service sets per
// connection so the cluster's ratekeeper can throttle one tenant without
// starving the others.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func tagsFromDSN(t *testing.T, dsn string) ([]string, error) {
	t.Helper()
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	opts, err := d.ConnectionOptions()
	if err != nil {
		return nil, err
	}
	v := opts.Get(api.OptTransactionTags)
	if v == nil {
		return nil, nil
	}
	tags, ok := v.([]string)
	if !ok {
		t.Fatalf("option value is %T, want []string", v)
	}
	return tags, nil
}

func TestDSNTransactionTagsSingle(t *testing.T) {
	t.Parallel()
	tags, err := tagsFromDSN(t, "fdbsql:///db?transaction_tags=tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(tags, ","), "tenant-a"; got != want {
		t.Errorf("tags = %q, want %q", got, want)
	}
}

func TestDSNTransactionTagsMultipleAreSorted(t *testing.T) {
	t.Parallel()
	tags, err := tagsFromDSN(t, "fdbsql:///db?transaction_tags=gamma,alpha,beta")
	if err != nil {
		t.Fatal(err)
	}
	// Sorted so the commit request's tagSet bytes are stable for a given DSN.
	if got, want := strings.Join(tags, ","), "alpha,beta,gamma"; got != want {
		t.Errorf("tags = %q, want %q", got, want)
	}
}

// Absent parameter must leave the option unset, not set it to an empty slice —
// an unset option is what keeps the default option set identical to Java's.
func TestDSNTransactionTagsAbsent(t *testing.T) {
	t.Parallel()
	tags, err := tagsFromDSN(t, "fdbsql:///db")
	if err != nil {
		t.Fatal(err)
	}
	if tags != nil {
		t.Errorf("absent parameter must leave tags unset, got %v", tags)
	}
}

func TestDSNTransactionTagsTrailingCommaIsNotAnError(t *testing.T) {
	t.Parallel()
	tags, err := tagsFromDSN(t, "fdbsql:///db?transaction_tags=a,b,")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(tags, ","), "a,b"; got != want {
		t.Errorf("tags = %q, want %q", got, want)
	}
}

// The DSN must enforce the SAME limits as a programmatic RecordContextConfig,
// and must do it at parse time — a bad tag should fail when the pool is opened,
// not on some later request's first statement.
func TestDSNTransactionTagsRejectsTooMany(t *testing.T) {
	t.Parallel()
	_, err := tagsFromDSN(t, "fdbsql:///db?transaction_tags=a,b,c,d,e,f")
	if err == nil {
		t.Fatal("6 tags must be rejected at DSN parse time")
	}
	if !strings.Contains(err.Error(), "At most 5 tags allowed") {
		t.Errorf("error = %q, want it to carry Java's wording", err)
	}
	if !strings.Contains(err.Error(), TransactionTagsParam) {
		t.Errorf("error = %q, want it to name the parameter", err)
	}
}

func TestDSNTransactionTagsRejectsTooLong(t *testing.T) {
	t.Parallel()
	_, err := tagsFromDSN(t, "fdbsql:///db?transaction_tags="+strings.Repeat("x", 17))
	if err == nil {
		t.Fatal("17-character tag must be rejected at DSN parse time")
	}
	if !strings.Contains(err.Error(), "Tag must be 16 characters or shorter") {
		t.Errorf("error = %q, want it to carry Java's wording", err)
	}
}

// The freeze contract: connection options are decoded and VALIDATED at
// OpenConnector, so a malformed tag is a DSN error rather than a surprise on
// some later statement. The tests above stop at ConnectionOptions, and the
// end-to-end proof of this is FDB-gated — which means on a machine without
// Docker nothing pins that OpenConnector is actually on the validating path.
// Moving the validation out of ConnectionOptions, or calling it after the
// Connector is built, would leave every other test in this file green.
func TestOpenConnectorRejectsAnInvalidTagWithoutDocker(t *testing.T) {
	t.Parallel()
	d := &Driver{}

	if _, err := d.OpenConnector(
		"fdbsql:///db?transaction_tags=" + strings.Repeat("x", 17),
	); err == nil {
		t.Error("OpenConnector must reject an over-long tag, not defer it to Connect")
	} else if !strings.Contains(err.Error(), "Tag must be 16 characters or shorter") {
		t.Errorf("error = %q, want the record layer's wording", err)
	}

	if _, err := d.OpenConnector("fdbsql:///db?transaction_tags=a,b,c,d,e,f"); err == nil {
		t.Error("OpenConnector must reject a 6-tag set")
	} else if !strings.Contains(err.Error(), "At most 5 tags allowed") {
		t.Errorf("error = %q, want Java's wording", err)
	}

	// Control: a valid tag set must NOT make OpenConnector fail, or the
	// assertions above would pass for the wrong reason.
	if _, err := d.OpenConnector("fdbsql:///db?transaction_tags=tenant-a,bulk"); err != nil {
		t.Errorf("a valid tag set must open cleanly, got %v", err)
	}
}
