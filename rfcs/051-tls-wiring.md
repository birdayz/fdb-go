# RFC-051: Wire TLS through the cluster/dial path (C++-aligned)

**Status:** Implemented (FDB C++ dev ACK, Torvalds ACK, bradfitz ACK) — landed on `fix/client-11-tls-wiring`
**Item:** RFC-010 audit #11 (MEDIUM)
**Scope:** `pkg/fdbgo/client` cluster-string + dial path, `pkg/fdbgo/transport` TLS

## Problem

The README advertises "TLS support (mutual auth + CA cert)", and the transport
layer *has* the machinery (`DialWithTLS`, `TLSConfig`, `upgradeTLS` — mutual auth
+ CA, all present). But it is **unreachable from the normal open path**:

- `database.getOrDialConn` hard-codes `transport.DialWith(dialCtx, addr, false, …)`
  — `useTLS=false`, no `TLSConfig`, on **every** connection. A cluster can never
  actually use TLS.
- `ParseClusterString` doesn't parse the FDB `:tls` coordinator suffix; an address
  like `127.0.0.1:4500:tls` fails `net.SplitHostPort` and is rejected outright.
- `database` has no `useTLS` / `tlsConfig` fields to thread.
- **Unsafe fallback:** `DialWithTLS(useTLS=true, tlsCfg=nil)` takes
  `if useTLS && tlsCfg != nil` (`conn.go:198`) — with a nil config it **skips the
  TLS upgrade and speaks plaintext** on a connection the caller asked to be TLS.
  Silent downgrade — the worst failure mode for a security feature.

So the README claim is false on the real path, and the one way to ask for TLS
(`Dial(addr, true)`) silently downgrades.

## Investigation — the C++ TLS model (the spec)

Read from `/tmp/fdbsrc` (FDB 7.3):

- **`:tls` suffix** (`flow/network.cpp` `NetworkAddress::parse`): after stripping a
  trailing `(fromHostname)`, an address ending in `:tls` sets `FLAG_TLS` (=2) in
  `NetworkAddress.flags`; `isTLS()` reads it. `parseList` splits coordinators on
  `,` and parses each independently. The flag is serialized on the wire, so a TLS
  cluster's proxy/storage addresses also carry it. `toString()` re-appends `:tls`.
- **A real cluster is uniformly TLS** — every member shares one transport config;
  there is no mixed-TLS deployment. The `:tls` on the coordinators in the cluster
  file is how the client learns to use TLS.
- **TLS config resolution** (`flow/TLSConfig.actor.cpp`), per field, first hit wins:
  - cert: explicit → `FDB_TLS_CERTIFICATE_FILE` → `<config_dir>/cert.pem` if it exists → none
  - key:  explicit → `FDB_TLS_KEY_FILE` → `<config_dir>/key.pem` if it exists → none
  - CA:   explicit → `FDB_TLS_CA_FILE` → none
  - also `FDB_TLS_PASSWORD`, `FDB_TLS_VERIFY_PEERS` (`<config_dir>` = `/etc/foundationdb` on Linux)
  - A cert/key is **not** mandatory — server-auth-only TLS (CA, no client cert) is valid.

The transport's `upgradeTLS` already does the actual TLS; the gaps are the
*config sourcing*, the *wiring*, and — per the bradfitz review — an idiomatic
user-facing surface (`*tls.Config`, not a bespoke file-path struct).

## Fix (C++-aligned internals, Go-idiomatic surface)

The user-facing API is built on the standard `*crypto/tls.Config` (bradfitz
review — see "API design" below); the FDB env/cluster-file resolution is a
convenience layer that *produces* one.

