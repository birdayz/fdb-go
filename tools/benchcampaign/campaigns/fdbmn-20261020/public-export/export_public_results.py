#!/usr/bin/env python3
"""Copy this campaign's retained estimates into eight public aggregate files.

This is an I/O/redaction parameterization of the original private exporter
(SHA256 3a06fa2ae9c3df8e03a1c47ff6231e316eca9f5d5c8f796748a897cae53dc93f).
It does not run the analyzer, bootstrap, or any other estimator. The report's
editorial narrative and separate raw/historical manifests are not generated.
"""
import argparse
import csv
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import tempfile


CAMPAIGN = "campaigns/fdbmn-20261020"
ANALYSIS = "analysis/measured-492817"
AGGREGATE_FILES = (
    "analysis/M1-comparisons.csv",
    "analysis/M1-absolutes.csv",
    "analysis/M1-historical.csv",
    "analysis/M2-comparisons.csv",
    "analysis/M2-absolutes.csv",
    "analysis/M2-historical.csv",
    "analysis/coverage.json",
    "analysis/method.json",
)


def relative_path(value):
    path = PurePosixPath(value)
    if (not value or path.is_absolute() or str(path) != value
            or ".." in path.parts or "\\" in value or "\x00" in value):
        raise ValueError("input paths must be normalized relative POSIX paths")
    return path


class VerifiedInputs:
    """Require a published digest for every private input before reading it."""

    def __init__(self, perf_home, manifest):
        self.root = Path(perf_home).resolve(strict=True)
        self.digests = {}
        for line in Path(manifest).read_text(encoding="utf-8").splitlines():
            match = re.fullmatch(r"([0-9a-f]{64})  (.+)", line)
            if not match:
                raise ValueError("invalid input manifest line")
            digest, name = match.groups()
            relative_path(name)
            if name in self.digests:
                raise ValueError("duplicate input manifest path")
            self.digests[name] = digest
        if not self.digests:
            raise ValueError("empty input manifest")

    def path(self, name):
        relative_path(name)
        if name not in self.digests:
            raise ValueError(f"unlisted input: {name}")
        path = (self.root / name).resolve(strict=True)
        if not path.is_relative_to(self.root):
            raise ValueError("input resolves outside FDB_PERF_HOME")
        return path

    def digest(self, name):
        with self.path(name).open("rb") as stream:
            actual = hashlib.file_digest(stream, "sha256").hexdigest()
        if actual != self.digests[name]:
            raise ValueError(f"input SHA256 mismatch: {name}")
        return actual

    def verify_all(self):
        for name in sorted(self.digests):
            self.digest(name)

    def text(self, name):
        data = self.path(name).read_bytes()
        if hashlib.sha256(data).hexdigest() != self.digests[name]:
            raise ValueError(f"input SHA256 mismatch: {name}")
        return data.decode("utf-8")

    def load(self, name):
        return json.loads(self.text(name))


def validate_public(files, redactions):
    tokens = []
    for key in ("identifiers", "names"):
        values = redactions.get(key)
        if not isinstance(values, list) or not all(isinstance(v, str) and v for v in values):
            raise ValueError(f"private redactions require a string list: {key}")
        tokens.extend(values)
    if not tokens:
        raise ValueError("private redactions must not be empty")
    for name, data in files.items():
        if any(token in data for token in tokens):
            raise ValueError(f"private identifier/name in output: {name}")
        if re.search(r"/(?:home|Users|root|srv)/|BEGIN .*PRIVATE KEY|hcloud_[A-Za-z0-9]", data):
            raise ValueError(f"private path/credential in output: {name}")
        if re.search(r"(?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?![\d.])", data):
            raise ValueError(f"IPv4-like address in output: {name}")
        if not data.endswith("\n") or any(line.rstrip() != line for line in data.splitlines()):
            raise ValueError(f"invalid output whitespace: {name}")
        if name.endswith(".json"):
            json.loads(data)
        if name.endswith(".csv"):
            rows = list(csv.reader(io.StringIO(data)))
            if not rows or not all(len(row) == len(rows[0]) for row in rows):
                raise ValueError(f"invalid CSV width: {name}")
    # Match substrings, including identifiers glued into resource names.
    if not all(any(token in "node" + item + "suffix" for token in tokens) for item in tokens):
        raise ValueError("redaction substring control failed")


