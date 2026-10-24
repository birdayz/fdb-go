# RFC-277: TLS verification as libfdb_c 7.3.77

**Status:** Draft

**Items** (the Go FDB client entries of the 2026-10-24 quality review, open PR #994):

1. `ServerName` is set to the dialed host (`transport/conn.go:1533-1535`), a hostname
   check libfdb_c does not make.
2. `FDB_TLS_VERIFY_PEERS` and `FDB_TLS_PASSWORD` are never read.
3. Match the C++ verify-peers semantics (`flow/TLSConfig.actor.cpp`) as far as Go's
   `crypto/tls` allows, and record the rest in DIVERGENCES.md.

## Problem

`client.resolveTLSConfig` (`client/tls.go`) builds a `*tls.Config` from
`FDB_TLS_CERTIFICATE_FILE`, `FDB_TLS_KEY_FILE` and `FDB_TLS_CA_FILE`, and
`transport.upgradeTLS` fills `ServerName` with the dialed host. Go therefore verifies
the server against the system roots when no CA file is set, and checks the host name
against the certificate's SANs. Neither happens in libfdb_c. Go ignores
`FDB_TLS_VERIFY_PEERS` and `FDB_TLS_PASSWORD`, so it cannot express a verify-peers
policy and cannot load an encrypted key. A set but empty path variable is treated as
unset, where C++ treats it as set.

## C++ (7.3.77)

**Configuration** (`flow/TLSConfig.actor.cpp`). These settings come from the network
option first and the environment second.
- Paths (`:168-212`): the network option, else `FDB_TLS_CERTIFICATE_FILE`,
  `FDB_TLS_KEY_FILE` or `FDB_TLS_CA_FILE`. A variable that is set but empty counts as
  set (`platform::getEnvironmentVar`, `Platform.actor.cpp:3092-3099`). Otherwise
  `/etc/foundationdb/{cert,key}.pem` if present. The CA has no default.
- Password (`:67-75`): the option, else `FDB_TLS_PASSWORD`, else empty.
- Verify-peers (`:54-65`): the option, else `FDB_TLS_VERIFY_PEERS`, else
  `{"Check.Valid=1"}`.

**Context** (`ConfigureSSLContext`, `:108-142`).
- The client sets `verify_peer|verify_fail_if_no_peer_cert`.
- The password is OpenSSL's password callback.
- The CA store holds only the configured CA: `add_certificate_authority` runs only when
  CA bytes exist, and nothing calls `set_default_verify_paths`.
- Nothing sets a host name: no `set1_host`, no `SSL_set_tlsext_host_name` (no SNI). A
  grep of `flow/`, `fdbrpc/` and `fdbclient/` finds none.

**Verify-peers** (`set_verify_peers` `:503-526`, `Rule::Rule` `:528-611`):
- Rules are split on `|`, and any rule that matches accepts the peer.
- A rule is comma-separated `term[<>]?=value` items: `=` is exact, `>=` prefix and `<=`
  suffix.
- `Check.Valid` and `Check.Unexpired` take exactly `0` or `1`.
- Other terms take an optional scope: `S.`/`Subject.` (the default), `I.`/`Issuer.` or
  `R.`/`Root.`. The field must be one of `C, CN, L, ST, O, OU, UID, DC, subjectAltName`
  (`abbrevToNID`, `:478-488`).
- Values are unescaped by `de4514` (`:384-468`), an RFC 4514 subset.
- A parse error clears the rules and throws `tls_error`.

**Matching** (`:613-790`):
- A DN field must occur exactly once in the name. A `subjectAltName` value is
  `GEN:val`, with `GEN` one of `EMAIL`, `DNS`, `URI` or `IP`.
- Both sides are compared as UTF-8 bytes; the criteria bytes are widened as Latin-1
  (an IA5 string through `ASN1_STRING_to_UTF8`).
- The `switch` in `match_extension_criteria` falls through: an email SAN is also tested
  against a `DNS:` criterion, and a URI SAN against an `IP:` criterion. An `IP:`
  criterion never matches an iPAddress SAN, an OCTET STRING that `ASN1_STRING_to_UTF8`
  rejects.
- `S.` checks the leaf, `I.` the leaf's issuer name (`I.subjectAltName` the leaf's own
  SAN) and `R.` the last certificate of the built chain.

**Verification** (`verify_peer`, `:792-834`):
- If any rule has `Check.Valid=0`, every peer is accepted.
- Otherwise the chain must verify, and then one rule must match.
- `Check.Unexpired` is parsed but never read: an expired certificate still fails.

## Change

