# R10B read1000 strace diagnostics

ONE old and ONE new process, old then new, 5,000 measured +200 warmup each. All syscall counts are process-lifetime, INCLUDING startup, executable hashing, setup, warmup and persisted validation. Normalization divides this whole-process population by 5,000 measured transactions; these are NOT measured-phase counts. Instrumented latency/CPU is not used as a timed comparison. No CGo syscall control was requested for this attribution step.

|Arm|Syscall|Calls|Errors|Calls / 5000|Errors / 5000|
|---|---|---:|---:|---:|---:|
|old|futex|66434|5986|13.286800|1.197200|
|old|futex_waitv|0|0|0.000000|0.000000|
|old|write|10573|0|2.114600|0.000000|
|old|writev|0|0|0.000000|0.000000|
|old|sendto|0|0|0.000000|0.000000|
|old|sendmsg|0|0|0.000000|0.000000|
|old|read|42096|10413|8.419200|2.082600|
|old|recvfrom|0|0|0.000000|0.000000|
|old|readv|0|0|0.000000|0.000000|
|old|recvmsg|0|0|0.000000|0.000000|
|old|recvmmsg|0|0|0.000000|0.000000|
|new|futex|63826|4122|12.765200|0.824400|
|new|futex_waitv|0|0|0.000000|0.000000|
|new|write|10569|0|2.113800|0.000000|
|new|writev|0|0|0.000000|0.000000|
|new|sendto|0|0|0.000000|0.000000|
|new|sendmsg|0|0|0.000000|0.000000|
|new|read|42087|10413|8.417400|2.082600|
|new|recvfrom|0|0|0.000000|0.000000|
|new|readv|0|0|0.000000|0.000000|
|new|recvmsg|0|0|0.000000|0.000000|
|new|recvmmsg|0|0|0.000000|0.000000|

old: measured/warmup callback retries 0/0; all 47 syscall rows sum exactly to strace total {'calls': 158130, 'errors': 16414}.

new: measured/warmup callback retries 0/0; all 47 syscall rows sum exactly to strace total {'calls': 183312, 'errors': 14547}.

Whole-process futex() new/old count ratio: 0.960743.

Whole-process write() new/old count ratio: 0.999622.

Whole-process read() new/old count ratio: 0.999786.
