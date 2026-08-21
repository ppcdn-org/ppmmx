#!/usr/bin/env bash
# Detects addresses to advertise as WebRTC ICE candidates when the node's
# real-world reachable address(es) don't match what the OS reports on its
# network interfaces (NAT'd cloud VMs, no public IP bound directly to the
# NIC, etc.) - i.e. the same problem webrtcAdditionalHosts (see
# internal/conf/conf.go) exists to solve, but resolved automatically at
# service start instead of hand-edited into each node's YAML.
#
# Usage: ./detect-webrtc-hosts.sh
#   Prints a comma-separated list of addresses to stdout (e.g.
#   "172.40.172.245,95.40.233.159"), or nothing (with warnings on stderr)
#   if every detection method failed. Never fails hard: this is a
#   best-effort helper meant to feed mmx-entrypoint.sh, not a
#   correctness-critical step.
#
# Detection sources, all attempted (not "first match wins" - a node can
# genuinely need more than one address, e.g. its cloud-assigned private/VPC
# IP as configured on the NIC *and* a separate NAT'd public IP that no
# metadata service reports, which is exactly the case on one of our AWS
# nodes: AWS's own public-ipv4 metadata is empty there, but the box is
# still reachable from the internet on a different NAT address):
#   1. Cloud metadata services (AWS, Alibaba Cloud, Tencent Cloud) - each
#      probed with a short timeout; providers this host isn't running on
#      simply don't answer and are skipped, not treated as fatal.
#   2. The primary network interface's own IPv4 (covers OVH dedicated/bare
#      metal servers and similar setups with no NAT and no metadata
#      service at all, where the NIC address already *is* the public one).
#   3. External echo services (ifconfig.me, ipify, icanhazip) as a
#      catch-all for NAT'd setups with no cloud metadata service (or a
#      metadata service that, like the AWS case above, doesn't happen to
#      expose a public IP) - this is intentionally always attempted, not
#      only as a fallback when step 1 finds nothing.

set -uo pipefail

CURL_OPTS=(--silent --show-error --max-time 2 --connect-timeout 1)

log() { echo "[detect-webrtc-hosts] $*" >&2; }

# Metadata endpoints return an HTML "404 Not Found" body (not an empty
# string) for a field that doesn't apply to this instance (observed for
# AWS's public-ipv4 on a node with no Elastic IP attached) - a bare
# [[ -n "$ip" ]] check would let that HTML through as a bogus "address".
# Every value pulled from a metadata service or echo service is validated
# against this before being accepted.
is_valid_ipv4() {
	[[ "$1" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]
}

# Fetches $2 with curl (args in $3+), prints it only if it looks like a
# real IPv4 address. Used by every cloud-metadata detector below so the
# "is this actually an IP" check lives in one place.
fetch_ip() {
	local url="$1"
	shift
	local ip
	ip=$(curl "${CURL_OPTS[@]}" "$@" "$url" 2>/dev/null) || return 1
	is_valid_ipv4 "$ip" || return 1
	echo "$ip"
}

# ---- 1. Cloud metadata services ----

detect_aws() {
	local token
	token=$(curl "${CURL_OPTS[@]}" -X PUT "http://169.254.169.254/latest/api/token" \
		-H "X-aws-ec2-metadata-token-ttl-seconds: 60" 2>/dev/null) || return 1
	[[ -n "$token" ]] || return 1
	fetch_ip "http://169.254.169.254/latest/meta-data/public-ipv4" \
		-H "X-aws-ec2-metadata-token: $token" || true
	fetch_ip "http://169.254.169.254/latest/meta-data/local-ipv4" \
		-H "X-aws-ec2-metadata-token: $token" || true
	return 0
}

detect_aliyun() {
	local token
	token=$(curl "${CURL_OPTS[@]}" -X PUT "http://100.100.100.200/latest/api/token" \
		-H "X-aliyun-ecs-metadata-token-ttl-seconds: 60" 2>/dev/null) || return 1
	[[ -n "$token" ]] || return 1
	fetch_ip "http://100.100.100.200/latest/meta-data/eipv4" \
		-H "X-aliyun-ecs-metadata-token: $token" || true
	fetch_ip "http://100.100.100.200/latest/meta-data/public-ipv4" \
		-H "X-aliyun-ecs-metadata-token: $token" || true
	fetch_ip "http://100.100.100.200/latest/meta-data/private-ipv4" \
		-H "X-aliyun-ecs-metadata-token: $token" || true
	return 0
}

# Tencent Cloud CVM metadata doesn't require a token (IMDSv1-style only).
detect_tencent() {
	fetch_ip "http://metadata.tencentyun.com/latest/meta-data/public-ipv4" || true
	fetch_ip "http://metadata.tencentyun.com/latest/meta-data/local-ipv4" || true
	return 0
}

# ---- 2. Primary network interface IPv4 ----
# Covers OVH dedicated/bare-metal servers and similar hosts where the NIC
# address is directly the internet-facing one (no NAT, no metadata
# service). Restricted to the interface used by the default route, so
# Docker bridges/VPN tunnels/loopback on a multi-NIC box aren't picked up.
detect_primary_iface_ip() {
	local iface
	iface=$(ip -4 route show default 2>/dev/null | awk '/default/ {for (i=1;i<=NF;i++) if ($i=="dev") print $(i+1)}' | head -n1)
	[[ -n "$iface" ]] || return 1
	ip -4 addr show dev "$iface" scope global 2>/dev/null | grep -oP '(?<=inet\s)\d+(\.\d+){3}'
}

# ---- 3. External echo services ----
# Always attempted (not just as a fallback): a cloud metadata service can
# be reachable but simply not expose a public IP (observed on an AWS node
# in this fleet - see the module doc comment above), so this is the only
# source that reliably reports "what does the internet actually see".
detect_echo_service() {
	local ip
	for url in "https://ifconfig.me" "https://api.ipify.org" "https://icanhazip.com"; do
		ip=$(curl "${CURL_OPTS[@]}" "$url" 2>/dev/null | tr -d '[:space:]') || continue
		if is_valid_ipv4 "$ip"; then
			echo "$ip"
			return 0
		fi
	done
	return 1
}

# ---- run all detectors, collect + dedupe ----

results=()

for detector in detect_aws detect_aliyun detect_tencent detect_primary_iface_ip detect_echo_service; do
	while IFS= read -r addr; do
		[[ -n "$addr" ]] && results+=("$addr")
	done < <("$detector" 2>/dev/null || true)
done

if [[ ${#results[@]} -eq 0 ]]; then
	log "no addresses detected from any source (cloud metadata, local interface, or external echo service); leaving webrtcAdditionalHosts untouched"
	exit 0
fi

# dedupe while preserving order
declare -A seen
unique=()
for addr in "${results[@]}"; do
	if [[ -z "${seen[$addr]:-}" ]]; then
		seen[$addr]=1
		unique+=("$addr")
	fi
done

IFS=,
echo "${unique[*]}"
