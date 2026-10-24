# RFC-277: TLS verification as libfdb_c 7.3.77

**Status:** Implemented

**Items** (the Go FDB client entries of the 2026-10-24 quality review, open PR #994):

1. `ServerName` is set to the dialed host (`transport/conn.go:1533-1535`), a hostname
   check libfdb_c does not make.
2. `FDB_TLS_VERIFY_PEERS` and `FDB_TLS_PASSWORD` are never read.
3. Match the C++ verify-peers semantics (`flow/TLSConfig.actor.cpp`) as far as Go's
   `crypto/tls` allows, and record the rest in DIVERGENCES.md.

## Problem

`client.resolveTLSConfig` (`client/tls.go`) builds the TLS configuration a `:tls`
cluster file uses. It reads `FDB_TLS_CERTIFICATE_FILE`, `FDB_TLS_KEY_FILE` and
`FDB_TLS_CA_FILE`, and `transport.upgradeTLS` fills `ServerName` with the dialed host.
That configuration differs from libfdb_c's in four ways:
- it verifies the server against the system roots when no CA file is set;
- it checks the host name against the certificate's SANs, and sends SNI for a host-name
  address;
- it ignores `FDB_TLS_VERIFY_PEERS` and `FDB_TLS_PASSWORD`, so it can express no
  verify-peers policy and cannot load an encrypted key;
- it treats a path variable that is set but empty as unset.

## C++ (7.3.77)

**Configuration** (`flow/TLSConfig.actor.cpp`). These settings come from the network
option first and the environment second.
- Paths (`:168-212`): the option, else `FDB_TLS_CERTIFICATE_FILE`, `FDB_TLS_KEY_FILE` or
  `FDB_TLS_CA_FILE`. A variable that is set but empty counts as set
  (`platform::getEnvironmentVar`, `Platform.actor.cpp:3092-3099`), giving no file and
  no default. Otherwise `/etc/foundationdb/{cert,key}.pem` if present. The CA has no
  default.
- Password (`:67-75`): the option, else `FDB_TLS_PASSWORD`, else empty.
- Verify-peers (`:54-65`): the option, else `FDB_TLS_VERIFY_PEERS`, else
  `{"Check.Valid=1"}`.

**Context** (`ConfigureSSLContext`, `:108-142`).
- The client sets `verify_peer|verify_fail_if_no_peer_cert`.
- The password is OpenSSL's password callback, so encrypted PKCS#8 and legacy PEM keys
  load.
- The CA store holds only the configured CA: `add_certificate_authority` runs only when
  CA bytes exist.
- Nothing sets a host name. `grep -rnE
  'set1_host|tlsext_host_name|X509_VERIFY_PARAM_set1|set_default_verify_paths|host_name_verification|rfc2818_verification'
  flow fdbrpc fdbclient` finds no line. The positive control
  `grep -rnE 'set_verify_callback|set_verify_mode' flow fdbrpc fdbclient` finds five lines:
  `TLSConfig.actor.cpp:116` and `:149`, and `TLSTest.cpp:97`, `:157` and `:196`. So
  libfdb_c sends no SNI, verifies no host name and loads no system roots.

**Verify-peers parsing** (`set_verify_peers` `:503-526`, `Rule::Rule` `:528-611`):
- Rules are split on `|`, and any rule that matches accepts the peer. An empty piece is
  a rule with no criteria.
- A rule is comma-separated `term[<>]?=value` items: `=` is exact, `>=` prefix and `<=`
  suffix. A trailing `,` is allowed.
- `Check.Valid` and `Check.Unexpired` take exactly one character, `0` or `1`, and no
  `>=`/`<=`. Any other `Check.` term is an error.
- Other terms take an optional scope: `S.`/`Subject.` (the default), `I.`/`Issuer.` or
  `R.`/`Root.`. The field must be one of `C, CN, L, ST, O, OU, UID, DC, subjectAltName`
  (`abbrevToNID`, `:478-488`), case-sensitive.
- Values are unescaped by `de4514` (`:384-468`), an RFC 4514 subset:
  - a value is empty or starts with `#` or a space: error;
  - `\` before one of ``␠ " # + , ; < = > | \`` gives that character, and `\HH` a byte;
    any other escape ends the value, which is then an error;
  - an unescaped `" + , ; < >` or NUL ends the value;
  - trailing unescaped spaces are dropped, and the parse then fails because the next
    character is not `,`.
- Criteria are a `std::set` of (field, value, operator) (`TLSConfig.actor.h:247-249`):
  a repeated term with another value is a further requirement, and an identical repeat
  is ignored with a warning.
- A parse error clears the rules and throws `tls_error`. A `|` whose preceding byte is
  a backslash never advances the split loop (`:509-516`). The check runs before any
  unescaping, so it covers `\|` and an escaped backslash before a separator, `\\|`.

**Matching** (`:613-790`):
- Both sides are compared as UTF-8 bytes. The criterion is widened from Latin-1 (an IA5
  string through `ASN1_STRING_to_UTF8`).
- An empty criterion or entry never matches, whatever the operator (`:624-628`).
- A DN field must occur exactly once in its name.
- A `subjectAltName` value is `GEN:val`, with `GEN` one of `EMAIL`, `DNS`, `URI` or
  `IP`; a value without `:` never matches.
- The `switch` in `match_extension_criteria` falls through: an email SAN is also tested
  against a `DNS:` criterion, and a URI SAN against an `IP:` criterion. An `IP:`
  criterion never matches an iPAddress SAN, an OCTET STRING that `ASN1_STRING_to_UTF8`
  rejects.
- `S.` checks the leaf, `I.` the leaf's issuer name (`I.subjectAltName` the leaf's own
  SAN) and `R.` the last certificate of the built chain.