1. **Parse `:tls` in `ParseClusterString`** faithfully to `NetworkAddress::parse`
   (`flow/network.cpp:151`): strip a trailing `(fromHostname)`, then a trailing
   `:tls` **only when the string is longer than 4 chars** (the C++ `f.size() > 4`
   guard — a bare `":tls"` is not a TLS flag, it's an invalid address), per
   coordinator; validate the remaining `host:port` (incl. IPv6 `[ip]:port`). Add
   `ClusterFile.UseTLS bool`. Require the coordinators to be **uniform** (all `:tls`
   or none) — mixed is not a real deployment and a database-level flag can't
   represent it; reject mixed with a clear error.
2. **`resolveTLSConfig(configDir) (*tls.Config, error)`** — the FDB env convenience
   layer, **invoked only for a TLS cluster** (so a non-TLS open never `os.Stat`s
   `/etc/foundationdb`). Per-field: `FDB_TLS_{CERTIFICATE,KEY,CA}_FILE` env →
   `<configDir>/{cert,key}.pem` if present (CA has no default; `configDir` is
   passed in, not a mutable global). It **loads** those files into a standard
   `*tls.Config` (`tls.LoadX509KeyPair`→`Certificates`, CA PEM→`RootCAs`). Returns a
   non-nil config (possibly empty / CA-only — all valid); **errors** if a file that
   *is* configured can't be loaded (don't run with half a config). `FDB_TLS_VERIFY_PEERS`
   rule DSL and `FDB_TLS_PASSWORD` (encrypted keys) are deferred — see Follow-ups
   (Go's standard CA+SNI verification is exactly the C++ `Check.Valid=1` default).
3. **Thread a `*tls.Config`:** `database` gains `tlsConfig *tls.Config` (no parallel
   `useTLS` bool — nil-ness IS the switch). `getOrDialConn` dials
   `transport.Dial(dialCtx, addr, db.tlsConfig, db.dialFn)`.
4. **`transport.Dial(ctx, addr, tlsConfig *tls.Config, dialFn)`** is the single
   dialer: `tlsConfig != nil` → TLS, `nil` → plaintext. This **deletes the silent
   downgrade by construction** — there is no `useTLS` bool to disagree with the
   config, so a connection the caller wanted encrypted can never go plaintext. (The
   old `Dial`/`DialWith`/`DialWithTLS` overloads + `transport.TLSConfig` file-path
   struct are removed.) An empty config still attempts a real handshake and fails
   closed. `upgradeTLS` now takes a `*tls.Config`, **clones** it, and sets
   `ServerName` (dialed host, for SNI) / `MinVersion` (TLS 1.2) **only if the caller
   left them unset** — everything else is the caller's to control.
5. **README** stays accurate: TLS is reachable via `:tls` + `FDB_TLS_*` (drop-in
   C++ compat) **or** `WithTLSConfig(*tls.Config)`.

### API design (bradfitz review)

The user-facing surface is functional options on `OpenDatabase`, the Go idiom:
```go
func OpenDatabase(clusterFile string, opts ...Option) (Database, error)
func WithTLSConfig(*tls.Config) Option // in-memory certs, GetClientCertificate, VerifyPeerCertificate, …
func WithDialFunc(DialFunc) Option     // also retires OpenDatabaseFromConfig's old positional nil dialFn
```
Precedence: `WithTLSConfig` (explicit, wins, and enables TLS even without `:tls`)
→ `FDB_TLS_*` env (when the cluster string is `:tls`) → default config dir. Options
compose and are forward-compatible. `fdb.WithTLSConfig`/`fdb.WithDialFunc` re-export
the `client` constructors so users stay in one package; `Option` is a type alias.

## Performance

TLS only engages for TLS clusters; non-TLS dials are unchanged (`tlsConfig == nil`,
no handshake). `resolveTLSConfig` runs once at database open (a few `os.Stat`/getenv
+ file loads). No hot-path impact.

## Test plan

- **Parse:** `ParseClusterString` table tests — `:tls` set/stripped, `(fromHostname)`
  stripped before `:tls`, IPv6 `[::1]:4500:tls`, mixed-TLS rejected, bare `:tls`
  rejected, non-TLS unchanged. Round-trips against the C++ `NetworkAddress::parse` rules.
- **Config resolution:** `resolveTLSPath` precedence (env wins / default-dir-if-exists
  / empty) and `resolveTLSConfig` loading real generated CA + cert/key into a
  `*tls.Config` (RootCAs + Certificates), incl. error paths (cert without key,
  unreadable CA) and the empty-non-nil case.
- **Real TLS handshake (not a mock):** a `crypto/tls` server on an in-process
  listener with a generated CA + server cert (client-cert-required for mutual auth)
  speaks the FDB ConnectPacket handshake *inside* the TLS tunnel; assert
  `Dial(ctx, addr, cfg, nil)` with a standard `*tls.Config` negotiates TLS and
  returns a working `Conn` (TLS is a transparent wrapper, so this is the exact
  production dial path). Negatives: wrong CA → fails; missing client cert when the
  server requires it → fails (proves mutual auth is enforced, not just advertised).
- **No silent-downgrade test needed:** the failure mode is gone by construction —
  TLS is on iff the config is non-nil; there is no boolean to disagree with it.
- A full **FDB-TLS testcontainer** e2e needs TLS-listen + cert-mount support the
  container helper lacks today; scoped as a follow-up. The real-TLS handshake test
  above proves the wiring; `upgradeTLS` is unchanged.
- `just test` (48 targets) green, `-race`.

## Follow-ups (out of scope, documented)

- **Per-address TLS flag.** C++ carries `FLAG_TLS` on every `NetworkAddress`, so a
  server advertising a dual-listen *secondary* address with a different TLS mode
  (`flow/Net2.actor.cpp`) is honored per-connection. Go's `ProxyInfo.Address` is a
  bare `host:port` string and we apply the database-level `useTLS` uniformly — fine
  for real (uniform) clusters, but a dual-listen non-TLS secondary would be dialed
  with TLS. Closing this needs per-address flags threaded from the wire
  `NetworkAddress.flags` bit through `ProxyInfo`/`connPool`.
- **`FDB_TLS_VERIFY_PEERS` rule DSL** (`Check.Unexpired`, `S.CN=…`, etc.) — Go uses
  standard CA + SNI verification (the C++ `Check.Valid=1` default).
- **TLS network options** (`FDB_NET_OPTION_TLS_CERT_BYTES`, …) — env + explicit
  `TLSConfig` only for now.
- **`FDB_TLS_PASSWORD` / encrypted private keys** — Go's `tls.LoadX509KeyPair`
  takes unencrypted PEM; encrypted-key decryption (deprecated `x509.DecryptPEMBlock`
  / PKCS#8) is the uncommon case and deferred. Unencrypted keys (the norm) work.
