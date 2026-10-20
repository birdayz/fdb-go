#!/usr/bin/env python3
"""Controller-only sequential orchestrator for hcloud-multinode-vpc-20261014.
Starts node_sampler.py on node1..node3 before each phase, takes client before/after
receipts, executes the sealed phase on the client VM, stops node samplers, pulls
all artifacts, and runs build_reconcile_input.py + reconcile_telemetry.py.
"""
import argparse, datetime, hashlib, json, pathlib, shlex, subprocess, sys, time

C = pathlib.Path('${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014')
hosts = json.loads((C/'work/provision/hosts-client-node1-node2-node3.json').read_text())
kh = str(C/'private/cloud-init/known_hosts')
key = str(pathlib.Path.home()/'.ssh/id_rsa_old')
DEADLINE = (C/'provenance/teardown-deadline-epoch.txt').read_text().strip()
TESTINGB_MANIFEST_SHA = hashlib.sha256((C/'plans/testingb/manifest.json').read_bytes()).hexdigest()
CLIENT_MANIFEST_SHA = hashlib.sha256((C/'plans/client/manifest.json').read_bytes()).hexdigest()
CLUSTER_SHA = hashlib.sha256((C/'cluster-ssd/fdb.cluster').read_bytes()).hexdigest()

def ssh_cmd(role):
    info = hosts[role]
    return ['ssh', '-i', key, '-o', 'IdentitiesOnly=yes', '-o', 'BatchMode=yes',
            '-o', 'StrictHostKeyChecking=yes', '-o', f'UserKnownHostsFile={kh}',
            '-o', f'HostKeyAlias={info["name"]}', f'root@{info["public_ipv4"]}']

def remote(role, cmd, **kw):
    return subprocess.run([*ssh_cmd(role), cmd], **kw)

def start_samplers(phase, sampler_phase=None):
    sp = sampler_phase or phase
    for node in ['node1', 'node2', 'node3']:
        unit = f'fdb-node-sampler-{phase}'
        cmd = f'systemctl stop {unit} || true; systemctl reset-failed {unit} || true; rm -rf /srv/fdb-bench/telemetry/{phase} && mkdir -p /srv/fdb-bench/telemetry && systemd-run --unit={unit} --property=CPUAffinity=0-1 python3 -B /srv/fdb-bench/tools/node_sampler.py run --host {node} --phase {sp} --output-dir /srv/fdb-bench/telemetry/{phase} --max-seconds 9000'
        remote(node, cmd, capture_output=True, text=True, check=True)
    # Wait for receipt-start.json on all 3 nodes
    for node in ['node1', 'node2', 'node3']:
        for _ in range(30):
            r = remote(node, f'test -f /srv/fdb-bench/telemetry/{phase}/receipt-start.json')
            if r.returncode == 0:
                break
            time.sleep(0.5)
        else:
            raise RuntimeError(f'sampler on {node} did not write receipt-start.json')
    return ['node1', 'node2', 'node3']

def stop_samplers_and_collect(phase, nodes):
    unit = f'fdb-node-sampler-{phase}'
    for node in nodes:
        remote(node, f'systemctl stop {unit} || true; for i in $(seq 1 30); do test -f /srv/fdb-bench/telemetry/{phase}/receipt-stop.json && break; sleep 0.5; done', check=True)
        local_dir = C / 'telemetry' / phase / node
        local_dir.parent.mkdir(parents=True, exist_ok=True)
        p = remote(node, f'tar -C /srv/fdb-bench/telemetry/{phase} -czf - .', stdout=subprocess.PIPE, check=True)
        local_dir.mkdir(parents=True, exist_ok=True)
        subprocess.run(['tar', '-C', str(local_dir), '-xzf', '-'], input=p.stdout, check=True)

def take_client_receipt(phase, kind):
    remote('client', f'mkdir -p /srv/fdb-bench/telemetry/{phase} && taskset -c 0,1 python3 -B /srv/fdb-bench/tools/node_sampler.py receipt --host client --phase {phase} --kind {kind} --cluster-file /srv/fdb-bench/cluster/fdb.cluster --output /srv/fdb-bench/telemetry/{phase}/client-{kind}.json', check=True)