**Verification** (`verify_peer`, `:792-834`):
- If any rule has `Check.Valid=0`, every peer is accepted.
- Otherwise the chain must verify, under the `ssl_server` purpose OpenSSL sets for a
  client, and then one rule must match.
- `Check.Unexpired` is parsed but never read: an expired certificate still fails.

## Change

1. **Policy** (`transport/tls_policy.go`). `transport.ParseVerifyPeers(s)` parses the
   grammar above into a `*VerifyPolicy`, or fails with a typed `*VerifyPeersError`.
   - `(*VerifyPolicy).Verify(certs, roots)` builds the chain with `x509.Verify`: the
     given roots, the peer's other certificates as intermediates, and
     `ExtKeyUsageServerAuth`. Nil roots mean no roots, as libfdb_c with no CA, never
     the system pool.
   - It then applies the rules as `verify_peer` and `check_verify` do, including the
     `Check.Valid=0` bypass, the empty-string rule and the SAN fall-through.
   - DN fields come from `Subject.Names` and `Issuer.Names` by OID. SAN entries come
     from the raw extension, so a URI is compared as written.
   - A rejected peer is a typed `*PeerVerifyError`.

   `(*VerifyPolicy).VerifyConnection(roots)` returns the `tls.Config.VerifyConnection`
   hook, so a `WithTLSConfig` caller can opt into the same check (with
   `InsecureSkipVerify`). It ignores `ConnectionState.ServerName`.
2. **Environment** (`client/tls.go`). The injected `getenv func(string) string` becomes
   a lookup, `func(string) (string, bool)`, used for every variable; production passes
   `os.LookupEnv`, and tests inject maps, so they stay parallel.
   - `resolveTLSConfig` reads `FDB_TLS_VERIFY_PEERS` (default `Check.Valid=1`) and
     `FDB_TLS_PASSWORD`. A path variable set but empty means no file and no default.
   - The config trusts only the CA file: an empty pool when none is set.
   - It sets `InsecureSkipVerify` (which turns off only the stdlib's own chain and
     host-name check) and a `VerifyConnection` that runs the policy, which reads no
     host name.
   - An encrypted PKCS#8 key (`ENCRYPTED PRIVATE KEY`) is decrypted with the password.
     The schemes are PBES2 with PBKDF2 (HMAC-SHA1/224/256/384/512) and AES-128/192/256-CBC
     or DES-EDE3-CBC, which `openssl pkcs8 -topk8 -v2` writes. An unencrypted key
     ignores the password, as OpenSSL does.
   - A configuration error is a typed `*TLSConfigError` that unwraps to
     `*wire.FDBError{Code: 2107}` (tls_error) and its cause. It covers an unreadable or
     empty file, a certificate without its key or a key without its certificate, a
     wrong password, an unsupported key format and a verify-peers parse error.