1. **Policy** (`transport/tls_policy.go`). `transport.ParseVerifyPeers(s)` parses the
   C++ grammar into a `*VerifyPolicy`, including `de4514` unescaping, the field
   whitelist and the duplicate rule. It fails with a typed `*VerifyPeersError`.
   `(*VerifyPolicy).Verify(certs, roots)` does two things:
   - it builds the chain with `x509.Verify` (roots as given, the peer's other
     certificates as intermediates, `ExtKeyUsageServerAuth`, OpenSSL's `ssl_server`
     purpose);
   - it then applies the rules as `verify_peer` and `check_verify` do, including the
     `Check.Valid=0` bypass and the SAN fall-through.

   DN fields come from `Subject.Names` and `Issuer.Names` by OID. SAN entries come from
   the raw extension, so a URI is compared as written. A failure is a typed
   `*PeerVerifyError`.
2. **Environment** (`client/tls.go`):
   - `resolveTLSConfig` reads `FDB_TLS_VERIFY_PEERS` (default `Check.Valid=1`) and
     `FDB_TLS_PASSWORD`. Path variables use `LookupEnv`, so a set but empty variable
     means no file and no default.
   - The config trusts only the CA file: an empty pool when none is set.
   - It sets `InsecureSkipVerify` (which only turns off the stdlib's own check) and a
     `VerifyConnection` that runs the policy. It sets no `ServerName`, so no SNI is
     sent.
   - An encrypted PKCS#8 key (`ENCRYPTED PRIVATE KEY`, PBES2 with PBKDF2 and AES-CBC or
     DES-EDE3-CBC, the schemes `openssl pkcs8 -topk8 -v2` writes) is decrypted with
     the password. An unencrypted key ignores the password, as OpenSSL does.
   - A configuration error is a typed `*TLSConfigError` carrying `tls_error` (2107).
3. **Transport** (`transport/conn.go`). `upgradeTLS` no longer sets `ServerName`. A
   caller's config (`WithTLSConfig`) that sets neither `ServerName` nor
   `InsecureSkipVerify` gets the `Check.Valid=1` policy against its own `RootCAs`, run
   before its own `VerifyConnection`. That is a chain check without a host-name check.
   A config with `ServerName` keeps Go's host-name verification, as an explicit opt-in.
4. **Docs.**
   - DIVERGENCES.md records what Go cannot or does not reproduce (below).
   - `rfcs/051-tls-wiring.md` and `docs/mt-saas.md` drop the claim that Go's SNI and
     host-name check are the C++ default.
   - CHANGELOG.md records the stricter roots and the dropped host-name check.

## Divergences kept (DIVERGENCES.md)

- A bad configuration (an unreadable file, a wrong password, a verify-peers parse
  error) fails the database open with `tls_error`. libfdb_c logs `Net2TLSInitError`
  and then fails every TLS connection (`Net2.actor.cpp:1405-1419`).
- Keys:
  - a legacy RFC 1423 encrypted PEM key (`Proc-Type: 4,ENCRYPTED`), PBES1, and scrypt
    PBES2 keys are refused with `tls_error`; OpenSSL loads them;
  - a certificate whose key does not match is refused by `crypto/tls`, where OpenSSL
    drops the key.
- Chains:
  - `x509.Verify` may return several chains. Go accepts the peer when a rule matches
    any one of them; OpenSSL builds a single chain.
  - Go accepts a non-self-signed CA in the pool as an anchor, rejects SHA-1
    signatures, and does not check OpenSSL's keyUsage and nsCertType purpose bits.
- Grammar: an escaped `\|` hangs libfdb_c's parser (`:509-516`); Go refuses it as a
  parse error.
- Not added: the `TLS_*` network options, `FDB_NETWORK_OPTION_TLS_*` variables, the
  12-hour certificate reload, and the 2 s handshake timeout. Go keeps its minimum of
  TLS 1.2.

## Wire compatibility

No bytes change. The handshake no longer carries SNI, as with libfdb_c.

## Tests

Regressions (each fails when its part is reverted):

- `transport`: verify-peers parsing over the grammar: operators, scopes, the field
  whitelist, `de4514` escapes, `|`, empty rules, `Check.*` values, and the errors C++
  throws. Matching over certificates the test builds:
  - DN exact, prefix and suffix, and a field present twice;
  - issuer and root scopes, and `I.subjectAltName`;
  - SAN `DNS`, `EMAIL`, `URI` and the fall-throughs;
  - the `Check.Valid=0` bypass, an unknown CA, and an expired leaf with
    `Check.Unexpired=0`.
- `transport`: a handshake against a server whose certificate names another host
  succeeds when the chain is valid. Before the change it fails the host-name check.
- `client`:
  - `FDB_TLS_VERIFY_PEERS` is applied: a policy the server certificate does not meet
    fails the handshake;
  - `FDB_TLS_PASSWORD` decrypts an encrypted PKCS#8 key, and a wrong one is a
    `tls_error`;
  - with no CA file the system roots are not trusted;
  - an empty `FDB_TLS_CERTIFICATE_FILE` suppresses the default file.

Non-regression: the existing mutual-TLS, wrong-CA and missing-client-cert handshakes,
and the `:tls` cluster-file and `WithTLSConfig` precedence tests.