def aggregate_files(inputs, campaign=CAMPAIGN, analysis=ANALYSIS):
    relative_path(campaign)
    relative_path(analysis)
    C = PurePosixPath(campaign)
    A = C / analysis
    files = {}

    def load(path):
        return inputs.load(str(path))

    def sha(path):
        return inputs.digest(str(path))

    def dump(name, value):
        files[str(name)] = json.dumps(value, indent=2, sort_keys=True) + '\n'

    def csv_dump(name, rows):
        fields = list(dict.fromkeys(k for row in rows for k in row))
        stream = io.StringIO(newline='')
        writer = csv.DictWriter(stream, fieldnames=fields, lineterminator='\n')
        writer.writeheader()
        for row in rows:
            writer.writerow({k:json.dumps(v,sort_keys=True,separators=(',',':')) if isinstance(v,(dict,list)) else v for k,v in row.items()})
        files[str(name)] = stream.getvalue()

    # The aggregate transformations below retain the original exporter's
    # selection, field order, stored values, and JSON/CSV serialization.
    m1_rows=[];m1_abs=[];m1_history=[];m2_rows=[];m2_abs=[];m2_history=[];coverage={}
    for phase in ('M1w1','M1w8','M1w64'):
        cases=load(A/phase/'summary.json')['workloads'];coverage[phase]={}
        for case,x in cases.items():
            clean=x['status']=='clean';coverage[phase][case]={'analysis_status':x['status'],'R4_verdict':'clean' if clean else x['r4_verdict']}
            if not clean:continue
            m1_rows += [dict(phase=phase,**c) for c in x['comparisons']]
            m1_abs += [dict(phase=phase,**r)for r in csv.DictReader(io.StringIO(inputs.text(str(A/phase/case/'denom-cgo/absolute.csv'))))]
            for r in x['historical']['records']:
                old,now=r['historical'],r['current'];m1_history.append({'phase':phase,'workload':case,'metric':r['metric'],'current_numerator':r['current_numerator'],'current_denominator':r['current_denominator'],'prior_numerator':'prior-final-Go','prior_denominator':'prior-CGo','prior_ratio':old['ratio'],'prior_ci95_low':old['ci95_low'],'prior_ci95_high':old['ci95_high'],'current_ratio':now['ratio'],'current_ci95_low':now['ci95_low'],'current_ci95_high':now['ci95_high'],'interpretation':'different cohorts; descriptive only; no pooled blocks or ratio-of-ratios'})
    coverage['M1w32']={w:{'analysis_status':'unexecuted-for-budget','R4_verdict':'not run'}for w in ('grv','get100','get1000','range100','set100','batchset50','batchset50x1000')}
    for phase in ('M2rl','M2skip'):
        report=load(A/phase/'report.json');coverage[phase]={}
        for case,x in report['cases'].items():
            clean=x['status']=='ok';coverage[phase][case]={'analysis_status':'clean' if clean else 'N/A','R4_verdict':'clean' if clean else 'contaminated'}
            if not clean:continue
            m2_rows += [dict(phase=phase,metric_role='audit'if c['metric']in('rows/op','prefix-B')else'performance',**c)for c in report['comparisons']if c['case']==case]
            for arm,metrics in x['absolutes'].items():
                for metric,value in metrics.items():m2_abs.append(dict(phase=phase,case=case,arm=arm,metric=metric,**value))
            for r in x['historical']['records']:
                old,now=r['historical'],r['current'];m2_history.append({'phase':phase,'case':case,'metric':r['metric'],'prior_placement_fraction':x['historical']['historical_placement']['placement_fraction'],'current_placement_fraction':x['placement_fraction'],'prior_ratio':old['gm'],'prior_nominal_95_interval':old['nominal_95_interval'],'current_ratio':now['gm'],'current_nominal_95_interval':now['nominal_95_interval'],'interpretation':'different cohorts; descriptive only; no pooled blocks or ratio-of-ratios'})
    assert len(m1_rows)==14*36 and len(m1_abs)==14*3 and len(m2_rows)==12*5
    csv_dump(Path('analysis/M1-comparisons.csv'),m1_rows)
    csv_dump(Path('analysis/M1-absolutes.csv'),m1_abs)
    csv_dump(Path('analysis/M1-historical.csv'),m1_history)
    csv_dump(Path('analysis/M2-comparisons.csv'),m2_rows)
    csv_dump(Path('analysis/M2-absolutes.csv'),m2_abs)
    csv_dump(Path('analysis/M2-historical.csv'),m2_history)
    dump(Path('analysis/coverage.json'),{'planned_cases':44,'cases_in_executed_phases':37,'eligible_cases':26,'contaminated_cases':7,'unverifiable_cases':4,'budget_unexecuted_cases':7,'A1_executions':0,'phases':coverage})
    source=load(C/'sealed/provenance/source-receipt.json')
    dump(Path('analysis/method.json'),{'seed':492817,'whole_paired_block_resamples':20000,'independent_blocks_per_case':6,'interval':'nominal 95% paired-block percentile; no multiplicity adjustment','production_revisions':source['production_revisions'],'frozen_harness_revision':'d94b4c61377d173b7c6884fde426a227dab0f0bb','client_harness_compatibility':{'upstream_commit':'066920ebb8d22c5618b31ef0a56fa7674c773cdb','archive_sha256':'76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508','patch_sha256':'31bc349b7a1ef71fc5bbf300ed728fe536e161f5610a1187edc718e63fddb007','scope':'cleanup-chain drain and matching validator only; no workload/timing/pacing/numeric-estimator change'},'CPU_metric':'shutdown_process_cpu_ns_per_op = lifecycle-inclusive process E-A divided by measured transactions; not isolated steady state','Go_heap_scope':'native C++ heap excluded','M1':'three arms; 18 runs per case; all six permutations; direct final/before, final/cgo, before/cgo','M2':'two methods on current production; 12 processes and 36 testing.B mean samples per case; three repeats clustered within process','historical':'separate cohort description only; no pooled blocks, cross-cohort CI, or ratio-of-ratios','analysis_adapter_sha256':sha(C/'execution/analyze_campaign.py'),'raw_summary_sha256':sha(A/'summary.json'),'sanitization':'aggregate estimates only; no raw transactions, host traces, addresses, cloud identifiers, private paths or credentials'})
    if set(files) != set(AGGREGATE_FILES):
        raise ValueError("unexpected aggregate output population")
    return files


