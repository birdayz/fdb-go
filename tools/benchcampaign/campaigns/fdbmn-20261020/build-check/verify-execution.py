#!/usr/bin/env python3
import hashlib
import io
import json
import os
import pathlib
import re
import subprocess
import tarfile

ROOT = pathlib.Path('${FDB_PERF_HOME}/campaigns/fdbmn-20261020/build-check')
REPO = '${FDB_GO_WORKTREES}/fdb-go-flake'
OLD = '267a41434a81723b07e7150869d702636a1b91a9'
NEW = 'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc'
HARNESS = 'd94b4c61377d173b7c6884fde426a227dab0f0bb'
REBENCH = pathlib.Path('${FDB_PERF_HOME}/hcloud-rebench-20261014')
ORIGINAL_BIN = pathlib.Path('${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014/binaries/rebench-20261014/bin')
RUNTIME = pathlib.Path('${FDB_PERF_HOME}/results/fdb-perf-20261009/baseline-complete-v3/artifacts/binaries')
COMPILER = 'e90c4e12df236ea0c2e7536fd68da8a47fc0b585c19588bf6c1a70cfc296a0f9'


def sha(path):
    with open(path, 'rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def checked_manifest(side, prefix):
    manifest = ROOT / f'logs/{prefix}-inputs.sha256'
    tree = ROOT / f'trees/{side}'
    entries = {}
    generated = {}
    for line in manifest.read_text().splitlines():
        digest, relative = line.split('  ', 1)
        assert relative.startswith('./'), relative
        relative = relative[2:]
        if relative == '.bazel-bep.jsonl':
            generated[relative] = {'manifest_sha256': digest, 'after_sha256': sha(tree / relative)}
            continue
        entries[relative] = digest
        assert sha(tree / relative) == digest, relative
    actual = {
        str(path.relative_to(tree))
        for path in tree.rglob('*')
        if path.is_file() and not path.is_symlink()
    }
    if '.bazel-bep.jsonl' in actual:
        generated.setdefault('.bazel-bep.jsonl', {'after_sha256': sha(tree / '.bazel-bep.jsonl')})
        actual.remove('.bazel-bep.jsonl')
    assert actual == set(entries), (side, actual ^ set(entries))
    assert entries
    return {'source_files': len(entries), 'all_source_names_and_hashes_unchanged': True, 'generated_cquery_metadata_not_source': generated}


expected_overlay = {}
for archive in sorted((REBENCH / 'overlays/final').glob('*.tar.gz')):
    with tarfile.open(archive) as tf:
        for member in tf:
            if member.isfile():
                expected_overlay[member.name] = tf.extractfile(member).read()
assert len(expected_overlay) == 44

result = {
    'production_revisions': {'old': OLD, 'master': NEW},
    'harness_revision': HARNESS,
    'authorization': 'Owner GO on current master fa8725f1d; requester mn-campaign #4156',
    'previous_rehearsal_preserved_at': str(ROOT / 'rehearsal-ae14b220'),
    'tests_run': False,
    'benchmarks_run': False,
    'cloud_mutations': False,
    'build_cpu_heavy_finished_at': (ROOT / 'logs/cpu-heavy-builds-finished-at.txt').read_text().strip(),
    'builds': {},
    'overlays': {},
    'executables': {},
    'sdk': {},
    'runtime': {},
    'linkage': {},
    'placementprobe_refusals': {},
}

for side, count in [('old', 7), ('master', 8)]:
    status = (ROOT / f'logs/{side}-build.status').read_text().strip()
    assert status == '0', (side, status)
    log = (ROOT / f'logs/{side}-build.log').read_text()
    assert 'Build completed successfully' in log
    result['builds'][side] = {
        'exit_code': 0,
        'target_count': 7,
        'regular_inputs_identical_after_all_builds': checked_manifest(side, side),
        'elapsed_seconds': re.search(r'Elapsed time: ([0-9.]+)s', log).group(1),
        'process_summary': re.findall(r'INFO: .* process(?:es)?: .*', log)[-1],
    }
    tree = ROOT / f'trees/{side}'
    frozen_inventory = {}
    for relative in ['tools/clientbench', 'pkg/testutil/benchcluster', 'pkg/recordlayer/benchfetch', 'pkg/relational/sqlbench']:
        for path in (tree / relative).rglob('*'):
            if path.is_file() and not path.is_symlink():
                frozen_inventory[str(path.relative_to(tree))] = path.read_bytes()
    for relative in ['pkg/recordlayer/benchmark_test.go', 'docs/benchmarks/campaign-harness.md']:
        frozen_inventory[relative] = (tree / relative).read_bytes()
    assert frozen_inventory == expected_overlay, (side, frozen_inventory.keys() ^ expected_overlay.keys())
    result['overlays'][side] = {'exact_archive_member_set_and_bytes': True, 'all_overlay_regular_files': 44, 'tools_clientbench_regular_files': 28}
    binaries = sorted((ROOT / f'bin/{side}').iterdir())
    assert len(binaries) == count, (side, len(binaries))
    for path in binaries:
        version = subprocess.check_output(['go', 'version', str(path)], env=dict(os.environ, GOTOOLCHAIN='local'), text=True).strip()
        assert version.endswith(': go1.26.9'), version
        result['executables'][str(path)] = {'sha256': sha(path), 'go_version': 'go1.26.9'}

status = (ROOT / 'logs/master-placement-build.status').read_text().strip()
assert status == '0'
log = (ROOT / 'logs/master-placement-build.log').read_text()
assert 'Build completed successfully' in log
result['builds']['master_placementprobe'] = {
    'exit_code': 0,
    'target': '//tools/benchcampaign/multinode/placementprobe/cmd/placementprobe:placementprobe_cgo',
    'regular_inputs_identical_after_build': checked_manifest('master', 'master-placement'),
    'elapsed_seconds': re.search(r'Elapsed time: ([0-9.]+)s', log).group(1),
    'process_summary': re.findall(r'INFO: .* process(?:es)?: .*', log)[-1],
}

placement_path = 'tools/benchcampaign/multinode/placementprobe'
expected_placement = {}
data = subprocess.check_output(['git', '-C', REPO, 'archive', NEW, placement_path])
with tarfile.open(fileobj=io.BytesIO(data)) as tf:
    for member in tf:
        if member.isfile():
            expected_placement[member.name] = tf.extractfile(member).read()
actual_placement = {
    str(path.relative_to(ROOT / 'trees/master')): path.read_bytes()
    for path in (ROOT / f'trees/master/{placement_path}').rglob('*')
    if path.is_file() and not path.is_symlink()
}
assert actual_placement == expected_placement
result['placementprobe_source'] = {'production_revision': NEW, 'regular_files': len(actual_placement), 'all_exact_git_archive_members': True}

for path in sorted((ROOT / 'cache/bazel').glob('*/external/rules_go++go_sdk+main___download_0/bin/go')):
    assert sha(path) == COMPILER
    version = subprocess.check_output([str(path), 'version'], text=True).strip()
    assert version == 'go version go1.26.9 linux/amd64'
    result['sdk'][str(path)] = {'sha256': COMPILER, 'version': version, 'matches_original_compiler': True}
assert len(result['sdk']) >= 2

for name, expected in [('fdbserver', '179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f'), ('fdbcli', '6dfe3328389fe8ff8175ddc080e278c5b07d1d2601f97079cba1194bfb2e41fb'), ('libfdb_c.so', 'e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1')]:
    path = RUNTIME / name
    assert sha(path) == expected
    result['runtime'][str(path)] = {'sha256': expected, 'matches_previous_runtime': True}

env = dict(os.environ, LD_LIBRARY_PATH=str(RUNTIME))
for name in ['client-cgo', 'placementprobe-cgo']:
    binary = ROOT / f'bin/master/{name}'
    default = subprocess.check_output(['ldd', str(binary)], text=True)
    resolved = subprocess.check_output(['ldd', str(binary)], env=env, text=True)
    assert f'libfdb_c.so => {RUNTIME}/libfdb_c.so ' in resolved
    dynamic = subprocess.check_output(['readelf', '-d', str(binary)], text=True)
    assert '${HOME}/.local/share/foundationdb/7.3.77/lib' in dynamic
    (ROOT / f'logs/{name}-ldd-default.txt').write_text(default)
    (ROOT / f'logs/{name}-ldd-original-runtime.txt').write_text(resolved)
    (ROOT / f'logs/{name}-dynamic.txt').write_text(dynamic)
    result['linkage'][name] = {'default_ldd': default, 'original_runtime_ldd': resolved, 'local_rpath_present': True, 'original_runtime_override_resolves_exact_library': True}

probe = ROOT / 'execution-refusal-probes'
probe.mkdir(mode=0o700, exist_ok=True)
cluster = probe / 'synthetic.cluster'
cluster.write_text('fdbbenchvpc:mn20261020@10.89.20.11:4500,10.89.20.12:4500,10.89.20.13:4500\n')
fixture = probe / 'fixture.json'
fixture.write_text('{"phase":"fixture","payload":"small","rows":1000,"prefix_bytes":3}\n')
output = probe / 'unowned-output.json'
assert not output.exists()
binary = ROOT / 'bin/master/placementprobe-cgo'
for name, args in [('noargs', []), ('unowned', ['probe', '--cluster-file', str(cluster), '--store', 'benchfetch/nope', '--fixture-receipt', str(fixture), '--output', str(output)])]:
    command = [str(binary), *args]
    completed = subprocess.run(command, cwd=probe, env=env, capture_output=True, timeout=5)
    (probe / f'{name}.stdout').write_bytes(completed.stdout)
    (probe / f'{name}.stderr').write_bytes(completed.stderr)
    assert completed.returncode == 2, (name, completed.returncode, completed.stderr)
    assert completed.stdout == b'' and not output.exists()
    result['placementprobe_refusals'][name] = {'argv': command, 'LD_LIBRARY_PATH': str(RUNTIME), 'exit_code': 2, 'stdout_bytes': 0, 'stderr': completed.stderr.decode(), 'output_receipt_exists': False}

assert sha(ROOT / 'bin/old/client-go') == sha(ORIGINAL_BIN / 'client-final-go')
result['old_client_go_identical_to_original'] = True
result['worktree_status_after'] = subprocess.check_output(['git', '-C', REPO, 'status', '--short'], text=True)
assert result['worktree_status_after'] == ''
(ROOT / 'execution-builds.json').write_text(json.dumps(result, indent=2, sort_keys=True) + '\n')
print(json.dumps({'builds': result['builds'], 'binary_count': len(result['executables']), 'placementprobe_refusals': result['placementprobe_refusals']}, indent=2, sort_keys=True))
