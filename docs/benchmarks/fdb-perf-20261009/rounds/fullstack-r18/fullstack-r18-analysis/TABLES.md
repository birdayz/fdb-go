# R18 exploratory GOMAXPROCS diagnosis

18 uninstrumented processes; six blocks with all six arm orders, fixed seed798001. go8/go2 are the SAME frozen NEW executable with GOMAXPROCS=8/2; CGo remains8. Clients retain CPUs8–15. No confidence intervals, hypothesis tests, equivalence, sample-size extension, or threshold-selected shipping decision. No R17 observations pooled; these diagnostics cannot rescue an R17 endpoint.

Paired GM is exp(mean of six within-block log ratios), NOT a ratio of medians. Ranges describe the six observed ratios, NOT confidence bounds.

|Ratio|Metric|GM|Observed ratio range|Six ratios in block order|
|---|---|---:|---|---|

|go2/go8|mean_ns|1.034390588900604|0.9377916995665954–1.2964511677402646|0.970496, 1.057639, 0.952186, 1.296451, 0.937792, 1.030841|

|go2/go8|p99_ns|1.1572160082016143|0.7874445575702916–1.4415539243598101|1.299722, 0.997219, 1.355056, 1.441554, 0.787445, 1.204585|

|go2/go8|window_cpu_ns_per_op|0.9277387846773962|0.8869795386748456–1.0253709785948886|0.903613, 0.927105, 0.907060, 1.025371, 0.922597, 0.886980|

|go2/go8|window_user_cpu_ns_per_op|0.8551575235049531|0.7847636347384147–0.9105320466465517|0.851540, 0.882612, 0.845230, 0.910532, 0.861573, 0.784764|

|go2/go8|window_system_cpu_ns_per_op|0.9787193812343327|0.9384002428140031–1.1118948999312375|0.938400, 0.959525, 0.946966, 1.111895, 0.965349, 0.960334|

|go2/go8|drained_cpu_ns_per_op|0.9185731887970939|0.8764152189623152–1.0152967034184566|0.898106, 0.918851, 0.895721, 1.015297, 0.913343, 0.876415|

|go2/go8|drained_user_cpu_ns_per_op|0.8407420957304416|0.767419041204118–0.9016619332795047|0.848067, 0.866705, 0.822653, 0.901662, 0.844077, 0.767419|

|go2/go8|drained_system_cpu_ns_per_op|0.9768177324791658|0.9335780358520188–1.106132874800638|0.933578, 0.959525, 0.946193, 1.106133, 0.965349, 0.959849|

|go2/go8|shutdown_cpu_ns_per_op|0.9094582936806542|0.8660564111520768–1.0066818734832097|0.882685, 0.906723, 0.892245, 1.006682, 0.908853, 0.866056|

|go2/go8|shutdown_user_cpu_ns_per_op|0.8267315050990258|0.7456062987075464–0.893468051520148|0.831022, 0.853155, 0.812786, 0.893468, 0.831729, 0.745606|

|go2/go8|shutdown_system_cpu_ns_per_op|0.975242518196272|0.9220985459188734–1.1012227630900915|0.922099, 0.951352, 0.951596, 1.101223, 0.971194, 0.963659|

|go2/go8|go_alloc_bytes_per_op|0.9967473245100513|0.9951481274455003–0.9978694743061942|0.996742, 0.997633, 0.997869, 0.996019, 0.997075, 0.995148|

|go2/go8|go_mallocs_per_op|0.9991200648112458|0.9989149191087268–0.9994006445897807|0.999205, 0.999009, 0.999401, 0.999092, 0.999099, 0.998915|

|go8/cgo|mean_ns|0.6330289747695199|0.4665922643831444–0.7296061829982892|0.682046, 0.641702, 0.729606, 0.466592, 0.723157, 0.597222|

|go8/cgo|p99_ns|0.5573074323900402|0.40486703739517843–0.6649098354144249|0.619933, 0.553422, 0.664910, 0.404867, 0.553399, 0.586212|

