set -euo pipefail

info="${MXL_FABRICS_INFO:-mxl-fabrics-info}"
provider="${1:-${MXL_FABRICS_PROVIDER:-}}"
output="${2:-}"

fail() {
  echo "$(basename "$0"): $*" >&2
  exit 1
}

[[ -n "$provider" ]] || fail "usage: $(basename "$0") <provider> [output-file]"

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
