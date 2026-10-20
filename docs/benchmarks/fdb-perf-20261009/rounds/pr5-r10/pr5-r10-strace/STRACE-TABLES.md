# R10 read1000 strace diagnostics

ONE old and ONE new process, old then new, 5,000 measured +200 warmup each. All syscall counts are process-lifetime, INCLUDING startup, executable hashing, setup, warmup and persisted validation. Normalization divides this whole-process population by 5,000 measured transactions; these are NOT measured-phase counts. Instrumented latency/CPU is not used as a timed comparison. No CGo syscall control was requested for this attribution step.

|Arm|Syscall|Calls|Errors|Calls / 5000|Errors / 5000|
|---|---|---:|---:|---:|---:|
|old|futex|66662|5511|13.332400|1.102200|
|old|futex_waitv|0|0|0.000000|0.000000|
|old|write|10569|0|2.113800|0.000000|
|old|writev|0|0|0.000000|0.000000|
|old|sendto|0|0|0.000000|0.000000|
|old|sendmsg|0|0|0.000000|0.000000|
|old|read|42093|10411|8.418600|2.082200|
|old|recvfrom|0|0|0.000000|0.000000|
|old|readv|0|0|0.000000|0.000000|
|old|recvmsg|0|0|0.000000|0.000000|
|old|recvmmsg|0|0|0.000000|0.000000|
|new|futex|61658|3989|12.331600|0.797800|
|new|futex_waitv|0|0|0.000000|0.000000|
|new|write|10579|0|2.115800|0.000000|
|new|writev|0|0|0.000000|0.000000|
|new|sendto|0|0|0.000000|0.000000|
|new|sendmsg|0|0|0.000000|0.000000|
|new|read|42095|10407|8.419000|2.081400|
|new|recvfrom|0|0|0.000000|0.000000|
|new|readv|0|0|0.000000|0.000000|
|new|recvmsg|0|0|0.000000|0.000000|
|new|recvmmsg|0|0|0.000000|0.000000|

old: measured/warmup callback retries 0/0; all 47 syscall rows sum exactly to strace total {'calls': 158027, 'errors': 15932}.

new: measured/warmup callback retries 0/0; all 47 syscall rows sum exactly to strace total {'calls': 176001, 'errors': 14405}.

Whole-process futex() new/old count ratio: 0.924935.

Whole-process write() new/old count ratio: 1.000946.

Whole-process read() new/old count ratio: 1.000048.
