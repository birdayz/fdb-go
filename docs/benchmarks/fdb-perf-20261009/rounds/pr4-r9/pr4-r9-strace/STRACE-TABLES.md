# R9 read1000 strace diagnostics

ONE old and ONE new process, old then new, 5,000 measured +200 warmup each. All syscall counts are process-lifetime, INCLUDING startup, executable hashing, setup, warmup and persisted validation. Normalization divides this whole-process population by 5,000 measured transactions; these are NOT measured-phase counts. Instrumented latency/CPU is not used as a timed comparison. No CGo syscall control was requested for this attribution step.

|Arm|Syscall|Calls|Errors|Calls / 5000|Errors / 5000|
|---|---|---:|---:|---:|---:|
|old|futex|67460|5524|13.492000|1.104800|
|old|futex_waitv|0|0|0.000000|0.000000|
|old|read|42081|10412|8.416200|2.082400|
|old|recvfrom|0|0|0.000000|0.000000|
|old|readv|0|0|0.000000|0.000000|
|old|recvmsg|0|0|0.000000|0.000000|
|old|recvmmsg|0|0|0.000000|0.000000|
|new|futex|65398|4282|13.079600|0.856400|
|new|futex_waitv|0|0|0.000000|0.000000|
|new|read|42087|10410|8.417400|2.082000|
|new|recvfrom|0|0|0.000000|0.000000|
|new|readv|0|0|0.000000|0.000000|
|new|recvmsg|0|0|0.000000|0.000000|
|new|recvmmsg|0|0|0.000000|0.000000|

old: measured/warmup callback retries 0/0; all 47 syscall rows sum exactly to strace total {'calls': 159788, 'errors': 15945}.

new: measured/warmup callback retries 0/0; all 47 syscall rows sum exactly to strace total {'calls': 156975, 'errors': 14703}.

Whole-process futex() new/old count ratio: 0.969434.

Whole-process read() new/old count ratio: 1.000143.