3. **Transport** (`transport/conn.go`) is unchanged. `upgradeTLS` still fills an empty
   `ServerName` with the dialed host, so a `WithTLSConfig` config keeps its meaning as a
   Go `*tls.Config`. That covers the stdlib's per-dial host check, its roots, the verified
   chains its callbacks receive, and the `cs.ServerName` a custom `VerifyConnection`
   reads. On the environment path the filled `ServerName` is never checked. A host-name
   address (a cluster-file coordinator) still sends it as SNI; IP addresses send none
   (`crypto/tls` `hostnameInSNI`). The `WithTLSConfig` doc comments and DIVERGENCES.md
   say so and point to `VerifyPolicy.VerifyConnection` for libfdb_c's check.
4. **Docs.**
   - `docs/mt-saas.md`: the "Certificates must be hostname-verifiable" section and the
     `verify_peers` bullet and paragraph are rewritten. The environment path verifies
     the chain and the policy, not the host name, and `FDB_TLS_VERIFY_PEERS` works in
     pure Go.
   - `docs/mt-saas.md:657-663`: the environment list adds `FDB_TLS_VERIFY_PEERS`,
     `FDB_TLS_PASSWORD` and the set-but-empty rule.
   - `rfcs/051-tls-wiring.md`: the variable list (`:43`), the deferral and its
     "exactly `Check.Valid=1`" claim (`:71-73`), the `ServerName` description (`:84`)
     and the follow-ups (`:140-147`) point to this RFC.
   - `docs/compatibility.md` gets a TLS entry for the boundaries below.
   - The `WithTLSConfig` doc comments (client and fdb) and the `upgradeTLS` comment are
     updated.
   - CHANGELOG.md records, for the environment path:
     - the stricter roots and the dropped host-name check;
     - `FDB_TLS_VERIFY_PEERS` and `FDB_TLS_PASSWORD`;
     - the set-but-empty rule and `*TLSConfigError`.

     It narrows the Unreleased `x509sslcertoverrideplatform` note (`:83-85`) to
     `WithTLSConfig` configs with nil `RootCAs`.

## Divergences kept (DIVERGENCES.md)

- `WithTLSConfig` keeps Go's verification, including the per-dial host check
  libfdb_c lacks (see Change 3). The environment path sends SNI for a host-name
  coordinator, which libfdb_c never sends; nothing verifies it.
- A bad configuration fails the database open with `tls_error`. libfdb_c logs
  `Net2TLSInitError` and then fails every TLS connection (`Net2.actor.cpp:1405-1419`).
- Keys:
  - a legacy RFC 1423 encrypted PEM key, PBES1 and scrypt PBES2 keys are refused with
    `tls_error`; OpenSSL loads them;
  - a certificate whose key does not match is refused by `crypto/tls`; OpenSSL drops the
    key;
  - a certificate without a key, a key without a certificate, or an empty file fails the
    open. OpenSSL loads what it has (`TLSConfig.actor.cpp:124-134`).
- Certificates: `crypto/tls` parses every peer certificate before any verification hook
  runs, so a certificate `crypto/x509` cannot parse is refused even under
  `Check.Valid=0`. That covers a non-ASCII DNS, email or URI SAN, an unparseable URI,
  an unsupported DN string type, and an RSA key over 8192 bits. The Latin-1 widening
  therefore reaches only DN fields.
- Chains:
  - `x509.Verify` may return several chains, and Go accepts the peer when a rule
    matches any one of them; OpenSSL builds one chain.
  - Go accepts a non-self-signed CA in the pool as an anchor, rejects SHA-1
    signatures, and checks only the extended key usage of OpenSSL's purpose.
- Grammar: a `|` after a backslash byte hangs libfdb_c's parser; Go refuses it as a
  parse error.