def export_results(perf_home, manifest, redactions_path, output, campaign=CAMPAIGN, analysis=ANALYSIS):
    output = Path(output)
    if output.exists():
        raise ValueError("output must not already exist")
    inputs = VerifiedInputs(perf_home, manifest)
    inputs.verify_all()
    redactions = inputs.load(redactions_path)
    files = aggregate_files(inputs, campaign, analysis)
    validate_public(files, redactions)
    output.parent.mkdir(parents=True, exist_ok=True)
    # Publish only after every input, aggregate, and redaction check succeeds.
    with tempfile.TemporaryDirectory(prefix="fdbmn-export-", dir=output.parent) as temporary:
        staging = Path(temporary) / "aggregate"
        staging.mkdir()
        for name, text in files.items():
            path = staging / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(text.encode("utf-8"))
        staging.rename(output)
    return {name: hashlib.sha256(text.encode("utf-8")).hexdigest() for name, text in sorted(files.items())}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--perf-home", default=os.environ.get("FDB_PERF_HOME"), help="retained archive root; defaults to FDB_PERF_HOME")
    parser.add_argument("--inputs-manifest", required=True, type=Path, help="published SHA256 manifest; paths relative to FDB_PERF_HOME")
    parser.add_argument("--private-redactions", "--redactions", required=True, help="manifest-pinned JSON input under FDB_PERF_HOME; identifiers/names string lists")
    parser.add_argument("--output", "--output-dir", required=True, type=Path, help="new output directory; never overwrites existing files")
    parser.add_argument("--input-dir", type=Path, help="campaign directory under FDB_PERF_HOME; alternative to --campaign")
    parser.add_argument("--campaign", default=CAMPAIGN, help="campaign path relative to FDB_PERF_HOME")
    parser.add_argument("--analysis", default=ANALYSIS, help="analysis path relative to the campaign")
    parser.add_argument("--aggregate-only", "--skip-report", action="store_true", help="explicit aggregate-only mode; this exporter never rewrites the narrative")
    args = parser.parse_args()
    if not args.perf_home:
        parser.error("set FDB_PERF_HOME or pass --perf-home")
    try:
        root = Path(args.perf_home).resolve(strict=True)
        campaign = args.campaign if args.input_dir is None else args.input_dir.resolve(strict=True).relative_to(root).as_posix()
        redactions = args.private_redactions
        if Path(redactions).is_absolute():
            redactions = Path(redactions).resolve(strict=True).relative_to(root).as_posix()
        hashes = export_results(root, args.inputs_manifest, redactions, args.output, campaign, args.analysis)
    except (ValueError, OSError, KeyError, AssertionError) as error:
        parser.exit(1, f"export refused: {error}\n")
    for name, digest in hashes.items():
        print(f"{digest}  {name}")


if __name__ == "__main__":
    main()
