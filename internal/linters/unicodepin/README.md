# Unicode 15 regression guard

`unicodepin` enforces two separate RFC-265 policies. Test files (`_test.go`)
are excluded from both.

## Pinned runtime closure

Every package under `fdb.dev/internal/unicode15` rejects:

- The standard-library `unicode` import.
- Any external, non-standard-library import. The snapshot includes its own
  transform implementation; upstream `golang.org/x/text/transform` is not needed.
- `strings` and `bytes` casing helpers (lower, upper, title, including Special
  variants), `EqualFold`, `TrimSpace`, `Fields`, and `FieldsSeq`.
- Compiler-release build constraints, including negated and legacy `+build` forms.

Package facts propagate violations through local `fdb.dev` runtime dependencies.
Missing local facts fail closed. The source census additionally checks imports
and build constraints in every pinned runtime Go file, including files excluded
by the current compiler or platform. Its inputs must include all nine runtime
packages; an empty population fails.

## Compatibility-critical callers

A narrower, direct-use policy applies only to these package/file pairs:

- `pkg/recordlayer/{text_tokenizer,text_index_maintainer,collate_function_key_expression,keystore_key_manager,java_parse}.go`
- `pkg/recordlayer/query/plan/cascades/values/{values,uuid_parse}.go`
- `pkg/relational/core/parser/case_insensitive.go`

These files reject `unicode`, upstream x/text `cases`, `unicode/norm`, `collate`
and `language` imports (including subpackages), plus `strings` and `bytes`
lower/upper/title helpers (including Special variants), `Title`, and `EqualFold`.
Type-resolved references detect calls, import aliases, dot imports, and saved
function values without rejecting local names that shadow an import.

This policy does **not** apply the closure's whole-dependency external import ban
to legacy caller packages. Other imports, indirect dependencies, `TrimSpace`,
Fields helpers, and build constraints remain outside this narrower policy. In
particular, existing caller whitespace behavior is unchanged.
Unrelated files in the same package are not directly policed.

## Limits

Standard-library import paths are recognized by the absence of a dot in their
first component. Standard-library implementations and APIs beyond the explicit
helper restrictions are trusted. The source census covers the pinned tree;
outside local wrappers are checked through their currently compiled source
facts. Compatibility-caller checks likewise inspect compiled source only.
