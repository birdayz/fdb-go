# R3 instrumentation — separate diagnostic runs

One extra old run and one extra CGo run for each instrument,3000 measured transactions+200 warmup,one worker. Counts cover the ENTIRE child process (setup/seed,warmup,measurement,validation/cleanup), not the measured phase. Counts/3000 below are whole-process counts normalized by measured transactions, NOT exact measured-phase event rates. Instrumentation overhead changes behavior; these runs are not unprofiled timing results or a counterbalanced performance comparison.

> UNRESOLVED ACCOUNTING: perf task-clock old6.197840s/CGo5.467520s exceeds whole GNU-time CPU old4.49s/CGo4.17s; measured-phase getrusage is3.924618s/3.584470s. Phase scope alone does not explain task-clock versus whole GNU time. Do not treat task-clock as interchangeable CPU accounting or use instrumented timings for performance claims. See ../old-batch-r3-build/R3-SCOPE.md.

## perf stat

|Event|Unit|Old whole-process|CGo whole-process|Old /3000|CGo /3000|
|---|---|---:|---:|---:|---:|
|task-clock|msec|6197.840000|5467.520000|2.065947|1.822507|
|cycles|count|3630503110.000000|4132662641.000000|1210167.703333|1377554.213667|
|instructions|count|3562449930.000000|3544795558.000000|1187483.310000|1181598.519333|
|context-switches|count|40408.000000|39109.000000|13.469333|13.036333|
|cpu-migrations|count|51.000000|11.000000|0.017000|0.003667|
|page-faults|count|23619.000000|63049.000000|7.873000|21.016333|

Raw event runtime/scaling columns are retained in perf-stat.csv and instruments.json. CPU events are guest-process measurements, not physical-host isolation evidence.

## strace -f -c syscall census

Zero means the tracer reported no call of that syscall in this process, not an unobserved client-internal counter. Syscall elapsed time is tracer-distorted and is not presented as client CPU attribution.

|Syscall|Old calls|CGo calls|Old calls/3000|CGo calls/3000|Old errors|CGo errors|
|---|---:|---:|---:|---:|---:|---:|
|write|7032|35|2.344000|0.011667|0|0|
|writev|0|0|0.000000|0.000000|0|0|
|sendmsg|0|6528|0.000000|2.176000|0|0|
|sendto|0|0|0.000000|0.000000|0|0|
|read|26883|905|8.961000|0.301667|6514|1|
|readv|0|0|0.000000|0.000000|0|0|
|recvfrom|0|13054|0.000000|4.351333|0|6526|
|recvmsg|0|0|0.000000|0.000000|0|0|
|epoll_wait|0|46000|0.000000|15.333333|0|0|
|epoll_pwait|13360|1694|4.453333|0.564667|0|0|
|epoll_pwait2|0|0|0.000000|0.000000|0|0|
|futex|48881|22219|16.293667|7.406333|3645|1122|
|futex_waitv|0|0|0.000000|0.000000|0|0|
|nanosleep|45530|45432|15.176667|15.144000|0|0|
|clock_nanosleep|0|0|0.000000|0.000000|0|0|
|access|1|2|0.000333|0.000667|1|2|
|arch_prctl|2|2|0.000667|0.000667|0|0|
|brk|3|1249|0.001000|0.416333|0|0|
|clone|9|0|0.003000|0.000000|0|0|
|clone3|0|8|0.000000|0.002667|0|0|
|close|13|36|0.004333|0.012000|0|0|
|connect|1|1|0.000333|0.000333|1|1|
|epoll_create1|1|2|0.000333|0.000667|0|0|
|epoll_ctl|10|7179|0.003333|2.393000|7|7|
|eventfd2|1|3|0.000333|0.001000|0|0|
|execve|2|2|0.000667|0.000667|0|0|
|fcntl|34|36|0.011333|0.012000|0|0|
|fstat|3|18|0.001000|0.006000|0|0|
|getpeername|1|0|0.000333|0.000000|0|0|
|getpid|71|791|0.023667|0.263667|0|0|
|getrandom|2|3|0.000667|0.001000|0|0|
|getrusage|2|2|0.000667|0.000667|0|0|
|getsockname|1|0|0.000333|0.000000|0|0|
|getsockopt|1|1|0.000333|0.000333|0|0|
|gettid|19|71|0.006333|0.023667|0|0|
|io_setup|0|1|0.000000|0.000333|0|0|
|ioctl|0|3|0.000000|0.001000|0|0|
|lseek|0|10|0.000000|0.003333|0|0|
|madvise|2|10|0.000667|0.003333|0|0|
|mmap|87|518|0.029000|0.172667|0|0|
|mprotect|3|3312|0.001000|1.104000|0|0|
|munmap|2|9|0.000667|0.003000|0|0|
|newfstatat|1|16|0.000333|0.005333|0|0|
|openat|13|46|0.004333|0.015333|0|9|
|poll|0|1|0.000000|0.000333|0|0|
|prctl|79|44|0.026333|0.014667|0|0|
|pread64|2|4|0.000667|0.001333|0|0|
|prlimit64|3|5|0.001000|0.001667|0|0|
|readlinkat|1|1|0.000333|0.000333|0|0|
|rseq|1|10|0.000333|0.003333|0|0|
|rt_sigaction|114|114|0.038000|0.038000|0|0|
|rt_sigprocmask|29|40653|0.009667|13.551000|0|0|
|rt_sigreturn|70|768|0.023333|0.256000|1|83|
|sched_getaffinity|2|3|0.000667|0.001000|0|0|
|sched_setaffinity|1|1|0.000333|0.000333|0|0|
|sched_yield|217|72|0.072333|0.024000|0|0|
|set_robust_list|1|10|0.000333|0.003333|0|0|
|set_tid_address|1|2|0.000333|0.000667|0|0|
|setsockopt|9|1|0.003000|0.000333|0|0|
|sigaltstack|20|18|0.006667|0.006000|0|0|
|socket|1|1|0.000333|0.000333|0|0|
|stat|0|3|0.000000|0.001000|0|0|
|tgkill|70|768|0.023333|0.256000|0|0|
|timerfd_create|0|1|0.000000|0.000333|0|0|
|timerfd_settime|0|16554|0.000000|5.518000|0|0|
|uname|0|1|0.000000|0.000333|0|0|