|go8/cgo|window_cpu_ns_per_op|0.9342479181282328|0.6911927781392965–1.1118822770887853|1.006726, 0.952174, 1.111882, 0.691193, 0.998099, 0.904303|

|go8/cgo|window_user_cpu_ns_per_op|0.7832109257060362|0.5748567027195425–0.8849131769225533|0.808061, 0.829649, 0.884913, 0.574857, 0.877754, 0.771082|

|go8/cgo|window_system_cpu_ns_per_op|1.080574089070015|0.8155437291875217–1.3324536702802363|1.204573, 1.066989, 1.332454, 0.815544, 1.104157, 1.032295|

|go8/cgo|drained_cpu_ns_per_op|0.9470050093571414|0.7038096612009928–1.125012198902386|1.019648, 0.964707, 1.125012, 0.703810, 1.008736, 0.918074|

|go8/cgo|drained_user_cpu_ns_per_op|0.8158125426671826|0.6033170390404736–0.922787231958139|0.840570, 0.859712, 0.922787, 0.603317, 0.910458, 0.804838|

|go8/cgo|drained_system_cpu_ns_per_op|1.0765599441949523|0.8119148832852122–1.3256898178594916|1.201039, 1.066280, 1.325690, 0.811915, 1.097698, 1.028882|

|go8/cgo|shutdown_cpu_ns_per_op|0.9566862640969482|0.7216006048518311–1.1191317837258774|1.027210, 0.973753, 1.119132, 0.721601, 1.017355, 0.932950|

|go8/cgo|shutdown_user_cpu_ns_per_op|0.855034804434138|0.6359373226147579–0.9641435432230523|0.874886, 0.908405, 0.964144, 0.635937, 0.949793, 0.844281|

|go8/cgo|shutdown_system_cpu_ns_per_op|1.0572538118499688|0.8130587898453904–1.2718462171430975|1.184549, 1.035834, 1.271846, 0.813059, 1.079421, 1.019730|

|go8/cgo|go_alloc_bytes_per_op|4.728428056084939|4.722988833920794–4.733030106219406|4.724966, 4.730591, 4.722989, 4.728694, 4.730307, 4.733030|

|go8/cgo|go_mallocs_per_op|4.078927238929233|4.077982568059889–4.0797134864274955|4.078684, 4.079617, 4.077983, 4.078885, 4.078682, 4.079713|

|go2/cgo|mean_ns|0.6547992140029893|0.6049140860181017–0.6947205027437056|0.661923, 0.678689, 0.694721, 0.604914, 0.678171, 0.615641|

|go2/cgo|p99_ns|0.6449250822514934|0.43577101202608953–0.9009901133105604|0.805741, 0.551883, 0.900990, 0.583638, 0.435771, 0.706141|

|go2/cgo|window_cpu_ns_per_op|0.8667380281516743|0.7087290153184101–1.008544334263984|0.909690, 0.882765, 1.008544, 0.708729, 0.920843, 0.802098|

|go2/cgo|window_user_cpu_ns_per_op|0.6697687156087958|0.5234254500557134–0.7562489748717816|0.688097, 0.732259, 0.747956, 0.523425, 0.756249, 0.605117|

|go2/cgo|window_system_cpu_ns_per_op|1.0575788038324578|0.9067989131545078–1.2617877246017069|1.130371, 1.023803, 1.261788, 0.906799, 1.065897, 0.991349|

|go2/cgo|drained_cpu_ns_per_op|0.8698934112520111|0.7145756288514287–1.0076975074054515|0.915752, 0.886423, 1.007698, 0.714576, 0.921322, 0.804614|

|go2/cgo|drained_user_cpu_ns_per_op|0.6858879468451874|0.5439880078016999–0.7684964327831763|0.712860, 0.745116, 0.759134, 0.543988, 0.768496, 0.617648|

|go2/cgo|drained_system_cpu_ns_per_op|1.0516028435664106|0.8980857439416963–1.2543590770022588|1.121263, 1.023123, 1.254359, 0.898086, 1.059662, 0.987571|

