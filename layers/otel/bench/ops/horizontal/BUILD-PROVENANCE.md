# Measured executable build and deployment contract v1

This is an artifact-creation recipe, **not permission to run an ingest job**.
Stage 0 and paid evidence require these receipts; correctness-test executables
built with the developer home rc are not measured artifacts. ALL THREE ingest arms
and the separate pure-Go verifier use **each toolchain's default optimized build under Bazel `-c opt`** (`--compilation_mode=opt`):
no LTO, PGO, target-cpu or codegen-units overrides on either side. A common
`fastbuild` label does not imply common optimization across Go and Rust.
Fastbuild/dbg or missing-mode receipts are refused, not normalized to opt. The ordinary
`just test` / verified lane keeps its existing rc configuration, unchanged.
No command below runs an ingest executable or issues campaign rows.

## Versioned formats and literal examples

- Receipt schema: [`paired-build-receipt-v1.schema.json`](paired-build-receipt-v1.schema.json).
  Four-role candidate SHA256: `772e664896c4b81ca3dc2c3770452c887253c6337a282ab324b80425a4af6f80`.
- Bundle manifest: receipt `files`, using that schema's `$defs.deployment_file`.
- Literal control-pure example: [`build-receipt-examples/control_pure_binary.json`](build-receipt-examples/control_pure_binary.json).
- Literal control-native example: [`build-receipt-examples/control_native_binary.json`](build-receipt-examples/control_native_binary.json).
- Literal Rust example: [`build-receipt-examples/rust_binary.json`](build-receipt-examples/rust_binary.json).
- Literal verifier example: [`build-receipt-examples/verifier_binary.json`](build-receipt-examples/verifier_binary.json).

Examples demonstrate exact field placement/types, **not real build attestations**.
Their repeated digests, byte counts and illustrative runfile paths must NOT be
copied as evidence. Generate actual receipts with `paired_build_receipt.py` below.

Plan executable assets are exactly `{path, sha256, build_receipt:{path,sha256}}`.
Each also requires a normal `{path,sha256}` deployment asset:

| Executable asset | Build target | Deployment asset | Source identity |
|---|---|---|---|
| `control_pure_binary` | `//layers/otel/cmd/otelrust-control:control-pure` | `control_pure_binary_deployment` | `sources.go_pure` |
| `control_native_binary` | `//layers/otel/cmd/otelrust-control:control-native` | `control_native_binary_deployment` | `sources.go_native` |
| `rust_binary` | `//layers/otel/rust:otel-rust` | `rust_binary_deployment` | `sources.rust_native` |
| `verifier_binary` | `//layers/otel/cmd/otelrust-control:verifier` | `verifier_binary_deployment` | `sources.go_pure` |

Root1234 decision B makes `verifier_binary` a fourth separately sealed pure-Go,
verify-only executable with the full source-derived oracle. It uses the same
integrated PR1048 source revision/manifest as `sources.go_pure`, but has its own
binary hash, build receipt and deployment archive. Control-pure retains corpus,
setup and pure ingest; control-native and Rust retain only their own ingest.
None of the three ingest binaries links the oracle merely because the verifier
exists. Keep verifier dependencies out of their build graphs; receipt creation
alone does not implement or authorize mode dispatch. The producer must enforce
the verifier's verify-only capability and prove the complete oracle population.
All four artifacts obey the opt/home-rc/readelf/bundle rules below, and the
verifier's `client_library_relative_path` is null with no native DT_NEEDED linkage.

Job/report field names and the frozen shared-job schema are unchanged.
`build_receipt` binds source revision/manifest, executable SHA256, exact successful
build argv, required `compilation_mode:"opt"`, required `configured_output_path`
(normalized `bazel-out/<configuration>-opt/bin/<original-target-relative-path>`,
including normal opt transition suffixes), developer home, the effective rc file
set and its hashes, exact
successful `readelf -d` command/output, deployment SHA256, original relative
executable path, optional native client runfile, and the COMPLETE file manifest.
Receipt objects are closed; duplicate keys and nonfinite JSON are rejected.

## Exact build / package / receipt commands

Run in the already approved clean producer source checkout, with its unchanged
repository/dependency pins and its actual source manifest. Use a private fresh
artifact directory (`umask 077`). The following is the per-executable recipe;
set `name`, `target` and `relative` from the table and actual Bazel output layout.
The shown shell argv is exactly the argv recorded in `build-command.json`.

