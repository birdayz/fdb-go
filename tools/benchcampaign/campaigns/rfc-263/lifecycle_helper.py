"""CPU-gate lifecycle guard using the harness producer's drained fixpoint."""


def validate_lifecycle(summary):
    life = summary.get('lifecycle', {})
    if (life.get('complete') is not True or life.get('measurement_barrier_complete') is not True or
            life.get('database_closed') is not True or
            life.get('network_stopped') is not summary['backend']['native_library']):
        raise ValueError('incomplete lifecycle accounting')
    for name in ('measurement_drain', 'shutdown_drain'):
        drain = life[name]
        before, after = drain['before'], drain['after']
        if (drain['complete'] is not True or not 2 <= drain['rounds'] <= 16 or drain['wall_ns'] <= 0 or
                any(type(v) is not int or v < 0 for counts in (before, after) for v in counts.values()) or
                any(after[k] < before[k] for k in before) or
                after['gc_cycles'] < before['gc_cycles'] + drain['rounds'] or
                after['finalizers_executed'] != after['finalizers_queued'] or
                after['cleanups_executed_estimate'] != after['cleanups_queued_estimate']):
            raise ValueError('incomplete finalizer fixpoint: ' + name)
    snapshots = [summary['stats_after_identity'], summary['stats_before'], summary['stats_after'],
                 life['stats_after_measurement_drain'], life['stats_after_shutdown']]
    required = {'process_user_cpu_us', 'process_system_cpu_us', 'go_total_alloc_bytes', 'go_mallocs',
                'go_gc_cycles', 'go_gc_pause_ns', 'process_high_water_rss_kb'}
    if any(set(s) != required or any(type(v) is not int or v < 0 for v in s.values()) for s in snapshots):
        raise ValueError('invalid lifecycle resource counter')
    if any(after[k] < before[k] for before, after in zip(snapshots, snapshots[1:]) for k in required):
        raise ValueError('decreasing lifecycle resource counter')
    if sum(snapshots[-1][k] - snapshots[0][k] for k in ('process_user_cpu_us', 'process_system_cpu_us')) <= 0:
        raise ValueError('nonpositive shutdown process CPU after identity')
    for drain, snapshot in [('measurement_drain', snapshots[3]), ('shutdown_drain', snapshots[4])]:
        if snapshot['go_gc_cycles'] < life[drain]['after']['gc_cycles']:
            raise ValueError('resource snapshot precedes finalizer drain')
    return life
