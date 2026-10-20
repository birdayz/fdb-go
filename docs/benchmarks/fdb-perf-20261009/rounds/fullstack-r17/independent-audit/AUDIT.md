# Independent R17 raw/math audit

PASS:640 raw runs,160 paired blocks/workload, all800 primary paired logs and all five GMs exactly match published values. Both100000-resample joint circular bootstraps were recomputed with literal expanded160-index draws, not production window-sum code.

Maximum CI endpoint absolute difference: 0. Fixed index-stream hashes match exactly.

|Endpoint|Metric|GM NEW/CGo|L8 99%CI|L16 99%CI|Both upper<1|
|---|---|---:|---|---|---|

|1|batchset50 shutdown_process_cpu_ns_per_op|0.862468890|[0.850939904,0.874872724]|[0.850373107,0.875915973]|True|

|2|batchset50 process_cpu_ns_per_op|0.788290628|[0.776788412,0.801046647]|[0.776379802,0.801644061]|True|

|3|read1000 mean_ns|0.625737070|[0.603493643,0.650043028]|[0.600815473,0.652733177]|True|

|4|read1000 p99_ns|0.507258519|[0.466695398,0.548761000]|[0.463166449,0.549549728]|True|

|5|read1000 shutdown_process_cpu_ns_per_op|0.908661165|[0.885677309,0.933149963]|[0.883869731,0.934491223]|True|


No sealed files were modified. The whole archive digest was independently verified;3200 directly used raw files are inventoried. This is not a repetition of all host gates or all27153 archived-file checks. No new fleet, causal, or correctness claim is added.