- Deferred, as TODO.md entries:
  - the `TLS_*` network options and `FDB_NETWORK_OPTION_TLS_*` variables (the `fdb`
    facade has no network options);
  - the 12-hour certificate reload;
  - the 2 s handshake timeout, against Go's 10 s.

  Go keeps its TLS 1.2 minimum.

## Wire compatibility

No FDB protocol bytes and no ClientHello change.

## Tests

Regressions (each fails when its part is reverted; rejections assert their cause):

- `transport`, parsing: operators, scopes, the field whitelist, `de4514` escapes and
  their failures, trailing spaces, `|` and empty rules, `Check.*` values, repeated and
  identical terms, and both backslash-before-`|` errors (`\|`, `\\|`).
- `transport`, matching, over certificates the tests build:
  - DN exact, prefix and suffix, and a field present twice;
  - Latin-1: `CN=caf\E9` matches the UTF-8 CN `café`; the raw UTF-8 bytes
    `CN=caf\C3\A9`, widened, do not;
  - the empty criterion `subjectAltName>=DNS:` against a DNS SAN;
  - issuer and root scopes, `I.subjectAltName`, and two valid chains where an `R.` rule
    matches only one;
  - SAN `DNS`, `EMAIL` and `URI`, the fall-throughs, and an `IP:` criterion against an
    iPAddress SAN (no match). A URI SAN written `SPIFFE://...` matches as written,
    where Go's parsed `URL.String()` reads `spiffe://...`.
  - Accepted under `Check.Valid=0`. Rejected: an unknown CA (`x509.UnknownAuthorityError`),
    a leaf with only ClientAuth (`x509.CertificateInvalidError`, `IncompatibleUsage`), an
    expired leaf with `Check.Unexpired=0` (`Expired`). A rule mismatch is a `*PeerVerifyError` with no chain error.
- `transport`, handshakes through `upgradeTLS` to `localhost`: a policy config accepts a
  server whose certificate names another host; a stdlib config and a config with a
  mismatched `ServerName` reject it with a host-name error.
- `client`, through `resolveTLSConfig` and `transport.Dial`:
  - mutual TLS from `FDB_TLS_CA_FILE`, `FDB_TLS_CERTIFICATE_FILE` and `FDB_TLS_KEY_FILE`
    to a server certificate for another host succeeds under the default policy and under
    a matching `FDB_TLS_VERIFY_PEERS`;
  - a non-matching rule fails with a `*PeerVerifyError`, and so does a server signed by
    another CA (unknown authority);
  - an unparseable `FDB_TLS_VERIFY_PEERS` is a `*TLSConfigError` with code 2107.
- `client`, keys written by OpenSSL 3.6 (`openssl pkcs8 -topk8`), checked in as DER
  without PEM armour:
  - AES-256 and SHA-256, AES-128 and SHA-1, AES-192 and SHA-512, AES-128 with SHA-224
    and with SHA-384, and DES-EDE3 with OpenSSL's default PRF (SHA-256). Each decrypts
    with `FDB_TLS_PASSWORD` to the public key recorded beside it, and loads with a
    matching certificate;
  - a wrong or missing password, a scrypt key, a PBES1 key and a legacy encrypted PEM
    key are each a `*TLSConfigError` with code 2107;
  - an unencrypted key loads with a password set.
- `client`, the system roots are not trusted. A test binary sets `SSL_CERT_FILE` to a CA
  it generates in `TestMain`, before the process loads its roots. A stdlib config then
  accepts a server signed by that CA, which is the control. The environment config with
  no CA file refuses it with unknown authority. Before the change it was accepted. The
  policy given nil roots refuses it too; nil roots read as the system pool would pass.
- `client`, configuration:
  - a path variable set but empty suppresses its file and default;
  - an unreadable CA or a certificate without a key is a `*TLSConfigError` with code
    2107, unwrapping to `fs.ErrNotExist` where a file is missing;
  - `TestResolveTLSConfig`'s "nothing configured" case asserts an empty, non-nil pool,
    `InsecureSkipVerify` and a `VerifyConnection`. It used to assert nil roots.

Non-regression: the existing mutual-TLS, wrong-CA and missing-client-cert handshakes,
and the `:tls` cluster-file and `WithTLSConfig` precedence tests.
