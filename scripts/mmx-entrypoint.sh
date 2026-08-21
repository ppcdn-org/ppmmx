#!/usr/bin/env bash
# systemd ExecStart wrapper: auto-detects this node's reachable address(es)
# for webrtcAdditionalHosts (see internal/conf/conf.go) before starting mmx,
# so a bare-metal node deployed to a new cloud VM/server doesn't need its
# YAML hand-edited every time - see scripts/detect-webrtc-hosts.sh for how
# detection works (AWS/Alibaba/Tencent Cloud metadata, primary NIC address,
# external echo services).
#
# Usage (systemd ExecStart, replacing a direct binary invocation):
#   ExecStart=/path/to/mmx-entrypoint.sh /path/to/mmx conf/origin.test.yml
#
# How the override reaches mmx: internal/conf/conf.go's Load() applies
# env.Load("MTX", conf) *after* parsing the YAML file, so setting
# MTX_WEBRTCADDITIONALHOSTS here overrides whatever's written in the YAML's
# webrtcAdditionalHosts field without needing a code change or a config
# file edit. This only touches that one field - every other setting still
# comes from the YAML as normal.
#
# Safety: if detection finds nothing (e.g. fully offline host, or a
# transient network hiccup at boot), MTX_WEBRTCADDITIONALHOSTS is left
# unset entirely, so mmx falls back to whatever's already in the YAML
# instead of starting with an empty/wrong address list. Detection failures
# are therefore never fatal to the service starting.
#
# MMX_ADDITIONAL_HOSTS_EXTRA (optional, comma-separated): addresses to
# always include on top of whatever's auto-detected, for cases detection
# can't cover (e.g. a DNS name, or a second NIC's address). Set it in the
# systemd unit's Environment= if needed.
#
# Important: this script never rewrites the YAML config file on disk - it
# only sets a process environment variable for this one run. Editing
# webrtcAdditionalHosts in the YAML and restarting therefore does NOT
# change what actually takes effect as long as detection keeps succeeding
# (which it reliably will on a host with working network/cloud metadata) -
# the detected value always wins. This is intentional (see conf.Load() doc
# above), not a bug: auto-rewriting the YAML in place would risk clobbering
# concurrent manual edits and silently diverging from what's checked into
# version control. To make the discrepancy visible instead of surprising,
# this script logs the YAML's own webrtcAdditionalHosts value alongside
# the value that's actually going to be used whenever they differ.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ $# -lt 1 ]]; then
	echo "usage: $0 <path-to-mmx-binary> [args...]" >&2
	exit 1
fi

BINARY="$1"
shift

# Last remaining arg is conventionally the YAML config path (matches how
# this script is invoked: mmx-entrypoint.sh <binary> <conf.yml>). Only
# used here to log what the file itself says, for the discrepancy warning
# below - never parsed for anything that affects startup behavior.
CONF_PATH="${*: -1}"

detected="$("$SCRIPT_DIR/detect-webrtc-hosts.sh" || true)"

combined="$detected"
if [[ -n "${MMX_ADDITIONAL_HOSTS_EXTRA:-}" ]]; then
	if [[ -n "$combined" ]]; then
		combined="${combined},${MMX_ADDITIONAL_HOSTS_EXTRA}"
	else
		combined="$MMX_ADDITIONAL_HOSTS_EXTRA"
	fi
fi

if [[ -n "$combined" ]]; then
	export MTX_WEBRTCADDITIONALHOSTS="$combined"
	echo "[mmx-entrypoint] MTX_WEBRTCADDITIONALHOSTS=$MTX_WEBRTCADDITIONALHOSTS" >&2

	# Best-effort extraction of the YAML's own webrtcAdditionalHosts line,
	# purely to warn if it disagrees with what's actually taking effect -
	# not a real YAML parser (multi-line list syntax isn't handled), and
	# never fatal if it can't find/parse the line.
	if [[ -n "${CONF_PATH:-}" && -f "$CONF_PATH" ]]; then
		yaml_value=$(grep -m1 -E '^[[:space:]]*webrtcAdditionalHosts[[:space:]]*:' "$CONF_PATH" 2>/dev/null | sed -E 's/^[[:space:]]*webrtcAdditionalHosts[[:space:]]*:[[:space:]]*//')
		if [[ -n "$yaml_value" && "$yaml_value" != *"$combined"* ]]; then
			echo "[mmx-entrypoint] NOTE: $CONF_PATH sets webrtcAdditionalHosts: $yaml_value, but auto-detection overrides it to: $combined (this script never rewrites the YAML file - see header comment)" >&2
		fi
	fi
else
	echo "[mmx-entrypoint] no addresses detected; leaving webrtcAdditionalHosts as configured in the YAML" >&2
fi

exec "$BINARY" "$@"
