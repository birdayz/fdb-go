# Cluster used by this lab

Native fdbserver 7.3.77 (`raw/identity.txt` has the hash), three processes, each
pinned to its own CPU, started from `/var/tmp/netg-fdb` (one long-running
foreground command each):

```sh
echo "netg:netg@127.0.0.1:4751" > fdb.cluster
taskset -c 9  ./fdbserver -p 127.0.0.1:4751 -C fdb.cluster -d s/data -L s/logs --class stateless   --locality-machineid m1 --locality-zoneid z1
taskset -c 10 ./fdbserver -p 127.0.0.1:4752 -C fdb.cluster -d t/data -L t/logs --class transaction --locality-machineid m2 --locality-zoneid z2
taskset -c 11 ./fdbserver -p 127.0.0.1:4753 -C fdb.cluster -d d/data -L d/logs --class storage     --locality-machineid m3 --locality-zoneid z3
./fdbcli -C fdb.cluster --exec "configure new single memory"
```

The client ran on CPUs 1-8 with `GOMAXPROCS=8` (`run_cell.py --client-cpus`).
