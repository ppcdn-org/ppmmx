#!/usr/bin/env bash
# Raises net.core.rmem_max/wmem_max so mmx's udpReadBufferSize (WebRTC ICE
# UDP socket, see internal/servers/webrtc/server.go) isn't silently capped
# by the kernel. setsockopt(SO_RCVBUF) never errors when the requested size
# exceeds rmem_max - it just clamps to whatever rmem_max allows, which is
# what caused "requested 2097152, got 212992" on this host's default Ubuntu
# sysctl config.
#
# Also raises net.core.netdev_max_backlog, the per-CPU queue of packets the
# kernel holds between the NIC driver and the protocol stack. Its default of
# 1000 is easily overrun by a multi-layer simulcast ingest burst (e.g. 3 x
# 5Mbps SRT/WHIP layers), and packets dropped there never reach the socket
# at all - so no amount of SO_RCVBUF or SRT latency tuning can recover them.
#
# Usage: sudo ./tune-udp-buffers.sh [size_bytes] [netdev_backlog]
#   size_bytes defaults to 8388608 (8MB). Pass 0 to only print the current
#   values without changing anything.
#   netdev_backlog defaults to 5000.

set -euo pipefail

TARGET_BYTES="${1:-8388608}"
NETDEV_BACKLOG="${2:-5000}"
SYSCTL_FILE=/etc/sysctl.d/99-mmx-udp-buffers.conf

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (sudo $0 $*)" >&2
  exit 1
fi

echo "current values:"
sysctl net.core.rmem_max net.core.rmem_default net.core.wmem_max net.core.wmem_default net.core.netdev_max_backlog

if [[ "$TARGET_BYTES" -eq 0 ]]; then
  exit 0
fi

# sanity check: on a small VPS (this host has 1.6GB RAM), an oversized
# rmem_max multiplied across many UDP sockets can pressure memory. 8MB per
# socket is a reasonable ceiling for a handful of WebRTC/RTMP listeners;
# raise further only if you know how many concurrent sockets will use it.
if [[ "$TARGET_BYTES" -gt 67108864 ]]; then
  echo "refusing to set rmem_max/wmem_max above 64MB (got ${TARGET_BYTES}) - this is a safety guard, not a hard OS limit; edit the script if you really need more" >&2
  exit 1
fi

cat > "$SYSCTL_FILE" <<EOF
# Managed by tune-udp-buffers.sh - do not edit by hand, re-run the script instead.
# Raises the ceiling for setsockopt(SO_RCVBUF/SO_SNDBUF); mmx's
# udpReadBufferSize config (see bin/conf/origin.test.yml) can then request up
# to this many bytes without being silently clamped by the kernel.
net.core.rmem_max = ${TARGET_BYTES}
net.core.rmem_default = ${TARGET_BYTES}
net.core.wmem_max = ${TARGET_BYTES}
net.core.wmem_default = ${TARGET_BYTES}
# NIC-to-stack queue depth; default 1000 is too shallow for multi-layer
# simulcast ingest bursts. Drops here happen before the socket, so they are
# invisible to SO_RCVBUF/SRT latency tuning.
net.core.netdev_max_backlog = ${NETDEV_BACKLOG}
EOF

sysctl -p "$SYSCTL_FILE"

echo
echo "applied. new values:"
sysctl net.core.rmem_max net.core.rmem_default net.core.wmem_max net.core.wmem_default net.core.netdev_max_backlog

echo
echo "persisted to $SYSCTL_FILE (survives reboot via /etc/sysctl.d)."
echo "you can now raise udpReadBufferSize in your mmx config, e.g. to 2097152 (2MB),"
echo "as long as it stays <= ${TARGET_BYTES}."
