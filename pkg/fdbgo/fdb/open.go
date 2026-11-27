package fdb

import (
	"context"
	"crypto/tls"
	"log/slog"
	"math"
	"sync/atomic"
	"time"
)

const defaultAPIVersion = 730

// Config configures a FoundationDB database opened via [Open].
type Config struct {
	// ClusterFile is the path to the fdb.cluster file. When empty, the
	// standard lookup order is used (FDB_CLUSTER_FILE, ./fdb.cluster, then
	// the platform default).
	ClusterFile string

	// APIVersion selects the FoundationDB API version (510-730). When 0, it
	// reuses the already-selected process API version if set, falling back to
	// 730 when unset. Selecting an API version is process-wide and permanent
	// (even if connecting fails); once set, selecting a different version fails
	// with api_version_already_set (2201).
	APIVersion int

	// TLSConfig enables TLS for cluster connections using the provided
	// configuration (equivalent to [WithTLSConfig]).
	TLSConfig *tls.Config

	// Logger sets the per-database structured logger for operational events
	// (equivalent to [WithLogger]). When nil, slog.Default() is used.
	Logger *slog.Logger

	// Tracer sets the OpenTelemetry tracer for client spans (equivalent to
	// [WithTracer]). When nil, tracing export is disabled.
	Tracer Tracer

	// DialFunc overrides the dialer used for cluster connections (equivalent
	// to [WithDialFunc]).
	DialFunc DialFunc

	// TransactionTimeout sets the default per-transaction timeout applied to
	// transactions created from the opened Database. Zero leaves timeouts
	// disabled; negative values or durations exceeding math.MaxInt32
	// milliseconds fail with invalid_option_value (2006).
	TransactionTimeout time.Duration

	// TransactionRetryLimit sets the default maximum number of retries for
	// transactions created from the opened Database. Zero leaves the default
	// unlimited retry policy unchanged; -1 explicitly sets unlimited retries;
	// values less than -1 fail with invalid_option_value (2006).
	TransactionRetryLimit int64

	// Options supplies additional [Option] values appended after the typed
	// fields above.
	Options []Option
}

// Open selects the API version (reusing the already-selected version or
// defaulting to 730 when [Config.APIVersion] is 0), connects to the
// FoundationDB cluster using ctx to bound bootstrap, and applies any
// configured transaction defaults.
//
// The API version choice is process-wide and permanent, even if connecting
// fails. Callers should call [Database.Close] (typically via `defer db.Close()`)
// when finished with the returned Database.
func Open(ctx context.Context, cfg Config) (Database, error) {
	return openWithConfig(ctx, cfg, &apiVersion, openDatabaseWithContext)
}

func validateOpenConfig(cfg Config) (timeoutMS int64, err error) {
	if cfg.TransactionTimeout < 0 {
		return 0, Error{Code: 2006}
	}
	if cfg.TransactionTimeout > 0 {
		timeoutMS = cfg.TransactionTimeout.Milliseconds()
		if timeoutMS == 0 {
			timeoutMS = 1
		}
		if timeoutMS > math.MaxInt32 {
			return 0, Error{Code: 2006}
		}
	}
	if cfg.TransactionRetryLimit < -1 {
		return 0, Error{Code: 2006}
	}
	return timeoutMS, nil
}

func buildOpenOptions(cfg Config) []Option {
	opts := make([]Option, 0, 4+len(cfg.Options))
	if cfg.TLSConfig != nil {
		opts = append(opts, WithTLSConfig(cfg.TLSConfig))
	}
	if cfg.Logger != nil {
		opts = append(opts, WithLogger(cfg.Logger))
	}
	if cfg.Tracer != nil {
		opts = append(opts, WithTracer(cfg.Tracer))
	}
	if cfg.DialFunc != nil {
		opts = append(opts, WithDialFunc(cfg.DialFunc))
	}
	opts = append(opts, cfg.Options...)
	return opts
}

func openWithConfig(
	ctx context.Context,
	cfg Config,
	cur *atomic.Int32,
	opener func(context.Context, string, ...Option) (Database, error),
) (Database, error) {
	timeoutMS, err := validateOpenConfig(cfg)
	if err != nil {
		return Database{}, err
	}
	apiVer := cfg.APIVersion
	if apiVer == 0 {
		if existing := int(cur.Load()); existing != 0 {
			apiVer = existing
		} else {
			apiVer = defaultAPIVersion
		}
	}
	if err := selectAPIVersion(cur, apiVer); err != nil {
		if !(cfg.APIVersion == 0 && cur.Load() != 0) {
			return Database{}, err
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := opener(ctx, cfg.ClusterFile, buildOpenOptions(cfg)...)
	if err != nil {
		return Database{}, err
	}
	if timeoutMS > 0 {
		if err := db.Options().SetTransactionTimeout(timeoutMS); err != nil {
			_ = db.Close()
			return Database{}, err
		}
	}
	if cfg.TransactionRetryLimit != 0 {
		if err := db.Options().SetTransactionRetryLimit(cfg.TransactionRetryLimit); err != nil {
			_ = db.Close()
			return Database{}, err
		}
	}
	return db, nil
}
