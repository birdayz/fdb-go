#!/usr/bin/env python3
"""Prepare a pinned external frozen harness. Never builds, tests or launches it."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import stat
import subprocess
import tarfile

from apply_patch import apply_patch

HERE = Path(__file__).resolve().parent
CONTEXT = HERE / 'source-context.json'
PATCH = HERE / 'measurement-fences.patch'
BASE_ARCHIVE_SHA256 = '76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508'
SOURCE_ROOT = 'tools/clientbench'
PATCHED_FILES = {SOURCE_ROOT + '/internal/bench/run.go', SOURCE_ROOT + '/internal/bench/bench_test.go',
                 SOURCE_ROOT + '/scripts/BUILD.bazel', SOURCE_ROOT + '/scripts/matrix_plan.py',
                 SOURCE_ROOT + '/scripts/r3_plan.py', SOURCE_ROOT + '/scripts/test_campaign.py',
                 SOURCE_ROOT + '/integration/smoke_test.go', SOURCE_ROOT + '/cmd/clientbench/BUILD.bazel',
                 SOURCE_ROOT + '/cmd/clientbench/backend_selection_test.go'}
FORMAT = 'fdbmn-measured-fence-harness/v1'

QUALIFICATION_RECIPE = '''#!/usr/bin/env bash
# Execute only in an owner-authorized quiet window, once per production export.
# Preparation did NOT execute these commands or qualify a measured binary.
set -euo pipefail
: "${FROZEN_EXPORT:?external production export with this harness overlaid}"
: "${HARNESS_RECEIPT:?absolute path to the prepared source-receipt.json}"
: "${FENCES_PREPARER:?absolute path to the pinned prepare_harness.py}"
python3 -B "$FENCES_PREPARER" --verify-tree "$FROZEN_EXPORT" --receipt "$HARNESS_RECEIPT"
# Verify again even when compilation/tests fail. Caller freezes the complete
# production export too; this verifier covers only the amended harness.
finish() {
  rc=$?
  trap - EXIT
  python3 -B "$FENCES_PREPARER" --verify-tree "$FROZEN_EXPORT" --receipt "$HARNESS_RECEIPT" || exit 4
  exit "$rc"
}
trap finish EXIT
flock -o "${FDB_LANE_LOCK:-/tmp/fdb-heavy-lane.lock}" bash -c '
  cd "$1"
  just test
  rc=$?
  bazelisk shutdown
  shutdown_rc=$?
  if (( shutdown_rc )); then exit "$shutdown_rc"; fi
  exit "$rc"
' harness-lane "$FROZEN_EXPORT"
'''


def need(condition, message):
    if not condition:
        raise ValueError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def json_bytes(value):
    return (json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + '\n').encode()


def relative_name(name):
    path = PurePosixPath(name)
    need(name and not path.is_absolute() and path.as_posix() == name and '..' not in path.parts
         and '.git' not in path.parts and '\\' not in name and '\x00' not in name, 'unsafe archive/member path')
    return path


def outside_git(path):
    """Read-only guard, including Git worktree .git files and nonexisting outputs."""
    raw = Path(path).absolute()
    need(not any(p.is_symlink() for p in (raw, *raw.parents)), 'symlinked output/tree path')
    target = raw.resolve()
    ancestor = target
    while not ancestor.exists():
        ancestor = ancestor.parent
    if not ancestor.is_dir():
        ancestor = ancestor.parent
    need(not any((parent / '.git').exists() or (parent / '.git').is_symlink()
                 for parent in (ancestor, *ancestor.parents)), 'output/tree is inside a Git worktree')
    result = subprocess.run(['git', '-C', str(ancestor), 'rev-parse', '--is-inside-work-tree'],
                            capture_output=True, text=True, timeout=30, check=False)
    need(result.returncode != 0 or result.stdout.strip() != 'true', 'output/tree is inside a Git worktree')
    return target


def load_spec(context, patch):
    context_bytes, patch_bytes = Path(context).read_bytes(), Path(patch).read_bytes()
    spec = json.loads(context_bytes)
    need(set(spec['original_sha256']) == set(spec['patched_sha256']) == PATCHED_FILES,
         'wrong patched-file population')
    need(digest(patch_bytes) == spec['patch_sha256'], 'patch SHA256 differs')
    return spec, patch_bytes, digest(context_bytes)


def read_archive(data, expected_sha256, expected_count):
    need(digest(data) == expected_sha256, 'input archive SHA256 differs')
    files, directories, seen = {}, set(), set()
    with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
        for member in archive:
            raw = member.name
            name = raw[:-1] if member.isdir() and raw.endswith('/') else raw
            path = relative_name(name)
            need(name not in seen, 'duplicate archive member')
            seen.add(name)
            need(member.isdir() or member.isfile(), 'archive contains a link or special member')
            need(name == 'tools' or name == SOURCE_ROOT or name.startswith(SOURCE_ROOT + '/'),
                 'archive member outside frozen harness')
            if member.isdir():
                directories.add(name)
            else:
                need(name.startswith(SOURCE_ROOT + '/'), 'regular file outside frozen harness')
                stream = archive.extractfile(member)
                need(stream is not None, 'unreadable archive member')
                body = stream.read()
                need(len(body) == member.size, 'truncated archive member')
                files[name] = dict(body=body, mode=member.mode & 0o777)
            for parent in path.parents:
                if parent.as_posix() != '.':
                    directories.add(parent.as_posix())
    need(len(files) == expected_count and files, 'unexpected regular-member population')
    need(not (set(files) & directories), 'regular file used as archive directory')
    return files, directories


def deterministic_archive(files, directories):
    raw = io.BytesIO()
    with gzip.GzipFile(fileobj=raw, mode='wb', filename='', mtime=0, compresslevel=9) as compressed:
        with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as archive:
            for name in sorted(set(files) | directories):
                member = tarfile.TarInfo(name)
                member.uid = member.gid = member.mtime = 0
                member.uname = member.gname = ''
                if name in directories:
                    member.type, member.mode = tarfile.DIRTYPE, 0o755
                    archive.addfile(member)
                else:
                    value = files[name]
                    member.mode, member.size = value['mode'], len(value['body'])
                    archive.addfile(member, io.BytesIO(value['body']))
    return raw.getvalue()


def tree_population(tree):
    root = Path(tree)
    scope = root / SOURCE_ROOT
    need(scope.is_dir() and not scope.is_symlink(), 'missing or symlinked frozen harness root')
    need(not (root / 'tools').is_symlink(), 'symlinked harness parent')
    files, directories = {}, set()
    def walk_error(error):
        raise error

    for current, dirs, names in os.walk(scope, followlinks=False, onerror=walk_error):
        current = Path(current)
        directories.add(current.relative_to(root).as_posix())
        for name in dirs:
            need(not (current / name).is_symlink(), 'symlinked source directory')
        for name in names:
            path = current / name
            info = path.lstat()
            need(stat.S_ISREG(info.st_mode), 'nonregular source file')
            relative = path.relative_to(root).as_posix()
            relative_name(relative)
            files[relative] = dict(sha256=digest(path.read_bytes()), mode=stat.S_IMODE(info.st_mode))
    return files, directories


def verify_tree(tree, receipt_path, context=CONTEXT, patch=PATCH, expected_archive_sha256=BASE_ARCHIVE_SHA256):
    """Verify all harness source bytes/population; unrelated production paths are out of scope."""
    tree = outside_git(tree)
    spec, _, context_sha256 = load_spec(context, patch)
    receipt = json.loads(Path(receipt_path).read_bytes())
    need(receipt.get('format') == FORMAT and receipt.get('input_archive_sha256') == expected_archive_sha256
         and receipt.get('patch_sha256') == spec['patch_sha256']
         and receipt.get('source_context_sha256') == context_sha256, 'foreign source receipt')
    original, expected = receipt['original_member_sha256'], receipt['member_sha256']
    need(original and set(original) == set(expected) == set(receipt['member_modes'])
         and receipt.get('regular_files') == len(expected), 'invalid receipt population')
    need(set(receipt['changed_files']) == PATCHED_FILES, 'wrong receipt changed-file population')
    for name in original:
        relative_name(name)
        need(name.startswith(SOURCE_ROOT + '/'), 'foreign source receipt path')
        need(expected[name] == spec['patched_sha256'].get(name, original[name]), 'unexpected patched source hash')
    for name in PATCHED_FILES:
        need(original.get(name) == spec['original_sha256'][name], 'original source receipt pin differs')
    files, directories = tree_population(tree)
    need(set(files) == set(expected), 'source regular-member population changed')
    need(directories == set(receipt['source_directories']), 'source directory population changed')
    need(all(v['sha256'] == expected[name] and v['mode'] == receipt['member_modes'][name]
             for name, v in files.items()), 'source bytes or modes changed')
    return dict(status='source-verified-not-runtime-qualified', regular_files=len(files),
                member_manifest_sha256=digest(json_bytes(files)))


def prepare(archive, output, context=CONTEXT, patch=PATCH, expected_archive_sha256=BASE_ARCHIVE_SHA256,
            expected_count=28):
    """Validate entirely before writing; fresh destinations only, never apply twice."""
    output = outside_git(output)
    need(not output.exists(), 'output directory already exists')
    spec, patch_bytes, context_sha256 = load_spec(context, patch)
    data = Path(archive).read_bytes()
    files, directories = read_archive(data, expected_archive_sha256, expected_count)
    original = {name: digest(value['body']) for name, value in files.items()}
    need(all(original.get(name) == value for name, value in spec['original_sha256'].items()),
         'complete original source SHA256 differs')
    sources = {name: files[name]['body'].decode('utf-8') for name in PATCHED_FILES}
    patched = apply_patch(sources, patch_bytes.decode('utf-8'))
    for name, body in patched.items():
        body = body.encode('utf-8')
        need(digest(body) == spec['patched_sha256'][name], 'complete patched source SHA256 differs')
        files[name] = dict(body=body, mode=files[name]['mode'])
    members = {name: digest(value['body']) for name, value in files.items()}
    changed = sorted(name for name in members if members[name] != original[name])
    need(set(changed) == PATCHED_FILES and set(members) == set(original), 'overlay changed wrong source population')
    archive_bytes = deterministic_archive(files, directories)
    receipt = dict(format=FORMAT, status='prepared-not-runtime-qualified',
                   base_harness_commit=spec['base_revision'], input_archive_sha256=expected_archive_sha256,
                   source_context_sha256=context_sha256, patch_sha256=spec['patch_sha256'],
                   output_archive_sha256=digest(archive_bytes), regular_files=len(files), changed_files=changed,
                   original_member_sha256=original, member_sha256=members,
                   member_modes={name: value['mode'] for name, value in files.items()},
                   source_directories=sorted(name for name in directories if name == SOURCE_ROOT or name.startswith(SOURCE_ROOT + '/')),
                   qualification_recipe_sha256=digest(QUALIFICATION_RECIPE.encode()))
    output.mkdir(parents=True, exist_ok=False)
    tree = output / 'tree'
    tree.mkdir()
    for name in sorted(directories):
        (tree / name).mkdir(parents=True, exist_ok=True)
    for name, value in files.items():
        path = tree / name
        with path.open('xb') as stream:
            stream.write(value['body'])
        path.chmod(value['mode'])
    (output / 'client-harness.tar.gz').write_bytes(archive_bytes)
    (output / 'source-receipt.json').write_bytes(json_bytes(receipt))
    recipe = output / 'qualify-harness.sh'
    recipe.write_text(QUALIFICATION_RECIPE)
    recipe.chmod(0o700)
    verify_tree(tree, output / 'source-receipt.json', context, patch, expected_archive_sha256)
    return receipt


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument('--archive', type=Path, help='retained cleanup-compatible frozen archive; fixed SHA256 required')
    mode.add_argument('--verify-tree', type=Path, help='external production export; check only the complete frozen harness subtree')
    parser.add_argument('--output-dir', type=Path, help='fresh external preparation directory')
    parser.add_argument('--receipt', type=Path, help='prepared supplemental source receipt for --verify-tree')
    args = parser.parse_args(argv)
    if args.archive:
        if args.output_dir is None or args.receipt is not None:
            parser.error('--archive requires --output-dir and cannot use --receipt')
    elif args.receipt is None or args.output_dir is not None:
        parser.error('--verify-tree requires --receipt and cannot use --output-dir')
    try:
        if args.archive:
            receipt = prepare(args.archive, args.output_dir)
            result = dict(status=receipt['status'], output_archive_sha256=receipt['output_archive_sha256'])
        else:
            result = verify_tree(args.verify_tree, args.receipt)
    except (OSError, ValueError, KeyError, tarfile.TarError, subprocess.SubprocessError) as error:
        parser.exit(2, 'REFUSED: ' + str(error) + '\n')
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
