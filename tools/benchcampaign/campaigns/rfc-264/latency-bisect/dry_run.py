#!/usr/bin/env python3
"""Full offline controller rehearsal: synthetic client/FDB/Docker, actual taskset."""
import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
from unittest.mock import patch

from common import ROOT, PLAN, dump, load, path, schedule, sha
import timing


def main():
    real = load()
    frozen_plan_sha = sha(PLAN)
    real_identities = timing.binaries(real, 'slot2')
    real_executables = timing.executable_preflight(real, real_identities)
    fixture = ROOT / 'slot1-reference/b00-get100-w08-p00-old/run'
    template_manifest = json.loads((fixture / 'manifest.json').read_text())
    template_summary = json.loads((fixture / 'summary.json').read_text())
    assert template_summary['success'] is True
    production_taskset = real_executables['taskset']
    records = []
    with tempfile.TemporaryDirectory(prefix='latency-full-dry-run-') as tmp:
        scratch = Path(tmp)
        dump(scratch / 'template-manifest.json', template_manifest)
        dump(scratch / 'template-summary.json', template_summary)
        script = '#!' + sys.executable + '\n' + r'''
import csv, hashlib, json, os, pathlib, sys
root = pathlib.Path(__file__).parent
name = pathlib.Path(sys.argv[0]).name
with (root / 'exec.jsonl').open('a') as f:
    f.write(json.dumps({'executable': name, 'argv': sys.argv}) + '\n')
if name == 'docker':
    assert sys.argv[1:] == ['ps', '--format', '{{.Image}} {{.Names}} {{.Labels}}']
    sys.exit(0)
if name == 'fdbcli':
    assert sys.argv[1] == '-C' and '--timeout' in sys.argv
    command = sys.argv[sys.argv.index('--exec') + 1]
    if command == 'status json':
        print(json.dumps({'client': {'database_status': {'available': True}}, 'cluster': {'processes': {
            's': {'address': '127.0.0.1:4500', 'roles': [{'role': 'stateless'}]},
            't': {'address': '127.0.0.1:4501', 'roles': [{'role': 'transaction'}]},
            'd': {'address': '127.0.0.1:4502', 'roles': [{'role': 'storage'}]}}}}))
    elif command.startswith('getrange clientbench/rfc-264-latency/'):
        print('Range limited to 1 keys')
    elif command.startswith('writemode on; clearrange clientbench/rfc-264-latency/'):
        print('Committed (SYNTHETIC DRY RUN; NO FDB CONNECTION)')
    else:
        raise RuntimeError('unexpected simulated fdbcli command: ' + command)
    sys.exit(0)
assert name.startswith('client-'), name
cfg = {}
args = sys.argv[1:]
i = 0
while i < len(args):
    key = args[i].removeprefix('--').replace('-', '_')
    if key == 'allow_writes':
        cfg[key] = True
        i += 1
    else:
        cfg[key] = args[i + 1]
        i += 2
for key in ('iterations', 'warmup', 'workers', 'block', 'transaction_timeout_ms', 'retry_limit'):
    cfg[key] = int(cfg[key])
assert cfg['profile'] == 'none' and cfg['allow_writes']
assert len(cfg['prefix']) == 68
out = pathlib.Path(cfg['output'])
out.mkdir(parents=True, exist_ok=False)
manifest = json.loads((root / 'template-manifest.json').read_text())
summary = json.loads((root / 'template-summary.json').read_text())
backend = {'backend': 'go', 'native_library': False, 'api_version': 730,
           'executable': str(pathlib.Path(sys.argv[0]).resolve()),
           'executable_sha256': hashlib.sha256(pathlib.Path(sys.argv[0]).read_bytes()).hexdigest(),
           'loaded_fdb_libraries_sha256': {}}
manifest.update(config=cfg, backend=backend, gomaxprocs=8, go_version='go1.26.9')
summary.update(config=cfg, backend=backend, profile='none', requested=cfg['iterations'], completed=cfg['iterations'],
               totalcompleted=cfg['iterations'], errors=0, warmup_requested=cfg['warmup'], warmup_completed=cfg['warmup'],
               warmup_errors=0, retries=0, warmup_retries=0, success=True, persisted_validation='ok', failure='')
summary['client_counters_delta']['values'].update(read_versions_completed=cfg['iterations'], grv_successful_batch_replies=1, grv_cache_hits=0)
for filename, data in (('manifest.json', manifest), ('summary.json', summary)):
    (out / filename).write_text(json.dumps(data) + '\n')
latency = 100000 if cfg['source_revision'].startswith('267a41434') else 120000
for filename, phase, count in (('samples.csv', 'measure', cfg['iterations']), ('warmup.csv', 'warmup', cfg['warmup'])):
    with (out / filename).open('w', newline='') as f:
        w = csv.writer(f)
        w.writerow(['phase', 'index', 'worker', 'start_offset_ns', 'latency_ns', 'attempts', 'error', 'check_status'])
        w.writerows([phase, n, n % cfg['workers'], n, latency, 1, '', 'ok'] for n in range(count))
'''
        for name in ('docker', 'fdbcli', *('client-' + a['id'] for a in real['arms'])):
            filename = scratch / name
            filename.write_text(script)
            filename.chmod(0o755)
        rehearsal = copy.deepcopy(real)
        rehearsal['lock'] = str(scratch / 'dry-run.lock')
        rehearsal['cluster_file'] = str(scratch / 'synthetic.cluster')
        path(rehearsal['cluster_file']).write_bytes(path(real['cluster_file']).read_bytes())
        rehearsal['fdbcli'] = str(scratch / 'fdbcli')
        rehearsal['fdbcli_sha256'] = sha(path(rehearsal['fdbcli']))
        rehearsal['settle_seconds'] = 0  # Only fake execution; real retry keeps the full 60 seconds.
        for arm in rehearsal['arms']:
            arm['binary'] = str(scratch / ('client-' + arm['id']))
            arm['sha256'] = sha(path(arm['binary']))
        rehearsal_plan = scratch / 'plan.json'
        dump(rehearsal_plan, rehearsal)
        shutil.copy2(Path(__file__).resolve().with_name('frozen_run.py'), scratch / 'frozen_run.py')
        builds = [{'id': a['id'], 'source_revision': a['revision'], 'binary': a['binary'], 'sha256': a['sha256']}
                  for a in rehearsal['arms'] if a.get('build')]
        dump(scratch / 'build-manifest.json', {'complete': True, 'plan_sha256': sha(rehearsal_plan), 'builds': builds})
        resolve = lambda name: production_taskset if name == 'taskset' else str(scratch / name)
        fake_affinity = {f'127.0.0.1:{port}': {'cpus': cpus} for port, cpus in ((4500, [8, 20]), (4501, [9, 21]), (4502, [10, 22]))}
        with patch.object(timing, 'ROOT', scratch), patch.object(timing, 'PLAN', rehearsal_plan), \
                patch.object(timing, 'resolve_executable', side_effect=resolve), \
                patch.object(timing, 'server_affinities', return_value=fake_affinity), \
                patch.object(timing, 'snapshot', return_value={'utc': 'SYNTHETIC DRY RUN', 'loadavg': ['0', '0', '0']}):
            # Every required path must fail closed when missing; no subprocess is launched here.
            requirements = timing.executable_preflight(rehearsal, timing.binaries(rehearsal, 'slot2'))
            for name, filename in requirements.items():
                if name == 'taskset':
                    continue  # Never rename a real utility. Its real argv/affinity runs below.
                source = Path(filename)
                missing = source.with_name(source.name + '.missing')
                source.rename(missing)
                try:
                    timing.executable_preflight(rehearsal, {a['id']: {'binary': a['binary']} for a in rehearsal['arms']})
                except RuntimeError:
                    pass
                else:
                    raise AssertionError('missing executable accepted: ' + name)
                finally:
                    missing.rename(source)
            first = timing.execute(rehearsal, 'slot1', scratch / 'slot1', 'OFFLINE SYNTHETIC ONLY', None)
            assert first['reproduction']['passed'] is True  # Synthetic ratio is exactly 1.2 by construction.
            timing.execute(rehearsal, 'slot2', scratch / 'slot2', 'OFFLINE SYNTHETIC ONLY', scratch / 'slot1')
            for slot in ('slot1', 'slot2'):
                identities = timing.binaries(rehearsal, slot)
                for job in schedule(rehearsal, slot):
                    folder = scratch / slot / job['id']
                    invocation = json.loads((folder / 'invocation.json').read_text())
                    expected = timing.command(rehearsal, job, identities, folder / 'run')
                    assert invocation['argv'] == expected
                    record = json.loads((folder / 'done.json').read_text())
                    assert record['returncode'] == 0
                    assert record['statistics']['operations'] == job['workload']['iterations']
                    # Only executable/fixture/output paths differ from the real frozen command.
                    production = timing.command(real, job, real_identities, Path('/REAL_OUTPUT') / job['id'] / 'run')
                    normalized = list(expected)
                    normalized[3] = real_identities[job['arm']]['binary']
                    normalized[normalized.index('--cluster-file') + 1] = str(path(real['cluster_file']))
                    normalized[normalized.index('--output') + 1] = str(Path('/REAL_OUTPUT') / job['id'] / 'run')
                    assert normalized == production
                    records.append({'slot': slot, 'id': job['id'], 'returncode': record['returncode'], 'production_argv': production})
        execs = [json.loads(line) for line in (scratch / 'exec.jsonl').read_text().splitlines()]
        clients = [record for record in execs if record['executable'].startswith('client-')]
        clis = [record for record in execs if record['executable'] == 'fdbcli']
        assert len(clients) == len(records) == 248
        assert len(clis) == 8  # Status, range-before, clear, range-after in both slots.
        assert all(record['executable'] in {'docker', 'fdbcli'} or record['executable'].startswith('client-') for record in execs)
        report = {'valid': True, 'scope': 'OFFLINE SYNTHETIC ONLY; no real client binary or FDB/Docker connection executed',
                  'production_plan_sha256': frozen_plan_sha, 'estimators_schedule_counts_arms_unchanged': True,
                  'real_executable_preflight': real_executables, 'synthetic_client_processes': len(clients),
                  'synthetic_fdbcli_processes': len(clis), 'synthetic_docker_processes': sum(r['executable'] == 'docker' for r in execs),
                  'actual_taskset_processes': 248, 'all_exact_production_argv_validated_modulo_only_stub_fixture_output_paths': True,
                  'complete_controller_paths': ['slot1: 32/32, frozen validators, cleanup, summary, reproduction gate',
                                                'slot2: 216/216, slot1 prerequisite, frozen validators, cleanup, summary'],
                  'negative_availability_tests': 'each stubbed external executable removed in turn; all refused before launch',
                  'production_argv_sha256': hashlib.sha256(json.dumps(records, sort_keys=True).encode()).hexdigest()}
        print(json.dumps(report, indent=2))
    assert sha(PLAN) == frozen_plan_sha


if __name__ == '__main__':
    main()