```sh
umask 077
SOURCE_ROOT="$PWD"
SOURCE_REVISION="$(git rev-parse HEAD)"
# SOURCE_MANIFEST is the approved, current manifest for this exact source tree.
# ARTIFACTS is a fresh private directory; do not overwrite earlier receipts.
mkdir -m 0700 "$ARTIFACTS"
cmd=(bazelisk --nohome_rc build --announce_rc --compilation_mode=opt "$target")
python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' "${cmd[@]}" \
  > "$ARTIFACTS/build-command.json"
if "${cmd[@]}" > "$ARTIFACTS/build.log" 2>&1; then
  build_exit=0
else
  build_exit=$?
  cat "$ARTIFACTS/build.log" >&2
  exit "$build_exit"
fi
BINDIR="$(bazelisk --nohome_rc info --compilation_mode=opt bazel-bin)"
bazelisk --nohome_rc cquery --compilation_mode=opt --output=files "$target" \
  > "$ARTIFACTS/configured-output.path"
# This must name exactly the configured executable, not bazel-bin or a guessed
# configuration. Stop if cquery reports multiple outputs or another mode.
configured_output="$(cat "$ARTIFACTS/configured-output.path")"
bazelisk --nohome_rc aquery --compilation_mode=opt --output=textproto "$target" \
  > "$ARTIFACTS/configured-actions.textproto"
# Copy for controller-side hashing/readelf only. It is NOT the deployed launcher.
cp -- "$SOURCE_ROOT/$configured_output" "$ARTIFACTS/$name"
LC_ALL=C readelf -d "$ARTIFACTS/$name" > "$ARTIFACTS/readelf.txt"
sha256sum "$ARTIFACTS/$name" > "$ARTIFACTS/executable.sha256"

# Include the original relative executable and its full runfiles tree. Include
# all existing $ORIGIN-relative support roots, e.g. _solib_k8, without flattening.
bundle_members=("$relative" "$relative.runfiles")
if test -d "$BINDIR/_solib_k8"; then bundle_members+=(_solib_k8); fi
tar --dereference --hard-dereference --mode='u+rwX,go+rX,go-w' \
  -C "$BINDIR" -cf "$ARTIFACTS/$name.deployment.tar" "${bundle_members[@]}"
sha256sum "$ARTIFACTS/$name.deployment.tar" > "$ARTIFACTS/deployment.sha256"
```

Do not guess the effective rc set. Preserve `build.log`; inspect `--announce_rc`,
startup rc options, and repository imports. Record every effective rc file once,
in effective order, via repeated `--rc-file /absolute/path`. Include the actual
repository `.bazelrc` when present. Developer `$HOME/.bazelrc` is forbidden even
through an explicit `--bazelrc`, symlink or hardlink. If the effective set is
uncertain, STOP rather than inventing a list. System/workspace/imported rc files
are not silently omitted. `--nohome_rc` must be a startup option BEFORE `build`;
a build option or later `--home_rc` does not satisfy this contract.

For `control_native_binary`, locate the actual canonical `libfdb_c.so` in this
bundle. Set `native_relative` to that exact original relative path and
`PINNED_LIBRARY` to the pinned source artifact; their SHA256 must agree with the
plan's `assets.libfdb_c.sha256` (FDB7.3.77). Do not invent a renamed file, alias,
symlink or replacement copy to evade mapped-file identity checks. Its directory
must contain no colon. Set `NATIVE_ARGS` only for the native Go executable:

```sh
NATIVE_ARGS=()
if test "$name" = control_native_binary; then
  NATIVE_ARGS=(--client-library-relative-path "$native_relative" \
               --client-library "$PINNED_LIBRARY")
fi
# RC_ARGS contains repeated --rc-file arguments from the actual effective set.
python3 -B "$RUNNER_HORIZONTAL/paired_build_receipt.py" \
  --executable-asset "$name" --binary "$ARTIFACTS/$name" \
  --source-root "$SOURCE_ROOT" --source-revision "$SOURCE_REVISION" \
  --source-manifest "$SOURCE_MANIFEST" --developer-home "$HOME" \
  --build-command-file "$ARTIFACTS/build-command.json" --build-exit-code "$build_exit" \
  --compilation-mode opt --configured-output-path "$configured_output" \
  "${RC_ARGS[@]}" --deployment "$ARTIFACTS/$name.deployment.tar" \
  --executable-relative-path "$relative" "${NATIVE_ARGS[@]}" \
  --output "$ARTIFACTS/$name.build-receipt.json" \
  > "$ARTIFACTS/$name.plan-assets.json"
```

