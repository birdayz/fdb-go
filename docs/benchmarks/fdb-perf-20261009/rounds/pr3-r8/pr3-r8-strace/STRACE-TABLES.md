# R8 read1000 strace diagnostics

ONE old and ONE new process, old then new, 5,000 measured +200 warmup each. All syscall counts are process-lifetime, INCLUDING startup, executable hashing, setup, warmup and persisted validation. Normalization divides this whole-process population by 5,000 measured transactions; these are NOT measured-phase counts. Instrumented latency/CPU is not used as a timed comparison. No CGo syscall control was requested for this attribution step.

|Arm|Syscall|Calls|Errors|Calls / 5000|Errors / 5000|
|---|---|---:|---:|---:|---:|
|old|read|42061|10414|8.412200|2.082800|
|old|recvfrom|0|0|0.000000|0.000000|
|old|readv|0|0|0.000000|0.000000|
|old|recvmsg|0|0|0.000000|0.000000|
|old|recvmmsg|0|0|0.000000|0.000000|
|new|read|21251|10413|4.250200|2.082600|
|new|recvfrom|0|0|0.000000|0.000000|
|new|readv|0|0|0.000000|0.000000|
|new|recvmsg|0|0|0.000000|0.000000|
|new|recvmmsg|0|0|0.000000|0.000000|

old: measured/warmup callback retries 0/0; all 47 syscall rows sum exactly to strace total {'calls': 155424, 'errors': 15964}.

new: measured/warmup callback retries 0/0; all 47 syscall rows sum exactly to strace total {'calls': 138881, 'errors': 15725}.

Whole-process read() new/old count ratio: 0.505242.
