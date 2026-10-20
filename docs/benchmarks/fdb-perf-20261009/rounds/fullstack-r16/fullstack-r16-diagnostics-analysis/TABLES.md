# R16 offline instrument diagnostics

wholeprocess/N incl startup/identity/fixture/warmup/drains/validation/shutdown; NOT phase counts.

phase-window syscall ENTRY counts in [end(B getrusage), start(C getrusage)), excluding A..E marker calls; divided by 200 measured transactions.

samples.csv has a relative epoch taken just after B, with an unknown submillisecond gap. No exact per-transaction boundary alignment is claimed. Do not interpret the merged syscall timeline as transaction ownership.

Instrumentation is diagnostic only: no latency, throughput or CPU-performance claims are made. Raw syscall elapsed values and perf task-clock are retained for provenance, not used as timed comparisons.

The short trace covers only the explicit trace filter; strace-c covers all traced syscalls. Whole-process and measured-window populations are separate. Boundary-crossing calls are counted by ENTRY, not completion or elapsed-time apportionment. Unknown ends are marked possible overlaps.


## strace-summary-wholeprocess-over-5000

|Syscall|NEW calls|CGo calls|NEW normalized|CGo normalized|

|---|---:|---:|---:|---:|

|access|1|2|0.000200|0.000400|

|arch_prctl|2|2|0.000400|0.000400|

|brk|3|19|0.000600|0.003800|

|clone|9|0|0.001800|0.000000|

|clone3|0|10|0.000000|0.002000|

|close|13|39|0.002600|0.007800|

|connect|1|1|0.000200|0.000200|

|epoll_create1|1|2|0.000200|0.000400|

|epoll_ctl|10|12104|0.002000|2.420800|

|epoll_pwait|20109|965|4.021800|0.193000|

|epoll_wait|0|72710|0.000000|14.542000|

|eventfd2|1|3|0.000200|0.000600|

|execve|2|2|0.000400|0.000400|

|fcntl|34|36|0.006800|0.007200|

|fstat|3|18|0.000600|0.003600|

|futex|50476|30533|10.095200|6.106600|

|getpeername|1|0|0.000200|0.000000|

|getpid|45|450|0.009000|0.090000|

|getrandom|3|3|0.000600|0.000600|

|getrusage|5|5|0.001000|0.001000|

|getsockname|1|0|0.000200|0.000000|

|getsockopt|1|1|0.000200|0.000200|

|gettid|19|74|0.003800|0.014800|

|io_setup|0|1|0.000000|0.000200|

|ioctl|0|3|0.000000|0.000600|

|lseek|0|10|0.000000|0.002000|

|madvise|37|129|0.007400|0.025800|

|mmap|59|202|0.011800|0.040400|

|mprotect|3|5689|0.000600|1.137800|

|munmap|1|13|0.000200|0.002600|

|nanosleep|28464|29563|5.692800|5.912600|

|newfstatat|1|16|0.000200|0.003200|

|openat|13|49|0.002600|0.009800|

|poll|0|1|0.000000|0.000200|

|prctl|51|38|0.010200|0.007600|

|pread64|2|4|0.000400|0.000800|

|prlimit64|3|5|0.000600|0.001000|

|read|21197|921|4.239400|0.184200|

|readlinkat|1|1|0.000200|0.000200|

|recvfrom|0|20838|0.000000|4.167600|

|rseq|1|12|0.000200|0.002400|

|rt_sigaction|114|114|0.022800|0.022800|

|rt_sigprocmask|29|63005|0.005800|12.601000|

|rt_sigreturn|44|423|0.008800|0.084600|

|sched_getaffinity|2|3|0.000400|0.000600|

|sched_setaffinity|1|1|0.000200|0.000200|

|sched_yield|1589|68|0.317800|0.013600|

|sendmsg|0|10419|0.000000|2.083800|

|set_robust_list|1|12|0.000200|0.002400|

|set_tid_address|1|2|0.000200|0.000400|

|setsockopt|9|1|0.001800|0.000200|

|sigaltstack|20|22|0.004000|0.004400|

|socket|1|1|0.000200|0.000200|

|stat|0|3|0.000000|0.000600|

