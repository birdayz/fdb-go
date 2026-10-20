#!/usr/bin/env python3
"""Artifact-only synthetic checks; no repository tests, acquisition, or real data."""
import hashlib
import math
import random

from r17_statistics import (BLOCKS, BOOTSTRAPS, PRIMARY, RESAMPLES,
                            circular_bootstrap, decisions, nearest_rank, validate_logs)


def expect_rejection(callback):
    try:
        callback()
    except ValueError:
        return
    raise AssertionError("invalid input was accepted")


def reference(logs, length, seed, count):
    rng = random.Random(seed)
    streams = [[] for _ in PRIMARY]
    digest = hashlib.sha256()
    for _ in range(count):
        starts = [rng.randrange(BLOCKS) for _ in range(BLOCKS // length)]
        digest.update(bytes(starts))
        indices = [(start + offset) % BLOCKS for start in starts for offset in range(length)]
        assert len(indices) == 160
        for column in range(5):
            streams[column].append(math.exp(math.fsum(logs[index][column] for index in indices) / BLOCKS))
    intervals = []
    for column in streams:
        column.sort()
        intervals.append((column[max(0, math.ceil(.005 * count) - 1)],
                          column[max(0, math.ceil(.995 * count) - 1)]))
    return digest.hexdigest(), intervals


def main():
    assert nearest_rank(list(range(100_000)), .005) == 499
    assert nearest_rank(list(range(100_000)), .995) == 99499
    assert nearest_rank([7], .005) == 7
    base = [((index % 19) - 9) / 32 for index in range(BLOCKS)]
    logs = [[value, value, -value, 2 * value, -.125] for value in base]
    for length, seed in BOOTSTRAPS:
        result = circular_bootstrap(logs, length, seed, 257)
        again = circular_bootstrap(logs, length, seed, 257)
        assert result == again, "same seed/input must give exact deterministic output"
        expected_digest, intervals = reference(logs, length, seed, 257)
        assert result['index_stream_sha256'] == expected_digest
        assert result['distribution_sha256_float64_be_before_sort'][0] == result['distribution_sha256_float64_be_before_sort'][1]
        for actual, expected in zip(result['intervals'], intervals):
            assert math.isclose(actual['ci99_low'], expected[0], rel_tol=1e-14, abs_tol=0)
            assert math.isclose(actual['ci99_high'], expected[1], rel_tol=1e-14, abs_tol=0)
        assert result['intervals'][4] == {'ci99_low': math.exp(-.125), 'ci99_high': math.exp(-.125)}
    # Exercise the exact 100k protocol with constants spanning pass/equality/fail.
    constant = [[math.log(.8), math.log(.9), 0., math.log(1.1), math.log(.95)] for _ in range(BLOCKS)]
    full = [circular_bootstrap(constant, length, seed) for length, seed in BOOTSTRAPS]
    expected_streams = {
        8: 'ec7459dd6e99f37bdb78d58d3885696005427c4231d4564fc61539bf4b0b49f2',
        16: '16c49e7439ee8b8607551b8cebdcfb48390caf1f46421007314d37f88fb5c959',
    }
    for result in full:
        assert result['index_stream_sha256'] == expected_streams[result['length']]
        assert result['resamples'] == RESAMPLES
        assert result['nearest_rank_zero_based_indices'] == [499, 99499]
        for col, interval in enumerate(result['intervals']):
            assert interval['ci99_low'] == interval['ci99_high']
            assert math.isclose(interval['ci99_low'], math.exp(constant[0][col]), rel_tol=1e-14)
    rows = decisions(constant, full)
    assert [r['improvement_confirmed'] for r in rows] == [True, True, False, False, True]
    assert not any(row['improvement_confirmed'] for row in decisions(constant, full, validity=False))
    altered = [dict(result, intervals=[dict(interval) for interval in result['intervals']]) for result in full]
    altered[1]['intervals'][0]['ci99_high'] = 1.0
    assert not decisions(constant, altered)[0]['improvement_confirmed'], "L16 cannot rescue/bypass the upper<1 gate"
    for malformed in (logs[:-1], logs + [logs[0]], [[0] * 4 for _ in logs], [[float('nan')] * 5 for _ in logs]):
        expect_rejection(lambda malformed=malformed: validate_logs(malformed))
    expect_rejection(lambda: circular_bootstrap(logs, 7, 797707))
    expect_rejection(lambda: decisions(logs, [dict(result, resamples=10) for result in full]))
    print('PASS: synthetic artifact-only bootstrap reference, joint draws, wrapping, shape, determinism, nearest rank, both gates, 2x100000 protocol.')
    for result in full:
        print(f"L{result['length']} seed={result['seed']} index_stream_sha256={result['index_stream_sha256']}")


if __name__ == '__main__':
    main()
