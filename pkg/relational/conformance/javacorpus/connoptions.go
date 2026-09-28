package javacorpus

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/javayamsql"
	"fdb.dev/pkg/relational/core/embedded"
)

// Java's yaml framework gives every connection the file's preamble
// connection_options (YamlExecutionContext.setConnectionOptions, read back by
// the YamlConnectionFactoryWithOptions every block connects through), and
// sets a setup block's or test block's own connection_options on top, entry
// by entry, through RelationalConnection.setOption (SetupBlock.java:128-131,
// TestBlock.java:534-538). The options are live: Java's
// EmbeddedRelationalConnection.setOption updates the database's options and
// RecordLayerDatabase.loadStore builds each store from them.
//
// Here each block pins a connection and installs the whole layered set on it,
// preamble then block. It is installed whole rather than added to, because a
// pooled connection keeps its options across checkouts (ResetSession resets
// the schema and the transaction, not the options), and a block's options must
// not leak into the next block's connection. The base is the empty set: the
// runner's DSN carries no connection options of its own, so the connector
// installed none.

// connectionOptions layers the preamble's entries, then each further layer's,
// into one option set.
func (r *runner) connectionOptions(layers ...[]javayamsql.Entry) (*api.Options, error) {
	opts := api.NoOptions()
	for _, layer := range append([][]javayamsql.Entry{r.fileConnOptions}, layers...) {
		for _, e := range layer {
			key, _ := e.Key.AsString() // the parser admits string keys only
			name := api.OptionName(key)
			v, err := r.optionValue(name, e.Val)
			if err != nil {
				return nil, fmt.Errorf("connection_options %s (line %d): %w", key, e.Key.Line, err)
			}
			opts = opts.With(name, v)
		}
	}
	return opts, nil
}

// optionValue is TestBlockOptions.parseConnectionOptions (TestBlock.java:264-282)
// for one entry: a YAML string goes through the option's string conversion,
// anything else is taken as YAML typed it (a list is an ordered collection of
// strings, null is null). A value Go cannot type the way Java's contract would
// is refused, never passed through as some other type the reader would ignore.
func (r *runner) optionValue(name api.OptionName, v *javayamsql.Value) (any, error) {
	switch v.Kind {
	case javayamsql.KindNull:
		return nil, nil
	case javayamsql.KindBool:
		return v.Bool, nil
	case javayamsql.KindInt:
		return v.Int, nil
	case javayamsql.KindSeq:
		out := make([]string, 0, len(v.Seq))
		for _, el := range v.Seq {
			if el.Kind != javayamsql.KindString {
				return nil, fmt.Errorf("a list element of kind %s; only strings are supported", el.Kind)
			}
			out = append(out, el.Str)
		}
		return out, nil
	case javayamsql.KindString:
		switch api.DefaultOptionValues()[name].(type) {
		case bool:
			switch strings.ToLower(v.Str) {
			case "true":
				return true, nil
			case "false":
				return false, nil
			}
			return nil, fmt.Errorf("%q is not a boolean", v.Str)
		case int, int64:
			n, err := strconv.ParseInt(v.Str, 10, 64)
			if err != nil {
				return nil, err
			}
			return n, nil
		case []string:
			return nil, fmt.Errorf("a collection option spelled as the string %q is not supported", v.Str)
		}
		if name == api.OptEncryptionKeyStore && r.cfg.WorkingDir != "" && !filepath.IsAbs(v.Str) {
			// A key store file named relative to the JVM's working directory,
			// which Java's yaml-tests run in (the module directory); the
			// corpus names `src/test/resources/serialization-keys.p12`.
			return filepath.Join(r.cfg.WorkingDir, v.Str), nil
		}
		return v.Str, nil
	}
	return nil, fmt.Errorf("a value of kind %s is not supported", v.Kind)
}

// execPinned runs a best-effort cleanup statement on a connection pinned to
// opts, so it does not run under whichever options a block last installed on
// a pooled connection. Its failure is not reported, as a cleanup's is not.
func execPinned(db *sql.DB, opts *api.Options, stmt string) {
	ctx := context.Background()
	conn, err := pin(ctx, db, opts)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.ExecContext(ctx, stmt)
}

// pin checks a connection out of db and installs opts on it. The caller closes it.
func pin(ctx context.Context, db *sql.DB, opts *api.Options) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	err = conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver conn is %T, want *embedded.EmbeddedConnection", dc)
		}
		ec.SetOptions(opts)
		return nil
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