|tgkill|44|423|0.008800|0.084600|

|timerfd_create|0|1|0.000000|0.000200|

|timerfd_settime|0|25688|0.000000|5.137600|

|uname|0|1|0.000000|0.000200|

|write|10513|63|2.102600|0.012600|


## sequence-wholeprocess-over-200

|Syscall|NEW calls|CGo calls|NEW normalized|CGo normalized|

|---|---:|---:|---:|---:|

|clock_nanosleep|0|0|0.000000|0.000000|

|epoll_ctl|10|1024|0.050000|5.120000|

|epoll_pwait|1633|140|8.165000|0.700000|

|epoll_wait|0|5692|0.000000|28.460000|

|futex|4178|2884|20.890000|14.420000|

|getrusage|5|5|0.025000|0.025000|

|nanosleep|2313|3224|11.565000|16.120000|

|pread64|2|4|0.010000|0.020000|

|pwrite64|0|0|0.000000|0.000000|

|read|1956|915|9.780000|4.575000|

|readv|0|0|0.000000|0.000000|

|recvfrom|0|1630|0.000000|8.150000|

|recvmmsg|0|0|0.000000|0.000000|

|recvmsg|0|0|0.000000|0.000000|

|sched_yield|161|42|0.805000|0.210000|

|sendmmsg|0|0|0.000000|0.000000|

|sendmsg|0|813|0.000000|4.065000|

|sendto|0|0|0.000000|0.000000|

|write|821|12|4.105000|0.060000|

|writev|0|0|0.000000|0.000000|


## sequence-measured-window-over-200

|Syscall|NEW calls|CGo calls|NEW normalized|CGo normalized|

|---|---:|---:|---:|---:|

|clock_nanosleep|0|0|0.000000|0.000000|

|epoll_ctl|0|514|0.000000|2.570000|

|epoll_pwait|798|51|3.990000|0.255000|

|epoll_wait|0|2796|0.000000|13.980000|

|futex|1922|1322|9.610000|6.610000|

|getrusage|0|0|0.000000|0.000000|

|nanosleep|1061|1470|5.305000|7.350000|

|pread64|0|0|0.000000|0.000000|

|pwrite64|0|0|0.000000|0.000000|

|read|800|0|4.000000|0.000000|

|readv|0|0|0.000000|0.000000|

|recvfrom|0|802|0.000000|4.010000|

|recvmmsg|0|0|0.000000|0.000000|

|recvmsg|0|0|0.000000|0.000000|

|sched_yield|67|0|0.335000|0.000000|

|sendmmsg|0|0|0.000000|0.000000|

|sendmsg|0|401|0.000000|2.005000|

|sendto|0|0|0.000000|0.000000|

|write|400|0|2.000000|0.000000|

|writev|0|0|0.000000|0.000000|


## Perf

Whole-process counters / 5000, never phase counts. Unsupported/not-counted events remain null, not zero; task-clock is not phase getrusage CPU.


|Event|NEW value|CGo value|NEW / 5000|CGo / 5000|Unit|

|---|---:|---:|---:|---:|---|

|context-switches|42474|40150|8.4948|8.03|count|

|cpu-migrations|54|33|0.0108|0.0066|count|

|task-clock|1889.49|2201.91|0.377898|0.440382|msec|

|syscalls:sys_enter_futex|41406|22950|8.2812|4.59|count|

|syscalls:sys_enter_write|10509|57|2.1018|0.0114|count|

|syscalls:sys_enter_read|21199|917|4.2398|0.1834|count|

|syscalls:sys_enter_epoll_pwait|21015|214|4.203|0.0428|count|

|syscalls:sys_enter_nanosleep|11388|8088|2.2776|1.6176|count|


## Trace evidence

Each sequence arm has markers.json, whole-process-counts.csv, measured-window-counts.csv, measured-window-timeline.json/csv/txt, and boundary-overlaps.json. Timeline includes carry-in/carry-out overlaps and B/C reference markers; only rows with entry_in_measured_window=true contribute to measured counts. Original trace.<tid> and samples.csv are preserved under raw/strace-sequence/{new,cgo}/. No absolute timestamp is synthesized for samples.