def run_phase(phase):
    print(f'=== STARTING PHASE {phase} ===', flush=True)
    samplers = start_samplers(phase)
    take_client_receipt(phase, 'before')
    try:
        if phase in ('M2rl', 'M2skip'):
            placement_tpl = json.dumps([
                'python3', '-B', '/srv/fdb-bench/tools/placement_command.py',
                '--store', '{store}', '--case', '{case}', '--when', '{when}',
                '--preset', '{preset}', '--layout', '{layout}', '--record-format', '{record_format}',
                '--cluster-file', '{cluster_file}', '--expect-cluster-sha256', CLUSTER_SHA,
                '--output', '{output}', '--placementprobe', '/srv/fdb-bench/bin/placementprobe-cgo',
                '--fdbcli', '/usr/bin/fdbcli', '--fixture-receipts', '/srv/fdb-bench/provenance/fixture-receipts.json',
                '--work-dir', f'/srv/fdb-bench/work/placement-{phase}'
            ])
            cmd = [
                'taskset', '--cpu-list', '0-1', 'python3', '-B', '/srv/fdb-bench/tools/bench_runner_mn.py', 'run',
                '--manifest', '/srv/fdb-bench/plans/testingb/manifest.json',
                '--manifest-sha256', TESTINGB_MANIFEST_SHA,
                '--phase', phase,
                '--protocol', '/srv/fdb-bench/protocol.json',
                '--orders', '/srv/fdb-bench/orders.json',
                '--layout', '/srv/fdb-bench/cluster-ssd/layout.json',
                '--vpc-cidr', '10.88.0.0/24',
                '--scripts', '/srv/fdb-bench/tools/clientbench/scripts',
                '--source-receipt', '/srv/fdb-bench/provenance/source-receipt.json',
                '--placement-command', placement_tpl,
                '--output', f'/srv/fdb-bench/results/{phase}',
                '--lifetime-deadline', DEADLINE
            ]
            proc = remote('client', shlex.join(cmd))
            print(f'Phase {phase} runner rc={proc.returncode}', flush=True)
        else:
            # M1 phase: run each sealed campaign sequentially via render_run
            sys.path.insert(0, str(C/'tools'))
            import seal_multinode as sm
            manifest = json.loads((C/'plans/client/manifest.json').read_text())
            campaigns = [c for c in manifest['campaigns'] if c['phase'] == phase]
            for c in campaigns:
                argv = ['taskset', '--cpu-list', '0-1', *sm.render_run(c['run'], CLIENT_MANIFEST_SHA, DEADLINE)]
                print(f'Running {phase}/{c["workload"]}...', flush=True)
                proc = remote('client', shlex.join(argv))
                print(f'{phase}/{c["workload"]} rc={proc.returncode}', flush=True)
                if proc.returncode != 0:
                    break
    finally:
        take_client_receipt(phase, 'after')
        stop_samplers_and_collect(phase, samplers)

    # Copy client results and client receipts to controller and push node telemetry to client for R4
    res_sub = f'results/{phase}' if phase in ('M2rl', 'M2skip') else f'results/client/{phase}'
    p_res = remote('client', f'tar -C /srv/fdb-bench -czf - {res_sub} telemetry/{phase}', stdout=subprocess.PIPE, check=True)
    subprocess.run(['tar', '-C', str(C), '-xzf', '-'], input=p_res.stdout, check=True)

    # Push collected node1..3 telemetry to client VM so build_reconcile_input + reconcile_telemetry can run on client VM
    p_tel = subprocess.Popen(['tar', '-C', str(C), '-czf', '-', f'telemetry/{phase}'], stdout=subprocess.PIPE)
    remote('client', 'tar -C /srv/fdb-bench -xzf -', stdin=p_tel.stdout, check=True)
    p_tel.wait()

    m_path = '/srv/fdb-bench/plans/testingb/manifest.json' if phase in ('M2rl', 'M2skip') else '/srv/fdb-bench/plans/client/manifest.json'
    m_sha = TESTINGB_MANIFEST_SHA if phase in ('M2rl', 'M2skip') else CLIENT_MANIFEST_SHA
    r4_cmd = f'''set -euo pipefail
R=/srv/fdb-bench
mkdir -p "$R/reconcile/{phase}"
python3 -B "$R/tools/build_reconcile_input.py" \
  --manifest {m_path} --manifest-sha256 {m_sha} --phase {phase} \
  --protocol "$R/protocol.json" --results "$R/{res_sub}" --layout "$R/cluster-ssd/layout.json" \
  --client-receipt "$R/telemetry/{phase}/client-before.json" \
  --client-receipt "$R/telemetry/{phase}/client-after.json" \
  --node node1="$R/telemetry/{phase}/node1" \
  --node node2="$R/telemetry/{phase}/node2" \
  --node node3="$R/telemetry/{phase}/node3" \
  --output "$R/reconcile/{phase}/input.json"
set +e
python3 -B "$R/tools/reconcile_telemetry.py" \
  --input "$R/reconcile/{phase}/input.json" \
  --output "$R/reconcile/{phase}/report.json"
rc=$?
set -e
echo "R4 reconcile {phase} rc=$rc"
'''
    r4_out = remote('client', r4_cmd, capture_output=True, text=True, check=True)
    print(r4_out.stdout.strip(), flush=True)
    p_r4 = remote('client', f'tar -C /srv/fdb-bench -czf - reconcile/{phase}', stdout=subprocess.PIPE, check=True)
    subprocess.run(['tar', '-C', str(C), '-xzf', '-'], input=p_r4.stdout, check=True)

