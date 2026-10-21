# Unicode 15 compatibility data

This package preserves Go 1.26.10's Unicode-15 classification, simple casing and
simple-fold policy independently of the compiler. It is not an implementation of
Java full-string casing or `equalsIgnoreCase`. Keep those policies separate;
[RFC-265](../../rfcs/265-unicode15-compatibility-pin.md) lists the retained Java
boundaries and the migration/versioning requirement for changing stored tokens.

## Inputs and licensing

`ucd/` contains the exact Unicode 15.0.0 inputs listed in `ucd/SHA256SUMS`, downloaded
from `https://www.unicode.org/Public/15.0.0/ucd/`. The files were also verified
against the official UCD archive. `ucd/LICENSE` is the Unicode license covering
these inputs and generated tables. Release notices include this license and the
frozen x/text snapshot's LICENSE/PATENTS explicitly, not merely through Go module
dependency discovery.

`tables.go` is generated from UnicodeData general categories/simple mappings,
PropList properties and CaseFolding C/S entries. The Cased union is independently
checked against DerivedCoreProperties. Full/Turkic folding and full-string case
expansions are deliberately not substituted for Go's existing simple policy.

Normalization and Go-only collation use the scoped, licensed snapshot in
[`text/`](text/README.md). Its Unicode data is frozen at 15.0.0, while collation
weights remain the original UCA 6.2 / CLDR 23 data; these are not Java JRE/ICU keys.

## Regeneration

The ordinary Bazel regression verifies all input hashes, regenerates properties
and normalization, and reimports/checks the complete runtime snapshot against a
SHA-256-pinned source-only x/text archive:

```sh
bazelisk test //tools/unicode15gen:regeneration_test
```

To intentionally regenerate source, obtain the repository-pinned gofumpt binary
(`bazelisk run --run_under=echo @cc_mvdan_gofumpt//:gofumpt`), then run:

```sh
uv run --no-project python tools/unicode15gen/properties.py \
  --data internal/unicode15/ucd --formatter /absolute/path/to/gofumpt \
  --output internal/unicode15/tables.go
bazelisk run //tools/unicode15gen/norm -- \
  -data "$PWD/internal/unicode15/ucd" \
  -output "$PWD/internal/unicode15/text/unicode/norm/tables15.0.0.go"
```

Both formatters use the fixed Go 1.26 language mode. Neither generator downloads
Unicode data, consults host Unicode classification/casing, or emits Go-release
selectors. Snapshot import instructions and its complete local-change inventory
are in `text/README.md`. Shared-machine scheduling rules still apply to these
commands.

## Normalization generator provenance

`tools/unicode15gen/{norm,internal/triegen,internal/ucd}` derives from x/text
v0.42.0; its original source hashes are in
`tools/unicode15gen/UPSTREAM_SHA256SUMS`, with upstream LICENSE retained there.
Local changes are limited to:

- Removing generator-only `ignore` build selectors and localizing imports.
- Replacing the upstream network/compiler-version generator helper with a small
  offline manifest-verifying helper fixed at Unicode 15.0.0.
- Emitting only the requested normalization table, without release selectors or
  the upstream internal test-data table; the independent UCD corpus is tested.
- Formatting output with the repository-pinned gofumpt implementation, fixed at
  Go 1.26 syntax, and formatting copied generator source for repository lint.
- Using byte-wise ASCII title spelling for the generator's fixed nfc/nfkc trie
  names rather than consulting host casing tables.
- Supplying missing code-point arguments in two upstream fatal diagnostics,
  making a rune conversion explicit, and using Fprint for a literal header.
  These lint adaptations do not change generated normalization data.

The nogo/dependency guard and source census protect the runtime closure, including
compiler-excluded Go files. Independent immutable Go-1.26.10 baseline fixtures,
Java-produced fixtures and real-FDB write/update/delete/rebuild tests protect
behavior; generator reproduction alone is not a compatibility proof.
