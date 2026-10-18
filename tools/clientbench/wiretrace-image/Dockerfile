# tcpdump for scripts/wirecapture.sh, run with --net=host and NET_RAW so it
# can capture loopback traffic without installing tcpdump on the host.
FROM alpine:3.22
RUN apk add --no-cache tcpdump
ENTRYPOINT ["tcpdump"]
