package sqldriver

// DSN plumbing for RESTRICT_DDL_TO_SESSION_DATABASE. No FDB needed — this pins
// the parameter name, the boolean spellings, and above all that a malformed
// value ERRORS rather than silently reading as "off". A security flag that
// degrades to off on a typo is worse than no flag.

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestDSNRestrictDDLOption(t *testing.T) {
	t.Parallel()

	t.Run("absent_leaves_option_unset", func(t *testing.T) {
		t.Parallel()
		dsn, err := ParseDSN("fdbsql:///db")
		if err != nil {
			t.Fatalf("ParseDSN: %v", err)
		}
		opts, err := dsn.ConnectionOptions()
		if err != nil {
			t.Fatalf("ConnectionOptions: %v", err)
		}
		if v := opts.Get(api.OptRestrictDDLToSessionDatabase); v != nil {
			t.Fatalf("absent parameter set the option to %v, want unset", v)
		}
		// Unset must read as false — this is the Java-parity default and the
		// reason the option is deliberately absent from defaultOptionValues.
		if _, present := api.DefaultOptionValues()[api.OptRestrictDDLToSessionDatabase]; present {
			t.Fatal("RESTRICT_DDL_TO_SESSION_DATABASE must not be in defaultOptionValues: " +
				"that map is wire-visible and must stay byte-identical to Java's")
		}
	})

	truthy := []string{"true", "TRUE", "1", "t", "yes", "on", ""}
	for _, raw := range truthy {
		t.Run("true_"+raw, func(t *testing.T) {
			t.Parallel()
			dsn, err := ParseDSN("fdbsql:///db?restrict_ddl_to_session_database=" + raw)
			if err != nil {
				t.Fatalf("ParseDSN: %v", err)
			}
			opts, err := dsn.ConnectionOptions()
			if err != nil {
				t.Fatalf("ConnectionOptions: %v", err)
			}
			if opts.Get(api.OptRestrictDDLToSessionDatabase) != true {
				t.Fatalf("%q did not enable the option", raw)
			}
		})
	}

	falsy := []string{"false", "FALSE", "0", "f", "no", "off"}
	for _, raw := range falsy {
		t.Run("false_"+raw, func(t *testing.T) {
			t.Parallel()
			dsn, err := ParseDSN("fdbsql:///db?restrict_ddl_to_session_database=" + raw)
			if err != nil {
				t.Fatalf("ParseDSN: %v", err)
			}
			opts, err := dsn.ConnectionOptions()
			if err != nil {
				t.Fatalf("ConnectionOptions: %v", err)
			}
			if opts.Get(api.OptRestrictDDLToSessionDatabase) != false {
				t.Fatalf("%q did not disable the option", raw)
			}
		})
	}

	t.Run("malformed_value_errors", func(t *testing.T) {
		t.Parallel()
		dsn, err := ParseDSN("fdbsql:///db?restrict_ddl_to_session_database=ture")
		if err != nil {
			t.Fatalf("ParseDSN: %v", err)
		}
		_, err = dsn.ConnectionOptions()
		if err == nil {
			t.Fatal("a misspelled boolean must error, not silently read as off")
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter {
			t.Fatalf("want ErrCodeInvalidParameter, got %v", err)
		}
	})
}
