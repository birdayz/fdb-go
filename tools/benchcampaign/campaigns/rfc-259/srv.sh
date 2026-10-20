#!/bin/sh
# usage: srv.sh <binary> <workload> <iterations> -> server CPU us/tx (whole run incl. warmup and setup)
SPID=1867988
t() { awk '{print $14+$15}' /proc/$SPID/stat; }
a=$(t); ./run.sh "$1" "$2" 32 "$3" srvtmp; b=$(t)
python3 -c "import sys; ticks=$b-$a; n=$3+$3//20; print('$(basename $1) $2 server_cpu_us_per_tx=%.1f' % (ticks*1e6/100/n))"