|go2/cgo|shutdown_cpu_ns_per_op|0.8700662573333302|0.7264222487988586–0.9985394220322161|0.906703, 0.882925, 0.998539, 0.726422, 0.924627, 0.807987|

|go2/cgo|shutdown_user_cpu_ns_per_op|0.7068842107818862|0.5681896805255475–0.7899699877267692|0.727049, 0.775010, 0.783642, 0.568190, 0.789970, 0.629502|

|go2/cgo|shutdown_system_cpu_ns_per_op|1.0310788698411713|0.8953588471082268–1.2102841759764236|1.092271, 0.985443, 1.210284, 0.895359, 1.048327, 0.982672|

|go2/cgo|go_alloc_bytes_per_op|4.713048014040926|4.7095727683944055–4.719395494239317|4.709573, 4.719395, 4.712926, 4.709867, 4.716469, 4.710066|

|go2/cgo|go_mallocs_per_op|4.07533804731933|4.075008838131542–4.075573798213654|4.075440, 4.075574, 4.075538, 4.075180, 4.075009, 4.075287|


## Accounting

- window: (C-B)/N: measured-window process CPU, not complete lifecycle; deferred cleanup may cross boundaries.

- drained: (D-B)/N: through finalizer fixpoint/native FIFO barrier; includes forced GC and pending setup/warmup cleanup.

- shutdown: (E-A)/N: includes startup, fixture, warmup, validation, CSV, forced GC and shutdown; excludes prior identity hashing, final JSON and process exit; NOT isolated steady-state CPU.

- absolute_E: E/N: absolute/unpaired only; includes identity hashing; NEVER paired cross-arm client-cost evidence.


Go allocation counters exclude the native C++ heap. CPU/latency values are ns/transaction. Raw A–E integer snapshots, per-run values and all paired values are in CSV/JSON. Absolute E/N is unpaired only.


## Absolute descriptive statistics

|Arm|Metric|Median of six run statistics|Observed minimum|Observed maximum|
|---|---|---:|---:|---:|

|go8|mean_ns|327674.2941|268549.1368|341476.4244|

|go8|p99_ns|465722.0|388127|567706|

|go8|window_cpu_ns_per_op|304132.1|279989.2|310185.2|

|go8|window_user_cpu_ns_per_op|123398.70000000001|119594.6|130746.4|

|go8|window_system_cpu_ns_per_op|180619.9|159680.2|185302.2|

|go8|drained_cpu_ns_per_op|312978.1|288820.4|319399.0|

|go8|drained_user_cpu_ns_per_op|131753.4|128102.8|139960.2|

|go8|drained_system_cpu_ns_per_op|180619.9|160512.0|185453.4|

|go8|shutdown_cpu_ns_per_op|337877.4|312666.8|345132.4|

|go8|shutdown_user_cpu_ns_per_op|147903.8|142282.2|156856.6|

|go8|shutdown_system_cpu_ns_per_op|188728.5|170384.6|193514.2|

|go8|go_alloc_bytes_per_op|7291.3408|7278.5792|7299.12|

|go8|go_mallocs_per_op|53.0715|53.057|53.0836|

|go8|absolute_E_cpu_ns_per_op|343141.7|318226.8|350203.6|

|go2|mean_ns|320011.5467|312531.0384|348160.842|

|go2|p99_ns|550831.5|447037|627216|

|go2|window_cpu_ns_per_op|280897.9|265998.6|287574.4|

|go2|window_user_cpu_ns_per_op|106503.6|98329.0|115398.4|

|go2|window_system_cpu_ns_per_op|173825.4|167669.6|177547.6|

|go2|drained_cpu_ns_per_op|285787.0|270686.8|293480.2|

|go2|drained_user_cpu_ns_per_op|112558.6|102768.0|121304.2|

|go2|drained_system_cpu_ns_per_op|173825.4|167918.8|177547.6|

|go2|shutdown_cpu_ns_per_op|306274.2|289586.4|314756.0|

|go2|shutdown_user_cpu_ns_per_op|124284.6|111594.0|133823.0|