def enable_dd_for_m1():
    print('=== Enabling DD, settling, and running R1+R3+R4 M1w1/grv diagnostic before M1 ===', flush=True)
    cmd = r'''set -euo pipefail
R=/srv/fdb-bench
CF="$R/cluster/fdb.cluster"
rm -f "$R/provenance/m1-roles-settled.json"
/usr/bin/fdbcli -C "$CF" --exec "datadistribution on" > "$R/provenance/m1-dd-on.txt"
sleep 2
while true; do
  st=$(/usr/bin/fdbcli -C "$CF" --exec "status json")
  if echo "$st" | jq -e '.cluster.data.moving_data.in_flight_bytes == 0 and .cluster.data.moving_data.in_queue_bytes == 0' >/dev/null; then
    echo "$st" > "$R/provenance/m1-dd-settled.json"
    break
  fi
  sleep 5
done
python3 -B "$R/tools/fdb_cluster.py" check-roles --layout "$R/cluster-ssd/layout.json" --status "$R/provenance/m1-dd-settled.json" --output "$R/provenance/m1-roles-settled.json"
'''
    remote('client', cmd, check=True)
    # Seal and run the 1-case R1+R3+R4 diagnostic (M1w1/grv, 2 blocks, 32 ops)
    seal_diag_m1 = r'''set -euo pipefail
R=/srv/fdb-bench
T="$R/tools"
P0="$R/provenance/p0"
CF="$R/cluster/fdb.cluster"
D="$R/diagnostic"
rm -rf "$D/plans/client" "$D/results/client" "$D/telemetry-m1" "$D/reconcile-m1"
mkdir -p "$D/plans" "$D/telemetry-m1" "$D/reconcile-m1"
COMMON_PINS=(
  --pin "$T/bench_runner_mn.py" --pin "$T/client_campaign_mn.py" --pin "$T/cluster_gate.py"
  --pin "$T/fdb_cluster.py" --pin "$T/mn_common.py" --pin "$T/seal_multinode.py"
  --pin "$T/placement_receipt.py" --pin "$T/placement_command.py" --pin "$T/node_sampler.py"
  --pin "$T/reconcile_telemetry.py" --pin "$T/build_reconcile_input.py" --pin "$T/bench_runner.py"
  --pin "$T/bench_analyze.py" --pin "$T/client_campaign.py"
  --pin "$R/tools/clientbench/scripts/run.py" --pin "$R/tools/clientbench/scripts/analyze.py"
  --pin "$R/tools/clientbench/scripts/matrix_plan.py" --pin "$R/tools/clientbench/scripts/r3_plan.py"
  --pin "$R/cluster-ssd/layout.json" --pin "$R/provenance/source-receipt.json"
  --pin "$R/overlays/final/client-harness.tar.gz" --pin "$R/provenance/fixture-receipts.json"
  --pin "$P0/fixture-small.json" --pin "$P0/fixture-split.json" --pin "$P0/fixture-planskip.json"
  --pin "$R/bin/placementprobe-cgo" --pin "$R/bin/placementprobe-go"
  --pin /usr/bin/fdbcli --pin /usr/sbin/fdbserver --pin /usr/lib/libfdb_c.so
)
python3 -B "$T/seal_multinode.py" client-plans \
  --protocol "$D/protocol.json" --orders "$D/orders.json" \
  --scripts-dir "$R/tools/clientbench/scripts" --cluster-file "$CF" \
  --native-library /usr/lib/libfdb_c.so --server-binary /usr/sbin/fdbserver \
  --harness-archive "$R/overlays/final/client-harness.tar.gz" \
  --binary final="$R/bin/client-final-go" --binary cgo="$R/bin/client-final-cgo" \
  --results-dir "$D/results/client" --adapter "$T/client_campaign_mn.py" \
  --layout "$R/cluster-ssd/layout.json" --source-receipt "$R/provenance/source-receipt.json" \
  --vpc-cidr 10.88.0.0/24 --phase M1w1 "${COMMON_PINS[@]}" --output-dir "$D/plans/client"
'''
    remote('client', seal_diag_m1, check=True)
    nodes = start_samplers('diag-M1w1', sampler_phase='M1w1')
    remote('client', 'taskset -c 0,1 python3 -B /srv/fdb-bench/tools/node_sampler.py receipt --host client --phase M1w1 --kind before --cluster-file /srv/fdb-bench/cluster/fdb.cluster --output /srv/fdb-bench/diagnostic/telemetry-m1/client-before.json', check=True)
    try:
        m_sha = remote('client', 'sha256sum /srv/fdb-bench/diagnostic/plans/client/manifest.json | cut -d" " -f1', capture_output=True, text=True, check=True).stdout.strip()
        run_diag_r1 = f'''set -euo pipefail
R=/srv/fdb-bench
D="$R/diagnostic"
python3 - "$R" "$D" "{m_sha}" "{DEADLINE}" <<'INNER'
import json, pathlib, subprocess, sys
R, D = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
m_sha, deadline = sys.argv[3], sys.argv[4]
sys.path.insert(0, str(R / 'tools'))
import seal_multinode as sm
manifest = json.loads((D / 'plans/client/manifest.json').read_text())
c = manifest['campaigns'][0]
argv = ['taskset', '--cpu-list', '0-1', *sm.render_run(c['run'], m_sha, deadline)]
subprocess.run(argv, check=True)
INNER
'''
        remote('client', run_diag_r1, check=True)
    finally:
        remote('client', 'taskset -c 0,1 python3 -B /srv/fdb-bench/tools/node_sampler.py receipt --host client --phase M1w1 --kind after --cluster-file /srv/fdb-bench/cluster/fdb.cluster --output /srv/fdb-bench/diagnostic/telemetry-m1/client-after.json', check=True)
        stop_samplers_and_collect('diag-M1w1', nodes)
    p_tel = subprocess.Popen(['tar', '-C', str(C), '-czf', '-', 'telemetry/diag-M1w1'], stdout=subprocess.PIPE)
    remote('client', 'tar -C /srv/fdb-bench -xzf -', stdin=p_tel.stdout, check=True)
    p_tel.wait()
    r4_m1 = f'''set -euo pipefail
R=/srv/fdb-bench
D="$R/diagnostic"
python3 -B "$R/tools/build_reconcile_input.py" \
  --manifest "$D/plans/client/manifest.json" --manifest-sha256 {m_sha} --phase M1w1 \
  --protocol "$D/protocol.json" --results "$D/results/client/M1w1" --layout "$R/cluster-ssd/layout.json" \
  --client-receipt "$D/telemetry-m1/client-before.json" \
  --client-receipt "$D/telemetry-m1/client-after.json" \
  --node node1="$R/telemetry/diag-M1w1/node1" \
  --node node2="$R/telemetry/diag-M1w1/node2" \
  --node node3="$R/telemetry/diag-M1w1/node3" \
  --output "$D/reconcile-m1/input-M1w1.json"
python3 -B "$R/tools/reconcile_telemetry.py" \
  --input "$D/reconcile-m1/input-M1w1.json" \
  --output "$D/reconcile-m1/report-M1w1.json"
jq -e '.phase_summary.clean==1' "$D/reconcile-m1/report-M1w1.json" >/dev/null
echo "R1+R3+R4 M1w1/grv diagnostic reconciled CLEAN!"
'''
    out_r4 = remote('client', r4_m1, capture_output=True, text=True, check=True)
    print(out_r4.stdout.strip(), flush=True)
    p_pull = remote('client', 'tar -C /srv/fdb-bench -czf - diagnostic provenance/m1-dd-on.txt provenance/m1-dd-settled.json provenance/m1-roles-settled.json', stdout=subprocess.PIPE, check=True)
    subprocess.run(['tar', '-C', str(C), '-xzf', '-'], input=p_pull.stdout, check=True)

if __name__ == '__main__':
    ap = argparse.ArgumentParser()
    ap.add_argument('--phase', action='append', required=True)
    args = ap.parse_args()
    for ph in args.phase:
        if ph == 'ENABLE_DD':
            enable_dd_for_m1()
        else:
            run_phase(ph)
