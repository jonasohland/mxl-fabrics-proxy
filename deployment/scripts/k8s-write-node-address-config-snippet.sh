#! /bin/bash
#
# Usage: k8s-write-node-address-config-snippet.sh <provider> [output-file]
#
# Discover a node address usable with a fabrics provider and write it out as a
# proxy configuration snippet:
#
#   defaults:
#     node: <address>
#
# The first address reported for the provider that is neither loopback nor
# link-local is used. Exits non-zero when there is none, so that an init
# container fails loudly instead of letting the proxy come up bound to an
# address no peer can reach.
#
# Pass the snippet to the proxy as the last --config argument so it wins over a
# node set in the base configuration, and do not also pass --node: CLI values
# are merged last and would override the discovered address.

set -euo pipefail

info="${MXL_FABRICS_INFO:-mxl-fabrics-info}"
provider="${1:-${MXL_FABRICS_PROVIDER:-}}"
output="${2:-}"

fail() {
    echo "$(basename "$0"): $*" >&2
    exit 1
}

[[ -n "$provider" ]] || fail "usage: $(basename "$0") <provider> [output-file]"

# mxl-fabrics-info writes its log output to stdout and "No matching interfaces
# found." to stderr, and exits 0 in both cases, so both streams are kept for
# diagnostics and only lines shaped like an interface entry are parsed.
if ! interfaces="$("$info" -p "$provider" 2>&1)"; then
    echo "$interfaces" >&2
    fail "mxl-fabrics-info failed for provider '$provider'"
fi

node="$(awk '
    $1 == "interface" && $3 == "node" {
        if ($4 ~ /^127\./ || $4 == "::1" || $4 ~ /^fe80:/ || $4 ~ /^169\.254\./) {
            next
        }
        print $4
        exit
    }' <<<"$interfaces")"

[[ -n "$node" ]] || fail "no usable node address for provider '$provider' in:
$interfaces"

snippet="# Generated for provider '$provider'. Do not edit.
defaults:
  node: $node
"

if [[ -n "$output" ]]; then
    printf '%s' "$snippet" >"$output"
else
    printf '%s' "$snippet"
fi

echo "discovered node address '$node' for provider '$provider'" >&2