|go2|shutdown_system_cpu_ns_per_op|181424.1|176634.8|187631.4|

|go2|go_alloc_bytes_per_op|7263.5976|7263.072|7273.776|

|go2|go_mallocs_per_op|53.0245|53.0224|53.028|

|go2|absolute_E_cpu_ns_per_op|311276.0|294641.6|319984.8|

|cgo|mean_ns|495386.5155|449987.8198|575554.1986|

|cgo|p99_ns|814839.0|692345|1025853|

|cgo|window_cpu_ns_per_op|317760.30000000005|274216.8|405081.2|

|cgo|window_user_cpu_ns_per_op|153976.0|135148.4|209285.2|

|cgo|window_system_cpu_ns_per_op|166412.2|139068.4|195796.0|

|cgo|drained_cpu_ns_per_op|323312.4|278713.6|410367.2|

|cgo|drained_user_cpu_ns_per_op|158485.9|138821.6|212671.6|

|cgo|drained_system_cpu_ns_per_op|166952.5|139892.0|197695.6|

|cgo|shutdown_cpu_ns_per_op|345348.9|302072.2|433296.2|

|cgo|shutdown_user_cpu_ns_per_op|169855.1|149920.0|223736.2|

|cgo|shutdown_system_cpu_ns_per_op|178196.4|152152.2|209560.0|

|cgo|go_alloc_bytes_per_op|1542.176|1541.0672|1542.208|

|cgo|go_mallocs_per_op|13.0115|13.0098|13.0118|

|cgo|absolute_E_cpu_ns_per_op|356939.80000000005|313585.4|444974.6|


## Separate trace population — NEVER timing evidence

Each trace is200 measured+200warmup, after all18 timings. Exact unique chronological A–E CPU-us/RSS signatures define [B return,C entry). Counts are syscall ENTRY counts, not completions or syscall-duration apportionment. Only phase counts are divided by200. samples.csv has a relative epoch just after B, with unknown submillisecond offset: no exact transaction alignment. Instrumentation perturbs scheduling; no CPU/latency improvement or causal attribution follows from these traces.


go8: whole selected entries=11108; phase entries=4955; thread-name observation records=0. See per-TID count tables, marker evidence and merged timeline.


go2: whole selected entries=9113; phase entries=4048; thread-name observation records=0. See per-TID count tables, marker evidence and merged timeline.


|Syscall|go8 whole entries|go2 whole entries|go8 phase entries|go2 phase entries|go8 phase/200|go2 phase/200|
|---|---:|---:|---:|---:|---:|---:|

|clock_nanosleep|0|0|0|0|0.000000|0.000000|

|epoll_ctl|10|10|0|0|0.000000|0.000000|

|epoll_pwait|1627|1590|788|779|3.940000|3.895000|

|epoll_wait|0|0|0|0|0.000000|0.000000|

|futex|4098|2532|1870|1139|9.350000|5.695000|

|getrusage|5|5|0|0|0.000000|0.000000|

|nanosleep|2447|2038|1051|864|5.255000|4.320000|

|pread64|2|2|0|0|0.000000|0.000000|

|pwrite64|0|0|0|0|0.000000|0.000000|

|read|1956|1961|800|801|4.000000|4.005000|

|readv|0|0|0|0|0.000000|0.000000|

|recvfrom|0|0|0|0|0.000000|0.000000|

|recvmmsg|0|0|0|0|0.000000|0.000000|

|recvmsg|0|0|0|0|0.000000|0.000000|

|sched_yield|142|150|46|64|0.230000|0.320000|

|sendmmsg|0|0|0|0|0.000000|0.000000|

|sendmsg|0|0|0|0|0.000000|0.000000|

|sendto|0|0|0|0|0.000000|0.000000|

|write|821|825|400|401|2.000000|2.005000|

|writev|0|0|0|0|0.000000|0.000000|


One reimaged existing instance/fresh cluster, not independent host replication. Discrete raw telemetry is descriptive; no new during-run rejection thresholds. Benchmark diagnostics are not source/review/CI correctness approval or merge permission.
