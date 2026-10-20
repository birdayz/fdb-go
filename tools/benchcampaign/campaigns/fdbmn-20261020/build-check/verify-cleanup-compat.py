#!/usr/bin/env python3
import ast
import hashlib
import json
import os
import pathlib
import re
import subprocess
import tarfile

ROOT = pathlib.Path('${FDB_PERF_HOME}/campaigns/fdbmn-20261020/build-check')
SNAPSHOT = ROOT / 'unpatched-execution-fa8725f1d'
REPO = '${FDB_GO_WORKTREES}/fdb-go-flake'
COMPILER = 'e90c4e12df236ea0c2e7536fd68da8a47fc0b585c19588bf6c1a70cfc296a0f9'


def sha(path):
    with open(path, 'rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def manifest(path):
    values = {}
    for line in path.read_text().splitlines():
        value, name = line.split('  ', 1)
        assert name.startswith('./')
        if name != './.bazel-bep.jsonl':
            values[name[2:]] = value
    return values


source = json.loads((ROOT / 'cleanup-compat-source-amendment.json').read_text())
previous = json.loads((SNAPSHOT / 'execution-builds.json').read_text())
result = {
    'description': source['description'],
    'authorization': 'mn-campaign #4169 exact upstream #948 plus #4171 matching Python amendment; supersedes #4168',
    'production_revisions': previous['production_revisions'],
    'base_harness_revision': 'd94b4c61377d173b7c6884fde426a227dab0f0bb',
    'source_amendment': source,
    'unpatched_execution_preserved_at': str(SNAPSHOT),
    'cpu_heavy_finished_at': (ROOT / 'logs/cleanup-compat-cpu-heavy-finished-at.txt').read_text().strip(),
    'builds': {},
    'source_inventory': {},
    'executables': {},
    'sdk': {},
    'runtime_linkage': {},
    'tests_run': False,
    'benchmarks_run': False,
    'cloud_mutations': False,
    'three_line_only_variant_created': False,
}
with tarfile.open(source['new_client_harness_archive']) as tf:
    expected = {member.name: tf.extractfile(member).read() for member in tf if member.isfile()}
assert len(expected) == 28
assert sha(source['new_client_harness_archive']) == source['new_archive_sha256']
assert sha(source['original_client_harness_archive']) == source['original_archive_sha256']
assert sha(ROOT / 'cleanup-compat-combined.patch') == source['combined_patch_sha256']
assert sha(ROOT / 'cleanup-compat-upstream.patch') == source['upstream_patch_sha256']

for side in ['old', 'master']:
    tree = ROOT / f'trees/{side}'
    actual_harness = {
        str(path.relative_to(tree)): path.read_bytes()
        for path in (tree / 'tools/clientbench').rglob('*')
        if path.is_file() and not path.is_symlink()
    }
    assert actual_harness == expected
    baseline = manifest(SNAPSHOT / f'logs/{side}-inputs.sha256')
    frozen = manifest(ROOT / f'logs/{side}-cleanup-inputs.sha256')
    assert baseline.keys() == frozen.keys()
    deltas = sorted(name for name in baseline if baseline[name] != frozen[name])
    assert deltas == sorted(source['changed_files']), (side, deltas)
    for name, change in source['changed_files'].items():
        assert baseline[name] == change['original_sha256']
        assert frozen[name] == change['cleanup_compat_sha256']
    actual_files = {
        str(path.relative_to(tree))
        for path in tree.rglob('*')
        if path.is_file() and not path.is_symlink() and path.name != '.bazel-bep.jsonl'
    }
    assert actual_files == frozen.keys(), (side, actual_files ^ frozen.keys())
    for name, digest in frozen.items():
        assert sha(tree / name) == digest, (side, name)
    result['source_inventory'][side] = {
        'regular_source_files': len(frozen),
        'all_source_names_and_hashes_unchanged_since_prebuild_freeze': True,
        'only_differences_from_unpatched_execution': deltas,
        'complete_client_harness_identical_to_cleanup_compat_archive': True,
        'client_harness_regular_files': 28,
        'generated_metadata_excluded': ['.bazel-bep.jsonl'],
    }
    status = (ROOT / f'logs/{side}-cleanup-build.status').read_text().strip()
    log = (ROOT / f'logs/{side}-cleanup-build.log').read_text()
    assert status == '0' and 'Build completed successfully' in log
    result['builds'][side] = {
        'exit_code': 0,
        'targets': ['//tools/clientbench/cmd/clientbench:clientbench_go', '//tools/clientbench/cmd/clientbench:clientbench_cgo'],
        'elapsed_seconds': re.search(r'Elapsed time: ([0-9.]+)s', log).group(1),
        'process_summary': re.findall(r'INFO: .* process(?:es)?: .*', log)[-1],
    }
    binaries = sorted((ROOT / f'bin/{side}').iterdir())
    assert len(binaries) == (7 if side == 'old' else 8)
    for binary in binaries:
        digest = sha(binary)
        old_digest = sha(SNAPSHOT / f'bin/{side}' / binary.name)
        rebuilt = binary.name in ['client-go', 'client-cgo']
        assert (digest != old_digest) if rebuilt else (digest == old_digest), binary
        version = subprocess.check_output(['go', 'version', str(binary)], env=dict(os.environ, GOTOOLCHAIN='local'), text=True).strip()
        assert version.endswith(': go1.26.9'), version
        result['executables'][str(binary)] = {'sha256': digest, 'go_version': 'go1.26.9', 'rebuilt_for_cleanup_compat': rebuilt, 'unpatched_sha256': old_digest}

for binary in (ROOT / 'cache/bazel').glob('*/external/rules_go++go_sdk+main___download_0/bin/go'):
    assert sha(binary) == COMPILER
    result['sdk'][str(binary)] = {'sha256': COMPILER, 'matches_original_compiler': True, 'version': subprocess.check_output([str(binary), 'version'], text=True).strip()}
assert len(result['sdk']) >= 2
assert sha(ROOT / 'bin/old/client-cgo') == sha(ROOT / 'bin/master/client-cgo')
result['old_and_master_cgo_binary_identical'] = True

runtime = pathlib.Path('${FDB_PERF_HOME}/results/fdb-perf-20261009/baseline-complete-v3/artifacts/binaries')
assert sha(runtime / 'libfdb_c.so') == 'e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1'
for side in ['old', 'master']:
    binary = ROOT / f'bin/{side}/client-cgo'
    default = subprocess.check_output(['ldd', str(binary)], text=True)
    override = subprocess.check_output(['ldd', str(binary)], env=dict(os.environ, LD_LIBRARY_PATH=str(runtime)), text=True)
    assert f'libfdb_c.so => {runtime}/libfdb_c.so ' in override
    dynamic = subprocess.check_output(['readelf', '-d', str(binary)], text=True)
    assert '${HOME}/.local/share/foundationdb/7.3.77/lib' in dynamic
    (ROOT / f'logs/{side}-cleanup-cgo-ldd-default.txt').write_text(default)
    (ROOT / f'logs/{side}-cleanup-cgo-ldd-original-runtime.txt').write_text(override)
    result['runtime_linkage'][side] = {'local_rpath_present': True, 'original_runtime_override_ldd': override, 'resolved_original_library_sha256': sha(runtime / 'libfdb_c.so')}

for name in source['changed_files']:
    if name.endswith('.py'):
        ast.parse(expected[name], filename=name)
result['python_syntax_checked_without_import_or_execution'] = True
run_path = 'tools/clientbench/scripts/run.py'
with tarfile.open(source['original_client_harness_archive']) as tf:
    original_run = tf.extractfile(run_path).read().decode()
    original_analyze = tf.extractfile('tools/clientbench/scripts/analyze.py').read()
old = "                after['cleanups_executed_estimate'] != after['cleanups_queued_estimate'] or\n                after['cleanups_queued_estimate'] != before['cleanups_queued_estimate']):"
new = "                after['cleanups_executed_estimate'] != after['cleanups_queued_estimate']):"
assert original_run.count(old) == 1
assert original_run.replace(old, new) == expected[run_path].decode()
result['python_validator_only_cleanup_growth_equality_removed'] = True

def numeric_analysis_ast(data):
    tree = ast.parse(data)
    for statement in tree.body:
        if isinstance(statement, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'LIFECYCLE_CPU_SCOPE' for target in statement.targets):
            statement.value = ast.Constant(value='scope prose omitted for comparison')
    return ast.dump(tree, include_attributes=False)
assert numeric_analysis_ast(original_analyze) == numeric_analysis_ast(expected['tools/clientbench/scripts/analyze.py'])
result['analyzer_only_lifecycle_scope_prose_changed'] = True
result['worktree_status_after'] = subprocess.check_output(['git', '-C', REPO, 'status', '--short'], text=True)
assert result['worktree_status_after'] == ''
(ROOT / 'cleanup-compat-builds.json').write_text(json.dumps(result, indent=2, sort_keys=True) + '\n')
print(json.dumps({'builds': result['builds'], 'source_inventory': result['source_inventory'], 'binary_count': len(result['executables']), 'new_archive_sha256': source['new_archive_sha256'], 'combined_patch_sha256': source['combined_patch_sha256']}, indent=2, sort_keys=True))