`RUNNER_HORIZONTAL` is this directory. The helper uses Python's standard library
and ordinary `readelf`; no Python package installation is required. It does NOT
build an executable, run an ingest, infer rc files or invent a successful build.
The creator supplies the observed zero build exit status. It scans the complete
tar, captures live readelf output, validates before publishing, creates the
receipt exclusively0600, and prints the two sealed plan asset entries. Preserve
all original build logs, command JSON, configured-output path, configured-action
capture, readelf output, receipts and archives. The creator also hashes the
configured source-tree output and requires it to equal the measured executable.
The controller repeats live ELF/hash/rc/tar validation before any provider call.
Both the recorded build command and the configured output must explicitly show
opt; a copied artifact path or a claimed `compilation_mode` alone is insufficient.
Contradictory fastbuild/dbg command options are refused even if a later opt option
would otherwise override them. Local Stage 0 receipt creation and paid preflight
use the same rule. Old receipts lacking these required fields must not be reused.

Retain and inspect actual top-level compile-action flags from the configured
capture: Rust opt-level, debug-assertions, overflow-checks, codegen-units, lto and
panic strategy; Go gc flags and control-native cgo copts. Record the observed
toolchain defaults, not assumed settings. Disclose the default-optimized rule in
any comparison table. Do not add tuning flags to make the arms appear aligned;
Optimization-level overrides (including `-O0`, `-Copt-level=0` and Go `-N`),
LTO, PGO, target-cpu and codegen-units overrides are forbidden. The command guard
rejects explicit forms; captured rc files and action flags remain audit evidence
for indirect settings. Rebuild all three arms under the same policy and never
join superseded fastbuild results with replacement opt results.

Every RUNPATH/RPATH line must contain NO `/home` path. Native Go must have
DT_NEEDED `libfdb_c.so`; control-pure, Rust and the verifier must not prelink it. Do not edit ELF rpaths
or weaken runtime mapping validation to make a failed build acceptable; rebuild
with the required startup configuration and a new exclusive receipt instead.

## Bundle manifest and installation

The deployment is a bounded tar (plain or compressed) containing the executable
and runfiles in ORIGINAL relative layout, plus required relative support trees.
Regular-file manifest entries are sorted lexicographically by `path`, and contain
exactly `{path,sha256,bytes,mode}`. `mode` is decimal420 (0644) or493 (0755).
Build receipts and installer request JSON are capped at4MiB; readelf capture is
capped at1MiB. Directory entries, including empty `<executable>.runfiles`, are retained but are
not regular-file manifest entries. Paths must be normalized relative paths.
Symlinks, hardlinks, sparse/special files, duplicate members, traversal, omitted
files, changed bytes/modes and extra files are rejected. Caps:512MiB archive,
512MiB total file data,10000 files and20000 total tar entries. GNU tar's two
dereference options materialize runfiles rather than transporting cache symlinks.

On every held host the backend verifies the uploaded archive and installs it
under `/srv/otel-horizontal/<campaign>/bin/<executable_asset>.bundle/`, verifying
every file hash/mode before atomically publishing the complete exclusive tree.
It invokes `executable_relative_path` INSIDE that tree, not the controller's
flat executable copy. Per-host deployment receipts include schema
`otel_paired_deployment_receipt_v1`, `archive`, `sha256`, `destination` and the
complete sorted `files` manifest. The independent watch seals the helper code;
residual-process checks include every owned deployment-root prefix.

Stage 0 must likewise use the verified installed bundle layout, not the flat
controller copy. The public `paired_builds.install_bundle(request)` performs the same bounded
installation. `INSTALL_SCRIPT` is self-contained Python for a target without a
checkout: its two arguments are the absolute private request-JSON filename and
that file's SHA256. The backend transfers and hashes that request file rather
than putting a potentially large file manifest into an SSH argument.
Local measurement dispatch and authoritative row-ledger issuance remain separate
root-owned gates; this recipe does not authorize either.

## Minimal launcher and actual library identity

The composed launcher is `systemd-run ... env -i` with only PATH, HOME,
GOMAXPROCS4, and for **go_native ingest ONLY**:
`LD_LIBRARY_PATH=<directory of job.client_library>`.
That path is the receipt-bound deployed `libfdb_c.so` runfile. Its filename must
be exactly `libfdb_c.so`; a directory containing `:` is refused. The launcher
never creates an alias or substitutes another same-bytes file. Runtime
same-file/device/inode/hash proof remains authoritative before native API/network
or timing. `$ORIGIN` runfiles entries still resolve in the preserved tree.
Control-pure, Rust and the verifier have NO LD_LIBRARY_PATH in their explicit minimal
environments, nor inherited FDB_NETWORK_OPTION_* variables. Rust opens its
sealed `client_library` explicitly; its separately transferred pinned artifact
is installed under the canonical name `libfdb_c.so`.
